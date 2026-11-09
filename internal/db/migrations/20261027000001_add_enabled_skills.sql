-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS enabled_skills (
    name TEXT PRIMARY KEY
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS enabled_skills;
-- +goose StatementEnd
