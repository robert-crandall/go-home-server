-- +goose Up
-- Sessions now slide: every cookie-authenticated API request rewrites
-- expires_at. An index on that column would have to be maintained on each of
-- those writes, and it would rule out HOT updates, for a table that holds one
-- row per logged-in browser and is only ever scanned by the opportunistic
-- "DELETE FROM sessions WHERE expires_at < now()" on login. A sequential scan
-- of a few rows is cheaper than the index that avoids it.
DROP INDEX sessions_expires_at_idx;

-- +goose Down
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);
