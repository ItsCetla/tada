package snapshot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// These tests change package settings, so they don't run in parallel.

func newHistory(t *testing.T) (history.Service, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	q := db.New(conn)
	sess, err := session.NewService(q, conn).Create(t.Context(), "snapshot")
	require.NoError(t, err)
	return history.NewService(q, conn), sess.ID
}

func set[T any](t *testing.T, v *T, to T) {
	t.Helper()
	old := *v
	*v = to
	t.Cleanup(func() { *v = old })
}

func contents(t *testing.T, files history.Service, sessionID, path string) []string {
	t.Helper()
	all, err := files.ListBySession(t.Context(), sessionID)
	require.NoError(t, err)
	var out []string
	for _, f := range all {
		if f.Path == path {
			out = append(out, f.Content)
		}
	}
	return out
}

func TestGuardRefusesHomeFolder(t *testing.T) {
	files, sessionID := newHistory(t)
	root := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Cleanup(Idle)

	r := Begin(t.Context(), files, dataDir, root, sessionID)
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("a\n"), 0o644))
	require.Equal(t, "Note: rewind didn't record this command's file changes: the folder is your home folder.", r.Finish(t.Context()))

	// The session is told once.
	r = Begin(t.Context(), files, dataDir, root, sessionID)
	require.Empty(t, r.Finish(t.Context()))
	require.Empty(t, contents(t, files, sessionID, filepath.Join(root, "a.txt")))
}

func TestGuardRefusesFolderWithTooManyFiles(t *testing.T) {
	files, sessionID := newHistory(t)
	set(t, &maxFiles, 5)
	root := t.TempDir()
	for i := range 6 {
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("f%d", i)), nil, 0o644))
	}
	// Files under .git don't count.
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	t.Cleanup(Idle)

	r := Begin(t.Context(), files, t.TempDir(), root, sessionID)
	require.Contains(t, r.Finish(t.Context()), "the folder has more than 5 files.")

	// One file fewer is fine.
	require.NoError(t, os.Remove(filepath.Join(root, "f0")))
	r = Begin(t.Context(), files, t.TempDir(), root, sessionID)
	require.NoError(t, os.WriteFile(filepath.Join(root, "f1"), []byte("x\n"), 0o644))
	require.Empty(t, r.Finish(t.Context()))
	require.Equal(t, []string{"", "x\n"}, contents(t, files, sessionID, filepath.Join(root, "f1")))
}

func TestBigFilesAreLeftOut(t *testing.T) {
	files, sessionID := newHistory(t)
	set(t, &bigFile, 8)
	root := t.TempDir()
	big := filepath.Join(root, "big [1].txt")
	require.NoError(t, os.WriteFile(big, []byte("0123456789\n"), 0o644))
	t.Cleanup(Idle)

	r := Begin(t.Context(), files, t.TempDir(), root, sessionID)
	require.NoError(t, os.WriteFile(big, []byte("changed\n"), 0o644))
	require.Empty(t, r.Finish(t.Context()))
	require.Empty(t, contents(t, files, sessionID, big))
}

// prune drops every object nothing references, as a prune running while
// a command is open would.
func prune(t *testing.T, s *store) {
	t.Helper()
	_, err := s.git(t.Context(), nil, "prune", "--expire=now")
	require.NoError(t, err)
}

func TestPruneKeepsOpenCommandsSnapshot(t *testing.T) {
	files, sessionID := newHistory(t)
	root := t.TempDir()
	dataDir := t.TempDir()
	path := filepath.Join(root, "a.txt")
	require.NoError(t, os.WriteFile(path, []byte("one\n"), 0o644))
	t.Cleanup(Idle)

	r := Begin(t.Context(), files, dataDir, root, sessionID)
	require.Empty(t, r.note)
	// Another command's snapshot moves the index on, so only the pin
	// keeps the first content.
	require.NoError(t, os.WriteFile(path, []byte("two\n"), 0o644))
	_, _, err := r.s.snapshot(t.Context(), "")
	require.NoError(t, err)
	prune(t, r.s)

	require.Empty(t, r.Finish(t.Context()))
	require.Equal(t, []string{"one\n", "two\n"}, contents(t, files, sessionID, path))
	out, err := r.s.git(t.Context(), nil, "for-each-ref", openRefs)
	require.NoError(t, err)
	require.Empty(t, string(out), "Finish unpins the snapshot")
}

