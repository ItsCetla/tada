// Package rewind turns a session back to an earlier turn. It restores the
// files the agent changed from the recorded file history, trims the chat, or
// both. Foreground bash commands are recorded too, by diffing hidden git
// snapshots taken before and after each one (see internal/snapshot). Not
// recorded: explicit background jobs, changes an auto-backgrounded command
// makes after the tool returns, binary, over-1 MiB and non-regular files (the
// tool result names them), files over 16 MiB (left out silently), gitignored
// files, files outside the working directory or in nested repositories,
// read-only commands, and everything when git is missing or keeps failing,
// the folder is the home folder, the file system root or holds more than
// 50,000 files, or another session runs a command in the same folder at the
// same time. Parallel bash calls in one session may attribute each other's
// changes, and edits the user makes while a command runs are taken for the
// command's, so rewinding it undoes them too. Turns are matched by the assistant message that wrote each
// version, falling back to timestamps for older rows. Each skipped command's
// tool result says why.
package rewind

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/session"
)

// ErrBusy is returned when the agent is still working on the session.
var ErrBusy = errors.New("the agent is still working on this session; cancel it first")

// ErrNotFound is returned when the requested message isn't a turn of the
// session.
var ErrNotFound = errors.New("message is not a turn in this session")

// Mode picks what a rewind restores.
type Mode string

const (
	// ModeBoth restores files and trims the chat.
	ModeBoth Mode = "both"
	// ModeChat trims the chat and leaves files alone.
	ModeChat Mode = "chat"
	// ModeFiles restores files and keeps the chat.
	ModeFiles Mode = "files"
)

// Scope picks which files a rewind touches.
type Scope string

const (
	// ScopeAll restores every file the session changed.
	ScopeAll Scope = "all"
	// ScopeOutside restores only files outside the session's working folder
	// and the project root.
	ScopeOutside Scope = "outside"
)

// Op is what a rewind does to one file.
type Op string

const (
	// OpRestore writes the earlier content back.
	OpRestore Op = "restore"
	// OpDelete removes a file the session created.
	OpDelete Op = "delete"
)

// Turn is one user prompt and everything the agent did in reply.
type Turn struct {
	// Index is the 1-based turn number.
	Index     int    `json:"index"`
	MessageID string `json:"message_id"`
	Prompt    string `json:"prompt"`
	CreatedAt int64  `json:"created_at"`
	// Files lists the paths the turn changed, as far as file history
	// knows.
	Files []string `json:"files,omitempty"`
	// Actions counts the turn's tool actions by permission class.
	Actions map[permission.Class]int `json:"actions,omitempty"`
	// Bash reports that a shell command ran during the turn. Only some of
	// its changes are recorded.
	Bash bool `json:"bash,omitempty"`
}

// Request asks for a rewind to just before a turn.
type Request struct {
	// MessageID is the user message that starts the turn.
	MessageID string `json:"message_id"`
	Mode      Mode   `json:"mode,omitempty"`
	Scope     Scope  `json:"scope,omitempty"`
	// Force restores files even when they changed since the session last
	// wrote them.
	Force bool `json:"force,omitempty"`
	// DryRun plans the rewind without changing anything.
	DryRun bool `json:"dry_run,omitempty"`
}

// FileChange is what a rewind does, or would do, to one file.
type FileChange struct {
	Path string `json:"path"`
	Op   Op     `json:"op"`
	// Skipped is set when the file was left alone; Reason says why.
	Skipped bool   `json:"skipped,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Result describes a rewind, or the plan for one when it was a dry run.
type Result struct {
	Files           []FileChange `json:"files,omitempty"`
	MessagesDeleted int          `json:"messages_deleted"`
	// Prompt is the removed turn's prompt, to put back in the editor.
	Prompt string `json:"prompt,omitempty"`
}

// Messages is the part of the message service rewind uses.
type Messages interface {
	List(ctx context.Context, sessionID string) ([]message.Message, error)
	Delete(ctx context.Context, id string) error
	FlushAll(ctx context.Context) error
}

// Sessions is the part of the session service rewind uses.
type Sessions interface {
	Get(ctx context.Context, id string) (session.Session, error)
	Save(ctx context.Context, s session.Session) (session.Session, error)
	Delete(ctx context.Context, id string) error
	ParseAgentToolSessionID(sessionID string) (messageID, toolCallID string, ok bool)
}

// History is the part of the file history service rewind uses.
type History interface {
	ListBySession(ctx context.Context, sessionID string) ([]history.File, error)
	CreateVersion(ctx context.Context, sessionID, path, content string) (history.File, error)
	CreateAbsent(ctx context.Context, sessionID, path string) (history.File, error)
}

// Options builds a [Service].
type Options struct {
	DB       *sql.DB
	Messages Messages
	Sessions Sessions
	History  History
	// ProjectRoot is the folder Ta-Da was launched in.
	ProjectRoot string
	// Busy reports whether the agent is working on a session. Nil means
	// never busy.
	Busy func(sessionID string) bool
	// Actions returns recent tool actions across sessions. Nil means none
	// are known.
	Actions func(ctx context.Context) []permission.Action
	// StoredPath maps a path to its read_files key. Nil uses the file
	// tracker's own mapping.
	StoredPath func(path string) string
}

// Service rewinds and forks sessions.
type Service struct {
	db          *sql.DB
	q           *db.Queries
	messages    Messages
	sessions    Sessions
	history     History
	projectRoot string
	busy        func(string) bool
	actions     func(context.Context) []permission.Action
	storedPath  func(string) string
}

// New returns a Service.
func New(o Options) *Service {
	s := &Service{
		db:          o.DB,
		q:           db.New(o.DB),
		messages:    o.Messages,
		sessions:    o.Sessions,
		history:     o.History,
		projectRoot: o.ProjectRoot,
		busy:        o.Busy,
		actions:     o.Actions,
		storedPath:  o.StoredPath,
	}
	if s.storedPath == nil {
		s.storedPath = filetracker.StoredPath
	}
	return s
}

func (s *Service) isBusy(sessionID string) bool {
	return s.busy != nil && s.busy(sessionID)
}

// isTurnStart reports whether msg starts a turn: a user message that the
// user typed rather than one generated to continue the model.
func isTurnStart(msg message.Message) bool {
	if msg.Role != message.User {
		return false
	}
	for _, p := range msg.Parts {
		if t, ok := p.(message.TextContent); ok && t.Hidden {
			return false
		}
	}
	return true
}

func promptOf(msg message.Message) string {
	return strings.TrimSpace(msg.Content().Text)
}
