package rewind

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

type env struct {
	ctx      context.Context
	conn     *sql.DB
	q        *db.Queries
	sessions session.Service
	messages message.Service
	history  history.Service
	root     string
	busy     map[string]bool
	actions  []permission.Action
	svc      *Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	q := db.New(conn)
	e := &env{
		ctx:      t.Context(),
		conn:     conn,
		q:        q,
		sessions: session.NewService(q, conn),
		messages: message.NewService(q, message.WithDebounce(0)),
		history:  history.NewService(q, conn),
		root:     t.TempDir(),
		busy:     map[string]bool{},
	}
	e.svc = New(Options{
		DB:          conn,
		Messages:    e.messages,
		Sessions:    e.sessions,
		History:     e.history,
		ProjectRoot: e.root,
		Busy:        func(id string) bool { return e.busy[id] },
		Actions:     func(context.Context) []permission.Action { return e.actions },
		StoredPath:  func(p string) string { return p },
	})
	return e
}

func (e *env) session(t *testing.T, title string) session.Session {
	t.Helper()
	s, err := e.sessions.Create(e.ctx, title)
	require.NoError(t, err)
	return s
}

// msg creates a message and pins its created_at.
func (e *env) msg(t *testing.T, sessionID string, role message.MessageRole, at int64, parts ...message.ContentPart) message.Message {
	t.Helper()
	m, err := e.messages.Create(e.ctx, sessionID, message.CreateMessageParams{Role: role, Parts: parts})
	require.NoError(t, err)
	_, err = e.conn.ExecContext(e.ctx, `UPDATE messages SET created_at = ? WHERE id = ?`, at, m.ID)
	require.NoError(t, err)
	return m
}

func (e *env) summary(t *testing.T, sessionID string, at int64) message.Message {
	t.Helper()
	m, err := e.messages.Create(e.ctx, sessionID, message.CreateMessageParams{
		Role:             message.Assistant,
		Parts:            []message.ContentPart{message.TextContent{Text: "summary"}},
		IsSummaryMessage: true,
	})
	require.NoError(t, err)
	_, err = e.conn.ExecContext(e.ctx, `UPDATE messages SET created_at = ? WHERE id = ?`, at, m.ID)
	require.NoError(t, err)
	return m
}

// at returns a function that checks a file version was recorded and pins
// its created_at, so calls read e.at(t, 100)(e.history.Create(...)).
func (e *env) at(t *testing.T, at int64) func(history.File, error) history.File {
	return func(f history.File, err error) history.File {
		t.Helper()
		require.NoError(t, err)
		_, err = e.conn.ExecContext(e.ctx, `UPDATE files SET created_at = ? WHERE id = ?`, at, f.ID)
		require.NoError(t, err)
		f.CreatedAt = at
		return f
	}
}

func user(text string) message.ContentPart { return message.TextContent{Text: text} }

func write(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

func TestRewindPicksVersionBeforeTurnIgnoringOtherSessions(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.session(t, "a")
	b := e.session(t, "b")
	path := filepath.Join(e.root, "main.go")

	e.msg(t, a.ID, message.User, 100, user("first"))
	e.at(t, 100)(e.history.Create(e.ctx, a.ID, path, "one"))
	e.at(t, 100)(e.history.CreateVersion(e.ctx, a.ID, path, "two"))
	// Another session writes the file in between; its version must not be
	// what session a goes back to.
	e.at(t, 150)(e.history.CreateVersion(e.ctx, b.ID, path, "bee"))
	turn2 := e.msg(t, a.ID, message.User, 200, user("second"))
	e.at(t, 200)(e.history.CreateVersion(e.ctx, a.ID, path, "three"))
	write(t, path, "three")

	res, err := e.svc.Rewind(e.ctx, a.ID, Request{MessageID: turn2.ID, Mode: ModeFiles})
	require.NoError(t, err)
	require.Equal(t, []FileChange{{Path: path, Op: OpRestore}}, res.Files)
	require.Equal(t, "two", read(t, path))
	require.Zero(t, res.MessagesDeleted)

	latest, err := e.history.GetByPathAndSession(e.ctx, path, a.ID)
	require.NoError(t, err)
	require.Equal(t, "two", latest.Content)

	msgs, err := e.messages.List(e.ctx, a.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 2, "files-only keeps the chat")
}

func TestRewindDeletesCreatedFile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := e.session(t, "s")
	path := filepath.Join(e.root, "new.txt")

	turn := e.msg(t, s.ID, message.User, 100, user("make a file"))
	e.at(t, 100)(e.history.CreateNew(e.ctx, s.ID, path))
	e.at(t, 100)(e.history.CreateVersion(e.ctx, s.ID, path, "hello"))
	write(t, path, "hello")

	res, err := e.svc.Rewind(e.ctx, s.ID, Request{MessageID: turn.ID})
	require.NoError(t, err)
	require.Equal(t, []FileChange{{Path: path, Op: OpDelete}}, res.Files)
	require.NoFileExists(t, path)
	require.Equal(t, 1, res.MessagesDeleted)
	require.Equal(t, "make a file", res.Prompt)

	latest, err := e.history.GetByPathAndSession(e.ctx, path, s.ID)
	require.NoError(t, err)
	require.Equal(t, history.OriginAbsent, latest.Origin)
}

