// Package wiring holds the foundation's cross-package composition test.
//
// Every other test in this module registers one package's endpoints on its own
// fresh huma API. That can't catch a collision between packages - two
// operations sharing an ID, or two input/output types reflecting to the same
// schema name - because nothing else ever mounts them all on one API. huma
// detects both at registration time by panicking, so the only way to find them
// is to actually perform the registrations.
//
// A reference app used to do this incidentally, via a CI job that generated its
// OpenAPI spec. This test is what's left after that app moved to its own repo.
//
// It is test-only on purpose: it exists to be run, not imported.
package wiring

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/robert-crandall/go-home-server/auth"
	"github.com/robert-crandall/go-home-server/files"
	"github.com/robert-crandall/go-home-server/notify"
	"github.com/robert-crandall/go-home-server/server"
)

// TestFoundationRegistersOnOneAPI mounts every endpoint the foundation offers
// onto a single huma API and serializes the resulting spec.
//
// The services hold a nil *pgxpool.Pool: registration only builds handlers and
// reflects their types, so no database is reachable from here. That keeps this
// test unconditional - it runs even when TEST_DATABASE_URL is unset.
func TestFoundationRegistersOnOneAPI(t *testing.T) {
	notifySvc, err := notify.NewService(nil, notify.VAPID{})
	if err != nil {
		t.Fatalf("notify.NewService: %v", err)
	}

	filesSvc, err := files.NewService(nil, files.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("files.NewService: %v", err)
	}

	authSvc := auth.NewService(nil, true)

	srv := server.New(server.Options{
		Title:       "Wiring",
		Version:     "0.0.0",
		Middlewares: []func(http.Handler) http.Handler{authSvc.Middleware},
	})

	currentUser := func(ctx context.Context) (int64, error) {
		u, err := auth.RequireUser(ctx)
		return u.ID, err
	}

	// A duplicate operation ID or a colliding schema name panics in here.
	authSvc.Register(srv.API)
	authSvc.RegisterTokens(srv.API)
	notify.Register(srv.API, notifySvc, currentUser)
	files.Register(srv.API, filesSvc, currentUser)

	spec := srv.API.OpenAPI()
	if _, err := json.Marshal(spec); err != nil {
		t.Fatalf("marshal OpenAPI spec: %v", err)
	}

	// The exact HTTP surface this module registers. Asserting the full set,
	// rather than just "some paths exist", is what makes the panic-catching
	// above meaningful: if notify.Register or RegisterTokens silently became a
	// no-op, auth's paths alone would keep a non-empty check green.
	//
	// This is deliberately a change-detector. Adding or renaming an endpoint in
	// a module other apps vendor should be a conscious edit, not a surprise.
	want := map[string]string{
		"register":                "POST /api/auth/register",
		"login":                   "POST /api/auth/login",
		"logout":                  "POST /api/auth/logout",
		"current-user":            "GET /api/auth/me",
		"create-api-token":        "POST /api/tokens",
		"list-api-tokens":         "GET /api/tokens",
		"delete-api-token":        "DELETE /api/tokens/{id}",
		"push-subscribe":          "POST /api/push/subscribe",
		"push-unsubscribe":        "POST /api/push/unsubscribe",
		"push-test":               "POST /api/push/test",
		"push-vapid-key":          "GET /api/push/vapid-public-key",
		"upload-file":             "POST /api/files",
		"list-files":              "GET /api/files",
		"download-file":           "GET /api/files/{id}",
		"download-file-thumbnail": "GET /api/files/{id}/thumbnail",
		"delete-file":             "DELETE /api/files/{id}",
	}

	got := map[string]string{}
	for path, item := range spec.Paths {
		for method, op := range map[string]*huma.Operation{
			http.MethodGet:    item.Get,
			http.MethodPost:   item.Post,
			http.MethodPut:    item.Put,
			http.MethodPatch:  item.Patch,
			http.MethodDelete: item.Delete,
		} {
			if op != nil {
				got[op.OperationID] = method + " " + path
			}
		}
	}

	for id, route := range want {
		switch actual, ok := got[id]; {
		case !ok:
			t.Errorf("operation %q was not registered", id)
		case actual != route:
			t.Errorf("operation %q is %s, want %s", id, actual, route)
		}
	}
	for id, route := range got {
		if _, ok := want[id]; !ok {
			t.Errorf("unexpected operation %q (%s): add it to want if intended", id, route)
		}
	}
}
