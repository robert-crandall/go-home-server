package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/robert-crandall/go-home-server/internal/apisecurity"
)

// APITokenPrefix marks a personal access token. The full plaintext is
// APITokenPrefix + "<id>_<secret>" (e.g. pat_42_<secret>): id is the api_tokens
// row id (a positive base-10 integer, so it never contains "_"); secret is a
// 256-bit random value shown to the user exactly once. Apps can change this
// prefix, but it must not contain "_" (the parser splits on the first "_" after
// the prefix).
const APITokenPrefix = "pat_"

// apiTokenSecretBytes is the entropy of an API-token secret before encoding. 32
// bytes = 256 bits. Like a session token the secret is high-entropy random, so
// a fast hash (SHA-256) over it is sufficient - bcrypt would be pointless work
// on every request.
const apiTokenSecretBytes = 32

// maxAPITokenLen bounds the plaintext we're willing to parse, so a pathological
// header value is rejected cheaply before any splitting or hashing.
const maxAPITokenLen = 200

// apiTokenTouchInterval throttles the best-effort last_used_at bump so an
// actively-used token doesn't cause a write on every request. It matches the
// interval in the throttled UPDATE in touchTokenIfStale.
const apiTokenTouchInterval = time.Minute

// APIToken is the public shape of a token. It never includes the secret or its
// hash. LastUsedAt and ExpiresAt are nil for "never used" and "no expiry".
type APIToken struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Last4      string     `json:"last4" doc:"Last 4 characters of the secret, to tell tokens apart"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	ExpiresAt  *time.Time `json:"expiresAt"`
}

// NewAPITokenSecret returns a URL-safe, unpadded base64 random secret for a new
// API token. It's shown to the user exactly once; only its SHA-256 is stored.
func NewAPITokenSecret() (string, error) {
	b := make([]byte, apiTokenSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: read api token secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashAPITokenSecret returns sha256(secret), the value stored in
// api_tokens.secret_hash. A fast hash is correct here because the secret is
// high-entropy (see NewAPITokenSecret), unlike a user-chosen password.
func HashAPITokenSecret(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// FormatAPIToken builds the plaintext token a client presents, from a row id
// and its secret.
func FormatAPIToken(id int64, secret string) string {
	return APITokenPrefix + strconv.FormatInt(id, 10) + "_" + secret
}

// ParseAPIToken splits a plaintext token into its row id and secret. ok is
// false for anything that isn't exactly APITokenPrefix + "<id>_<secret>" with a
// canonical positive base-10 id and a non-empty secret. The id can't contain
// "_", so splitting on the first "_" after the prefix is unambiguous even when
// the base64url secret contains "_".
func ParseAPIToken(token string) (id int64, secret string, ok bool) {
	if len(token) == 0 || len(token) > maxAPITokenLen {
		return 0, "", false
	}
	rest, found := strings.CutPrefix(token, APITokenPrefix)
	if !found {
		return 0, "", false
	}
	idPart, sec, found := strings.Cut(rest, "_")
	if !found || idPart == "" || sec == "" {
		return 0, "", false
	}
	parsed, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, "", false
	}
	// Require the id to be in canonical form, so "+1", "007", " 1", and other
	// non-canonical spellings that ParseInt would otherwise accept are rejected
	// (they'd map many strings to one row id).
	if idPart != strconv.FormatInt(parsed, 10) {
		return 0, "", false
	}
	return parsed, sec, true
}

// APITokenLast4 returns the last 4 characters of a secret, the display hint
// stored in api_tokens.last4. Secrets are always longer than 4 chars, but the
// short-input guard keeps this total.
func APITokenLast4(secret string) string {
	if len(secret) <= 4 {
		return secret
	}
	return secret[len(secret)-4:]
}

// --- persistence -----------------------------------------------------------

// CreateAPIToken mints a new token for a user. It generates the secret, stores
// only sha256(secret) plus a last-4 display hint, and returns the row alongside
// the plaintext token - which exists nowhere else and can never be recovered
// after this call. expiresAt is optional; nil means the token never expires.
func (s *Service) CreateAPIToken(ctx context.Context, userID int64, name string, expiresAt *time.Time) (APIToken, string, error) {
	secret, err := NewAPITokenSecret()
	if err != nil {
		return APIToken{}, "", err
	}

	var (
		t          APIToken
		lastUsed   pgtype.Timestamptz
		expires    pgtype.Timestamptz
		expiresArg = tsFromPtr(expiresAt)
	)
	err = s.db.QueryRow(ctx,
		`INSERT INTO api_tokens (user_id, name, secret_hash, last4, expires_at)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, name, last4, created_at, last_used_at, expires_at`,
		userID, name, HashAPITokenSecret(secret), APITokenLast4(secret), expiresArg,
	).Scan(&t.ID, &t.Name, &t.Last4, &t.CreatedAt, &lastUsed, &expires)
	if err != nil {
		return APIToken{}, "", err
	}
	t.LastUsedAt = ptrFromTS(lastUsed)
	t.ExpiresAt = ptrFromTS(expires)

	return t, FormatAPIToken(t.ID, secret), nil
}

// ListAPITokens returns a user's tokens, newest first. It never exposes the
// secret or its hash - only the metadata needed to identify and revoke a token.
func (s *Service) ListAPITokens(ctx context.Context, userID int64) ([]APIToken, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, name, last4, created_at, last_used_at, expires_at
		   FROM api_tokens
		  WHERE user_id = $1
		  ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tokens := []APIToken{}
	for rows.Next() {
		var (
			t        APIToken
			lastUsed pgtype.Timestamptz
			expires  pgtype.Timestamptz
		)
		if err := rows.Scan(&t.ID, &t.Name, &t.Last4, &t.CreatedAt, &lastUsed, &expires); err != nil {
			return nil, err
		}
		t.LastUsedAt = ptrFromTS(lastUsed)
		t.ExpiresAt = ptrFromTS(expires)
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// DeleteAPIToken revokes one of a user's tokens by id. It's scoped to the owner,
// so a cross-user (or unknown) id deletes nothing and reports deleted=false
// rather than revealing that a token exists on another account. A revoked token
// fails authentication immediately on its next use.
func (s *Service) DeleteAPIToken(ctx context.Context, userID, id int64) (bool, error) {
	tag, err := s.db.Exec(ctx,
		`DELETE FROM api_tokens WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// userFromAPIToken authenticates a plaintext bearer token: it parses the token,
