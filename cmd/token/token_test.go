package main

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robert-crandall/go-home-server/apiclient"
)

// tokenServer stands in for an app: /api/auth/login sets a session cookie, and
// /api/tokens mints only when that cookie comes back.
func tokenServer(t *testing.T, loginStatus, tokensStatus int) (*httptest.Server, *loginRecord) {
	t.Helper()
	rec := &loginRecord{}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		rec.method = r.Method
		rec.contentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&rec.body)
		if loginStatus != http.StatusOK {
			w.WriteHeader(loginStatus)
			_, _ = w.Write([]byte(`{"detail":"invalid email or password"}`))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "session-value", Path: "/"})
		_, _ = w.Write([]byte(`{"id":1,"email":"me@example.com"}`))
	})
	mux.HandleFunc("/api/tokens", func(w http.ResponseWriter, r *http.Request) {
		if tokensStatus != http.StatusCreated {
			w.WriteHeader(tokensStatus)
			return
		}
		c, err := r.Cookie("session")
		if err != nil || c.Value != "session-value" {
			// The whole reason for the cookie jar: without it the app answers
			// 403 here, exactly as it would for a bearer token.
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&rec.tokenBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":7,"name":"my-app-mcp","token":"pat_7_secret"}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, rec
}

type loginRecord struct {
	method      string
	contentType string
	body        map[string]string
	tokenBody   map[string]string
}

func newTestClient(t *testing.T) *http.Client {
	t.Helper()
	c, err := newClient()
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	return c
}

func TestMintLogsInThenCreatesToken(t *testing.T) {
	srv, rec := tokenServer(t, http.StatusOK, http.StatusCreated)

	token, err := mint(context.Background(), newTestClient(t), srv.URL, "me@example.com", "hunter22", "my-app-mcp")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if token != "pat_7_secret" {
		t.Errorf("token = %q, want pat_7_secret", token)
	}
	if rec.method != http.MethodPost || rec.contentType != "application/json" {
		t.Errorf("login was %s with Content-Type %q, want POST application/json", rec.method, rec.contentType)
	}
	if rec.body["email"] != "me@example.com" || rec.body["password"] != "hunter22" {
		t.Errorf("login body = %v, want the supplied credentials", rec.body)
	}
	if rec.tokenBody["name"] != "my-app-mcp" {
		t.Errorf("token name = %q, want my-app-mcp", rec.tokenBody["name"])
	}
}

// A trailing slash on the URL is the likeliest hand-typed mistake, and "//api"
// would 404 on a chi router.
func TestMintTrimsTrailingSlash(t *testing.T) {
	srv, _ := tokenServer(t, http.StatusOK, http.StatusCreated)

	if _, err := mint(context.Background(), newTestClient(t), srv.URL+"/", "me@example.com", "hunter22", "cli"); err != nil {
		t.Fatalf("mint with trailing slash: %v", err)
	}
}

func TestMintReportsBadCredentials(t *testing.T) {
	srv, _ := tokenServer(t, http.StatusUnauthorized, http.StatusCreated)

	_, err := mint(context.Background(), newTestClient(t), srv.URL, "me@example.com", "wrong", "cli")
	if err == nil {
		t.Fatal("mint succeeded with bad credentials")
	}
	if !strings.Contains(err.Error(), "check the email and password") {
		t.Errorf("error = %q, want it to name the likely cause", err)
	}
}

// An app that never called RegisterTokens 404s here, and "HTTP 404" alone
// doesn't tell you that's the reason.
func TestMintReportsMissingTokenEndpoint(t *testing.T) {
	srv, _ := tokenServer(t, http.StatusOK, http.StatusNotFound)

	_, err := mint(context.Background(), newTestClient(t), srv.URL, "me@example.com", "hunter22", "cli")
	if err == nil {
		t.Fatal("mint succeeded against an app with no /api/tokens")
	}
	if !strings.Contains(err.Error(), "RegisterTokens") {
		t.Errorf("error = %q, want it to point at authSvc.RegisterTokens", err)
	}
}

func TestMintRejectsAResponseWithNoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tokens" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":7,"name":"cli"}`))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "session-value", Path: "/"})
	}))
	t.Cleanup(srv.Close)

	if _, err := mint(context.Background(), newTestClient(t), srv.URL, "me@example.com", "hunter22", "cli"); err == nil {
		t.Fatal("mint returned an empty token as success")
	}
}

func TestWriteConfigCreatesAPrivateFile(t *testing.T) {
	// A nested path so this also covers the first-run case where ~/.config
	// doesn't exist yet.
	root := filepath.Join(t.TempDir(), "nested", "config")
	t.Setenv("XDG_CONFIG_HOME", root)

	path, err := writeConfig("my-app", "https://my-app.example.com", "pat_7_secret")
	if err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	if want := filepath.Join(root, "my-app.json"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	assertMode(t, path, 0o600)

	cfg, err := apiclient.LoadFileConfig("my-app")
	if err != nil {
		t.Fatalf("LoadFileConfig: %v", err)
	}
	if cfg.AppURL != "https://my-app.example.com" || cfg.Token != "pat_7_secret" {
		t.Errorf("config = %+v, want the minted values", cfg)
	}

	// The temp file is an implementation detail; it must not survive.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the config file", len(entries))
	}
}

// The reason for temp-file + rename: os.WriteFile leaves an existing file's mode
// alone, so refreshing a config someone created by hand at 0644 would leave a
// live token world-readable.
func TestWriteConfigTightensAnExistingFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	path := filepath.Join(root, "my-app.json")
	if err := os.WriteFile(path, []byte(`{"appUrl":"http://localhost:8080","token":"old"}`), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	// os.WriteFile's mode is subject to the umask, so a umask of 077 would seed
	// the file at 0600 and this test would pass without testing anything.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod seed config: %v", err)
	}
	assertMode(t, path, 0o644)

	if _, err := writeConfig("my-app", "http://localhost:8080", "pat_9_new"); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	assertMode(t, path, 0o600)

	cfg, err := apiclient.LoadFileConfig("my-app")
	if err != nil {
		t.Fatalf("LoadFileConfig: %v", err)
	}
	if cfg.Token != "pat_9_new" {
		t.Errorf("token = %q, want the new one", cfg.Token)
	}
}

func TestWriteConfigRejectsAPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := writeConfig("github.com/you/my-app", "http://localhost:8080", "pat_1_x"); err == nil {
		t.Fatal("writeConfig accepted a module path as a config name")
	}
}

func TestReadPasswordPrefersTheEnvironment(t *testing.T) {
	t.Setenv("APP_PASSWORD", "hunter22")
	got, err := readPassword("me@example.com")
	if err != nil {
		t.Fatalf("readPassword: %v", err)
	}
	if got != "hunter22" {
		t.Errorf("password = %q, want the value of APP_PASSWORD", got)
	}
}

func assertMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("mode of %s = %04o, want %04o", path, got, want)
	}
}
