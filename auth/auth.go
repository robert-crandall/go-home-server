// Package auth provides opaque server-side session authentication for apps
// built on the foundation.
//
// The model, per the stack's recommended default: a random session token is
// delivered to the browser as an httpOnly cookie, and only its SHA-256 hash is
// stored in Postgres. Sessions are therefore revocable (delete the row) and the
// raw token never lives at rest. Passwords are hashed with bcrypt.
//
// Wiring in an app is three lines:
//
//	svc := auth.NewService(pool, cfg.IsProduction())
//	// add svc.Middleware to the server so every request can resolve a user
//	svc.Register(api) // mounts /api/auth/{register,login,logout,me}
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

const (
	cookieName = "session"
	sessionTTL = 30 * 24 * time.Hour
)

// User is the authenticated principal. It deliberately omits the password hash.
type User struct {
	ID        int64     `json:"id"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"createdAt"`
}

// Errors returned by the service.
var (
	ErrNotFound     = errors.New("auth: not found")
	ErrInvalidLogin = errors.New("auth: invalid email or password")

	errRegistrationClosed = errors.New("auth: registration is closed")
	errEmailTaken         = errors.New("auth: email already registered")
	errPasswordTooLong    = errors.New("auth: password too long")
)

// dummyHash is a valid bcrypt hash compared against on the "no such user" path
// so login takes roughly the same time whether or not the email exists,
// avoiding a timing side-channel that would leak which emails are registered.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("timing-equalizer"), bcrypt.DefaultCost)

// isUniqueViolation reports whether err is a Postgres unique-constraint error.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// registrationLock is an arbitrary, fixed advisory-lock key used to serialize
// first-user registration checks so the "first user only" gate can't be raced.
const registrationLock int64 = 8021994

// Service holds the dependencies for authentication.
type Service struct {
	db     *pgxpool.Pool
	secure bool // set the Secure cookie flag (true in production/HTTPS)

	// OpenRegistration allows anyone to register. When false (the default),
	// registration is first-user-only: it succeeds only while no active user
	// exists, then closes. This is the safe default for single-user apps.
	OpenRegistration bool

	// apiTokensEnabled gates bearer (API token) authentication. It's flipped on
	// by RegisterTokens, so an app that never opts into API tokens neither
	// exposes the token-management endpoints nor honors bearer credentials -
	// even against a shared database that happens to contain token rows. When
	// false, an Authorization: Bearer header is ignored and the request falls
	// back to cookie auth.
	apiTokensEnabled bool
}

// NewService constructs an auth service. Pass secureCookies=true in production
// so the session cookie is only sent over HTTPS.
func NewService(db *pgxpool.Pool, secureCookies bool) *Service {
	return &Service{db: db, secure: secureCookies}
}

// --- user + session persistence -------------------------------------------

// CreateUser hashes the password and inserts a new user.
func (s *Service) CreateUser(ctx context.Context, email, password string) (User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, err
	}
	var u User
	err = s.db.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, $2)
		 RETURNING id, email, created_at`,
		email, string(hash),
	).Scan(&u.ID, &u.Email, &u.CreatedAt)
	return u, err
}

// registerUser gates registration and creates the user + an initial session in
// a single transaction. When OpenRegistration is false, it takes a DB advisory
// lock and refuses if any active user already exists, so concurrent first
// registrations can't both win the race.
func (s *Service) registerUser(ctx context.Context, email, password string) (User, string, time.Time, error) {
	// Hash outside the transaction to keep the lock hold time short.
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		if errors.Is(err, bcrypt.ErrPasswordTooLong) {
			return User{}, "", time.Time{}, errPasswordTooLong
		}
		return User{}, "", time.Time{}, err
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return User{}, "", time.Time{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if !s.OpenRegistration {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, registrationLock); err != nil {
			return User{}, "", time.Time{}, err
		}
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM users WHERE deleted_at IS NULL`).Scan(&count); err != nil {
			return User{}, "", time.Time{}, err
		}
		if count > 0 {
			return User{}, "", time.Time{}, errRegistrationClosed
		}
	}

	var u User
	if err := tx.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, $2)
		 RETURNING id, email, created_at`,
		email, string(hash),
	).Scan(&u.ID, &u.Email, &u.CreatedAt); err != nil {
		if isUniqueViolation(err) {
			return User{}, "", time.Time{}, errEmailTaken
		}
		return User{}, "", time.Time{}, err
	}

	token := randomToken()
	expires := time.Now().Add(sessionTTL)
	if _, err := tx.Exec(ctx,
		`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		hashToken(token), u.ID, expires,
	); err != nil {
		return User{}, "", time.Time{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return User{}, "", time.Time{}, err
	}
	return u, token, expires, nil
}