func TestPruneWithoutPinLosesSnapshot(t *testing.T) {
	files, sessionID := newHistory(t)
	set(t, &pin, false)
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	require.NoError(t, os.WriteFile(path, []byte("one\n"), 0o644))
	t.Cleanup(Idle)

	r := Begin(t.Context(), files, t.TempDir(), root, sessionID)
	require.Empty(t, r.note)
	require.NoError(t, os.WriteFile(path, []byte("two\n"), 0o644))
	_, _, err := r.s.snapshot(t.Context(), "")
	require.NoError(t, err)
	prune(t, r.s)

	require.Contains(t, r.Finish(t.Context()), "rewind didn't record")
}

func TestPruneRunsOnlyWithNoCommandsOpen(t *testing.T) {
	files, sessionID := newHistory(t)
	set(t, &gcEvery, 1)
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	require.NoError(t, os.WriteFile(path, []byte("one\n"), 0o644))
	t.Cleanup(Idle)
	ctx := context.Background()

	first := Begin(ctx, files, t.TempDir(), root, sessionID)
	second := Begin(ctx, files, first.s.dataDir, root, sessionID)
	require.NoError(t, os.WriteFile(path, []byte("two\n"), 0o644))
	// The second command closes while the first is open: no prune.
	require.Empty(t, second.Finish(ctx))
	Idle()
	first.s.mu.Lock()
	n := first.s.snapshots
	first.s.mu.Unlock()
	require.NotZero(t, n, "a prune ran while a command was open")
	require.Empty(t, first.Finish(ctx))
	Idle()
	first.s.mu.Lock()
	defer first.s.mu.Unlock()
	require.Zero(t, first.s.snapshots, "the last Finish started a prune")
}

func TestGuardSkipsIgnoredFolders(t *testing.T) {
	files, sessionID := newHistory(t)
	set(t, &maxFiles, 5)
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/\n"), 0o644))
	deps := filepath.Join(root, "node_modules")
	require.NoError(t, os.MkdirAll(deps, 0o755))
	for i := range 10 {
		require.NoError(t, os.WriteFile(filepath.Join(deps, fmt.Sprintf("f%d", i)), nil, 0o644))
	}
	path := filepath.Join(root, "a.txt")
	t.Cleanup(Idle)

	r := Begin(t.Context(), files, t.TempDir(), root, sessionID)
	require.NoError(t, os.WriteFile(path, []byte("a\n"), 0o644))
	require.Empty(t, r.Finish(t.Context()))
	require.Equal(t, []string{"", "a\n"}, contents(t, files, sessionID, path))
}

// livePin is a pin name of a live process other than this one.
func livePin() string {
	return fmt.Sprintf("%d-deadbeef-1", os.Getppid())
}

// deadPin is a pin name of a process that has exited.
func deadPin(t *testing.T) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "--version")
	require.NoError(t, cmd.Run())
	return fmt.Sprintf("%d-deadbeef-1", cmd.ProcessState.Pid())
}

// warmStore returns a ready store for a new folder holding a.txt.
func warmStore(t *testing.T, files history.Service, sessionID string) (*store, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	require.NoError(t, os.WriteFile(path, []byte("one\n"), 0o644))
	t.Cleanup(Idle)
	r := Begin(t.Context(), files, t.TempDir(), root, sessionID)
	require.Empty(t, r.Finish(t.Context()))
	return r.s, path
}

