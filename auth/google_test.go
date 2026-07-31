package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
)

const testClientID = "test-client-id"

// testGoogleConfig is a valid config; tests override the one field they're
// exercising.
func testGoogleConfig() GoogleConfig {
	return GoogleConfig{
		ClientID:     testClientID,
		ClientSecret: "test-client-secret",
		RedirectURL:  "https://app.test/api/auth/google/callback",
	}
}

// idToken builds an unsigned JWT with the given claims. Unsigned is faithful to
// what the code does: parseIDToken never looks at the signature, because the
// token only ever arrives over TLS from the token endpoint.
func idToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func validClaims() map[string]any {
	return map[string]any{
		"iss":            "https://accounts.google.com",
		"aud":            testClientID,
		"exp":            time.Now().Add(time.Hour).Unix(),
		"email":          "person@example.com",
		"email_verified": true,
	}
}

// googleFixture wires the two endpoints onto a router, with the OAuth token
// endpoint pointed at a stub standing in for Google.
func googleFixture(t *testing.T, svc *Service, tokenEndpoint http.HandlerFunc) (*chi.Mux, *googleAuth) {
	t.Helper()
	g, err := newGoogleAuth(testGoogleConfig())
	if err != nil {
		t.Fatalf("newGoogleAuth: %v", err)
	}
	if tokenEndpoint != nil {
		stub := httptest.NewServer(tokenEndpoint)
		t.Cleanup(stub.Close)
		g.oauth.Endpoint.TokenURL = stub.URL
	}

	mux := chi.NewMux()
	svc.registerGoogle(humachi.New(mux, huma.DefaultConfig("test", "1.0.0")), g)
	return mux, g
}

// tokenEndpointReturning is a stub Google token endpoint that hands back one
// id_token.
func tokenEndpointReturning(raw string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at","token_type":"Bearer","id_token":"` + raw + `"}`))
	}
}

