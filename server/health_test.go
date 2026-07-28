package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthzOKWithoutCheck(t *testing.T) {
	srv := New(Options{})
	rec := httptest.NewRecorder()
	srv.Router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("body = %q, want status ok", rec.Body.String())
	}
}

func TestHealthzOKWhenCheckPasses(t *testing.T) {
	srv := New(Options{HealthCheck: func(context.Context) error { return nil }})
	rec := httptest.NewRecorder()
	srv.Router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestHealthz503WhenCheckFails(t *testing.T) {
	srv := New(Options{HealthCheck: func(context.Context) error { return errors.New("db down") }})
	rec := httptest.NewRecorder()
	srv.Router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"degraded"`) {
		t.Fatalf("body = %q, want status degraded", rec.Body.String())
	}
}
