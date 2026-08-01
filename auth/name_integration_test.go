package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/robert-crandall/go-home-server/server"
)

// These cover the optional display name on users. They need Postgres; testPool
// skips without it.
//
// The name is read back by seven different SQL statements, and a forgotten one
// is invisible from any single endpoint - so these drive each statement through
// the path that actually reaches it rather than checking /api/auth/me and
// calling it covered.

// nameHarness is the real stack: the auth middleware in front of the real
// endpoints, with API tokens enabled so bearer requests take the token branch.
type nameHarness struct {
	t   *testing.T
	svc *Service
	srv *server.Server
}

func newNameHarness(t *testing.T) *nameHarness {
	t.Helper()
	svc := NewService(testPool(t), false)
	svc.OpenRegistration = true
	srv := server.New(server.Options{
		Title:       "Name",
		Version:     "0.0.0",
		Middlewares: []func(http.Handler) http.Handler{svc.Middleware},
		HumaConfig:  svc.TokenHumaConfig, // this is what enables bearer auth
	})
	svc.Register(srv.API)
	return &nameHarness{t: t, svc: svc, srv: srv}
}

func (h *nameHarness) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.srv.Router.ServeHTTP(rec, req)
	return rec
}

func (h *nameHarness) json(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return h.do(req)
}

// register posts a raw JSON body so a test can omit "name" entirely, which is
// the case that proves the field is optional.
func (h *nameHarness) register(body string) (User, *http.Cookie) {
	h.t.Helper()
	rec := h.json(http.MethodPost, "/api/auth/register", body)
	u := decodeUser(h.t, rec)
	for _, c := range (&http.Response{Header: rec.Header()}).Cookies() {
		if c.Name == cookieName {
			return u, c
		}
	}
	h.t.Fatal("register set no session cookie")
	return User{}, nil
}

func (h *nameHarness) bearerToken(userID int64) string {
	h.t.Helper()
	_, plaintext, err := h.svc.CreateAPIToken(context.Background(), userID, "script", nil)
	if err != nil {
		h.t.Fatalf("CreateAPIToken: %v", err)
	}
	return plaintext
}

func decodeUser(t *testing.T, rec *httptest.ResponseRecorder) User {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var u User
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
		t.Fatalf("decode user: %v (body: %s)", err, rec.Body.String())
	}
	return u
}

