// Package snapshot records the file changes a shell command makes, so
// rewind can undo them.
//
// It keeps a hidden git repository under Ta-Da's data directory, separate
// from the user's own repository. A snapshot stages the work tree into
// that repository's private index and writes it as a tree; comparing the
// trees from before and after a command gives the files it changed. The
// user's .git, index and branches are never read or written, and
// .gitignore'd files are left out.
//
// Snapshotting is best-effort. When git is missing, slow or failing, or
// the folder is too big to index, the command runs as usual and its
// result carries a short note saying its changes weren't recorded.
//
// Commands can overlap. Within one session, rows are never written twice
// for the same content, and a file the edit tools wrote while a command
// ran keeps the rows they wrote. When another session runs a command in
// the same folder at the same time, in this process or another Ta-Da
// process, neither command records anything, since there is no telling
// whose changes are whose.
package snapshot

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/lock"
)

const (
	// MaxFileSize is the largest file rewind keeps content for.
	MaxFileSize = 1 << 20

	// firstTimeout covers the first snapshot of a work tree, which hashes
	// every file.
	firstTimeout = 20 * time.Second
	// timeout covers later snapshots, which only rehash changed files.
	timeout = 5 * time.Second
	// maxFailures is how many snapshots in a row may fail before a work
	// tree stops being snapshotted for the rest of the process.
	maxFailures = 2
	// staleLock is how old an index.lock must be before it is taken for
	// one a crashed git left behind.
	staleLock = time.Minute
)

// These are variables so tests can lower them.
var (
	// maxFiles is the most files a folder may hold for its changes to be
	// recorded. Bigger folders take too long to index.
	maxFiles = 50_000
	// bigFile is the size above which a file found while counting is
	// left out of snapshots, so it isn't copied into the hidden
	// repository.
	bigFile int64 = 16 << 20
	// warmWait is how long a command waits for a folder's first index
	// before running unrecorded.
	warmWait = 2 * time.Second
	// gcEvery is how many snapshots pass between prunes of the hidden
	// repository.
	gcEvery = 50
	// pin keeps each open command's before tree reachable, so a prune
	// can't drop it. Tests turn it off to show it matters.
	pin = true
)

// attributes overrides the work tree's .gitattributes so blobs hold the
// exact bytes on disk: no line ending conversion, filters or keyword
// expansion.
const attributes = "* -text -filter -ident -working-tree-encoding\n"

// openRefs is where the before trees of running commands are pinned, as
// <pid>-<token>-<id>. A command that starts while another process's
// command is open marks that one under taintRefs, by the same name.
const (
	openRefs  = "refs/tada/open/"
	taintRefs = "refs/tada/taint/"
)

// History is the part of the file history service snapshots record into.
type History interface {
	Create(ctx context.Context, sessionID, path, content string) (history.File, error)
	CreateNew(ctx context.Context, sessionID, path string) (history.File, error)
	CreateAbsent(ctx context.Context, sessionID, path string) (history.File, error)
	CreateVersion(ctx context.Context, sessionID, path, content string) (history.File, error)
	GetByPathAndSession(ctx context.Context, path, sessionID string) (history.File, error)
	Mark(ctx context.Context) (int64, error)
	ListPathSince(ctx context.Context, path string, mark int64) ([]history.File, error)
}

// store is the hidden repository for one work tree.
type store struct {
	root    string
	gitDir  string
	dataDir string
	// lock serialises git runs on the shared index within the process. It
	// is a channel so a wait can give up when the context ends. A file
	// lock covers other processes.
	lock chan struct{}
	// recording serialises writing history rows, so overlapping commands
	// see each other's rows.
	recording sync.Mutex

	warmOnce sync.Once
	warmDone chan struct{}

	mu        sync.Mutex
	disabled  string
	big       []string
	ready     bool
	warmed    bool
	failures  int
	open      map[int64]*window
	nextID    int64
	snapshots int
	gcRunning bool
	noted     map[string]bool
}

// window is a command running in the work tree.
type window struct {
	session string
	tainted bool
}

var (
	storesMu sync.Mutex
	stores   = map[string]*store{}
)

