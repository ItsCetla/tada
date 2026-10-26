package rewind

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/google/uuid"
)

// forkTitlePrefix starts the title of every forked session.
const forkTitlePrefix = "Fork of "

// Fork copies a session into a new top-level session and returns it.
//
// With an empty atMessageID the whole chat is copied. A user message that
// starts a turn cuts the copy just before it; any other message cuts it
// after the end of the turn it belongs to. File history and read records
// from before the cut come along so the fork can be rewound and edited
// like the original. Files on disk are not copied, and neither are token
// counts, cost, todos, the channel or sub-agent sessions.
//
// Everything is written in one transaction. No session created event is
// published; callers open the fork themselves.
func (s *Service) Fork(ctx context.Context, sessionID, atMessageID string) (session.Session, error) {
	if s.db == nil {
		return session.Session{}, fmt.Errorf("forking needs a database")
	}
	if err := s.messages.FlushAll(ctx); err != nil {
		return session.Session{}, fmt.Errorf("flushing messages: %w", err)
	}
	src, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		return session.Session{}, fmt.Errorf("loading session: %w", err)
	}
	msgs, err := s.messages.List(ctx, sessionID)
	if err != nil {
		return session.Session{}, fmt.Errorf("listing messages: %w", err)
	}
	cut, err := forkCut(msgs, atMessageID)
	if err != nil {
		return session.Session{}, err
	}
	// The turn in flight is still being written; don't copy half of it.
	if s.isBusy(sessionID) && cut == len(msgs) && lastTurnStart(msgs) >= 0 {
		return session.Session{}, ErrBusy
	}

	keep := make(map[string]bool, cut)
	for _, m := range msgs[:cut] {
		keep[m.ID] = true
	}
	// Versions and reads recorded before the first message left out belong
	// to the copied part. Versions are placed by the message that wrote
	// them when known; otherwise, as for reads, same-second ties go to the
	// part left out, like rewind does.
	at := newPlacer(msgs)
	boundary := int64(-1)
	if cut < len(msgs) {
		boundary = msgs[cut].CreatedAt
	}
	before := func(t int64) bool { return boundary < 0 || t < boundary }

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return session.Session{}, fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	q := s.q.WithTx(tx)

	newID := uuid.New().String()
	if _, err := q.CreateSession(ctx, db.CreateSessionParams{
		ID:    newID,
		Title: forkTitle(src.Title),
	}); err != nil {
		return session.Session{}, fmt.Errorf("creating session: %w", err)
	}
	if src.WorkingDir != "" {
		if _, err := q.SetSessionWorkingDir(ctx, db.SetSessionWorkingDirParams{
			WorkingDir: sql.NullString{String: src.WorkingDir, Valid: true},
			ID:         newID,
		}); err != nil {
			return session.Session{}, fmt.Errorf("setting working folder: %w", err)
		}
	}

	rows, err := q.ListMessagesBySession(ctx, sessionID)
	if err != nil {
		return session.Session{}, fmt.Errorf("listing messages: %w", err)
	}
	var summary string
	newIDs := make(map[string]string, cut)
	for _, r := range rows {
		if !keep[r.ID] {
			continue
		}
		id := uuid.New().String()
		newIDs[r.ID] = id
		if err := q.InsertMessageCopy(ctx, db.InsertMessageCopyParams{
			ID:                      id,
			SessionID:               newID,
			Role:                    r.Role,
			Parts:                   r.Parts,
			Model:                   r.Model,
			Provider:                r.Provider,
			IsSummaryMessage:        r.IsSummaryMessage,
			PrismModelID:            r.PrismModelID,
			PrismModelName:          r.PrismModelName,
			PrismHypercreditSavings: r.PrismHypercreditSavings,
			PrismDollarSavings:      r.PrismDollarSavings,
			FinishedAt:              r.FinishedAt,
			CreatedAt:               r.CreatedAt,
			UpdatedAt:               r.UpdatedAt,
		}); err != nil {
			return session.Session{}, fmt.Errorf("copying message: %w", err)
		}
		if r.IsSummaryMessage != 0 {
			summary = id
		}
	}

	files, err := q.ListFilesBySession(ctx, sessionID)
	if err != nil {
		return session.Session{}, fmt.Errorf("listing file history: %w", err)
	}
	for _, f := range files {
		if !at.before(history.FromDB(f), cut) {
			continue
		}
		msgID := newIDs[f.MessageID.String]
		if err := q.InsertFileCopy(ctx, db.InsertFileCopyParams{
			ID:        uuid.New().String(),
			SessionID: newID,
			Path:      f.Path,
			Content:   f.Content,
			Version:   f.Version,
			IsNew:     f.IsNew,
			MessageID: sql.NullString{String: msgID, Valid: msgID != ""},
			CreatedAt: f.CreatedAt,
			UpdatedAt: f.UpdatedAt,
		}); err != nil {
			return session.Session{}, fmt.Errorf("copying file history: %w", err)
		}
	}

	reads, err := q.ListSessionReadFiles(ctx, sessionID)
	if err != nil {
		return session.Session{}, fmt.Errorf("listing file reads: %w", err)
	}
	for _, r := range reads {
		// Only the latest read of each file is kept, so a file read again
		// after the cut has to be read again in the fork.
		if !before(r.ReadAt) {
			continue
		}
		if err := q.InsertFileRead(ctx, db.InsertFileReadParams{
			SessionID: newID,
			Path:      r.Path,
			ReadAt:    r.ReadAt,
		}); err != nil {
			return session.Session{}, fmt.Errorf("copying file reads: %w", err)
		}
	}

	if summary != "" {
		if _, err := q.UpdateSession(ctx, db.UpdateSessionParams{
			Title:            forkTitle(src.Title),
			SummaryMessageID: sql.NullString{String: summary, Valid: true},
			ID:               newID,
		}); err != nil {
			return session.Session{}, fmt.Errorf("setting summary: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return session.Session{}, fmt.Errorf("saving fork: %w", err)
	}
	return s.sessions.Get(ctx, newID)
}

// forkCut returns how many of msgs, from the start, a fork at atMessageID
// copies.
func forkCut(msgs []message.Message, atMessageID string) (int, error) {
	if atMessageID == "" {
		return len(msgs), nil
	}
	idx := slices.IndexFunc(msgs, func(m message.Message) bool { return m.ID == atMessageID })
	if idx < 0 {
		return 0, ErrNotFound
	}
	if isTurnStart(msgs[idx]) {
		return idx, nil
	}
	for i := idx + 1; i < len(msgs); i++ {
		if isTurnStart(msgs[i]) {
			return i, nil
		}
	}
	return len(msgs), nil
}

// lastTurnStart returns the index of the last message that starts a turn,
// or -1.
func lastTurnStart(msgs []message.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if isTurnStart(msgs[i]) {
			return i
		}
	}
	return -1
}

func forkTitle(title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "untitled session"
	}
	if strings.HasPrefix(title, forkTitlePrefix) {
		return title
	}
	return forkTitlePrefix + title
}
