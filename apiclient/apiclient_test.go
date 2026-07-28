package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDoSendsBearerAndRoundTripsJSON(t *testing.T) {
	type reqBody struct {
		Body string `json:"body"`
	}
	type respBody struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	}

	var gotAuth, gotAccept, gotContentType, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotContentType = r.Header.Get("Content-Type")
		gotMethod = r.Method
		gotPath = r.URL.Path

		var in reqBody
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(respBody{ID: 7, Body: in.Body})
	}))
	defer srv.Close()

	c := New(srv.URL, "pat_1_secret", WithHTTPClient(srv.Client()))
	var out respBody
	err := c.Do(context.Background(), http.MethodPost, "/api/notes", reqBody{Body: "hi"}, &out)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	if gotAuth != "Bearer pat_1_secret" {
		t.Errorf("Authorization = %q, want bearer token", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q", gotAccept)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json when sending a body", gotContentType)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/notes" {
		t.Errorf("method/path = %s %s", gotMethod, gotPath)
	}
	if out.ID != 7 || out.Body != "hi" {
		t.Errorf("out = %+v", out)
	}
}

func TestDoNoContentTypeWhenNoBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "" {
			t.Errorf("Content-Type = %q, want empty when no body sent", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]struct{}{})
	}))
	defer srv.Close()

	c := New(srv.URL, "tok", WithHTTPClient(srv.Client()))
	var out []struct{}
	if err := c.Do(context.Background(), http.MethodGet, "/api/notes", nil, &out); err != nil {
		t.Fatalf("Do: %v", err)
	}
}

func TestDoHandles204WithNilOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, "tok", WithHTTPClient(srv.Client()))
	if err := c.Do(context.Background(), http.MethodDelete, "/api/notes/1", nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
}

func TestDoHandles204WithOutTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, "tok", WithHTTPClient(srv.Client()))
	out := struct {
		ID int64 `json:"id"`
	}{ID: 99}
	if err := c.Do(context.Background(), http.MethodGet, "/api/x", nil, &out); err != nil {
		t.Fatalf("Do: %v", err)
	}
	// Empty 2xx body leaves out untouched rather than erroring on EOF.
	if out.ID != 99 {
		t.Errorf("out.ID = %d, want left unchanged (99)", out.ID)
	}
}

func TestDoNon2xxReturnsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"detail":"nope"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok", WithHTTPClient(srv.Client()))
	err := c.Do(context.Background(), http.MethodGet, "/api/notes", nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.Status != http.StatusForbidden {
		t.Errorf("Status = %d", apiErr.Status)
	}
	if !strings.Contains(apiErr.Body, "nope") {
		t.Errorf("Body = %q, want the response snippet", apiErr.Body)
	}
}

func TestAPIErrorNeverLeaksToken(t *testing.T) {
	const secret = "pat_1_supersecretvalue"
	// Simulate an app that reflects the request Authorization header back in its
	// error body - the worst case for a token-carrying client.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("unauthorized: " + r.Header.Get("Authorization")))
	}))
	defer srv.Close()

	c := New(srv.URL, secret, WithHTTPClient(srv.Client()))
	err := c.Do(context.Background(), http.MethodGet, "/api/notes", nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("APIError.Error() leaked the token: %q", err.Error())
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if strings.Contains(apiErr.Body, secret) {
		t.Fatalf("APIError.Body leaked the token: %q", apiErr.Body)
	}
	if !strings.Contains(apiErr.Body, "[REDACTED]") {
		t.Errorf("Body = %q, want the token replaced with [REDACTED]", apiErr.Body)
	}
}