// looks the row up by primary key, constant-time compares sha256(secret) with
// the stored hash, and checks the optional expiry. Only after all of that passes
// does it best-effort (throttled) bump last_used_at. Any failure returns
// ErrNotFound, so the caller resolves no user.
func (s *Service) userFromAPIToken(ctx context.Context, plaintext string) (User, error) {
	id, secret, ok := ParseAPIToken(plaintext)
	if !ok {
		return User{}, ErrNotFound
	}

	var (
		u          User
		storedHash []byte
		expires    pgtype.Timestamptz
		lastUsed   pgtype.Timestamptz
	)
	err := s.db.QueryRow(ctx,
		`SELECT u.id, u.email, u.name, u.created_at, t.secret_hash, t.expires_at, t.last_used_at
		   FROM api_tokens t
		   JOIN users u ON u.id = t.user_id
		  WHERE t.id = $1 AND u.deleted_at IS NULL`,
		id,
	).Scan(&u.ID, &u.Email, &u.Name, &u.CreatedAt, &storedHash, &expires, &lastUsed)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}

	// Constant-time compare so a right-id/wrong-secret pair is rejected without
	// leaking timing. Both sides are 32-byte SHA-256 digests.
	if subtle.ConstantTimeCompare(HashAPITokenSecret(secret), storedHash) != 1 {
		return User{}, ErrNotFound
	}
	// Optional expiry. Invalid (NULL) means long-lived.
	if expires.Valid && !expires.Time.After(time.Now()) {
		return User{}, ErrNotFound
	}

	s.touchTokenIfStale(ctx, id, lastUsed)
	return u, nil
}

