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
	"reflect"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/robert-crandall/go-home-server/auth"
	"github.com/robert-crandall/go-home-server/files"
	"github.com/robert-crandall/go-home-server/notify"
	"github.com/robert-crandall/go-home-server/server"
)

type securitySchemeDocument struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	In     string `json:"in"`
	Scheme string `json:"scheme"`
}

type operationDocument struct {
	OperationID string          `json:"operationId"`
	Security    json.RawMessage `json:"security"`
}

type openAPIDocument struct {
	Components struct {
		SecuritySchemes map[string]securitySchemeDocument `json:"securitySchemes"`
	} `json:"components"`
	Paths map[string]map[string]operationDocument `json:"paths"`
}

type operationExpectation struct {
	route    string
	security []map[string][]string
}

func readOpenAPI(t *testing.T, api huma.API) openAPIDocument {
	t.Helper()

	data, err := json.Marshal(api.OpenAPI())
	if err != nil {
		t.Fatalf("marshal OpenAPI spec: %v", err)
	}
	var document openAPIDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("unmarshal OpenAPI spec: %v", err)
	}
	return document
}

func readOperations(t *testing.T, document openAPIDocument) map[string]operationExpectation {
	t.Helper()

	operations := map[string]operationExpectation{}
	for path, item := range document.Paths {
		for method, operation := range item {
			if operation.OperationID == "" {
				continue
			}
			var security []map[string][]string
			if err := json.Unmarshal(operation.Security, &security); err != nil {
				t.Fatalf("operation %q security is missing or invalid: %v", operation.OperationID, err)
			}
			operations[operation.OperationID] = operationExpectation{
				route:    strings.ToUpper(method) + " " + path,
				security: security,
			}
		}
	}
	return operations
}

func TestHumaConfigHook(t *testing.T) {
	const schemeName = "test-bearer"

	srv := server.New(server.Options{
		HumaConfig: func(cfg huma.Config) huma.Config {
			cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
				schemeName: {Type: "http", Scheme: "bearer"},
			}
			return cfg
		},
	})

	scheme := srv.API.OpenAPI().Components.SecuritySchemes[schemeName]
	if scheme == nil {
		t.Fatalf("security scheme %q was not registered", schemeName)
	}
	if scheme.Type != "http" || scheme.Scheme != "bearer" {
		t.Errorf("security scheme = %#v, want HTTP bearer", scheme)
	}
}

func TestSessionOnlyOpenAPISecurity(t *testing.T) {
	authSvc := auth.NewService(nil, true)
	srv := server.New(server.Options{})
	authSvc.Register(srv.API)

	document := readOpenAPI(t, srv.API)
	wantSchemes := map[string]securitySchemeDocument{
		"session": {Type: "apiKey", Name: "session", In: "cookie"},
	}
	if !reflect.DeepEqual(document.Components.SecuritySchemes, wantSchemes) {
		t.Fatalf("security schemes = %#v, want %#v", document.Components.SecuritySchemes, wantSchemes)
	}

	operation := readOperations(t, document)["current-user"]
	wantSecurity := []map[string][]string{
		{"session": []string{}},
	}
	if !reflect.DeepEqual(operation.security, wantSecurity) {
		t.Fatalf("current-user security = %#v, want %#v", operation.security, wantSecurity)
	}
}

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
		HumaConfig:  authSvc.TokenHumaConfig,
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

	document := readOpenAPI(t, srv.API)
	wantSchemes := map[string]securitySchemeDocument{
		"session": {Type: "apiKey", Name: "session", In: "cookie"},
		"bearer":  {Type: "http", Scheme: "bearer"},
	}
	if !reflect.DeepEqual(document.Components.SecuritySchemes, wantSchemes) {
		t.Fatalf("security schemes = %#v, want %#v", document.Components.SecuritySchemes, wantSchemes)
	}

	// The exact HTTP surface this module registers. Asserting the full set,
	// rather than just "some paths exist", is what makes the panic-catching
	// above meaningful: if notify.Register or RegisterTokens silently became a
	// no-op, auth's paths alone would keep a non-empty check green.
	//
	// This is deliberately a change-detector. Adding or renaming an endpoint in
	// a module other apps vendor should be a conscious edit, not a surprise.
	public := []map[string][]string{}
	sessionOnly := []map[string][]string{
		{"session": []string{}},
	}
	sessionOrBearer := []map[string][]string{
		{"session": []string{}},
		{"bearer": []string{}},
	}
	want := map[string]operationExpectation{
		"register":                {route: "POST /api/auth/register", security: public},
		"login":                   {route: "POST /api/auth/login", security: public},
		"logout":                  {route: "POST /api/auth/logout", security: public},
		"current-user":            {route: "GET /api/auth/me", security: sessionOrBearer},
		"create-api-token":        {route: "POST /api/tokens", security: sessionOnly},
		"list-api-tokens":         {route: "GET /api/tokens", security: sessionOnly},
		"delete-api-token":        {route: "DELETE /api/tokens/{id}", security: sessionOnly},
		"push-subscribe":          {route: "POST /api/push/subscribe", security: sessionOrBearer},
		"push-unsubscribe":        {route: "POST /api/push/unsubscribe", security: sessionOrBearer},
		"push-test":               {route: "POST /api/push/test", security: sessionOrBearer},
		"push-vapid-key":          {route: "GET /api/push/vapid-public-key", security: public},
		"upload-file":             {route: "POST /api/files", security: sessionOrBearer},
		"list-files":              {route: "GET /api/files", security: sessionOrBearer},
		"download-file":           {route: "GET /api/files/{id}", security: sessionOrBearer},
		"download-file-thumbnail": {route: "GET /api/files/{id}/thumbnail", security: sessionOrBearer},
		"delete-file":             {route: "DELETE /api/files/{id}", security: sessionOrBearer},
	}

	got := readOperations(t, document)
	for id, expected := range want {
		switch actual, ok := got[id]; {
		case !ok:
			t.Errorf("operation %q was not registered", id)
		case actual.route != expected.route:
			t.Errorf("operation %q is %s, want %s", id, actual.route, expected.route)
		case !reflect.DeepEqual(actual.security, expected.security):
			t.Errorf("operation %q security = %#v, want %#v", id, actual.security, expected.security)
		}
	}
	for id, operation := range got {
		if _, ok := want[id]; !ok {
			t.Errorf("unexpected operation %q (%s): add it to want if intended", id, operation.route)
		}
	}
}
