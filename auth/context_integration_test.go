package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type middlewareContextResult struct {
	session Session
	ok      bool
	err     error
}

func middlewareContext(svc *Service, req *http.Request) middlewareContextResult {
	var result middlewareContextResult
	svc.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		result.session, result.ok = SessionFromContext(r.Context())
		result.err = ErrorFromContext(r.Context())
	})).ServeHTTP(httptest.NewRecorder(), req)
	return result
}

func TestMiddlewareWithoutCredentialsHasNoSessionOrError(t *testing.T) {
	svc := NewService(nil, false)
	result := middlewareContext(svc, httptest.NewRequest(http.MethodGet, "/api/probe", nil))
	if result.ok {
		t.Fatal("request without credentials has a session")
	}
	if result.err != nil {
		t.Fatalf("request without credentials error = %v, want nil", result.err)
	}
}

func TestMiddlewareExposesCookieSessionHash(t *testing.T) {
	svc, _, token := slideFixture(t, time.Now().Add(time.Hour))
	result := middlewareContext(svc, apiRequestWithCookie(token))
	if !result.ok {
		t.Fatal("cookie-authenticated request has no session")
	}
	if result.session.TokenHash != hashToken(token) {
		t.Fatalf("TokenHash = %q, want %q", result.session.TokenHash, hashToken(token))
	}
	if result.err != nil {
		t.Fatalf("cookie-authenticated request error = %v, want nil", result.err)
	}
}

func TestInvalidCookieCredentialsHaveNoOperationalError(t *testing.T) {
	t.Run("garbage cookie", func(t *testing.T) {
		svc := NewService(testPool(t), false)
		result := middlewareContext(svc, apiRequestWithCookie("not-a-session"))
		if result.ok {
			t.Fatal("garbage cookie has a session")
		}
		if result.err != nil {
			t.Fatalf("garbage cookie error = %v, want nil", result.err)
		}
	})

	t.Run("expired session", func(t *testing.T) {
		svc, _, token := slideFixture(t, time.Now().Add(-time.Hour))
		result := middlewareContext(svc, apiRequestWithCookie(token))
		if result.ok {
			t.Fatal("expired session authenticated")
		}
		if result.err != nil {
			t.Fatalf("expired session error = %v, want nil", result.err)
		}
	})

	t.Run("soft-deleted user", func(t *testing.T) {
		svc, u, token := slideFixture(t, time.Now().Add(time.Hour))
		if _, err := svc.db.Exec(context.Background(),
			`UPDATE users SET deleted_at = now() WHERE id = $1`, u.ID); err != nil {
			t.Fatalf("soft delete user: %v", err)
		}
		result := middlewareContext(svc, apiRequestWithCookie(token))
		if result.ok {
			t.Fatal("soft-deleted user's session authenticated")
		}
		if result.err != nil {
			t.Fatalf("soft-deleted user error = %v, want nil", result.err)
		}
	})
}

func TestCookieLookupOperationalErrorIsExposed(t *testing.T) {
	svc := NewService(testPool(t), false)
	svc.db.Close()

	result := middlewareContext(svc, apiRequestWithCookie("any-nonempty-token"))
	if result.ok {
		t.Fatal("failed cookie lookup has a session")
	}
	if result.err == nil {
		t.Fatal("failed cookie lookup error = nil, want operational error")
	}
}

func TestBearerContextSemantics(t *testing.T) {
	svc, u := tokenTestUser(t)
	ctx := context.Background()
	cookie, _, err := svc.createSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	_, plaintext, err := svc.CreateAPIToken(ctx, u.ID, "context-test", nil)
	if err != nil {
		t.Fatalf("create API token: %v", err)
	}

	request := func(service *Service, bearer string) middlewareContextResult {
		req := apiRequestWithCookie(cookie)
		req.Header.Set("Authorization", "Bearer "+bearer)
		return middlewareContext(service, req)
	}

	t.Run("selected bearer has no session", func(t *testing.T) {
		result := request(svc, plaintext)
		if result.ok {
			t.Fatal("bearer-authenticated request has a cookie session")
		}
		if result.err != nil {
			t.Fatalf("valid bearer error = %v, want nil", result.err)
		}
	})

	t.Run("invalid bearer has no operational error", func(t *testing.T) {
		result := request(svc, "not-a-token")
		if result.ok {
			t.Fatal("invalid bearer has a session")
		}
		if result.err != nil {
			t.Fatalf("invalid bearer error = %v, want nil", result.err)
		}
	})

	t.Run("tokens disabled uses cookie session", func(t *testing.T) {
		disabled := NewService(svc.db, false)
		result := request(disabled, plaintext)
		if !result.ok {
			t.Fatal("tokens-disabled cookie fallback has no session")
		}
		if result.session.TokenHash != hashToken(cookie) {
			t.Fatalf("TokenHash = %q, want %q", result.session.TokenHash, hashToken(cookie))
		}
		if result.err != nil {
			t.Fatalf("tokens-disabled cookie fallback error = %v, want nil", result.err)
		}
	})

	svc.db.Close()
	t.Run("lookup failure is exposed", func(t *testing.T) {
		result := request(svc, plaintext)
		if result.ok {
			t.Fatal("failed bearer lookup has a session")
		}
		if result.err == nil {
			t.Fatal("failed bearer lookup error = nil, want operational error")
		}
	})
}