// touchTokenIfStale best-effort bumps last_used_at, but only if the token hasn't
// been touched within apiTokenTouchInterval, so an actively-used token doesn't
// cause a write on every request. The cutoff is computed once from
// apiTokenTouchInterval and used for both the Go short-circuit and the SQL
// guard, so there's a single source of truth. The UPDATE re-checks the cutoff in
// SQL so concurrent requests don't stampede the row. Any failure is ignored:
// last_used_at is a display nicety, not part of authentication.
func (s *Service) touchTokenIfStale(ctx context.Context, id int64, lastUsed pgtype.Timestamptz) {
	cutoff := time.Now().Add(-apiTokenTouchInterval)
	if lastUsed.Valid && lastUsed.Time.After(cutoff) {
		return
	}
	_, _ = s.db.Exec(ctx,
		`UPDATE api_tokens
		    SET last_used_at = now()
		  WHERE id = $1
		    AND (last_used_at IS NULL OR last_used_at < $2)`,
		id, cutoff)
}

// --- pgtype <-> *time.Time helpers -----------------------------------------

func tsFromPtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func ptrFromTS(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	out := t.Time
	return &out
}

// --- Authorization header parsing ------------------------------------------

// hasBearerScheme reports whether the request carries an Authorization header
// whose scheme is "bearer" (case-insensitive per RFC 7235). A present Bearer
// header commits the request to token auth with no cookie fallback, so even a
// malformed one (empty/garbage token) resolves no user rather than silently
// falling back to the cookie.
func hasBearerScheme(r *http.Request) bool {
	scheme, _ := splitAuthScheme(r.Header.Get("Authorization"))
	return strings.EqualFold(scheme, "bearer")
}

// bearerToken extracts the token from a Bearer Authorization header. ok is false
// when the token part is empty. The scheme is matched case-insensitively; the
// token itself is returned verbatim (it's case-sensitive).
func bearerToken(r *http.Request) (string, bool) {
	scheme, rest := splitAuthScheme(r.Header.Get("Authorization"))
	if !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	token := strings.TrimRight(rest, " \t")
	if token == "" {
		return "", false
	}
	return token, true
}

// splitAuthScheme splits an Authorization header value into its auth-scheme and
// the remaining credentials, on the first run of spaces or tabs. Leading
// whitespace is ignored, so a Bearer token is recognized regardless of which
// whitespace the client used. When there's no whitespace the whole value is the
// scheme and rest is empty (a bare "Bearer" still commits to token auth and
// resolves no user, since rest is empty).
func splitAuthScheme(h string) (scheme, rest string) {
	h = strings.TrimLeft(h, " \t")
	if i := strings.IndexAny(h, " \t"); i >= 0 {
		return h[:i], strings.TrimLeft(h[i+1:], " \t")
	}
	return h, ""
}

// --- huma endpoints --------------------------------------------------------

// maxTokenNameLen bounds the human label on a token. It's only a display name,
// so the cap just keeps a pathological value out of the column.
const maxTokenNameLen = 200

type createTokenInput struct {
	Body struct {
		Name string `json:"name" minLength:"1" maxLength:"200" doc:"A human label so you can tell tokens apart"`
		// ExpiresAt is optional. Omit it for a token that never expires;
		// a present RFC3339 timestamp must be in the future.
		ExpiresAt *time.Time `json:"expiresAt,omitempty" doc:"Optional expiry (RFC3339); must be in the future"`
	}
}

type createTokenOutput struct {
	// The plaintext token is returned exactly once, so keep it out of any
	// browser or proxy cache.
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		APIToken
		// Token is the plaintext credential, shown exactly once and never
		// stored or returned again.
		Token string `json:"token" doc:"The plaintext token - copy it now, it is shown only once"`
	}
}