// callback drives one GET of the callback endpoint with the given query and
// state cookie.
func callback(mux *chi.Mux, query url.Values, stateCookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/auth/google/callback?"+query.Encode(), nil)
	if stateCookie != "" {
		req.AddCookie(&http.Cookie{Name: stateCookieName, Value: stateCookie})
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func responseCookie(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range (&http.Response{Header: rec.Header()}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// assertFailureRedirect checks the browser was sent back to the login screen
// with the expected code, and that the in-flight state cookie was cleared.
func assertFailureRedirect(t *testing.T, rec *httptest.ResponseRecorder, code string) {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Location"), "/login?error="+code; got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
	state := responseCookie(t, rec, stateCookieName)
	if state == nil || state.MaxAge >= 0 {
		t.Fatalf("state cookie = %#v, want it cleared", state)
	}
	if responseCookie(t, rec, cookieName) != nil {
		t.Fatal("a failed sign-in must not set a session cookie")
	}
}

func TestNewGoogleAuthRejectsIncompleteConfig(t *testing.T) {
	cases := map[string]func(*GoogleConfig){
		"no client ID":     func(c *GoogleConfig) { c.ClientID = "" },
		"no client secret": func(c *GoogleConfig) { c.ClientSecret = "" },
		"no redirect URL":  func(c *GoogleConfig) { c.RedirectURL = "" },
		// A protocol-relative path would walk the browser off the site.
		"protocol-relative success": func(c *GoogleConfig) { c.SuccessPath = "//evil.test/" },
		"protocol-relative failure": func(c *GoogleConfig) { c.FailurePath = "//evil.test/" },
		"absolute success":          func(c *GoogleConfig) { c.SuccessPath = "https://evil.test/" },
		"relative success":          func(c *GoogleConfig) { c.SuccessPath = "home" },
		// ?error= is appended by concatenation, so an existing query or
		// fragment would produce a malformed URL.
		"query in failure":    func(c *GoogleConfig) { c.FailurePath = "/login?next=/" },
		"fragment in success": func(c *GoogleConfig) { c.SuccessPath = "/app#top" },
	}
	for name, mangle := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := testGoogleConfig()
			mangle(&cfg)
			if _, err := newGoogleAuth(cfg); err == nil {
				t.Fatalf("newGoogleAuth(%#v) = nil error, want a rejection", cfg)
			}
		})
	}
}

func TestNewGoogleAuthDefaultsAndScopes(t *testing.T) {
	g, err := newGoogleAuth(testGoogleConfig())
	if err != nil {
		t.Fatalf("newGoogleAuth: %v", err)
	}
	if g.successPath != "/" || g.failurePath != "/login" {
		t.Fatalf("paths = %q/%q, want //login", g.successPath, g.failurePath)
	}
	// Without openid Google returns no ID token at all, and without email the
	// claims this depends on aren't in it.
	if want := []string{"openid", "email"}; strings.Join(g.oauth.Scopes, " ") != strings.Join(want, " ") {
		t.Fatalf("scopes = %v, want %v", g.oauth.Scopes, want)
	}
}

func TestParseIDTokenAccepts(t *testing.T) {
	claims, err := parseIDToken(idToken(t, validClaims()), testClientID)
	if err != nil {
		t.Fatalf("parseIDToken: %v", err)
	}
	if claims.Email != "person@example.com" {
		t.Fatalf("email = %q, want person@example.com", claims.Email)
	}
}

// The bare issuer form is what Google actually sends for some clients.
func TestParseIDTokenAcceptsBareIssuer(t *testing.T) {
	claims := validClaims()
	claims["iss"] = "accounts.google.com"
	if _, err := parseIDToken(idToken(t, claims), testClientID); err != nil {
		t.Fatalf("parseIDToken: %v", err)
	}
}

func TestParseIDTokenRejects(t *testing.T) {
	cases := map[string]func(map[string]any){
		// The audience check is the one thing TLS to the token endpoint can't
		// do for us: it's what says this token was minted for this app.
		"wrong audience":     func(c map[string]any) { c["aud"] = "someone-elses-client-id" },
		"foreign issuer":     func(c map[string]any) { c["iss"] = "https://accounts.evil.test" },
		"expired":            func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() },
		"unverified email":   func(c map[string]any) { c["email_verified"] = false },
		"missing verified":   func(c map[string]any) { delete(c, "email_verified") },
		"no email":           func(c map[string]any) { delete(c, "email") },
		"audience as array":  func(c map[string]any) { c["aud"] = []string{testClientID} },
		"empty audience":     func(c map[string]any) { c["aud"] = "" },
		"nonsense structure": func(c map[string]any) { c["exp"] = "not-a-number" },
	}
	for name, mangle := range cases {
		t.Run(name, func(t *testing.T) {
			claims := validClaims()
			mangle(claims)
			if _, err := parseIDToken(idToken(t, claims), testClientID); err == nil {
				t.Fatalf("parseIDToken(%v) = nil error, want a rejection", claims)
			}
		})
	}
}

func TestParseIDTokenRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":           "",
		"not a jwt":       "just-a-string",
		"two segments":    "header.payload",
		"bad base64":      "header.!!!not-base64!!!.signature",
		"payload not obj": "header." + base64.RawURLEncoding.EncodeToString([]byte(`"a string"`)) + ".sig",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseIDToken(raw, testClientID); err == nil {
				t.Fatalf("parseIDToken(%q) = nil error, want a rejection", raw)
			}
		})
	}
}

