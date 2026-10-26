package history

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/google/uuid"
)

const (
	InitialVersion = 0
)

// Origin says whether a version stands for a file that existed on disk.
type Origin int

const (
	// OriginUnknown marks versions recorded before origins were tracked,
	// and ordinary versions where only the content matters.
	OriginUnknown Origin = iota
	// OriginExisted marks a first version that captured an existing file.
	OriginExisted
	// OriginAbsent marks a version that stands for no file at all: the
	// first version of a file the session created, or a file rewind
	// deleted.
	OriginAbsent
)

type File struct {
	ID        string
	SessionID string
	Path      string
	Content   string
	Version   int64
	Origin    Origin
	// MessageID is the assistant message whose tool call wrote the
	// version, or "" when unknown.
	MessageID string
	CreatedAt int64
	UpdatedAt int64
}

type messageIDKey struct{}

// WithMessageID returns a context whose history writes are stamped with
// the assistant message id, so rewind can tell which turn wrote them.
func WithMessageID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, messageIDKey{}, id)
}

func messageIDFrom(ctx context.Context) sql.NullString {
	id, _ := ctx.Value(messageIDKey{}).(string)
	return sql.NullString{String: id, Valid: id != ""}
}

// Service manages file versions and history for sessions.
type Service interface {
	pubsub.Subscriber[File]
	Create(ctx context.Context, sessionID, path, content string) (File, error)

	// CreateNew records that path did not exist before the session wrote
	// it. It stands in for Create when a tool creates a file, so rewind can
	// delete the file instead of leaving it empty.
	CreateNew(ctx context.Context, sessionID, path string) (File, error)

	// CreateAbsent records a new version saying path no longer exists.
	CreateAbsent(ctx context.Context, sessionID, path string) (File, error)

	// CreateVersion creates a new version of a file.
	CreateVersion(ctx context.Context, sessionID, path, content string) (File, error)

	Get(ctx context.Context, id string) (File, error)
	GetByPathAndSession(ctx context.Context, path, sessionID string) (File, error)
	ListBySession(ctx context.Context, sessionID string) ([]File, error)
	ListLatestSessionFiles(ctx context.Context, sessionID string) ([]File, error)
	Delete(ctx context.Context, id string) error
	DeleteSessionFiles(ctx context.Context, sessionID string) error

	// Mark returns a marker for the newest version recorded so far, in
	// any session.
	Mark(ctx context.Context) (int64, error)
	// ListPathSince lists path's versions, from any session, recorded
	// after mark, oldest first.
	ListPathSince(ctx context.Context, path string, mark int64) ([]File, error)
}

type service struct {
	*pubsub.Broker[File]
	db *sql.DB
	q  *db.Queries
}

func NewService(q *db.Queries, db *sql.DB) Service {
	return &service{
		Broker: pubsub.NewBroker[File](),
		q:      q,
		db:     db,
	}
}

func (s *service) Create(ctx context.Context, sessionID, path, content string) (File, error) {
	return s.createWithVersion(ctx, sessionID, path, content, InitialVersion, OriginExisted)
}

func (s *service) CreateNew(ctx context.Context, sessionID, path string) (File, error) {
	return s.createWithVersion(ctx, sessionID, path, "", InitialVersion, OriginAbsent)
}

func (s *service) CreateAbsent(ctx context.Context, sessionID, path string) (File, error) {
	next, err := s.nextVersion(ctx, path)
	if err != nil {
		return File{}, err
	}
	return s.createWithVersion(ctx, sessionID, path, "", next, OriginAbsent)
}

// nextVersion returns the version after the newest one recorded for path
// in any session.
func (s *service) nextVersion(ctx context.Context, path string) (int64, error) {
	files, err := s.q.ListFilesByPath(ctx, path)
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return InitialVersion, nil
	}
	return files[0].Version + 1, nil
}

// CreateVersion creates a new version of a file with auto-incremented version
// number. If no previous versions exist for the path, it creates the initial
// version. The provided content is stored as the new version.
func (s *service) CreateVersion(ctx context.Context, sessionID, path, content string) (File, error) {
	// Get the latest version for this path
	files, err := s.q.ListFilesByPath(ctx, path)
	if err != nil {
		return File{}, err
	}

	if len(files) == 0 {
		// No previous versions, create initial
		return s.Create(ctx, sessionID, path, content)
	}

	// Get the latest version
	latestFile := files[0] // Files are ordered by version DESC, created_at DESC
	nextVersion := latestFile.Version + 1

	return s.createWithVersion(ctx, sessionID, path, content, nextVersion, OriginUnknown)
}

