-- +goose Up
-- Personal access tokens: bearer credentials a user mints so scripts, cron
-- jobs, or an MCP server can call the API without a browser session. Only
-- sha256(secret) is stored (the secret is high-entropy, so a fast hash is
-- correct); the plaintext is shown to the user exactly once at creation.
CREATE TABLE api_tokens (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id      bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         text NOT NULL,
    -- Exactly a 32-byte SHA-256 digest of the secret.
    secret_hash  bytea NOT NULL CHECK (octet_length(secret_hash) = 32),
    -- Last 4 chars of the secret, a display hint so users can tell tokens apart.
    last4        text NOT NULL CHECK (char_length(last4) = 4),
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    -- NULL means the token never expires.
    expires_at   timestamptz
);

CREATE INDEX api_tokens_user_id_idx ON api_tokens (user_id);

-- +goose Down
DROP TABLE api_tokens;
