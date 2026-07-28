-- +goose Up
CREATE TABLE users (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email         text NOT NULL,
    password_hash text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz
);

-- Case-insensitive uniqueness, scoped to non-deleted rows (soft delete).
CREATE UNIQUE INDEX users_email_unique ON users (lower(email)) WHERE deleted_at IS NULL;

-- +goose Down
DROP TABLE users;