// storeFor returns the repository for root, kept under dataDir.
func storeFor(dataDir, root string) *store {
	sum := sha256.Sum256([]byte(root))
	gitDir := filepath.Join(dataDir, "snapshots", hex.EncodeToString(sum[:8]))
	storesMu.Lock()
	defer storesMu.Unlock()
	s, ok := stores[gitDir]
	if !ok {
		s = &store{
			root:     root,
			gitDir:   gitDir,
			dataDir:  dataDir,
			lock:     make(chan struct{}, 1),
			warmDone: make(chan struct{}),
			open:     map[int64]*window{},
			noted:    map[string]bool{},
		}
		stores[gitDir] = s
	}
	return s
}

// Background work (indexing, pruning) is counted so tests and shutdown can
// wait for it. bgIdle is closed whenever none runs. Its git runs use bgCtx,
// which Shutdown cancels.
var (
	bgMu            sync.Mutex
	bgN             int
	bgIdle          = closed()
	bgCtx, bgCancel = context.WithCancel(context.Background())
)

func closed() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

func goBackground(f func()) {
	bgMu.Lock()
	if bgN == 0 {
		bgIdle = make(chan struct{})
	}
	bgN++
	bgMu.Unlock()
	go func() {
		defer func() {
			bgMu.Lock()
			bgN--
			if bgN == 0 {
				close(bgIdle)
			}
			bgMu.Unlock()
		}()
		f()
	}()
}

func idle() <-chan struct{} {
	bgMu.Lock()
	defer bgMu.Unlock()
	return bgIdle
}

// Idle waits until no indexing or pruning runs in the background.
func Idle() {
	<-idle()
}

// Wait is Idle bounded by ctx: it returns when background work is done or
// ctx ends, whichever is first.
func Wait(ctx context.Context) {
	select {
	case <-idle():
	case <-ctx.Done():
	}
}

// Shutdown waits for background work until ctx ends, then kills the git
// runs still going and waits briefly for them to exit, so none outlives
// the process. Background work started later fails at once.
func Shutdown(ctx context.Context) {
	Wait(ctx)
	bgCancel()
	t := time.NewTimer(2 * time.Second)
	defer t.Stop()
	select {
	case <-idle():
	case <-t.C:
	}
}

// Warm starts indexing root in the background, so the first command run
// there doesn't wait for it. Call it before anything that may call Wait,
// so Wait sees it. It does nothing when files is nil.
func Warm(files History, dataDir, root string) {
	if files == nil || dataDir == "" || root == "" {
		return
	}
	storeFor(dataDir, root).startWarm()
}

func (s *store) startWarm() {
	s.warmOnce.Do(func() {
		goBackground(func() {
			defer close(s.warmDone)
			s.warm()
		})
	})
}

// warm checks the folder can be recorded and takes its first snapshot.
func (s *store) warm() {
	if _, err := exec.LookPath("git"); err != nil {
		s.disable("git isn't installed")
		return
	}
	if reason := s.refuse(); reason != "" {
		s.disable(reason)
		return
	}
	reason, err := s.guard(bgCtx)
	if err != nil {
		slog.Warn("Failed to count files for shell change recording", "root", s.root, "error", err)
	}
	if reason != "" {
		s.disable(reason)
		return
	}
	if _, _, err := s.snapshot(bgCtx, ""); err != nil {
		slog.Warn("Failed to index folder for shell change recording", "root", s.root, "error", err)
	}
}

func (s *store) disable(reason string) {
	slog.Info("Not recording shell changes in this folder", "root", s.root, "reason", reason)
	s.mu.Lock()
	s.disabled = reason
	s.mu.Unlock()
}

// disabledReason says why the folder is never recorded, or "".
func (s *store) disabledReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled != "" {
		return s.disabled
	}
	if s.failures >= maxFailures {
		return "snapshots of this folder kept failing"
	}
	return ""
}

// refuse refuses folders too broad to snapshot: the home folder and the
// file system root.
func (s *store) refuse() string {
	real := s.root
	if r, err := filepath.EvalSymlinks(real); err == nil {
		real = r
	}
	real = filepath.Clean(real)
	if filepath.Dir(real) == real {
		return "the folder is the root of the file system"
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if h, err := filepath.EvalSymlinks(home); err == nil {
			home = h
		}
		if filepath.Clean(home) == real {
			return "the folder is your home folder"
		}
	}
	return ""
}

