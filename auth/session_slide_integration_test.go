package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/robert-crandall/go-home-server/server"
)

// These cover the sliding session: an authenticated cookie request pushes the
// session's expiry a full sessionTTL out and re-sends the cookie, so an active
// user is never logged out. They need Postgres; testPool skips without it.

// slideFixture returns a service, a user, and a session token whose row expires
// at the given instant.
func slideFixture(t *testing.T, expires time.Time) (*Service, User, string) {
	t.Helper()
	svc := NewService(testPool(t), false)
	u, err := svc.CreateUser(context.Background(), "slide@example.com", "supersecret")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	token := randomToken()
	if _, err := svc.db.Exec(context.Background(),
		`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		hashToken(token), u.ID, expires,
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	return svc, u, token
}

// sessionExpiry reads the row's expiry, or reports that the row is gone.
func sessionExpiry(t *testing.T, svc *Service, token string) (time.Time, bool) {
	t.Helper()
	var got time.Time
	err := svc.db.QueryRow(context.Background(),
		`SELECT expires_at FROM sessions WHERE token_hash = $1`, hashToken(token)).Scan(&got)
	if err != nil {
		return time.Time{}, false
	}
	return got, true
}

// serveThroughMiddleware runs one request through the auth middleware and
// reports the user it resolved plus the response recorder.
func serveThroughMiddleware(svc *Service, req *http.Request) (User, bool, *httptest.ResponseRecorder) {
	var (
		got User
		ok  bool
	)
	rec := httptest.NewRecorder()
	svc.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, ok = UserFromContext(r.Context())
	})).ServeHTTP(rec, req)
	return got, ok, rec
}

// sessionCookies returns the session cookies set on a recorded response.
func sessionCookies(rec *httptest.ResponseRecorder) []*http.Cookie {
	var out []*http.Cookie
	for _, c := range (&http.Response{Header: rec.Header()}).Cookies() {
		if c.Name == cookieName {
			out = append(out, c)
		}
	}
	return out
}

func apiRequestWithCookie(token string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	return req
}

// The main behavior: a session ten days from expiry is pushed back to a full
// TTL, and the browser is handed the same token with the new expiry.
func TestSessionSlidesOnAuthenticatedRequest(t *testing.T) {
	svc, u, token := slideFixture(t, time.Now().Add(10*24*time.Hour))

	got, ok, rec := serveThroughMiddleware(svc, apiRequestWithCookie(token))
	if !ok || got.ID != u.ID {
		t.Fatalf("resolved user = %+v (ok=%v), want user %d", got, ok, u.ID)
	}

	want := time.Now().Add(sessionTTL)
	stored, found := sessionExpiry(t, svc, token)
	if !found {
		t.Fatal("session row disappeared")
	}
	if stored.Sub(want).Abs() > time.Minute {
		t.Fatalf("stored expires_at = %v, want ~%v", stored, want)
	}

	cookies := sessionCookies(rec)
	if len(cookies) != 1 {
		t.Fatalf("got %d session cookies, want 1", len(cookies))
	}
	if cookies[0].Value != token {
		t.Fatal("refreshed cookie carries a different token; the session should not rotate")
	}
	// The cookie must expire with the row, or the browser drops it early (or
	// keeps a token the server has already forgotten).
	if cookies[0].Expires.Sub(stored).Abs() > time.Minute {
		t.Fatalf("cookie Expires = %v, stored expires_at = %v", cookies[0].Expires, stored)
	}
	if !cookies[0].HttpOnly {
		t.Fatal("refreshed cookie lost HttpOnly")
	}
}

// An already-expired session is not authenticated and, more importantly, not
// resurrected by the update.
func TestExpiredSessionIsNotSlid(t *testing.T) {
	expired := time.Now().Add(-time.Hour)
	svc, _, token := slideFixture(t, expired)

	_, ok, rec := serveThroughMiddleware(svc, apiRequestWithCookie(token))
	if ok {
		t.Fatal("expired session authenticated a request")
	}
	if len(sessionCookies(rec)) != 0 {
		t.Fatal("expired session got a refreshed cookie")
	}
	stored, found := sessionExpiry(t, svc, token)
	if !found {
		t.Fatal("session row disappeared")
	}
	if stored.Sub(expired).Abs() > time.Minute {
		t.Fatalf("expired session was resurrected: expires_at = %v, want ~%v", stored, expired)
	}
}

// A soft-deleted user's session stays dead, and stays un-slid.
func TestSoftDeletedUserSessionIsNotSlid(t *testing.T) {
	original := time.Now().Add(10 * 24 * time.Hour)
	svc, u, token := slideFixture(t, original)
	if _, err := svc.db.Exec(context.Background(),
		`UPDATE users SET deleted_at = now() WHERE id = $1`, u.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	_, ok, rec := serveThroughMiddleware(svc, apiRequestWithCookie(token))
	if ok {
		t.Fatal("soft-deleted user authenticated a request")
	}
	if len(sessionCookies(rec)) != 0 {
		t.Fatal("soft-deleted user's session got a refreshed cookie")
	}
	stored, _ := sessionExpiry(t, svc, token)
	if stored.Sub(original).Abs() > time.Minute {
		t.Fatalf("expires_at = %v, want it left at ~%v", stored, original)
	}
}

// A bearer request commits to token auth with no cookie fallback, so a session
// cookie riding along on it must not be slid - otherwise a script polling with
// an API token would keep a browser session alive forever.
func TestBearerRequestDoesNotSlideCookieSession(t *testing.T) {
	original := time.Now().Add(10 * 24 * time.Hour)
	svc, u, token := slideFixture(t, original)
	svc.apiTokensEnabled = true // TokenHumaConfig does this in a real app
	_, plaintext, err := svc.CreateAPIToken(context.Background(), u.ID, "script", nil)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	req := apiRequestWithCookie(token)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	got, ok, rec := serveThroughMiddleware(svc, req)
	if !ok || got.ID != u.ID {
		t.Fatalf("bearer request resolved %+v (ok=%v), want user %d", got, ok, u.ID)
	}
	if len(sessionCookies(rec)) != 0 {
		t.Fatal("bearer request refreshed the session cookie")
	}
	stored, _ := sessionExpiry(t, svc, token)
	if stored.Sub(original).Abs() > time.Minute {
		t.Fatalf("expires_at = %v, want it left at ~%v", stored, original)
	}
}

// Logout runs through the whole huma stack because the middleware and the
// handler both want to set a session cookie on the same response. huma writes
// scalar output headers with Header().Set, so the handler's clearing cookie
// replaces the middleware's refresh rather than appending to it. This asserts
// that, since getting it wrong would leave the browser holding a live-looking
// cookie after logging out.
func TestLogoutClearsCookieDespiteRefresh(t *testing.T) {
	// Ten days out, so the middleware really does refresh it on the way in.
	svc, _, token := slideFixture(t, time.Now().Add(10*24*time.Hour))

	// Runs after svc.Middleware and before the handler, so it sees the refresh
	// cookie the middleware wrote. Without it this test would also pass on a
	// non-sliding middleware, where there was never a second cookie to replace.
	var refreshed []string
	spy := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			refreshed = w.Header().Values("Set-Cookie")
			next.ServeHTTP(w, r)
		})
	}

	srv := server.New(server.Options{
		Title:       "Slide",
		Version:     "0.0.0",
		Middlewares: []func(http.Handler) http.Handler{svc.Middleware, spy},
	})
	svc.Register(srv.API)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	rec := httptest.NewRecorder()
	srv.Router.ServeHTTP(rec, req)

	if len(refreshed) != 1 {
		t.Fatalf("middleware set %v before the handler ran, want one refresh cookie", refreshed)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204 (body: %s)", rec.Code, rec.Body.String())
	}
	cookies := sessionCookies(rec)
	if len(cookies) != 1 {
		t.Fatalf("got %d session cookies, want exactly 1 (the clearing one)", len(cookies))
	}
	if cookies[0].Value != "" || cookies[0].MaxAge >= 0 {
		t.Fatalf("logout left a live session cookie: %+v", cookies[0])
	}
	if _, found := sessionExpiry(t, svc, token); found {
		t.Fatal("logout did not delete the session row")
	}
}