func (s *Service) authenticate(ctx context.Context, email, password string) (User, error) {
	var (
		u    User
		hash string
	)
	err := s.db.QueryRow(ctx,
		`SELECT id, email, created_at, password_hash
		   FROM users
		  WHERE lower(email) = lower($1) AND deleted_at IS NULL`,
		email,
	).Scan(&u.ID, &u.Email, &u.CreatedAt, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		// Compare against a dummy hash so a missing user takes about the same
		// time as a wrong password, preventing email enumeration by timing.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return User{}, ErrInvalidLogin
	}
	if err != nil {
		return User{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return User{}, ErrInvalidLogin
	}
	return u, nil
}

func (s *Service) createSession(ctx context.Context, userID int64) (token string, expires time.Time, err error) {
	// Opportunistically clear expired sessions so the table doesn't grow
	// without bound. Best-effort; a failure here shouldn't block login.
	_, _ = s.db.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`)

	token = randomToken()
	expires = time.Now().Add(sessionTTL)
	_, err = s.db.Exec(ctx,
		`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		hashToken(token), userID, expires,
	)
	return token, expires, err
}

func (s *Service) userFromToken(ctx context.Context, token string) (User, error) {
	var u User
	err := s.db.QueryRow(ctx,
		`SELECT u.id, u.email, u.created_at
		   FROM sessions s
		   JOIN users u ON u.id = s.user_id
		  WHERE s.token_hash = $1 AND s.expires_at > now() AND u.deleted_at IS NULL`,
		hashToken(token),
	).Scan(&u.ID, &u.Email, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

func (s *Service) deleteSession(ctx context.Context, token string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hashToken(token))
	return err
}

// --- cookies ---------------------------------------------------------------

func (s *Service) cookie(token string, expires time.Time) http.Cookie {
	return http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	}
}