func TestRewindDeletesLegacyCreatedFile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := e.session(t, "s")
	path := filepath.Join(e.root, "legacy.txt")

	turn := e.msg(t, s.ID, message.User, 100, user("make a file"))
	// Before origins were tracked, new files got an empty first version.
	f, err := e.q.CreateFile(e.ctx, db.CreateFileParams{ID: "legacy", SessionID: s.ID, Path: path, Version: 0})
	e.at(t, 100)(history.FromDB(f), err)
	e.at(t, 100)(e.history.CreateVersion(e.ctx, s.ID, path, "hello"))
	write(t, path, "hello")

	res, err := e.svc.Rewind(e.ctx, s.ID, Request{MessageID: turn.ID, Mode: ModeFiles})
	require.NoError(t, err)
	require.Equal(t, []FileChange{{Path: path, Op: OpDelete}}, res.Files)
	require.NoFileExists(t, path)
}

func TestRewindSkipsHandEditedFilesUnlessForced(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := e.session(t, "s")
	path := filepath.Join(e.root, "a.txt")

	turn := e.msg(t, s.ID, message.User, 100, user("edit"))
	e.at(t, 100)(e.history.Create(e.ctx, s.ID, path, "before"))
	e.at(t, 100)(e.history.CreateVersion(e.ctx, s.ID, path, "after"))
	write(t, path, "after, then edited by hand")

	res, err := e.svc.Rewind(e.ctx, s.ID, Request{MessageID: turn.ID, Mode: ModeFiles})
	require.NoError(t, err)
	require.Equal(t, []FileChange{{Path: path, Op: OpRestore, Skipped: true, Reason: reasonModified}}, res.Files)
	require.Equal(t, "after, then edited by hand", read(t, path))

	res, err = e.svc.Rewind(e.ctx, s.ID, Request{MessageID: turn.ID, Mode: ModeFiles, Force: true})
	require.NoError(t, err)
	require.Equal(t, []FileChange{{Path: path, Op: OpRestore}}, res.Files)
	require.Equal(t, "before", read(t, path))
}

func TestRewindPreservesFileMode(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := e.session(t, "s")
	path := filepath.Join(e.root, "run.sh")

	turn := e.msg(t, s.ID, message.User, 100, user("edit"))
	e.at(t, 100)(e.history.Create(e.ctx, s.ID, path, "echo one"))
	e.at(t, 100)(e.history.CreateVersion(e.ctx, s.ID, path, "echo two"))
	write(t, path, "echo two")
	require.NoError(t, os.Chmod(path, 0o755))

	_, err := e.svc.Rewind(e.ctx, s.ID, Request{MessageID: turn.ID, Mode: ModeFiles})
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	require.Equal(t, "echo one", read(t, path))
}

func TestRewindOutsideScopeSkipsProjectFiles(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := e.session(t, "s")
	inside := filepath.Join(e.root, "in.txt")
	outside := filepath.Join(t.TempDir(), "out.txt")

	turn := e.msg(t, s.ID, message.User, 100, user("edit"))
	for _, p := range []string{inside, outside} {
		e.at(t, 100)(e.history.Create(e.ctx, s.ID, p, "old"))
		e.at(t, 100)(e.history.CreateVersion(e.ctx, s.ID, p, "new"))
		write(t, p, "new")
	}

	res, err := e.svc.Rewind(e.ctx, s.ID, Request{MessageID: turn.ID, Mode: ModeFiles, Scope: ScopeOutside})
	require.NoError(t, err)
	require.Equal(t, []FileChange{{Path: outside, Op: OpRestore}}, res.Files)
	require.Equal(t, "new", read(t, inside))
	require.Equal(t, "old", read(t, outside))
}

