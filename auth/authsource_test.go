package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

func statusOf(t *testing.T, err error) int {
	t.Helper()
	var se huma.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error %v is not a huma.StatusError", err)
	}
	return se.GetStatus()
}

func TestRequireSessionUser(t *testing.T) {
	u := User{ID: 1, Email: "a@example.com"}

	// A session-authed request is allowed.
	if _, err := RequireSessionUser(withUser(context.Background(), u, AuthSession)); err != nil {
		t.Fatalf("session user should be allowed, got %v", err)
	}

	// A token-authed request is forbidden (403), so a leaked token can't manage
	// tokens.
	_, err := RequireSessionUser(withUser(context.Background(), u, AuthToken))
	if err == nil {
		t.Fatal("token-authed request should be forbidden")
	}
	if code := statusOf(t, err); code != 403 {
		t.Fatalf("token-authed status = %d, want 403", code)
	}

	// No user at all is unauthorized (401), and fails closed on the missing
	// source rather than being mistaken for a session.
	_, err = RequireSessionUser(context.Background())
	if err == nil {
		t.Fatal("missing user should be unauthorized")
	}
	if code := statusOf(t, err); code != 401 {
		t.Fatalf("missing-user status = %d, want 401", code)
	}
}

func TestRequireUserAcceptsBothSources(t *testing.T) {
	u := User{ID: 2, Email: "b@example.com"}
	for _, src := range []AuthSource{AuthSession, AuthToken} {
		if _, err := RequireUser(withUser(context.Background(), u, src)); err != nil {
			t.Fatalf("RequireUser should accept source %d, got %v", src, err)
		}
	}
	if _, err := RequireUser(context.Background()); err == nil {
		t.Fatal("RequireUser should reject a request with no user")
	}
}
