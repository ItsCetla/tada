package db

import "context"

// These queries use SQLite's implicit rowid, which sqlc can't parse, so
// they are written by hand. A new row's rowid is one more than the
// largest in the table, so rows inserted later have a higher one (unless
// the newest row is deleted and its rowid reused). Shell change recording
// uses it as an insertion clock that doesn't depend on second-resolution
// timestamps or on per-path version numbers, which restart at 0 in each
// session.

const maxFileRowID = `SELECT CAST(COALESCE(MAX(rowid), 0) AS INTEGER) FROM files`

// MaxFileRowID returns the newest file history row's rowid, or 0.
func (q *Queries) MaxFileRowID(ctx context.Context) (int64, error) {
	row := q.queryRow(ctx, nil, maxFileRowID)
	var n int64
	err := row.Scan(&n)
	return n, err
}

const listFilesByPathAfterRowID = `SELECT id, session_id, path, content, version, created_at, updated_at, is_new, message_id
FROM files
WHERE path = ? AND rowid > ?
ORDER BY rowid ASC`

// ListFilesByPathAfterRowID returns path's rows, from any session,
// inserted after the row with afterRowID, oldest first.
func (q *Queries) ListFilesByPathAfterRowID(ctx context.Context, path string, afterRowID int64) ([]File, error) {
	rows, err := q.query(ctx, nil, listFilesByPathAfterRowID, path, afterRowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []File{}
	for rows.Next() {
		var i File
		if err := rows.Scan(
			&i.ID,
			&i.SessionID,
			&i.Path,
			&i.Content,
			&i.Version,
			&i.CreatedAt,
			&i.UpdatedAt,
			&i.IsNew,
			&i.MessageID,
		); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return items, rows.Err()
}
