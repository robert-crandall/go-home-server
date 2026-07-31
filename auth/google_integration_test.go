package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// These cover what Google sign-in does to the users table: who it lets in, who
// it creates, and who it turns away. They need Postgres; testPool skips without
// it.

// signInWithGoogle drives one complete callback for a verified Google email and
// returns the response.
func signInWithGoogle(t *testing.T, svc *Service, email string) *httptest.ResponseRecorder {
	t.Helper()
	claims := validClaims()
	claims["email"] = email
	mux, _ := googleFixture(t, svc, tokenEndpointReturning(idToken(t, claims)))

	const state = "state-for-this-sign-in"
	return callback(mux, url.Values{"code": {"the-code"}, "state": {state}}, state)
}

func activeUserCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM users WHERE deleted_at IS NULL`).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	return n
}

// assertSignedInAs checks the response set a session cookie that really
// authenticates as the expected user - the point of the whole feature is that
// the Google door produces the same credential the password door does.
func assertSignedInAs(t *testing.T, svc *Service, rec *httptest.ResponseRecorder, wantID int64) {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/" {
		t.Fatalf("Location = %q, want /", got)
	}
	session := responseCookie(t, rec, cookieName)
	if session == nil || session.Value == "" {
		t.Fatal("no session cookie was set")
	}
	if !session.HttpOnly {
		t.Fatal("session cookie must be HttpOnly")
	}
	// The in-flight state is spent, so it gets cleared alongside.
	if state := responseCookie(t, rec, stateCookieName); state == nil || state.MaxAge >= 0 {
		t.Fatalf("state cookie = %#v, want it cleared", state)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(session)
	u, ok, _ := serveThroughMiddleware(svc, req)
	if !ok {
		t.Fatal("the session cookie from Google sign-in does not authenticate")
	}
	if u.ID != wantID {
		t.Fatalf("signed in as user %d, want %d", u.ID, wantID)
	}
}

// The headline case: one account, two ways in. A user who registered with a
// password is found by their Google email - no second row, no linking step.
func TestGoogleSignInUsesTheExistingPasswordAccount(t *testing.T) {
	pool := testPool(t)
	svc := NewService(pool, false) // registration closed after the first user

	existing, err := svc.CreateUser(context.Background(), "Person@Example.com", "supersecret")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Google reports the address lowercased; matching is case-insensitive, the
	// same way password login is.
	rec := signInWithGoogle(t, svc, "person@example.com")

	assertSignedInAs(t, svc, rec, existing.ID)
	if n := activeUserCount(t, pool); n != 1 {
		t.Fatalf("user count = %d, want 1 - Google sign-in duplicated the account", n)
	}
	// And the password still works, so the account really does have both doors.
	if _, err := svc.authenticate(context.Background(), "person@example.com", "supersecret"); err != nil {
		t.Fatalf("password login broke after Google sign-in: %v", err)
	}
}

// The gate. With registration closed, a Google account nobody registered is a
// stranger and gets bounced - this is what stops the whole internet logging in.
func TestGoogleSignInRejectsUnknownEmailWhenRegistrationClosed(t *testing.T) {
	pool := testPool(t)
	svc := NewService(pool, false)

	if _, err := svc.CreateUser(context.Background(), "owner@example.com", "supersecret"); err != nil {
		t.Fatalf("create user: %v", err)
	}

	rec := signInWithGoogle(t, svc, "stranger@example.com")

	assertFailureRedirect(t, rec, errCodeRegistrations)
	if n := activeUserCount(t, pool); n != 1 {
		t.Fatalf("user count = %d, want 1 - a stranger got an account", n)
	}
}

// Google never bootstraps the first account, even though password registration
// is first-user-open. Keeping that door password-only guarantees the account a
// single-user app runs on always has a password to fall back on.
func TestGoogleSignInDoesNotCreateTheFirstUser(t *testing.T) {
	pool := testPool(t)
	svc := NewService(pool, false)

	rec := signInWithGoogle(t, svc, "first@example.com")

	assertFailureRedirect(t, rec, errCodeRegistrations)
	if n := activeUserCount(t, pool); n != 0 {
		t.Fatalf("user count = %d, want 0", n)
	}
}

// With ALLOW_OPEN_REGISTRATION on, Google is a self-service signup, exactly like
// POST /api/auth/register already is.
func TestGoogleSignInCreatesUserWhenRegistrationIsOpen(t *testing.T) {
	pool := testPool(t)
	svc := NewService(pool, false)
	svc.OpenRegistration = true

	rec := signInWithGoogle(t, svc, "newcomer@example.com")

	created, err := svc.userByEmail(context.Background(), "newcomer@example.com")
	if err != nil {
		t.Fatalf("the user was not created: %v", err)
	}
	assertSignedInAs(t, svc, rec, created.ID)
	if n := activeUserCount(t, pool); n != 1 {
		t.Fatalf("user count = %d, want 1", n)
	}

	// Signing in again reuses the row rather than failing on the unique index.
	again := signInWithGoogle(t, svc, "newcomer@example.com")
	assertSignedInAs(t, svc, again, created.ID)
	if n := activeUserCount(t, pool); n != 1 {
		t.Fatalf("user count = %d after a second sign-in, want 1", n)
	}
}

// A Google-created account has a password hash (the column is NOT NULL and this
// change deliberately keeps it that way), but the plaintext was random and was
// never stored, so nothing can log in with it.
func TestGoogleCreatedUserHasNoUsablePassword(t *testing.T) {
	pool := testPool(t)
	svc := NewService(pool, false)
	svc.OpenRegistration = true

	if rec := signInWithGoogle(t, svc, "google-only@example.com"); rec.Code != http.StatusFound {
		t.Fatalf("sign-in status = %d, want 302", rec.Code)
	}

	var hash string
	if err := pool.QueryRow(context.Background(),
		`SELECT password_hash FROM users WHERE email = $1`, "google-only@example.com",
	).Scan(&hash); err != nil {
		t.Fatalf("read password_hash: %v", err)
	}
	if hash == "" {
		t.Fatal("password_hash is empty; it must stay a real, unusable bcrypt hash")
	}

	// The obvious guesses, plus the value the hash was actually derived from
	// being unavailable by construction.
	for _, guess := range []string{"", "password", hash, "google-only@example.com"} {
		if _, err := svc.authenticate(context.Background(), "google-only@example.com", guess); err == nil {
			t.Fatalf("password %q logged in to a Google-created account", guess)
		}
	}
}

// A soft-deleted user is gone, so their Google account is a stranger again.
func TestGoogleSignInIgnoresSoftDeletedUsers(t *testing.T) {
	pool := testPool(t)
	svc := NewService(pool, false)

	if _, err := svc.CreateUser(context.Background(), "gone@example.com", "supersecret"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET deleted_at = now() WHERE email = $1`, "gone@example.com"); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	assertFailureRedirect(t, signInWithGoogle(t, svc, "gone@example.com"), errCodeRegistrations)
}