func TestRewindOutsideScopeHonorsSessionWorkingDir(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := e.session(t, "s")
	wd := t.TempDir()
	_, err := e.sessions.SetWorkingDir(e.ctx, s.ID, wd)
	require.NoError(t, err)
	path := filepath.Join(wd, "x.txt")

	turn := e.msg(t, s.ID, message.User, 100, user("edit"))
	e.at(t, 100)(e.history.Create(e.ctx, s.ID, path, "old"))
	e.at(t, 100)(e.history.CreateVersion(e.ctx, s.ID, path, "new"))
	write(t, path, "new")

	res, err := e.svc.Rewind(e.ctx, s.ID, Request{MessageID: turn.ID, Mode: ModeFiles, Scope: ScopeOutside})
	require.NoError(t, err)
	require.Empty(t, res.Files)
}

func TestRewindChatCutsWithinSameSecond(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := e.session(t, "s")

	u1 := e.msg(t, s.ID, message.User, 100, user("one"))
	a1 := e.msg(t, s.ID, message.Assistant, 100, message.TextContent{Text: "ok"})
	u2 := e.msg(t, s.ID, message.User, 100, user("two"))
	a2 := e.msg(t, s.ID, message.Assistant, 100,
		message.ToolCall{ID: "call-1", Name: "agent", Finished: true})
	// Sub-agent sessions of the removed turn go with it; others stay.
	_, err := e.sessions.CreateTaskSession(e.ctx, e.sessions.CreateAgentToolSessionID(a2.ID, "call-1"), s.ID, "child")
	require.NoError(t, err)
	_, err = e.sessions.CreateTaskSession(e.ctx, "call-1", s.ID, "task child")
	require.NoError(t, err)
	keep, err := e.sessions.CreateTaskSession(e.ctx, e.sessions.CreateAgentToolSessionID(a1.ID, "call-0"), s.ID, "kept")
	require.NoError(t, err)

	res, err := e.svc.Rewind(e.ctx, s.ID, Request{MessageID: u2.ID, Mode: ModeChat})
	require.NoError(t, err)
	require.Equal(t, 2, res.MessagesDeleted)
	require.Equal(t, "two", res.Prompt)

	msgs, err := e.messages.List(e.ctx, s.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, u1.ID, msgs[0].ID)
	require.Equal(t, a1.ID, msgs[1].ID)

	kids, err := e.q.ListChildSessions(e.ctx, sql.NullString{String: s.ID, Valid: true})
	require.NoError(t, err)
	require.Len(t, kids, 1)
	require.Equal(t, keep.ID, kids[0].ID)
}

func TestRewindRepairsSummaryPointer(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := e.session(t, "s")

	u1 := e.msg(t, s.ID, message.User, 100, user("one"))
	s1 := e.summary(t, s.ID, 110)
	u2 := e.msg(t, s.ID, message.User, 120, user("two"))
	s2 := e.summary(t, s.ID, 130)
	s.SummaryMessageID = s2.ID
	_, err := e.sessions.Save(e.ctx, s)
	require.NoError(t, err)

	_, err = e.svc.Rewind(e.ctx, s.ID, Request{MessageID: u2.ID, Mode: ModeChat})
	require.NoError(t, err)
	got, err := e.sessions.Get(e.ctx, s.ID)
	require.NoError(t, err)
	require.Equal(t, s1.ID, got.SummaryMessageID)

	_, err = e.svc.Rewind(e.ctx, s.ID, Request{MessageID: u1.ID, Mode: ModeChat})
	require.NoError(t, err)
	got, err = e.sessions.Get(e.ctx, s.ID)
	require.NoError(t, err)
	require.Empty(t, got.SummaryMessageID)
}

func TestRewindDryRunChangesNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := e.session(t, "s")
	path := filepath.Join(e.root, "a.txt")

	turn := e.msg(t, s.ID, message.User, 100, user("edit"))
	e.at(t, 100)(e.history.Create(e.ctx, s.ID, path, "before"))
	e.at(t, 100)(e.history.CreateVersion(e.ctx, s.ID, path, "after"))
	write(t, path, "after")
	e.busy[s.ID] = true // A preview is fine while busy.

	res, err := e.svc.Rewind(e.ctx, s.ID, Request{MessageID: turn.ID, DryRun: true})
	require.NoError(t, err)
	require.Equal(t, []FileChange{{Path: path, Op: OpRestore}}, res.Files)
	require.Equal(t, 1, res.MessagesDeleted)
	require.Equal(t, "after", read(t, path))
	msgs, err := e.messages.List(e.ctx, s.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
}

func TestRewindRefusesWhileBusy(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := e.session(t, "s")
	turn := e.msg(t, s.ID, message.User, 100, user("edit"))
	e.busy[s.ID] = true

	_, err := e.svc.Rewind(e.ctx, s.ID, Request{MessageID: turn.ID})
	require.ErrorIs(t, err, ErrBusy)
}

func TestRewindRejectsNonTurnMessage(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := e.session(t, "s")
	e.msg(t, s.ID, message.User, 100, user("edit"))
	a := e.msg(t, s.ID, message.Assistant, 100, message.TextContent{Text: "ok"})
	hidden := e.msg(t, s.ID, message.User, 100, message.TextContent{Text: "continue", Hidden: true})

	for _, id := range []string{a.ID, hidden.ID, "nope"} {
		_, err := e.svc.Rewind(e.ctx, s.ID, Request{MessageID: id})
		require.ErrorIs(t, err, ErrNotFound)
	}
}

func TestTimeline(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := e.session(t, "s")
	path := filepath.Join(e.root, "a.txt")

	u1 := e.msg(t, s.ID, message.User, 100, user("first line\nsecond line"))
	e.msg(t, s.ID, message.Assistant, 101,
		message.ToolCall{ID: "c1", Name: "edit", Finished: true},
		message.ToolCall{ID: "c2", Name: "bash", Finished: true})
	e.at(t, 101)(e.history.Create(e.ctx, s.ID, path, "a"))
	e.at(t, 101)(e.history.CreateVersion(e.ctx, s.ID, path, "b"))
	e.msg(t, s.ID, message.User, 150, message.TextContent{Text: "continue", Hidden: true})
	u2 := e.msg(t, s.ID, message.User, 200, user("again"))
	e.actions = []permission.Action{
		{ToolCallID: "c1", Class: permission.ClassWrite},
		{ToolCallID: "c2", Class: permission.ClassRead},
		{ToolCallID: "elsewhere", Class: permission.ClassNetwork},
	}

	turns, err := e.svc.Timeline(e.ctx, s.ID)
	require.NoError(t, err)
	require.Len(t, turns, 2)
	require.Equal(t, Turn{
		Index:     1,
		MessageID: u1.ID,
		Prompt:    "first line\nsecond line",
		CreatedAt: 100,
		Files:     []string{path},
		Actions:   map[permission.Class]int{permission.ClassWrite: 1, permission.ClassRead: 1},
		Bash:      true,
	}, turns[0])
	require.Equal(t, Turn{Index: 2, MessageID: u2.ID, Prompt: "again", CreatedAt: 200}, turns[1])
}

func TestRewindSkipsPathsThatNowLinkOutside(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := e.session(t, "s")
	outside := t.TempDir()
	path := filepath.Join(e.root, "sub", "a.txt")

	turn := e.msg(t, s.ID, message.User, 100, user("change it"))
	e.at(t, 100)(e.history.Create(e.ctx, s.ID, path, "old"))
	e.at(t, 100)(e.history.CreateVersion(e.ctx, s.ID, path, "new"))
	// A later command swaps the folder for a link out of the project.
	write(t, filepath.Join(outside, "a.txt"), "new")
	require.NoError(t, os.Symlink(outside, filepath.Join(e.root, "sub")))

	res, err := e.svc.Rewind(e.ctx, s.ID, Request{MessageID: turn.ID, Mode: ModeFiles, Force: true})
	require.NoError(t, err)
	require.Equal(t, []FileChange{{Path: path, Op: OpRestore, Skipped: true, Reason: reasonLinked}}, res.Files)
	require.Equal(t, "new", read(t, filepath.Join(outside, "a.txt")))
}