// guard refuses folders with more than maxFiles files that snapshots
// would include, so ignored files don't count. It also notes files larger
// than bigFile, to leave them out. It returns a reason when the folder is
// refused; an error means the files couldn't be counted.
func (s *store) guard(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, firstTimeout)
	defer cancel()
	release, err := s.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	// init writes the exclude file without the big files, so they are
	// listed and found again.
	if err := s.init(ctx); err != nil {
		return "", err
	}

	list, cancelList := context.WithCancel(ctx)
	defer cancelList()
	cmd := s.command(list, nil, "ls-files", "-z", "-c", "-o", "--exclude-standard")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	r := bufio.NewReader(out)
	count := 0
	var big []string
	for {
		rel, err := r.ReadString(0)
		if err != nil {
			break
		}
		count++
		if count > maxFiles {
			// Stop git; its exit error is expected.
			cancelList()
			_ = cmd.Wait()
			return fmt.Sprintf("the folder has more than %d files", maxFiles), nil
		}
		rel = strings.TrimSuffix(rel, "\x00")
		if bigFile > 0 {
			info, err := os.Lstat(filepath.Join(s.root, filepath.FromSlash(rel)))
			if err == nil && info.Mode().IsRegular() && info.Size() > bigFile {
				big = append(big, rel)
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("git ls-files: %w", err)
	}
	s.mu.Lock()
	s.big = big
	s.mu.Unlock()
	return "", s.writeExcludes()
}

// Run is a command's recording window: the snapshot taken before it,
// waiting for the one after.
type Run struct {
	s       *store
	files   History
	session string
	id      int64
	mark    int64
	before  string
	// note says why the command isn't recorded; quiet means it isn't, but
	// the session was already told why.
	note  string
	quiet bool
}

// Begin snapshots root before a command run by sessionID. It returns nil
// when files is nil; Finish on a nil Run does nothing.
func Begin(ctx context.Context, files History, dataDir, root, sessionID string) *Run {
	if files == nil || dataDir == "" || root == "" {
		return nil
	}
	s := storeFor(dataDir, root)
	s.startWarm()
	r := &Run{s: s, files: files, session: sessionID, id: s.register(sessionID)}

	wait := time.NewTimer(warmWait)
	defer wait.Stop()
	select {
	case <-s.warmDone:
	case <-wait.C:
		r.note = "Ta-Da is still indexing this folder"
		return r
	case <-ctx.Done():
		r.note = "the command was cancelled"
		return r
	}

	if reason := s.disabledReason(); reason != "" {
		if s.firstNote(sessionID) {
			r.note = reason
		} else {
			r.quiet = true
		}
		return r
	}

	mark, err := files.Mark(ctx)
	if err != nil {
		slog.Warn("Failed to read file history mark", "error", err)
		r.note = "file history couldn't be read"
		return r
	}
	r.mark = mark
	tree, foreign, err := s.snapshot(ctx, r.name())
	if err != nil {
		slog.Debug("Skipping shell change recording", "root", root, "error", err)
		r.note = "the folder couldn't be snapshotted before it ran"
		return r
	}
	// The pin stays even when another process overlaps, so that process
	// sees this command too.
	r.before = tree
	if foreign {
		r.note = overlapNote
	}
	return r
}

const overlapNote = "another session ran a command in this folder at the same time"

// owner names this process in pin refs: its pid, and a random token so a
// pin left by an earlier process with the same pid isn't taken for ours.
var owner = func() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return strconv.Itoa(os.Getpid()) + "-" + hex.EncodeToString(b)
}()

// name is the run's pin name, under openRefs and taintRefs.
func (r *Run) name() string {
	return owner + "-" + strconv.FormatInt(r.id, 10)
}

// ownPin reports whether a pin name belongs to this process.
func ownPin(name string) bool {
	return strings.HasPrefix(name, owner+"-")
}

