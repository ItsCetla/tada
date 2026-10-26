package rewind

import (
	"context"
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
const reasonModified = "changed since Ta-Da last wrote it"

// fileStep is one planned file change.
type fileStep struct {
	change  FileChange
	content string
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
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if _, err := s.history.CreateAbsent(ctx, sessionID, path); err != nil {
			slog.Warn("Failed to record rewound file", "path", path, "error", err)
		}
	default:
		if err := writeFileAtomic(path, []byte(st.content)); err != nil {
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

// insideProject reports whether path is under the session's working folder
// or the project root.
func (s *Service) insideProject(sess session.Session, path string) bool {
	roots := []string{s.projectRoot}
	if sess.WorkingDir != "" {
		wd := sess.WorkingDir
		if !filepath.IsAbs(wd) && s.projectRoot != "" {
			wd = filepath.Join(s.projectRoot, wd)
		}
		roots = append(roots, wd)
	}
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

// canonical resolves symlinks in p, or in its folder when p itself is gone.
func canonical(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if r, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(r, filepath.Base(p))
	}
	return p
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
