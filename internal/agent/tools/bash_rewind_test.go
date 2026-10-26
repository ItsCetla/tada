package tools

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/rewind"
	"github.com/charmbracelet/crush/internal/sandbox"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/snapshot"
	"github.com/stretchr/testify/require"
)

type rewindEnv struct {
	ctx      context.Context
	conn     *sql.DB
	sessions session.Service
	history  history.Service
	messages message.Service
	svc      *rewind.Service
	sess     session.Session
}

func newRewindEnv(t *testing.T, root string) *rewindEnv {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	e := &rewindEnv{
		conn:     conn,
		sessions: sessions,
		history:  history.NewService(q, conn),
		messages: message.NewService(q, message.WithDebounce(0)),
	}
	e.svc = rewind.New(rewind.Options{
		DB:          conn,
		Messages:    e.messages,
		Sessions:    sessions,
		History:     e.history,
		ProjectRoot: root,
		StoredPath:  func(p string) string { return p },
	})
	e.sess, err = sessions.Create(t.Context(), "shell")
	require.NoError(t, err)
	e.ctx = context.WithValue(t.Context(), SessionIDContextKey, e.sess.ID)
	return e
}

func newHistoryBashTool(t *testing.T, files history.Service, root, dataDir string) fantasy.AgentTool {
	t.Helper()
	// Wait for background indexing and pruning before the folders go.
	t.Cleanup(snapshot.Idle)
	perms := &mockBashPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	return NewBashTool(perms, files, root, dataDir, attribution, "test-model", sandbox.Settings{})
}

func TestBashToolRewindUndoesShellChanges(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	e := newRewindEnv(t, root)
	// The data directory sits inside the project, as it does by default,
	// and has no .gitignore of its own.
	tool := newHistoryBashTool(t, e.history, root, filepath.Join(root, ".tada"))

	write := func(name, content string) {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(content), 0o644))
	}
	write("keep.txt", "old\n")
	write("gone.txt", "bye\n")
	write("crlf.txt", "a\r\nb\r\n")
	write(".gitattributes", "* text eol=lf\n")
	write("bin.dat", "\x00\x01\x02")

	turn, err := e.messages.Create(e.ctx, e.sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "change things"}},
	})
	require.NoError(t, err)

	resp := runBashTool(t, tool, e.ctx, BashParams{
		Command: `printf 'new\n' > keep.txt && rm gone.txt bin.dat && echo hi > new.txt && printf 'x\r\n' > crlf.txt`,
	})
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "rewind can't restore these changed files")
	require.Contains(t, resp.Content, "bin.dat")
	require.Equal(t, "hi\n", readFile(t, filepath.Join(root, "new.txt")))

	files, err := e.history.ListBySession(e.ctx, e.sess.ID)
	require.NoError(t, err)
	for _, f := range files {
		require.NotContains(t, f.Path, ".tada", "the data directory must not be recorded")
	}

	res, err := e.svc.Rewind(e.ctx, e.sess.ID, rewind.Request{MessageID: turn.ID, Mode: rewind.ModeFiles})
	require.NoError(t, err)
	for _, f := range res.Files {
		require.False(t, f.Skipped, "%s skipped: %s", f.Path, f.Reason)
	}
	require.Len(t, res.Files, 4)

	require.Equal(t, "old\n", readFile(t, filepath.Join(root, "keep.txt")))
	require.Equal(t, "bye\n", readFile(t, filepath.Join(root, "gone.txt")))
	require.Equal(t, "a\r\nb\r\n", readFile(t, filepath.Join(root, "crlf.txt")))
	require.NoFileExists(t, filepath.Join(root, "new.txt"))
}

func TestBashToolRewindRecordsOnTopOfEdits(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	e := newRewindEnv(t, root)
	tool := newHistoryBashTool(t, e.history, root, t.TempDir())
	path := filepath.Join(root, "a.txt")

	// The session already wrote the file, then the user changed it.
	require.NoError(t, os.WriteFile(path, []byte("user\n"), 0o644))
	_, err := e.history.Create(e.ctx, e.sess.ID, path, "orig\n")
	require.NoError(t, err)
	_, err = e.history.CreateVersion(e.ctx, e.sess.ID, path, "agent\n")
	require.NoError(t, err)

	runBashTool(t, tool, e.ctx, BashParams{Command: `echo shell > a.txt`})

	files, err := e.history.ListBySession(e.ctx, e.sess.ID)
	require.NoError(t, err)
	var contents []string
	for _, f := range files {
		contents = append(contents, f.Content)
	}
	require.Equal(t, []string{"orig\n", "agent\n", "user\n", "shell\n"}, contents)
}

