-- name: GetFile :one
SELECT *
FROM files
WHERE id = ? LIMIT 1;

-- name: GetFileByPathAndSession :one
SELECT *
FROM files
WHERE path = ? AND session_id = ?
ORDER BY version DESC, created_at DESC
LIMIT 1;

-- name: ListFilesBySession :many
SELECT *
FROM files
WHERE session_id = ?
ORDER BY version ASC, created_at ASC;

-- name: ListFilesByPath :many
SELECT *
FROM files
WHERE path = ?
ORDER BY version DESC, created_at DESC;

-- name: CreateFile :one
INSERT INTO files (
    id,
    session_id,
    path,
    content,
    version,
    is_new,
    message_id,
    created_at,
    updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, strftime('%s', 'now'), strftime('%s', 'now')
)
RETURNING *;

-- name: InsertFileCopy :exec
-- Copies a history version into another session (fork), keeping its
-- timestamps so it still sorts before the messages that follow it.
INSERT INTO files (
    id,
    session_id,
    path,
    content,
    version,
    is_new,
    message_id,
    created_at,
    updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, ?
);

-- name: DeleteFile :exec
DELETE FROM files
WHERE id = ?;

-- name: DeleteSessionFiles :exec
DELETE FROM files
WHERE session_id = ?;

-- name: ListLatestSessionFiles :many
-- The newest version of each path this session touched. Versions are
-- numbered across sessions but only ever grow, so the session's highest
-- version is its latest.
SELECT f.*
FROM files f
WHERE f.session_id = sqlc.arg(session_id)
  AND f.version = (
    SELECT MAX(f2.version)
    FROM files f2
    WHERE f2.session_id = f.session_id AND f2.path = f.path
  )
ORDER BY f.path;

-- name: ListNewFiles :many
SELECT *
FROM files
WHERE is_new = 1
ORDER BY version DESC, created_at DESC;
