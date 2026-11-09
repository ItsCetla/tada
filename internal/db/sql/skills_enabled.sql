-- name: ListEnabledSkills :many
SELECT name FROM enabled_skills ORDER BY name;

-- name: InsertEnabledSkill :exec
INSERT OR IGNORE INTO enabled_skills (name) VALUES (?);

-- name: DeleteEnabledSkill :exec
DELETE FROM enabled_skills WHERE name = ?;