func TestBashToolWithoutGitStillRuns(t *testing.T) {
	// t.Setenv rules out t.Parallel.
	root := t.TempDir()
	e := newRewindEnv(t, root)
	t.Setenv("PATH", t.TempDir())
	tool := newHistoryBashTool(t, e.history, root, t.TempDir())

	resp := runBashTool(t, tool, e.ctx, BashParams{Command: `echo hi > f.txt`})
	require.False(t, resp.IsError, resp.Content)
	require.Equal(t, "hi\n", readFile(t, filepath.Join(root, "f.txt")))
	require.Contains(t, resp.Content, "rewind didn't record this command's file changes: git isn't installed")

	files, err := e.history.ListBySession(e.ctx, e.sess.ID)
	require.NoError(t, err)
	require.Empty(t, files)
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// waitFor is a shell loop that waits for a file in the work tree.
func waitFor(name string) string {
	return "while [ ! -f " + name + " ]; do sleep 0.05; done; "
}

// pathRows returns the contents of session's history rows for path.
func pathRows(t *testing.T, e *rewindEnv, sessionID, path string) []string {
	t.Helper()
	files, err := e.history.ListBySession(e.ctx, sessionID)
	require.NoError(t, err)
	var out []string
	for _, f := range files {
		if f.Path == path {
			out = append(out, f.Content)
		}
	}
	return out
}

func TestBashToolOverlappingSessionsRecordNothing(t *testing.T) {
	t.Parallel()
	requireGit(t)
	root := t.TempDir()
	e := newRewindEnv(t, root)
	other, err := e.sessions.Create(t.Context(), "other")
	require.NoError(t, err)
	otherCtx := context.WithValue(t.Context(), SessionIDContextKey, other.ID)
	dataDir := t.TempDir()
	toolA := newHistoryBashTool(t, e.history, root, dataDir)
	toolB := newHistoryBashTool(t, e.history, root, dataDir)
	snapshot.Idle()

	// Each command waits for the other to start, so they overlap.
	type result struct {
		resp fantasy.ToolResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		input := `{"command":"touch b.started; ` + waitFor("a.started") + `echo b > b.txt"}`
		resp, err := toolB.Run(otherCtx, fantasy.ToolCall{ID: "b", Name: BashToolName, Input: input})
		done <- result{resp, err}
	}()
	respA := runBashTool(t, toolA, e.ctx, BashParams{
		Command: "touch a.started; " + waitFor("b.started") + "echo a > a.txt",
	})
	b := <-done
	require.NoError(t, b.err)

	const note = "another session ran a command in this folder at the same time"
	require.Contains(t, respA.Content, note)
	require.Contains(t, b.resp.Content, note)
	require.Equal(t, "a\n", readFile(t, filepath.Join(root, "a.txt")))
	require.Equal(t, "b\n", readFile(t, filepath.Join(root, "b.txt")))
	for _, id := range []string{e.sess.ID, other.ID} {
		files, err := e.history.ListBySession(e.ctx, id)
		require.NoError(t, err)
		require.Empty(t, files)
	}
}

func TestBashToolKeepsEditsMadeWhileRunning(t *testing.T) {
	t.Parallel()
	requireGit(t)
	root := t.TempDir()
	e := newRewindEnv(t, root)
	tool := newHistoryBashTool(t, e.history, root, t.TempDir())
	path := filepath.Join(root, "a.txt")
	require.NoError(t, os.WriteFile(path, []byte("orig\n"), 0o644))
	snapshot.Idle()

	turn, err := e.messages.Create(e.ctx, e.sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "edit while the shell runs"}},
	})
	require.NoError(t, err)

	// The edit tool writes a.txt while the command runs, as a parallel
	// tool call would.
	edited := make(chan error, 1)
	go func() {
		for {
			if _, err := os.Stat(filepath.Join(root, "started")); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if _, err := e.history.Create(e.ctx, e.sess.ID, path, "orig\n"); err != nil {
			edited <- err
			return
		}
		if _, err := e.history.CreateVersion(e.ctx, e.sess.ID, path, "edited\n"); err != nil {
			edited <- err
			return
		}
		if err := os.WriteFile(path, []byte("edited\n"), 0o644); err != nil {
			edited <- err
			return
		}
		edited <- os.WriteFile(filepath.Join(root, "edited"), nil, 0o644)
	}()
	resp := runBashTool(t, tool, e.ctx, BashParams{
		Command: "touch started; " + waitFor("edited") + "echo shell > b.txt",
	})
	require.NoError(t, <-edited)
	require.False(t, resp.IsError, resp.Content)
	require.NotContains(t, resp.Content, "Note:")

	// No stale "orig" row after the edit's rows, and no repeat of them.
	require.Equal(t, []string{"orig\n", "edited\n"}, pathRows(t, e, e.sess.ID, path))

	_, err = e.svc.Rewind(e.ctx, e.sess.ID, rewind.Request{MessageID: turn.ID, Mode: rewind.ModeFiles})
	require.NoError(t, err)
	require.Equal(t, "orig\n", readFile(t, path))
	require.NoFileExists(t, filepath.Join(root, "b.txt"))
}