func (s *Service) clearCookie() http.Cookie {
	return http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// --- middleware + context --------------------------------------------------

type ctxKey int

const (
	userKey ctxKey = iota
	sourceKey
)

// AuthSource records which credential authenticated a request. Its zero value
// is AuthNone, so an unset value can never be mistaken for a real session -
// RequireSessionUser fails closed on it.
type AuthSource int

const (
	// AuthNone: no credential was resolved onto the request.
	AuthNone AuthSource = iota
	// AuthSession: authenticated by the session cookie (a browser).
	AuthSession
	// AuthToken: authenticated by an Authorization: Bearer API token (a script
	// or an MCP server).
	AuthToken
)

// withUser returns a context carrying the resolved user and the credential type
// that authenticated it. The two are always set together, so a user in context
// always has a matching source.
func withUser(ctx context.Context, u User, src AuthSource) context.Context {
	ctx = context.WithValue(ctx, userKey, u)
	return context.WithValue(ctx, sourceKey, src)
}

// Middleware resolves the credential on a request into a User and stashes it on
// the request context. It never blocks the request; use RequireUser inside a
// handler to enforce authentication. It only does work for API requests, so
// static asset requests don't incur an auth lookup.
//
// When API tokens are enabled (RegisterTokens was called), an Authorization
// header with the Bearer scheme commits the request to token auth: the token is
// the sole credential, with no cookie fallback, so a malformed, unknown,
// expired, or wrong-secret bearer resolves no user even if a valid session
// cookie is also present. Without a Bearer header (or when tokens are disabled)
// the session cookie is used as before.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAPIPath(r.URL.Path) {
			if s.apiTokensEnabled && hasBearerScheme(r) {
				if token, ok := bearerToken(r); ok {
					if u, err := s.userFromAPIToken(r.Context(), token); err == nil {
						r = r.WithContext(withUser(r.Context(), u, AuthToken))
					}
				}
			} else if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
				if u, err := s.userFromToken(r.Context(), c.Value); err == nil {
					r = r.WithContext(withUser(r.Context(), u, AuthSession))
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// isAPIPath reports whether a request path belongs to the JSON API, which the
// foundation always mounts under /api.
func isAPIPath(p string) bool {
	return p == "/api" || strings.HasPrefix(p, "/api/")
}

// UserFromContext returns the authenticated user, if any.
func UserFromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(userKey).(User)
	return u, ok
}

// AuthSourceFromContext returns the credential type that authenticated the
// request. ok is false when no user was resolved.
func AuthSourceFromContext(ctx context.Context) (AuthSource, bool) {
	src, ok := ctx.Value(sourceKey).(AuthSource)
	return src, ok
}

// RequireUser returns the authenticated user or a huma 401 error, for use at
// the top of protected huma handlers. It accepts either credential type: a
// session cookie or an API token can both drive the app's API.
func RequireUser(ctx context.Context) (User, error) {
	u, ok := UserFromContext(ctx)
	if !ok {
		return User{}, huma.Error401Unauthorized("authentication required")
	}
	return u, nil
}

// RequireSessionUser is RequireUser plus a hard requirement that the request was
// authenticated by the session cookie (not an API token). A token-authed request
// gets a 403. It guards token management so a leaked API token can't mint, list,
// or revoke tokens - keeping revocation meaningful. It fails closed: a missing
// source (AuthNone) is rejected too.
func RequireSessionUser(ctx context.Context) (User, error) {
	u, err := RequireUser(ctx)
	if err != nil {
		return User{}, err
	}
	if src, _ := AuthSourceFromContext(ctx); src != AuthSession {
		return User{}, huma.Error403Forbidden("this endpoint requires session (cookie) authentication")
	}
	return u, nil
}

// --- huma endpoints --------------------------------------------------------

type credentials struct {
	Email    string `json:"email" format:"email" doc:"Email address"`
	Password string `json:"password" minLength:"8" maxLength:"72" doc:"Password (8-72 chars)"`
}

type credentialsInput struct {
	Body credentials
}

type sessionOutput struct {
	SetCookie http.Cookie `header:"Set-Cookie"`
	Body      User
}

// Register mounts the auth endpoints on the given huma API under /api/auth.
func (s *Service) Register(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "register",
		Method:      http.MethodPost,
		Path:        "/api/auth/register",
		Summary:     "Register a new user",
		Tags:        []string{"auth"},
		Errors:      []int{http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity},
	}, func(ctx context.Context, in *credentialsInput) (*sessionOutput, error) {
		u, token, exp, err := s.registerUser(ctx, in.Body.Email, in.Body.Password)
		if err != nil {
			switch {
			case errors.Is(err, errRegistrationClosed):
				return nil, huma.Error403Forbidden("registration is closed")
			case errors.Is(err, errEmailTaken):
				return nil, huma.Error409Conflict("email already registered")
			case errors.Is(err, errPasswordTooLong):
				return nil, huma.Error422UnprocessableEntity("password is too long (max 72 bytes)")
			default:
				return nil, huma.Error500InternalServerError("could not create user", err)
			}
		}
		return &sessionOutput{SetCookie: s.cookie(token, exp), Body: u}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "login",
		Method:      http.MethodPost,
		Path:        "/api/auth/login",
		Summary:     "Log in",
		Tags:        []string{"auth"},
	}, func(ctx context.Context, in *credentialsInput) (*sessionOutput, error) {
		u, err := s.authenticate(ctx, in.Body.Email, in.Body.Password)
		if err != nil {
			if errors.Is(err, ErrInvalidLogin) {
				return nil, huma.Error401Unauthorized("invalid email or password")
			}
			return nil, huma.Error500InternalServerError("login failed", err)
		}
		token, exp, err := s.createSession(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		return &sessionOutput{SetCookie: s.cookie(token, exp), Body: u}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "logout",
		Method:      http.MethodPost,
		Path:        "/api/auth/logout",
		Summary:     "Log out",
		Tags:        []string{"auth"},
	}, func(ctx context.Context, in *struct {
		Session string `cookie:"session"`
	}) (*struct {
		SetCookie http.Cookie `header:"Set-Cookie"`
	}, error) {
		if in.Session != "" {
			// Fail loudly if we couldn't revoke the session server-side, rather
			// than clearing the cookie and pretending the token is dead.
			if err := s.deleteSession(ctx, in.Session); err != nil {
				return nil, huma.Error500InternalServerError("could not revoke session", err)
			}
		}
		return &struct {
			SetCookie http.Cookie `header:"Set-Cookie"`
		}{SetCookie: s.clearCookie()}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "current-user",
		Method:      http.MethodGet,
		Path:        "/api/auth/me",
		Summary:     "Get the current user",
		Tags:        []string{"auth"},
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body User }, error) {
		u, err := RequireUser(ctx)
		if err != nil {
			return nil, err
		}
		return &struct{ Body User }{Body: u}, nil
	})
}

// --- helpers ---------------------------------------------------------------

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
