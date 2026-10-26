package rewind

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

// reasonModified is shown for files that changed after the session last
// wrote them.
const (
	reasonModified = "changed since Ta-Da last wrote it"
	reasonLinked   = "a folder on its path now links outside the project"
)

// fileStep is one planned file change.
type fileStep struct {
	change  FileChange
	content string
	// root is the project folder the file sits under by name, if any.
	// Writes there go through os.Root, so a folder swapped for a link
	// after planning still can't lead them outside.
	root string
	// project is set when root is the project root.
	project bool
}

// Rewind turns the session back to just before the turn that starts with
// req.MessageID. With req.DryRun it only reports what it would do.
func (s *Service) Rewind(ctx context.Context, sessionID string, req Request) (Result, error) {
	if req.Mode == "" {
		req.Mode = ModeBoth
	}
	if req.Scope == "" {
		req.Scope = ScopeAll
	}
	if !req.DryRun && s.isBusy(sessionID) {
		return Result{}, ErrBusy
	}
	if err := s.messages.FlushAll(ctx); err != nil {
		return Result{}, fmt.Errorf("flushing messages: %w", err)
	}
	sess, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		return Result{}, fmt.Errorf("loading session: %w", err)
	}
	msgs, err := s.messages.List(ctx, sessionID)
	if err != nil {
		return Result{}, fmt.Errorf("listing messages: %w", err)
	}
	idx := slices.IndexFunc(msgs, func(m message.Message) bool {
		return m.ID == req.MessageID && isTurnStart(m)
	})
	if idx < 0 {
		return Result{}, ErrNotFound
	}
	anchor := msgs[idx]

	var res Result
	if req.Mode != ModeChat {
		steps, err := s.planFiles(ctx, sess, newPlacer(msgs), idx, req)
		if err != nil {
			return Result{}, err
		}
		for _, st := range steps {
			if !req.DryRun && !st.change.Skipped {
				if err := s.applyFile(ctx, sessionID, st); err != nil {
					slog.Warn("Failed to rewind file", "path", st.change.Path, "error", err)
					st.change.Skipped = true
					st.change.Reason = err.Error()
				}
			}
			res.Files = append(res.Files, st.change)
		}
	}
	if req.Mode != ModeFiles {
		res.MessagesDeleted = len(msgs) - idx
		res.Prompt = promptOf(anchor)
		if !req.DryRun {
			if err := s.trimChat(ctx, sess, msgs, idx); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

// planFiles works out what each file the session changed at or after
// msgs[start] should go back to.
//
// The target is the session's newest version recorded before start. When
// there is none, the session's first version is the state before its first
// change. Versions are placed by the message that wrote them; older ones
// only have second resolution, so a change made in the same second as the
// prompt counts as part of the turn.
func (s *Service) planFiles(ctx context.Context, sess session.Session, at placer, start int, req Request) ([]fileStep, error) {
	files, err := s.history.ListBySession(ctx, sess.ID)
	if err != nil {
		return nil, fmt.Errorf("listing file history: %w", err)
	}
	groups := byPath(files)
	paths := make([]string, 0, len(groups))
	for p := range groups {
		paths = append(paths, p)
	}
	slices.Sort(paths)

	var steps []fileStep
	for _, path := range paths {
		rs := groups[path]
		if !slices.ContainsFunc(rs, func(f history.File) bool { return !at.before(f, start) }) {
			continue
		}
		if req.Scope == ScopeOutside && s.insideProject(sess, path) {
			continue
		}
		if s.linksOutside(sess, path) {
			// Writing here would follow the link out of the project, so
			// skip it even when forced.
			steps = append(steps, fileStep{change: FileChange{
				Path: path, Op: OpRestore, Skipped: true, Reason: reasonLinked,
			}})
			continue
		}
		target, targetFirst := rs[0], true
		for i, f := range rs {
			if at.before(f, start) {
				target, targetFirst = f, i == 0
			}
		}
		wantExists := existed(target, targetFirst)
		last := rs[len(rs)-1]
		lastExists := existed(last, len(rs) == 1)

		st := fileStep{change: FileChange{Path: path, Op: OpRestore}, content: target.Content}
		st.root, st.project = s.rootFor(sess, path)
		if !wantExists {
			st.change.Op = OpDelete
		}
		disk, err := os.ReadFile(path)
		onDisk := err == nil
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			st.change.Skipped = true
			st.change.Reason = err.Error()
			steps = append(steps, st)
			continue
		}
		if onDisk == wantExists && (!wantExists || string(disk) == target.Content) {
			// Already as it was.
			continue
		}
		modified := onDisk != lastExists || (onDisk && string(disk) != last.Content)
		if modified && !req.Force {
			st.change.Skipped = true
			st.change.Reason = reasonModified
		}
		steps = append(steps, st)
	}
	return steps, nil
}

// existed reports whether a version stands for a file that was on disk.
// Versions from before origins were tracked count as a created file when
// they are the session's first and empty, since that's how tools used to
// record new files.
func existed(f history.File, first bool) bool {
	switch f.Origin {
	case history.OriginAbsent:
		return false
	case history.OriginExisted:
		return true
	default:
		return !first || f.Content != ""
	}
}

// applyFile carries out one file step and records it in history.
func (s *Service) applyFile(ctx context.Context, sessionID string, st fileStep) error {
	path := st.change.Path
	switch st.change.Op {
	case OpDelete:
		if err := s.removeFile(st); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if _, err := s.history.CreateAbsent(ctx, sessionID, path); err != nil {
			slog.Warn("Failed to record rewound file", "path", path, "error", err)
		}
	default:
		if err := s.writeFile(st); err != nil {
			return err
		}
		if _, err := s.history.CreateVersion(ctx, sessionID, path, st.content); err != nil {
			slog.Warn("Failed to record rewound file", "path", path, "error", err)
		}
	}
	// The agent's memory of having read the file no longer matches the
	// disk, and read times only have second resolution, so recording a new
	// read could let a stale edit through. Forget the read; the agent has
	// to view the file again before editing it.
	if err := s.q.DeleteFileRead(ctx, db.DeleteFileReadParams{
		SessionID: sessionID,
		Path:      s.storedPath(path),
	}); err != nil {
		slog.Warn("Failed to clear file read", "path", path, "error", err)
	}
	return nil
}

// removeFile deletes the step's file, inside its root when it has one.
func (s *Service) removeFile(st fileStep) error {
	if st.root == "" {
		return os.Remove(st.change.Path)
	}
	r, rel, err := s.openRoot(st)
	if err != nil {
		return err
	}
	defer r.Close()
	return r.Remove(rel)
}

// writeFile writes the step's content, inside its root when it has one.
func (s *Service) writeFile(st fileStep) error {
	if st.root == "" {
		return writeFileAtomic(st.change.Path, []byte(st.content))
	}
	r, rel, err := s.openRoot(st)
	if err != nil {
		return err
	}
	defer r.Close()
	return writeInRoot(r, rel, []byte(st.content))
}

// openRoot opens the step's root and returns the file's path inside it.
func (s *Service) openRoot(st fileStep) (*os.Root, string, error) {
	rel, err := filepath.Rel(st.root, s.absPath(st.change.Path))
	if err != nil {
		return nil, "", err
	}
	r, err := os.OpenRoot(st.root)
	if err != nil {
		return nil, "", err
	}
	// os.Root guards only below the root, so make sure the project
	// folder itself wasn't swapped for a link since startup.
	if st.project && s.rootInfo != nil {
		info, err := r.Stat(".")
		if err != nil || !os.SameFile(info, s.rootInfo) {
			r.Close()
			return nil, "", errors.New("the project folder was moved or replaced")
		}
	}
	return r, rel, nil
}

// writeInRoot is writeFileAtomic for a path inside r. os.Root refuses any
// step that would follow a link out of r, at the moment of each call.
func writeInRoot(r *os.Root, rel string, data []byte) error {
	mode := fs.FileMode(0o644)
	if info, err := r.Stat(rel); err == nil {
		mode = info.Mode().Perm()
	}
	dir := filepath.Dir(rel)
	if err := r.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	name := filepath.Join(dir, ".tada-rewind-"+rand.Text())
	f, err := r.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer r.Remove(name) //nolint:errcheck
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return r.Rename(name, rel)
}

// writeFileAtomic writes data next to path and renames it into place,
// keeping the existing file's mode.
func writeFileAtomic(path string, data []byte) error {
	mode := fs.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tada-rewind-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) //nolint:errcheck
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// roots returns the project root and the session's working folder.
func (s *Service) roots(sess session.Session) []string {
	roots := []string{s.projectRoot}
	if sess.WorkingDir != "" {
		wd := sess.WorkingDir
		if !filepath.IsAbs(wd) && s.projectRoot != "" {
			wd = filepath.Join(s.projectRoot, wd)
		}
		roots = append(roots, wd)
	}
	return roots
}

// absPath makes a stored path absolute against the project root.
func (s *Service) absPath(path string) string {
	if !filepath.IsAbs(path) && s.projectRoot != "" {
		path = filepath.Join(s.projectRoot, path)
	}
	return filepath.Clean(path)
}

// rootFor returns the root that path sits under by name, either as given
// or with links resolved, or "". project reports whether it is the
// project root.
func (s *Service) rootFor(sess session.Session, path string) (root string, project bool) {
	path = s.absPath(path)
	for i, r := range s.roots(sess) {
		if r == "" {
			continue
		}
		for _, form := range []string{filepath.Clean(r), canonical(r)} {
			if under(form, path) {
				return form, i == 0
			}
		}
	}
	return "", false
}

// insideProject reports whether path is under the session's working folder
// or the project root.
func (s *Service) insideProject(sess session.Session, path string) bool {
	roots := s.roots(sess)
	if !filepath.IsAbs(path) && s.projectRoot != "" {
		path = filepath.Join(s.projectRoot, path)
	}
	p := canonical(path)
	for _, root := range roots {
		if root == "" {
			continue
		}
		rel, err := filepath.Rel(canonical(root), p)
		if err != nil {
			continue
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// linksOutside reports whether path sits under a project root by name, but
// one of its folders is now a symlink that leads outside that root. Shell
// commands can swap a folder for such a link after a file was recorded.
func (s *Service) linksOutside(sess session.Session, path string) bool {
	roots := s.roots(sess)
	if !filepath.IsAbs(path) && s.projectRoot != "" {
		path = filepath.Join(s.projectRoot, path)
	}
	path = filepath.Clean(path)
	dir := canonical(filepath.Dir(path))
	for _, root := range roots {
		if root == "" || (!under(filepath.Clean(root), path) && !under(canonical(root), path)) {
			continue
		}
		if !under(canonical(root), dir) {
			return true
		}
	}
	return false
}

// under reports whether p is root or inside it.
func under(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// canonical resolves symlinks in p. When p doesn't exist it resolves the
// deepest folder that does and adds the rest back, so a link higher up
// still counts.
func canonical(p string) string {
	p = filepath.Clean(p)
	tail := ""
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, tail)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, tail)
		}
		tail = filepath.Join(filepath.Base(p), tail)
		p = parent
	}
}

// trimChat deletes msgs[idx:] and everything that hangs off them.
func (s *Service) trimChat(ctx context.Context, sess session.Session, msgs []message.Message, idx int) error {
	cut := msgs[idx:]
	deleted := make(map[string]bool, len(cut))
	calls := map[string]bool{}
	ids := make([]string, 0, len(cut))
	for _, m := range cut {
		deleted[m.ID] = true
		ids = append(ids, m.ID)
		for _, tc := range m.ToolCalls() {
			calls[tc.ID] = true
		}
	}

	// Sub-agent sessions belong to the tool calls being removed.
	kids, err := s.q.ListChildSessions(ctx, sql.NullString{String: sess.ID, Valid: true})
	if err != nil {
		slog.Warn("Failed to list child sessions", "session_id", sess.ID, "error", err)
	}
	for _, k := range kids {
		msgID, callID, ok := s.sessions.ParseAgentToolSessionID(k.ID)
		if calls[k.ID] || (ok && (deleted[msgID] || calls[callID])) {
			if err := s.sessions.Delete(ctx, k.ID); err != nil {
				slog.Warn("Failed to delete child session", "session_id", k.ID, "error", err)
			}
		}
	}

	// Newest first, so a failure part way leaves a clean prefix.
	for i := len(cut) - 1; i >= 0; i-- {
		if err := s.messages.Delete(ctx, cut[i].ID); err != nil {
			return fmt.Errorf("deleting message: %w", err)
		}
	}

	s.cleanMagicContext(ctx, sess.ID, ids)

	if sess.SummaryMessageID != "" && deleted[sess.SummaryMessageID] {
		newest := ""
		for _, m := range msgs[:idx] {
			if m.IsSummaryMessage {
				newest = m.ID
			}
		}
		fresh, err := s.sessions.Get(ctx, sess.ID)
		if err != nil {
			return fmt.Errorf("reloading session: %w", err)
		}
		fresh.SummaryMessageID = newest
		if _, err := s.sessions.Save(ctx, fresh); err != nil {
			return fmt.Errorf("saving session: %w", err)
		}
	}
	return nil
}

// cleanMagicContext drops magic-context rows for deleted messages, and any
// compartment that summarized them, so the context engine rebuilds from
// what is left. It is best-effort: failures are logged.
func (s *Service) cleanMagicContext(ctx context.Context, sessionID string, ids []string) {
	if s.db == nil || len(ids) == 0 {
		return
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		slog.Warn("Failed to clean magic context", "error", err)
		return
	}
	defer tx.Rollback() //nolint:errcheck

	first := int64(-1)
	for _, id := range ids {
		var ord int64
		err := tx.QueryRowContext(ctx,
			`SELECT ordinal FROM mc_ordinals WHERE session_id = ? AND message_id = ?`,
			sessionID, id).Scan(&ord)
		if err == nil && (first < 0 || ord < first) {
			first = ord
		}
		for _, q := range []string{
			`DELETE FROM mc_tags WHERE session_id = ? AND message_id = ?`,
			`DELETE FROM mc_message_index WHERE session_id = ? AND message_id = ?`,
			`DELETE FROM mc_ordinals WHERE session_id = ? AND message_id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, sessionID, id); err != nil {
				slog.Warn("Failed to clean magic context", "error", err)
				return
			}
		}
	}
	if first >= 0 {
		for _, q := range []string{
			`DELETE FROM mc_compartments WHERE session_id = ? AND end_ordinal >= ?`,
			`DELETE FROM mc_recomp_compartments WHERE session_id = ? AND end_ordinal >= ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, sessionID, first); err != nil {
				slog.Warn("Failed to clean magic context", "error", err)
				return
			}
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE mc_session_meta SET render_epoch = render_epoch + 1 WHERE session_id = ?`,
		sessionID); err != nil {
		slog.Warn("Failed to clean magic context", "error", err)
		return
	}
	if err := tx.Commit(); err != nil {
		slog.Warn("Failed to clean magic context", "error", err)
	}
}