// One registration, then every way of reading that user back. Each assertion
// names the statement it covers, because they are the thing that can silently
// drop the column.
func TestNameSurvivesEveryUserLookup(t *testing.T) {
	h := newNameHarness(t)
	const want = "Robert"

	registered, cookie := h.register(
		`{"email":"named@example.com","password":"supersecret","name":"Robert"}`)
	if registered.Name != want {
		t.Errorf("register response name = %q, want %q (registerUser)", registered.Name, want)
	}

	loggedIn := decodeUser(t, h.json(http.MethodPost, "/api/auth/login",
		`{"email":"named@example.com","password":"supersecret"}`))
	if loggedIn.Name != want {
		t.Errorf("login response name = %q, want %q (authenticate)", loggedIn.Name, want)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(cookie)
	if got := decodeUser(t, h.do(req)); got.Name != want {
		t.Errorf("cookie /me name = %q, want %q (userFromToken)", got.Name, want)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+h.bearerToken(registered.ID))
	if got := decodeUser(t, h.do(req)); got.Name != want {
		t.Errorf("bearer /me name = %q, want %q (userFromAPIToken)", got.Name, want)
	}
}

// The whole point of the omitempty on the register body: an SPA written against
// the previous version of this module posts no name and must still succeed.
func TestRegisterWithoutNameLeavesItEmpty(t *testing.T) {
	h := newNameHarness(t)

	u, _ := h.register(`{"email":"anon@example.com","password":"supersecret"}`)
	if u.Name != "" {
		t.Fatalf("name = %q, want empty", u.Name)
	}
}

func TestUpdateCurrentUserSetsName(t *testing.T) {
	h := newNameHarness(t)
	_, cookie := h.register(`{"email":"rename@example.com","password":"supersecret"}`)

	req := httptest.NewRequest(http.MethodPatch, "/api/auth/me",
		strings.NewReader(`{"name":"Robert"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	if got := decodeUser(t, h.do(req)); got.Name != "Robert" {
		t.Fatalf("patch response name = %q, want %q", got.Name, "Robert")
	}

	// Read it back through a fresh request, so this can't pass on a handler
	// that echoes the input without writing it.
	req = httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(cookie)
	if got := decodeUser(t, h.do(req)); got.Name != "Robert" {
		t.Fatalf("name after patch = %q, want %q", got.Name, "Robert")
	}
}

// The endpoint advertises session-or-bearer in the OpenAPI spec, and the wiring
// test only checks that advertisement. This checks the handler agrees: reaching
// for RequireSessionUser instead of RequireUser would 403 a token here while
// leaving the spec test green.
func TestUpdateCurrentUserAcceptsAPIToken(t *testing.T) {
	h := newNameHarness(t)
	u, _ := h.register(`{"email":"script@example.com","password":"supersecret"}`)

	req := httptest.NewRequest(http.MethodPatch, "/api/auth/me",
		strings.NewReader(`{"name":"From a script"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.bearerToken(u.ID))
	if got := decodeUser(t, h.do(req)); got.Name != "From a script" {
		t.Fatalf("name = %q, want %q", got.Name, "From a script")
	}
}

func TestUpdateCurrentUserTrimsName(t *testing.T) {
	h := newNameHarness(t)
	_, cookie := h.register(`{"email":"padded@example.com","password":"supersecret"}`)

	req := httptest.NewRequest(http.MethodPatch, "/api/auth/me",
		strings.NewReader(`{"name":"  Robert \n"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	if got := decodeUser(t, h.do(req)); got.Name != "Robert" {
		t.Fatalf("name = %q, want %q", got.Name, "Robert")
	}
}

// maxLength on the body is the only bound on this column, so it has to hold.
func TestUpdateCurrentUserRejectsOverlongName(t *testing.T) {
	h := newNameHarness(t)
	_, cookie := h.register(`{"email":"long@example.com","password":"supersecret"}`)

	body, err := json.Marshal(map[string]string{"name": strings.Repeat("a", 101)})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/api/auth/me", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	if rec := h.do(req); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
}

// CreateUser keeps its two-argument signature - apps vendor it - so it leans on
// the column default instead.
func TestCreateUserLeavesNameEmpty(t *testing.T) {
	svc := NewService(testPool(t), false)

	u, err := svc.CreateUser(context.Background(), "plain@example.com", "supersecret")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.Name != "" {
		t.Fatalf("name = %q, want empty", u.Name)
	}
}

// The two Google lookups are called directly: the callback only ever returns a
// redirect plus a cookie, so driving it end to end would exercise userFromToken
// again rather than these.
func TestGoogleLookupsCarryName(t *testing.T) {
	ctx := context.Background()
	svc := NewService(testPool(t), false)

	existing, err := svc.CreateUser(ctx, "gmail-user@example.com", "supersecret")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := svc.updateName(ctx, existing.ID, "Robert"); err != nil {
		t.Fatalf("updateName: %v", err)
	}

	found, err := svc.userByEmail(ctx, "gmail-user@example.com")
	if err != nil {
		t.Fatalf("userByEmail: %v", err)
	}
	if found.Name != "Robert" {
		t.Errorf("userByEmail name = %q, want %q", found.Name, "Robert")
	}

	// An account Google creates has no name: the ID token doesn't carry one at
	// the scopes this asks for.
	created, err := svc.createGoogleUser(ctx, "new-google-user@example.com")
	if err != nil {
		t.Fatalf("createGoogleUser: %v", err)
	}
	if created.Name != "" {
		t.Errorf("createGoogleUser name = %q, want empty", created.Name)
	}
}

// A user soft-deleted between the middleware resolving them and the handler
// running matches nothing, and must not be written to.
func TestUpdateNameSkipsSoftDeletedUser(t *testing.T) {
	ctx := context.Background()
	svc := NewService(testPool(t), false)

	u, err := svc.CreateUser(ctx, "gone@example.com", "supersecret")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := svc.db.Exec(ctx, `UPDATE users SET deleted_at = now() WHERE id = $1`, u.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	if _, err := svc.updateName(ctx, u.ID, "Robert"); err != ErrNotFound {
		t.Fatalf("updateName error = %v, want ErrNotFound", err)
	}
	var stored string
	if err := svc.db.QueryRow(ctx, `SELECT name FROM users WHERE id = $1`, u.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "" {
		t.Fatalf("soft-deleted user's name = %q, want it left empty", stored)
	}
}
