-- +goose Up
CREATE TABLE files (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id      bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    storage_key  text NOT NULL UNIQUE,
    filename     text NOT NULL,
    content_type text NOT NULL,
    size_bytes   bigint NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Listing a user's files newest-first is the only read pattern. id breaks
-- created_at ties, so it belongs in the index or Postgres re-sorts.
CREATE INDEX files_user_id_created_at_idx ON files (user_id, created_at DESC, id DESC);

-- +goose Down
DROP TABLE files;