func TestBashToolSkipsFilesAnotherSessionEdited(t *testing.T) {
	t.Parallel()
	requireGit(t)
	root := t.TempDir()
	e := newRewindEnv(t, root)
	other, err := e.sessions.Create(t.Context(), "other")
	require.NoError(t, err)
	tool := newHistoryBashTool(t, e.history, root, t.TempDir())
	path := filepath.Join(root, "a.txt")
	require.NoError(t, os.WriteFile(path, []byte("orig\n"), 0o644))
	snapshot.Idle()

	edited := make(chan error, 1)
	go func() {
		for {
			if _, err := os.Stat(filepath.Join(root, "started")); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if _, err := e.history.Create(e.ctx, other.ID, path, "orig\n"); err != nil {
			edited <- err
			return
		}
		if err := os.WriteFile(path, []byte("theirs\n"), 0o644); err != nil {
			edited <- err
			return
		}
		edited <- os.WriteFile(filepath.Join(root, "edited"), nil, 0o644)
	}()
	resp := runBashTool(t, tool, e.ctx, BashParams{
		Command: "touch started; " + waitFor("edited") + "echo shell > b.txt",
	})
	require.NoError(t, <-edited)
	require.Contains(t, resp.Content, "another session also changed while this command ran: a.txt")
	require.Empty(t, pathRows(t, e, e.sess.ID, path))
	require.Equal(t, []string{"", "shell\n"}, pathRows(t, e, e.sess.ID, filepath.Join(root, "b.txt")))
}

func TestBashToolRewindUsesMessageIDForSameSecondChanges(t *testing.T) {
	t.Parallel()
	requireGit(t)
	root := t.TempDir()
	e := newRewindEnv(t, root)
	tool := newHistoryBashTool(t, e.history, root, t.TempDir())
	path := filepath.Join(root, "a.txt")
	require.NoError(t, os.WriteFile(path, []byte("old\n"), 0o644))
	snapshot.Idle()

	text := func(s string) []message.ContentPart {
		return []message.ContentPart{message.TextContent{Text: s}}
	}
	_, err := e.messages.Create(e.ctx, e.sess.ID, message.CreateMessageParams{Role: message.User, Parts: text("one")})
	require.NoError(t, err)
	reply, err := e.messages.Create(e.ctx, e.sess.ID, message.CreateMessageParams{Role: message.Assistant, Parts: text("on it")})
	require.NoError(t, err)
	resp := runBashTool(t, tool, history.WithMessageID(e.ctx, reply.ID), BashParams{Command: `echo new > a.txt`})
	require.False(t, resp.IsError, resp.Content)
	second, err := e.messages.Create(e.ctx, e.sess.ID, message.CreateMessageParams{Role: message.User, Parts: text("two")})
	require.NoError(t, err)

	// Everything lands in the same second, so timestamps can't place the
	// change; the message id still can.
	_, err = e.conn.ExecContext(e.ctx, `UPDATE files SET created_at = 1000`)
	require.NoError(t, err)
	_, err = e.conn.ExecContext(e.ctx, `UPDATE messages SET created_at = 1000`)
	require.NoError(t, err)

	turns, err := e.svc.Timeline(e.ctx, e.sess.ID)
	require.NoError(t, err)
	require.Len(t, turns, 2)
	require.Equal(t, []string{path}, turns[0].Files)
	require.Empty(t, turns[1].Files)

	res, err := e.svc.Rewind(e.ctx, e.sess.ID, rewind.Request{MessageID: second.ID, Mode: rewind.ModeFiles})
	require.NoError(t, err)
	require.Empty(t, res.Files)
	require.Equal(t, "new\n", readFile(t, path))

	// Without the message id, time alone puts the change on the second
	// turn.
	_, err = e.conn.ExecContext(e.ctx, `UPDATE files SET message_id = NULL`)
	require.NoError(t, err)
	turns, err = e.svc.Timeline(e.ctx, e.sess.ID)
	require.NoError(t, err)
	require.Empty(t, turns[0].Files)
	require.Equal(t, []string{path}, turns[1].Files)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

func TestBashToolNotesCommandsOutsideProject(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	e := newRewindEnv(t, root)
	tool := newHistoryBashTool(t, e.history, root, t.TempDir())
	outside := t.TempDir()

	resp := runBashTool(t, tool, e.ctx, BashParams{WorkingDir: outside, Command: `echo hi > f.txt`})
	require.False(t, resp.IsError, resp.Content)
	require.Equal(t, "hi\n", readFile(t, filepath.Join(outside, "f.txt")))
	require.Contains(t, resp.Content, "ran outside the project folder")

	// Commands inside the project, and read-only ones, get no note.
	resp = runBashTool(t, tool, e.ctx, BashParams{Command: `echo hi > g.txt`})
	require.NotContains(t, resp.Content, "outside the project")
	resp = runBashTool(t, tool, e.ctx, BashParams{WorkingDir: outside, Command: `ls`})
	require.NotContains(t, resp.Content, "outside the project")
}