func (s *service) createWithVersion(ctx context.Context, sessionID, path, content string, version int64, origin Origin) (File, error) {
	// Maximum number of retries for transaction conflicts
	const maxRetries = 3
	var file File
	var err error

	// Retry loop for transaction conflicts
	for attempt := range maxRetries {
		// Start a transaction
		tx, txErr := s.db.BeginTx(ctx, nil)
		if txErr != nil {
			return File{}, fmt.Errorf("failed to begin transaction: %w", txErr)
		}

		// Create a new queries instance with the transaction
		qtx := s.q.WithTx(tx)

		// Try to create the file within the transaction
		dbFile, txErr := qtx.CreateFile(ctx, db.CreateFileParams{
			ID:        uuid.New().String(),
			SessionID: sessionID,
			Path:      path,
			Content:   content,
			Version:   version,
			IsNew:     originToDB(origin),
			MessageID: messageIDFrom(ctx),
		})
		if txErr != nil {
			// Rollback the transaction
			tx.Rollback()

			// Check if this is a uniqueness constraint violation
			if strings.Contains(txErr.Error(), "UNIQUE constraint failed") {
				if attempt < maxRetries-1 {
					// If we have retries left, increment version and try again
					version++
					continue
				}
			}
			return File{}, txErr
		}

		// Commit the transaction
		if txErr = tx.Commit(); txErr != nil {
			return File{}, fmt.Errorf("failed to commit transaction: %w", txErr)
		}

		file = s.fromDBItem(dbFile)
		s.Publish(pubsub.CreatedEvent, file)
		return file, nil
	}

	return file, err
}

func (s *service) Get(ctx context.Context, id string) (File, error) {
	dbFile, err := s.q.GetFile(ctx, id)
	if err != nil {
		return File{}, err
	}
	return s.fromDBItem(dbFile), nil
}

func (s *service) GetByPathAndSession(ctx context.Context, path, sessionID string) (File, error) {
	dbFile, err := s.q.GetFileByPathAndSession(ctx, db.GetFileByPathAndSessionParams{
		Path:      path,
		SessionID: sessionID,
	})
	if err != nil {
		return File{}, err
	}
	return s.fromDBItem(dbFile), nil
}

func (s *service) ListBySession(ctx context.Context, sessionID string) ([]File, error) {
	dbFiles, err := s.q.ListFilesBySession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	files := make([]File, len(dbFiles))
	for i, dbFile := range dbFiles {
		files[i] = s.fromDBItem(dbFile)
	}
	return files, nil
}

func (s *service) ListLatestSessionFiles(ctx context.Context, sessionID string) ([]File, error) {
	dbFiles, err := s.q.ListLatestSessionFiles(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	files := make([]File, len(dbFiles))
	for i, dbFile := range dbFiles {
		files[i] = s.fromDBItem(dbFile)
	}
	return files, nil
}

func (s *service) Delete(ctx context.Context, id string) error {
	file, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	err = s.q.DeleteFile(ctx, id)
	if err != nil {
		return err
	}
	s.Publish(pubsub.DeletedEvent, file)
	return nil
}

func (s *service) DeleteSessionFiles(ctx context.Context, sessionID string) error {
	files, err := s.ListBySession(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, file := range files {
		err = s.Delete(ctx, file.ID)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *service) Mark(ctx context.Context) (int64, error) {
	return s.q.MaxFileRowID(ctx)
}

func (s *service) ListPathSince(ctx context.Context, path string, mark int64) ([]File, error) {
	rows, err := s.q.ListFilesByPathAfterRowID(ctx, path, mark)
	if err != nil {
		return nil, err
	}
	files := make([]File, len(rows))
	for i, r := range rows {
		files[i] = s.fromDBItem(r)
	}
	return files, nil
}

func (s *service) fromDBItem(item db.File) File {
	return FromDB(item)
}

// FromDB converts a database row into a File.
func FromDB(item db.File) File {
	return File{
		ID:        item.ID,
		SessionID: item.SessionID,
		Path:      item.Path,
		Content:   item.Content,
		Version:   item.Version,
		Origin:    originFromDB(item.IsNew),
		MessageID: item.MessageID.String,
		CreatedAt: item.CreatedAt,
		UpdatedAt: item.UpdatedAt,
	}
}

func originToDB(o Origin) sql.NullInt64 {
	switch o {
	case OriginExisted:
		return sql.NullInt64{Int64: 0, Valid: true}
	case OriginAbsent:
		return sql.NullInt64{Int64: 1, Valid: true}
	default:
		return sql.NullInt64{}
	}
}

func originFromDB(v sql.NullInt64) Origin {
	switch {
	case !v.Valid:
		return OriginUnknown
	case v.Int64 == 1:
		return OriginAbsent
	default:
		return OriginExisted
	}
}

// OriginToDB converts an Origin to its database column value.
func OriginToDB(o Origin) sql.NullInt64 { return originToDB(o) }
