-- +goose Up
CREATE TABLE push_subscriptions (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    endpoint   text NOT NULL UNIQUE,
    p256dh     text NOT NULL,
    auth       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX push_subscriptions_user_id_idx ON push_subscriptions (user_id);

-- +goose Down
DROP TABLE push_subscriptions;