// pinAlive reports whether the process that wrote a pin may still run.
// Pins in an older format, and pins with this process's pid but another
// token, are dead.
func pinAlive(name string) bool {
	if ownPin(name) {
		return true
	}
	pid, _, ok := strings.Cut(name, "-")
	if !ok {
		return false
	}
	n, err := strconv.Atoi(pid)
	if err != nil || n <= 0 || n == os.Getpid() {
		return false
	}
	return processAlive(n)
}

// pins lists the pin names under prefix.
func (s *store) pins(ctx context.Context, prefix string) ([]string, error) {
	out, err := s.git(ctx, nil, "for-each-ref", "--format=%(refname)", prefix)
	if err != nil {
		return nil, err
	}
	var names []string
	for ref := range strings.FieldsSeq(string(out)) {
		names = append(names, strings.TrimPrefix(ref, prefix))
	}
	return names, nil
}

// overlap pins tree as name and reports whether a command of another live
// process is open. Each such command is marked under taintRefs, so it
// learns of this one even if this one ends first. The caller holds the
// lock.
func (s *store) overlap(ctx context.Context, name, tree string) (bool, error) {
	if _, err := s.git(ctx, nil, "update-ref", openRefs+name, tree); err != nil {
		return false, err
	}
	names, err := s.pins(ctx, openRefs)
	if err != nil {
		return false, err
	}
	var taint strings.Builder
	for _, n := range names {
		if !ownPin(n) && pinAlive(n) {
			taint.WriteString("update " + taintRefs + n + " " + tree + "\n")
		}
	}
	if taint.Len() == 0 {
		return false, nil
	}
	_, err = s.git(ctx, strings.NewReader(taint.String()), "update-ref", "--stdin")
	return true, err
}

// sweep drops the pins and marks of processes that are gone. The caller
// holds the lock.
func (s *store) sweep(ctx context.Context) error {
	var drop strings.Builder
	for _, prefix := range []string{openRefs, taintRefs} {
		names, err := s.pins(ctx, prefix)
		if err != nil {
			return err
		}
		for _, n := range names {
			if !pinAlive(n) {
				drop.WriteString("delete " + prefix + n + "\n")
			}
		}
	}
	if drop.Len() == 0 {
		return nil
	}
	_, err := s.git(ctx, strings.NewReader(drop.String()), "update-ref", "--stdin")
	return err
}

// register opens a window for session. Windows of different sessions
// that overlap taint each other.
func (s *store) register(session string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	w := &window{session: session}
	for _, o := range s.open {
		if o.session != session {
			o.tainted = true
			w.tainted = true
		}
	}
	s.open[s.nextID] = w
	return s.nextID
}

// firstNote reports whether session hasn't yet been told why the folder
// isn't recorded.
func (s *store) firstNote(session string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.noted[session] {
		return false
	}
	s.noted[session] = true
	return true
}

func (s *store) tainted(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.open[id]
	return w != nil && w.tainted
}