// The start endpoint has to put the same state in the URL and the cookie, or
// the callback's check is meaningless.
func TestGoogleStartRedirectsWithMatchingState(t *testing.T) {
	mux, _ := googleFixture(t, NewService(nil, false), nil)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/google/start", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}
	target, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if target.Host != "accounts.google.com" {
		t.Fatalf("redirect host = %q, want accounts.google.com", target.Host)
	}
	query := target.Query()
	if query.Get("client_id") != testClientID {
		t.Fatalf("client_id = %q, want %q", query.Get("client_id"), testClientID)
	}
	if query.Get("redirect_uri") != testGoogleConfig().RedirectURL {
		t.Fatalf("redirect_uri = %q, want the configured URL", query.Get("redirect_uri"))
	}
	if query.Get("scope") != "openid email" {
		t.Fatalf("scope = %q, want \"openid email\"", query.Get("scope"))
	}

	state := responseCookie(t, rec, stateCookieName)
	if state == nil {
		t.Fatal("no state cookie was set")
	}
	if state.Value != query.Get("state") {
		t.Fatalf("state cookie %q != state param %q", state.Value, query.Get("state"))
	}
	if !state.HttpOnly || state.SameSite != http.SameSiteLaxMode {
		t.Fatalf("state cookie = %#v, want HttpOnly and SameSite=Lax", state)
	}
	// Lax is required, not incidental: the callback is a cross-site top-level
	// navigation from accounts.google.com, which Strict would withhold on.
	if state.SameSite == http.SameSiteStrictMode {
		t.Fatal("SameSite=Strict would withhold the cookie on Google's callback")
	}
}

// Every one of these has to bounce the browser to the login screen. None may
// reach the database, so the service holds a nil pool on purpose: a query would
// panic rather than quietly pass.
func TestGoogleCallbackFailuresRedirect(t *testing.T) {
	const state = "the-state-value"
	valid := url.Values{"code": {"the-code"}, "state": {state}}

	cases := []struct {
		name   string
		query  url.Values
		cookie string
		token  http.HandlerFunc
		want   string
	}{
		{
			name:   "user declined at Google",
			query:  url.Values{"error": {"access_denied"}},
			cookie: state,
			want:   errCodeDenied,
		},
		{
			// Google's own error string is never echoed into the redirect.
			name:   "google error is not passed through",
			query:  url.Values{"error": {"<script>alert(1)</script>"}},
			cookie: state,
			want:   errCodeDenied,
		},
		{
			name:   "no code",
			query:  url.Values{"state": {state}},
			cookie: state,
			want:   errCodeInvalidState,
		},
		{
			name:   "no state param",
			query:  url.Values{"code": {"the-code"}},
			cookie: state,
			want:   errCodeInvalidState,
		},
		{
			name:  "no state cookie",
			query: valid,
			want:  errCodeInvalidState,
		},
		{
			// The CSRF check itself: a callback forged without knowing the
			// cookie can't log anyone in.
			name:   "state mismatch",
			query:  url.Values{"code": {"the-code"}, "state": {"attacker-chosen"}},
			cookie: state,
			want:   errCodeInvalidState,
		},
		{
			name:   "token endpoint rejects the code",
			query:  valid,
			cookie: state,
			token:  func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) },
			want:   errCodeExchange,
		},
		{
			name:   "response carries no id_token",
			query:  valid,
			cookie: state,
			token: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"at","token_type":"Bearer"}`))
			},
			want: errCodeInvalidToken,
		},
		{
			name:   "id token for another client",
			query:  valid,
			cookie: state,
			token: func() http.HandlerFunc {
				claims := validClaims()
				claims["aud"] = "someone-elses-client-id"
				payload, _ := json.Marshal(claims)
				return tokenEndpointReturning(
					"header." + base64.RawURLEncoding.EncodeToString(payload) + ".sig")
			}(),
			want: errCodeInvalidToken,
		},
		{
			name:   "unverified email",
			query:  valid,
			cookie: state,
			token: func() http.HandlerFunc {
				claims := validClaims()
				claims["email_verified"] = false
				payload, _ := json.Marshal(claims)
				return tokenEndpointReturning(
					"header." + base64.RawURLEncoding.EncodeToString(payload) + ".sig")
			}(),
			want: errCodeInvalidToken,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux, _ := googleFixture(t, NewService(nil, false), tc.token)
			assertFailureRedirect(t, callback(mux, tc.query, tc.cookie), tc.want)
		})
	}
}
