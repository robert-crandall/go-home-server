-- +goose Up
CREATE TABLE notes (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    body       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);

CREATE INDEX notes_user_id_idx ON notes (user_id) WHERE deleted_at IS NULL;

-- +goose Down
DROP TABLE notes;
