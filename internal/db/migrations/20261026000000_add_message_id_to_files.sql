-- +goose Up
-- message_id names the assistant message whose tool call wrote a history
-- version. Rewind uses it to put the version on the right turn, since
-- created_at only has second resolution. NULL for rows written before this
-- column, or outside a tool call (rewind's own restores).
ALTER TABLE files ADD COLUMN message_id TEXT;

-- +goose Down
ALTER TABLE files DROP COLUMN message_id;