// unpin reports whether a command of another live process overlapped r:
// one is open now, or one opened while r ran and marked it. With drop, it
// also drops r's pin and overlap mark; Finish keeps them until its
// changes are read.
func (s *store) unpin(ctx context.Context, r *Run, drop bool) (bool, error) {
	if r.before == "" || !pin {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	release, err := s.acquire(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	foreign := false
	for _, prefix := range []string{openRefs, taintRefs} {
		names, err := s.pins(ctx, prefix)
		if err != nil {
			return false, err
		}
		for _, n := range names {
			if prefix == taintRefs && n == r.name() ||
				prefix == openRefs && !ownPin(n) && pinAlive(n) {
				foreign = true
			}
		}
	}
	if !drop {
		return foreign, nil
	}
	refs := "delete " + openRefs + r.name() + "\ndelete " + taintRefs + r.name() + "\n"
	if _, err := s.git(ctx, strings.NewReader(refs), "update-ref", "--stdin"); err != nil {
		return foreign, err
	}
	r.before = ""
	return foreign, nil
}

// unregister closes r's window, unpins its before tree, and prunes the
// hidden repository now and then once no windows are open.
func (s *store) unregister(ctx context.Context, r *Run) {
	if _, err := s.unpin(ctx, r, true); err != nil {
		slog.Debug("Failed to unpin snapshot", "name", r.name(), "error", err)
	}
	s.mu.Lock()
	delete(s.open, r.id)
	start := len(s.open) == 0 && !s.gcRunning && s.snapshots >= gcEvery
	if start {
		s.gcRunning = true
		s.snapshots = 0
	}
	s.mu.Unlock()
	if start {
		goBackground(s.gc)
	}
}

// gc drops objects no snapshot needs any more. It skips the prune when a
// command started since it was scheduled.
func (s *store) gc() {
	defer func() {
		s.mu.Lock()
		s.gcRunning = false
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(bgCtx, firstTimeout)
	defer cancel()
	release, err := s.acquire(ctx)
	if err != nil {
		return
	}
	defer release()
	s.mu.Lock()
	busy := len(s.open) > 0
	s.mu.Unlock()
	if busy {
		return
	}
	// Other processes' open commands stay pinned, so the prune keeps
	// their snapshots.
	if err := s.sweep(ctx); err != nil {
		slog.Debug("Failed to drop stale shell snapshot pins", "root", s.root, "error", err)
		return
	}
	if _, err := s.git(ctx, nil, "prune", "--expire=now"); err != nil {
		slog.Debug("Failed to prune shell snapshots", "root", s.root, "error", err)
	}
}

// notRecorded is the note for a command whose changes weren't recorded.
func notRecorded(reason string) string {
	return "Note: rewind didn't record this command's file changes: " + reason + "."
}

// Finish snapshots the work tree again and records every file that
// changed since Begin as file history for the session. It returns a note
// for the tool result about changes rewind can't undo, or "" when there
// are none.
func (r *Run) Finish(ctx context.Context) string {
	if r == nil {
		return ""
	}
	// Record even when the command was cancelled: it may have changed
	// files before it stopped.
	ctx = context.WithoutCancel(ctx)
	s := r.s
	defer s.unregister(ctx, r)
	if r.quiet {
		return ""
	}
	if r.note != "" {
		return notRecorded(r.note)
	}
	after, _, err := s.snapshot(ctx, "")
	if err != nil {
		slog.Warn("Failed to snapshot files after shell command", "root", s.root, "error", err)
		return notRecorded("the folder couldn't be snapshotted after it ran")
	}
	foreign, err := s.unpin(ctx, r, false)
	if err != nil {
		slog.Warn("Failed to check for overlapping shell commands", "root", s.root, "error", err)
		return notRecorded("the folder couldn't be snapshotted after it ran")
	}
	if foreign || s.tainted(r.id) {
		return notRecorded(overlapNote)
	}
	if after == r.before {
		return ""
	}
	changes, err := s.changes(ctx, r.before, after)
	if err != nil {
		slog.Warn("Failed to read shell command changes", "root", s.root, "error", err)
		return notRecorded("its changes couldn't be read")
	}

	s.recording.Lock()
	defer s.recording.Unlock()
	var skipped, others []string
	for _, c := range changes {
		if c.skip != "" {
			slog.Info("Rewind can't restore shell change", "path", c.path, "reason", c.skip)
			skipped = append(skipped, c.rel)
			continue
		}
		since, err := r.files.ListPathSince(ctx, c.path, r.mark)
		if err != nil {
			slog.Warn("Failed to read file history", "path", c.path, "error", err)
			skipped = append(skipped, c.rel)
			continue
		}
		if wroteByOther(since, r.session) {
			others = append(others, c.rel)
			continue
		}
		if err := record(ctx, r.files, r.session, c, since); err != nil {
			slog.Warn("Failed to record shell change", "path", c.path, "error", err)
			skipped = append(skipped, c.rel)
		}
	}
	var notes []string
	if n := skippedNote(skipped); n != "" {
		notes = append(notes, n)
	}
	if len(others) > 0 {
		notes = append(notes, "Note: rewind didn't record these files, which another session also changed while this command ran: "+list(others))
	}
	return strings.Join(notes, "\n")
}

func wroteByOther(rows []history.File, session string) bool {
	for _, f := range rows {
		if f.SessionID != session {
			return true
		}
	}
	return false
}

// skippedNote names the files rewind can't restore.
func skippedNote(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return "Note: rewind can't restore these changed files (binary, too large, or not regular files): " + list(paths)
}

func list(paths []string) string {
	const show = 10
	s := strings.Join(paths[:min(len(paths), show)], ", ")
	if len(paths) > show {
		s += fmt.Sprintf(" and %d more", len(paths)-show)
	}
	return s
}

// change is one file a command changed. A nil side means the file didn't
// exist.
type change struct {
	path   string
	rel    string
	before *string
	after  *string
	skip   string
}

// record writes the history rows the edit tools would write for the same
// change, so rewind reads them the same way. since holds the session's
// rows for the path written while the command ran, by the edit tools or
// an overlapping command; they already cover the before side, and no row
// repeats the newest one.
func record(ctx context.Context, files History, sessionID string, c change, since []history.File) error {
	var latest history.File
	known := len(since) > 0
	if known {
		latest = since[len(since)-1]
	} else {
		var err error
		latest, err = files.GetByPathAndSession(ctx, c.path, sessionID)
		known = err == nil
		if c.before == nil {
			// Created. Mark the file as absent before it, as the edit tools
			// do for a new file; a session that already has versions gets
			// a new absent version, since the first version number is
			// taken.
			switch {
			case !known:
				if latest, err = files.CreateNew(ctx, sessionID, c.path); err != nil {
					return err
				}
			case latest.Origin != history.OriginAbsent:
				if latest, err = files.CreateAbsent(ctx, sessionID, c.path); err != nil {
					return err
				}
			}
		} else {
			// Changed or deleted. Keep what was there before, as edit does,
			// including a change made since the session last wrote it.
			switch {
			case !known:
				if latest, err = files.Create(ctx, sessionID, c.path, *c.before); err != nil {
					return err
				}
			case latest.Origin == history.OriginAbsent || latest.Content != *c.before:
				if latest, err = files.CreateVersion(ctx, sessionID, c.path, *c.before); err != nil {
					return err
				}
			}
		}
	}

	var err error
	if c.after == nil {
		if latest.Origin != history.OriginAbsent {
			_, err = files.CreateAbsent(ctx, sessionID, c.path)
		}
	} else if latest.Origin == history.OriginAbsent || latest.Content != *c.after {
		_, err = files.CreateVersion(ctx, sessionID, c.path, *c.after)
	}
	return err
}

// acquire takes the hidden repository's lock, in this process and across
// processes.
func (s *store) acquire(ctx context.Context) (func(), error) {
	select {
	case s.lock <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	dir := filepath.Join(s.gitDir, "tada")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		<-s.lock
		return nil, err
	}
	unlock, err := lock.File(ctx, filepath.Join(dir, "lock"))
	if err != nil {
		<-s.lock
		return nil, err
	}
	return func() {
		unlock()
		<-s.lock
	}, nil
}

// snapshot stages the work tree and returns its tree hash. When name isn't
// "", the tree is pinned under it before the lock is let go, so a prune
// can't drop it, and foreign reports whether another live process has a
// command open.
func (s *store) snapshot(ctx context.Context, name string) (tree string, foreign bool, err error) {
	s.mu.Lock()
	limit := timeout
	if !s.warmed {
		limit = firstTimeout
	}
	s.mu.Unlock()

	// Waiting for the lock and running git get a deadline each, so a long
	// wait, such as for a prune, doesn't kill git.
	wait, cancel := context.WithTimeout(ctx, limit)
	release, err := s.acquire(wait)
	cancel()
	if err != nil {
		return "", false, err
	}
	defer release()

	run, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	tree, err = s.take(run)
	if err == nil && name != "" && pin {
		foreign, err = s.overlap(run, name, tree)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		lockBusy := strings.Contains(err.Error(), "index.lock")
		// A killed git leaves its lock behind, which would fail every
		// later snapshot. The index is ours, and we hold its locks. A lock
		// we only waited on may be held by a git still running, so it
		// stays until it is stale.
		if run.Err() != nil && !lockBusy {
			_ = os.Remove(filepath.Join(s.gitDir, "index.lock"))
		}
		// Only real git failures count: not a cancelled command, a
		// shutdown, or a lock held by someone else.
		if ctx.Err() == nil && !lockBusy {
			s.failures++
			if s.failures == maxFailures {
				slog.Warn("Turning off shell change recording for this folder", "root", s.root, "error", err)
			}
		}
		return "", false, err
	}
	s.failures = 0
	s.warmed = true
	s.snapshots++
	return tree, foreign, nil
}

func (s *store) take(ctx context.Context) (string, error) {
	if err := s.init(ctx); err != nil {
		return "", err
	}
	indexLock := filepath.Join(s.gitDir, "index.lock")
	if info, err := os.Stat(indexLock); err == nil && time.Since(info.ModTime()) > staleLock {
		_ = os.Remove(indexLock)
	}
	var err error
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
				// Still the lock error, so the lock isn't removed.
				return "", err
			}
		}
		if _, err = s.git(ctx, nil, "add", "-A"); err == nil || !strings.Contains(err.Error(), "index.lock") {
			break
		}
	}
	if err != nil {
		return "", err
	}
	out, err := s.git(ctx, nil, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// init creates the repository and its config, ignore and attribute files,
// and drops pins left by processes that stopped mid-command. The caller
// holds the lock.
func (s *store) init(ctx context.Context) error {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if ready {
		return nil
	}
	// An empty global config and hooks folder of our own stand in for
	// /dev/null, which Windows lacks.
	if err := os.MkdirAll(filepath.Join(s.gitDir, "tada", "hooks"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.gitDir, "tada", "config"), nil, 0o600); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(s.gitDir, "HEAD")); err != nil {
		if _, err := s.git(ctx, nil, "init", "-q"); err != nil {
			return err
		}
	}
	// The untracked cache lets add -A skip folders whose listing hasn't
	// changed since the last snapshot.
	for _, kv := range [][2]string{
		{"core.untrackedCache", "true"},
		{"core.preloadIndex", "true"},
	} {
		if _, err := s.git(ctx, nil, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	info := filepath.Join(s.gitDir, "info")
	if err := os.MkdirAll(info, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(info, "attributes"), []byte(attributes), 0o600); err != nil {
		return err
	}
	if err := s.writeExcludes(); err != nil {
		return err
	}
	if err := s.sweep(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	return nil
}

func (s *store) writeExcludes() error {
	return os.WriteFile(filepath.Join(s.gitDir, "info", "exclude"), []byte(s.excludes()), 0o600)
}

// excludes leaves out the data directory, when it sits in the work tree,
// files too big to keep, and whatever the user's own repository excludes
// locally.
func (s *store) excludes() string {
	var b strings.Builder
	if rel, err := filepath.Rel(s.root, s.dataDir); err == nil && rel != "." &&
		rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		b.WriteString("/" + filepath.ToSlash(rel) + "/\n")
	}
	s.mu.Lock()
	big := s.big
	s.mu.Unlock()
	for _, rel := range big {
		b.WriteString("/" + escapePattern(rel) + "\n")
	}
	if own, err := os.ReadFile(filepath.Join(s.root, ".git", "info", "exclude")); err == nil {
		b.Write(own)
		b.WriteString("\n")
	}
	return b.String()
}

// escapePattern makes a slash-separated path match only itself as an
// ignore pattern.
func escapePattern(rel string) string {
	var b strings.Builder
	for _, r := range rel {
		if strings.ContainsRune(`\*?[`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	out := b.String()
	if trimmed := strings.TrimRight(out, " "); len(trimmed) < len(out) {
		out = trimmed + strings.Repeat(`\ `, len(out)-len(trimmed))
	}
	return out
}

// changes lists the files that differ between two trees.
func (s *store) changes(ctx context.Context, before, after string) ([]change, error) {
	out, err := s.git(ctx, nil, "diff-tree", "-r", "-z", "--no-renames", "--raw", before, after)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(string(out), "\x00")
	var (
		list  []change
		blobs []string
		sides [][2]string
	)
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) < 5 {
			continue
		}
		oldMode, newMode, oldSHA, newSHA := meta[0], meta[1], meta[2], meta[3]
		rel := fields[i+1]
		c := change{path: filepath.Join(s.root, filepath.FromSlash(rel)), rel: rel}
		if !fileMode(oldMode) || !fileMode(newMode) {
			c.skip = "not a regular file"
			list = append(list, c)
			sides = append(sides, [2]string{})
			continue
		}
		if oldSHA == newSHA {
			// Only the mode changed; there is no content to restore.
			continue
		}
		var side [2]string
		if oldMode != absentMode {
			side[0] = oldSHA
			blobs = append(blobs, oldSHA)
		}
		if newMode != absentMode {
			side[1] = newSHA
			blobs = append(blobs, newSHA)
		}
		list = append(list, c)
		sides = append(sides, side)
	}

	contents, err := s.blobs(ctx, blobs)
	if err != nil {
		return nil, err
	}
	for i := range list {
		c := &list[i]
		if c.skip != "" {
			continue
		}
		for j, sha := range sides[i] {
			if sha == "" {
				continue
			}
			text, ok := contents[sha]
			if !ok {
				c.skip = "binary or larger than 1 MiB"
				break
			}
			if j == 0 {
				c.before = &text
			} else {
				c.after = &text
			}
		}
	}
	return list, nil
}

const absentMode = "000000"

// fileMode reports whether a tree mode is a regular file or no file.
func fileMode(mode string) bool {
	return mode == absentMode || mode == "100644" || mode == "100755"
}

// blobs reads the text blobs among shas. Blobs that are too large or not
// text are left out.
func (s *store) blobs(ctx context.Context, shas []string) (map[string]string, error) {
	out := map[string]string{}
	if len(shas) == 0 {
		return out, nil
	}
	check, err := s.git(ctx, strings.NewReader(strings.Join(shas, "\n")+"\n"), "cat-file", "--batch-check")
	if err != nil {
		return nil, err
	}
	var small []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(check)), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[1] != "blob" {
			continue
		}
		if size, err := strconv.ParseInt(f[2], 10, 64); err == nil && size <= MaxFileSize {
			small = append(small, f[0])
		}
	}
	if len(small) == 0 {
		return out, nil
	}
	raw, err := s.git(ctx, strings.NewReader(strings.Join(small, "\n")+"\n"), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	r := bufio.NewReader(bytes.NewReader(raw))
	for {
		header, err := r.ReadString('\n')
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		f := strings.Fields(header)
		if len(f) != 3 {
			return nil, fmt.Errorf("unexpected cat-file header %q", header)
		}
		size, err := strconv.Atoi(f[2])
		if err != nil {
			return nil, err
		}
		body := make([]byte, size+1)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, err
		}
		body = body[:size]
		if bytes.IndexByte(body, 0) >= 0 || !utf8.Valid(body) {
			continue
		}
		out[f[0]] = string(body)
	}
	return out, nil
}

// git runs git on the hidden repository. Inherited GIT_* variables are
// dropped so nothing can point it at another repository or index, no
// system or user config is read, and git runs no programs on its own:
// no hooks or file system monitor, and no content rewriting.
func (s *store) git(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := s.command(ctx, stdin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("git %s: %w", args[0], ctx.Err())
		}
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// command builds the hardened git run that git starts.
func (s *store) command(ctx context.Context, stdin io.Reader, args ...string) *exec.Cmd {
	safe := []string{
		"-c", "core.quotepath=off",
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=" + filepath.Join(s.gitDir, "tada", "hooks"),
		"-c", "core.autocrlf=false",
		"-c", "core.safecrlf=false",
		"-c", "gc.auto=0",
		"-c", "maintenance.auto=false",
		"-c", "advice.addEmbeddedRepo=false",
	}
	cmd := exec.CommandContext(ctx, "git", append(safe, args...)...)
	cmd.Dir = s.root
	env := make([]string, 0, len(os.Environ())+8)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env,
		"GIT_DIR="+s.gitDir,
		"GIT_WORK_TREE="+s.root,
		"GIT_INDEX_FILE="+filepath.Join(s.gitDir, "index"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+filepath.Join(s.gitDir, "tada", "config"),
		"GIT_TERMINAL_PROMPT=0",
	)
	cmd.Stdin = stdin
	return cmd
}