func ref(t *testing.T, s *store, name string) {
	t.Helper()
	tree, _, err := s.snapshot(t.Context(), "")
	require.NoError(t, err)
	_, err = s.git(t.Context(), nil, "update-ref", name, tree)
	require.NoError(t, err)
}

func refs(t *testing.T, s *store) string {
	t.Helper()
	out, err := s.git(t.Context(), nil, "for-each-ref", "--format=%(refname)", "refs/tada/")
	require.NoError(t, err)
	return string(out)
}

func TestOtherProcessOpenCommandSkipsRecording(t *testing.T) {
	files, sessionID := newHistory(t)
	s, path := warmStore(t, files, sessionID)
	ref(t, s, openRefs+livePin())

	r := Begin(t.Context(), files, s.dataDir, s.root, sessionID)
	require.Equal(t, overlapNote, r.note)
	require.NoError(t, os.WriteFile(path, []byte("two\n"), 0o644))
	require.Contains(t, r.Finish(t.Context()), overlapNote)
	require.Empty(t, contents(t, files, sessionID, path))
	// The other process learns of this command.
	require.Contains(t, refs(t, s), taintRefs+livePin())
}

func TestOtherProcessCommandDuringRunSkipsRecording(t *testing.T) {
	files, sessionID := newHistory(t)
	s, path := warmStore(t, files, sessionID)

	r := Begin(t.Context(), files, s.dataDir, s.root, sessionID)
	require.Empty(t, r.note)
	// Another process opens a command and marks this one, then ends
	// before this one does.
	ref(t, s, openRefs+livePin())
	ref(t, s, taintRefs+r.name())
	_, err := s.git(t.Context(), nil, "update-ref", "-d", openRefs+livePin())
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(path, []byte("two\n"), 0o644))
	require.Contains(t, r.Finish(t.Context()), overlapNote)
	require.Empty(t, contents(t, files, sessionID, path))
	require.NotContains(t, refs(t, s), r.name(), "Finish drops its pin and mark")
}

func TestInitSweepsOnlyDeadProcessesPins(t *testing.T) {
	files, sessionID := newHistory(t)
	s, _ := warmStore(t, files, sessionID)
	dead := deadPin(t)
	ref(t, s, openRefs+livePin())
	ref(t, s, openRefs+dead)
	ref(t, s, taintRefs+dead)
	ref(t, s, openRefs+"old-format")

	s.mu.Lock()
	s.ready = false
	s.mu.Unlock()
	release, err := s.acquire(t.Context())
	require.NoError(t, err)
	err = s.init(t.Context())
	release()
	require.NoError(t, err)
	require.Equal(t, "refs/tada/open/"+livePin()+"\n", refs(t, s))
}

func TestBusyLockDoesntTurnOffRecording(t *testing.T) {
	files, sessionID := newHistory(t)
	s, _ := warmStore(t, files, sessionID)
	// Another git holds the index lock.
	indexLock := filepath.Join(s.gitDir, "index.lock")
	require.NoError(t, os.WriteFile(indexLock, nil, 0o644))
	for range maxFailures + 1 {
		_, _, err := s.snapshot(t.Context(), "")
		require.ErrorContains(t, err, "index.lock")
	}
	s.mu.Lock()
	failures := s.failures
	s.mu.Unlock()
	require.Zero(t, failures)
	require.Empty(t, s.disabledReason())
	require.FileExists(t, indexLock, "a lock we only waited on stays")
}

func TestCancelledSnapshotsDontTurnOffRecording(t *testing.T) {
	files, sessionID := newHistory(t)
	s, _ := warmStore(t, files, sessionID)
	// A cancelled command, such as at shutdown, isn't a git failure.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for range maxFailures + 1 {
		_, _, err := s.snapshot(ctx, "")
		require.Error(t, err)
	}
	s.mu.Lock()
	failures := s.failures
	s.mu.Unlock()
	require.Zero(t, failures)
	require.Empty(t, s.disabledReason())
}