type listTokensOutput struct {
	Body []APIToken
}

// TokenHumaConfig enables bearer authentication in Middleware and adds its
// session and bearer schemes to the OpenAPI config. Pass it as
// server.Options.HumaConfig when RegisterTokens will be called.
func (s *Service) TokenHumaConfig(cfg huma.Config) huma.Config {
	s.apiTokensEnabled = true
	return apisecurity.ConfigureTokenAuth(cfg)
}

// RegisterTokens mounts the token-management endpoints under /api/tokens.
// Call it only from apps that want programmatic API access, and pair it with
// TokenHumaConfig when constructing the server. Apps that don't call it expose
// no token endpoints.
//
// Every endpoint here requires session (cookie) auth via RequireSessionUser, so
// a leaked API token can't mint, list, or revoke tokens - only a browser session
// can.
func (s *Service) RegisterTokens(api huma.API) {
	if !s.apiTokensEnabled || !apisecurity.BearerConfigured(api) {
		panic("auth: RegisterTokens requires HumaConfig: authSvc.TokenHumaConfig")
	}

	huma.Register(api, huma.Operation{
		OperationID:   "create-api-token",
		Method:        http.MethodPost,
		Path:          "/api/tokens",
		Summary:       "Create an API token",
		Tags:          []string{"tokens"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusUnprocessableEntity},
		Security:      apisecurity.Session(api),
	}, func(ctx context.Context, in *createTokenInput) (*createTokenOutput, error) {
		u, err := RequireSessionUser(ctx)
		if err != nil {
			return nil, err
		}
		name := strings.TrimSpace(in.Body.Name)
		if name == "" {
			return nil, huma.Error422UnprocessableEntity("name is required")
		}
		if len(name) > maxTokenNameLen {
			return nil, huma.Error422UnprocessableEntity("name is too long")
		}
		var expiresAt *time.Time
		if in.Body.ExpiresAt != nil {
			if !in.Body.ExpiresAt.After(time.Now()) {
				return nil, huma.Error422UnprocessableEntity("expiresAt must be in the future")
			}
			expiresAt = in.Body.ExpiresAt
		}

		tok, plaintext, err := s.CreateAPIToken(ctx, u.ID, name, expiresAt)
		if err != nil {
			return nil, huma.Error500InternalServerError("could not create token", err)
		}

		out := &createTokenOutput{CacheControl: "no-store"}
		out.Body.APIToken = tok
		out.Body.Token = plaintext
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-api-tokens",
		Method:      http.MethodGet,
		Path:        "/api/tokens",
		Summary:     "List your API tokens",
		Tags:        []string{"tokens"},
		Errors:      []int{http.StatusUnauthorized, http.StatusForbidden},
		Security:    apisecurity.Session(api),
	}, func(ctx context.Context, _ *struct{}) (*listTokensOutput, error) {
		u, err := RequireSessionUser(ctx)
		if err != nil {
			return nil, err
		}
		tokens, err := s.ListAPITokens(ctx, u.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError("could not list tokens", err)
		}
		return &listTokensOutput{Body: tokens}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-api-token",
		Method:        http.MethodDelete,
		Path:          "/api/tokens/{id}",
		Summary:       "Revoke an API token",
		Tags:          []string{"tokens"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound},
		Security:      apisecurity.Session(api),
	}, func(ctx context.Context, in *struct {
		ID int64 `path:"id" doc:"Token id to revoke"`
	}) (*struct{}, error) {
		u, err := RequireSessionUser(ctx)
		if err != nil {
			return nil, err
		}
		deleted, err := s.DeleteAPIToken(ctx, u.ID, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError("could not delete token", err)
		}
		if !deleted {
			return nil, huma.Error404NotFound("token not found")
		}
		return &struct{}{}, nil
	})
}