func TestAPIErrorRedactsTokenAcrossTruncationBoundary(t *testing.T) {
	const secret = "pat_1_supersecretvalue"
	// Place the token so it straddles the display cap: without boundary-aware
	// redaction the truncation would slice the token in half and leak a prefix.
	pad := strings.Repeat("x", maxErrorBodyBytes-len(secret)/2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(pad + secret + " trailing"))
	}))
	defer srv.Close()

	c := New(srv.URL, secret, WithHTTPClient(srv.Client()))
	err := c.Do(context.Background(), http.MethodGet, "/api/notes", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if strings.Contains(apiErr.Body, secret) {
		t.Fatalf("APIError.Body leaked the full token: %q", apiErr.Body)
	}
	// Any non-trivial run of the secret surviving means a prefix leaked.
	for n := len(secret); n >= 6; n-- {
		if strings.Contains(apiErr.Body, secret[:n]) {
			t.Fatalf("APIError.Body leaked a %d-char token prefix: %q", n, apiErr.Body)
		}
	}
}

func TestErrorSnippetTrimsBoundaryTokenFragments(t *testing.T) {
	secret := "pat_1_" + strings.Repeat("A", 48)

	cases := map[string]string{
		// The token straddles the display cap, so truncation alone slices it and
		// leaves a prefix behind.
		"straddling the cap": strings.Repeat("x", maxErrorBodyBytes-len(secret)/2) + secret + " trailing",
		// Leading whitespace pushes the token entirely past the cap. Trimming
		// before the window is fixed drags it back into view unscrubbed.
		"behind leading whitespace": strings.Repeat(" ", maxErrorBodyBytes+len(secret)-50) + secret,
		// Whole tokens earlier in the body shorten the string when replaced,
		// which shifts a later partial token in from beyond the cap.
		"shifted in by earlier redactions": secret + secret +
			strings.Repeat("x", maxErrorBodyBytes+len(secret)-2*len(secret)-30) + secret,
		// The ordinary case: a short body that simply contains the token.
		"in a short body": `{"error":"bad token ` + secret + `"}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got := errorSnippet(strings.NewReader(body), secret)

			for n := len(secret); n >= 6; n-- {
				if strings.Contains(got, secret[:n]) {
					t.Fatalf("leaked a %d-char token prefix: %q", n, got[max(0, len(got)-60):])
				}
			}
			if len(got) > maxErrorBodyBytes {
				t.Errorf("snippet is %d bytes, want it capped at %d", len(got), maxErrorBodyBytes)
			}
		})
	}
}

func TestErrorSnippetStaysCappedWhenRedactionGrowsTheBody(t *testing.T) {
	// A misconfigured token shorter than the placeholder makes every
	// replacement expand the string, so the cap has to be re-applied after
	// scrubbing.
	const secret = "t"
	got := errorSnippet(strings.NewReader(strings.Repeat(secret, maxErrorBodyBytes)), secret)

	if len(got) > maxErrorBodyBytes {
		t.Errorf("snippet is %d bytes, want it capped at %d", len(got), maxErrorBodyBytes)
	}
}

func TestErrorSnippetLeavesCompleteBodiesIntact(t *testing.T) {
	// The trailing-fragment trim only makes sense when the read was cut short.
	// On a complete body a trailing prefix match is a coincidence, and trimming
	// it would silently eat the end of an ordinary error message.
	secret := "pat_1_" + strings.Repeat("A", 48)
	body := `{"error":"forbidden, check scopes p` // ends with "p", a one-character prefix of the token

	if got := errorSnippet(strings.NewReader(body), secret); got != body {
		t.Errorf("snippet = %q, want the body unchanged", got)
	}
}

func TestFromEnvRequiresToken(t *testing.T) {
	t.Setenv("MCP_APP_TOKEN", "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error when MCP_APP_TOKEN is unset")
	}
}

func TestFromEnvUsesDefaultsAndToken(t *testing.T) {
	t.Setenv("MCP_APP_TOKEN", "pat_1_x")
	t.Setenv("MCP_APP_URL", "")
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.baseURL != DefaultBaseURL {
		t.Errorf("baseURL = %q, want default %q", c.baseURL, DefaultBaseURL)
	}
	if c.token != "pat_1_x" {
		t.Errorf("token = %q", c.token)
	}
}

func TestNewTrimsTrailingSlash(t *testing.T) {
	c := New("http://localhost:8080/", "tok")
	if c.baseURL != "http://localhost:8080" {
		t.Errorf("baseURL = %q, want trailing slash trimmed", c.baseURL)
	}
}
