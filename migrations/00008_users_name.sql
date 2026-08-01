-- +goose Up
-- An optional display name, so an app can render "Hi, Robert" instead of an
-- email address. NOT NULL DEFAULT '' rather than nullable: every read path
-- stays a plain string scan, '' is the single representation of "not set", and
-- rows that already exist in every app vendoring this table get a value without
-- a backfill.
ALTER TABLE users ADD COLUMN name text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE users DROP COLUMN name;
