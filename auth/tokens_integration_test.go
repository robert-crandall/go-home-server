package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// tokenTestUser creates a user and returns the service (with API tokens enabled)
// and the user. It relies on testPool (auth_integration_test.go) for a clean DB.
func tokenTestUser(t *testing.T) (*Service, User) {
	t.Helper()
	pool := testPool(t)
	svc := NewService(pool, false)
	svc.apiTokensEnabled = true // RegisterTokens would do this in a real app
	u, err := svc.CreateUser(context.Background(), "token-user@example.com", "supersecret")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return svc, u
}

func TestAPITokenLifecycle(t *testing.T) {
	svc, u := tokenTestUser(t)
	ctx := context.Background()

	tok, plaintext, err := svc.CreateAPIToken(ctx, u.ID, "laptop", nil)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if tok.Last4 == "" || len(tok.Last4) != 4 {
		t.Fatalf("last4 = %q, want 4 chars", tok.Last4)
	}
	if tok.ExpiresAt != nil {
		t.Fatal("expiresAt should be nil for a non-expiring token")
	}

	// The plaintext authenticates as the owner.
	got, err := svc.userFromAPIToken(ctx, plaintext)
	if err != nil {
		t.Fatalf("userFromAPIToken: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("token resolved to user %d, want %d", got.ID, u.ID)
	}

	// It shows up in the list.
	list, err := svc.ListAPITokens(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListAPITokens: %v", err)
	}
	if len(list) != 1 || list[0].ID != tok.ID {
		t.Fatalf("list = %+v, want the one token %d", list, tok.ID)
	}

	// Revoking it makes the plaintext stop working immediately.
	deleted, err := svc.DeleteAPIToken(ctx, u.ID, tok.ID)
	if err != nil || !deleted {
		t.Fatalf("DeleteAPIToken: deleted=%v err=%v", deleted, err)
	}
	if _, err := svc.userFromAPIToken(ctx, plaintext); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked token error = %v, want ErrNotFound", err)
	}
}

func TestAPITokenWrongSecretRejected(t *testing.T) {
	svc, u := tokenTestUser(t)
	ctx := context.Background()

	tok, _, err := svc.CreateAPIToken(ctx, u.ID, "x", nil)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	// Right id, wrong secret.
	forged := FormatAPIToken(tok.ID, "not-the-real-secret")
	if _, err := svc.userFromAPIToken(ctx, forged); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong-secret error = %v, want ErrNotFound", err)
	}
}

func TestAPITokenExpiredRejected(t *testing.T) {
	svc, u := tokenTestUser(t)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	_, plaintext, err := svc.CreateAPIToken(ctx, u.ID, "expired", &past)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if _, err := svc.userFromAPIToken(ctx, plaintext); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired token error = %v, want ErrNotFound", err)
	}
}

func TestAPITokenDeleteIsOwnerScoped(t *testing.T) {
	svc, u := tokenTestUser(t)
	ctx := context.Background()
	other, err := svc.CreateUser(ctx, "other@example.com", "supersecret")
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}

	tok, _, err := svc.CreateAPIToken(ctx, u.ID, "x", nil)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	// Another user deleting it: no rows, no error, and the token survives.
	deleted, err := svc.DeleteAPIToken(ctx, other.ID, tok.ID)
	if err != nil {
		t.Fatalf("DeleteAPIToken: %v", err)
	}
	if deleted {
		t.Fatal("a non-owner should not be able to delete another user's token")
	}
	if list, _ := svc.ListAPITokens(ctx, u.ID); len(list) != 1 {
		t.Fatalf("token should survive a cross-user delete, list=%+v", list)
	}
}

// TestMiddlewareCredentialSelection is the core of the non-blocking-middleware
// adaptation: it proves who gets resolved (and how) for each combination of a
// session cookie and a bearer token.
func TestMiddlewareCredentialSelection(t *testing.T) {
	svc, u := tokenTestUser(t)
	ctx := context.Background()

	// A valid session cookie.
	cookie, _, err := svc.createSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("createSession: %v", err)
	}
	// A valid API token.
	_, tokenPlain, err := svc.CreateAPIToken(ctx, u.ID, "cli", nil)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	probe := func(svc *Service, cookieVal, authHeader string) (int, User, AuthSource, bool) {
		req := httptest.NewRequest(http.MethodGet, "/api/probe", nil)
		if cookieVal != "" {
			req.AddCookie(&http.Cookie{Name: cookieName, Value: cookieVal})
		}
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		rec := httptest.NewRecorder()

		var (
			gotUser User
			gotSrc  AuthSource
			gotOK   bool
		)
		svc.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotUser, gotOK = UserFromContext(r.Context())
			gotSrc, _ = AuthSourceFromContext(r.Context())
		})).ServeHTTP(rec, req)
		return rec.Code, gotUser, gotSrc, gotOK
	}

	t.Run("cookie only resolves a session user", func(t *testing.T) {
		_, user, src, ok := probe(svc, cookie, "")
		if !ok || user.ID != u.ID || src != AuthSession {
			t.Fatalf("got ok=%v id=%d src=%d, want session user %d", ok, user.ID, src, u.ID)
		}
	})

	t.Run("valid bearer resolves a token user", func(t *testing.T) {
		_, user, src, ok := probe(svc, "", "Bearer "+tokenPlain)
		if !ok || user.ID != u.ID || src != AuthToken {
			t.Fatalf("got ok=%v id=%d src=%d, want token user %d", ok, user.ID, src, u.ID)
		}
	})

	t.Run("bearer wins over cookie", func(t *testing.T) {
		_, _, src, ok := probe(svc, cookie, "Bearer "+tokenPlain)
		if !ok || src != AuthToken {
			t.Fatalf("got ok=%v src=%d, want token source", ok, src)
		}
	})

	t.Run("bad bearer does not fall back to cookie", func(t *testing.T) {
		_, _, _, ok := probe(svc, cookie, "Bearer garbage")
		if ok {
			t.Fatal("a bad bearer must not fall back to the session cookie")
		}
	})

	t.Run("bare bearer does not fall back to cookie", func(t *testing.T) {
		_, _, _, ok := probe(svc, cookie, "Bearer")
		if ok {
			t.Fatal("a bare Bearer must not fall back to the session cookie")
		}
	})

	t.Run("tokens disabled ignores bearer and uses cookie", func(t *testing.T) {
		disabled := NewService(svc.db, false) // apiTokensEnabled stays false
		_, user, src, ok := probe(disabled, cookie, "Bearer "+tokenPlain)
		if !ok || user.ID != u.ID || src != AuthSession {
			t.Fatalf("got ok=%v id=%d src=%d, want cookie fallback to user %d", ok, user.ID, src, u.ID)
		}
	})
}
