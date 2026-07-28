-- +goose Up
-- Thumbnails live next to their original as "<storage_key>.thumb.jpg", so the
-- row only has to record whether one exists. Rows predating this migration get
-- false and fall back to serving the original, same as a format we can't
-- decode; there is no backfill.
ALTER TABLE files ADD COLUMN has_thumbnail boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE files DROP COLUMN has_thumbnail;
