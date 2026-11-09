-- name: ListDisabledSkills :many
SELECT name FROM disabled_skills ORDER BY name;

-- name: InsertDisabledSkill :exec
INSERT OR IGNORE INTO disabled_skills (name) VALUES (?);

-- name: DeleteDisabledSkill :exec
DELETE FROM disabled_skills WHERE name = ?;
