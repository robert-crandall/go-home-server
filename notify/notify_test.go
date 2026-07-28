package notify

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	webpush "github.com/SherClockHolmes/webpush-go"
)

func TestValidateVAPIDKeys(t *testing.T) {
	pub, priv, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := VerifyKeyPair(pub, priv); err != nil {
		t.Fatalf("a freshly generated pair should validate, got: %v", err)
	}

	// A mismatched pair (public from a different key) must be rejected.
	otherPub, _, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := VerifyKeyPair(otherPub, priv); err == nil {
		t.Fatal("a mismatched public/private pair should not validate")
	}

	// Malformed inputs.
	for name, tc := range map[string]struct{ pub, priv string }{
		"empty":       {"", ""},
		"only-public": {pub, ""},
		"bad-base64":  {"not!base64", priv},
		"short-pub":   {"AAAA", priv},
		// Over-padded key: webpush-go decodes strictly and would reject this at
		// send time, so the startup validator must reject it too (not silently
		// strip the padding), or the fail-fast check is defeated.
		"over-padded-pub": {pub + "==", priv},
	} {
		if err := VerifyKeyPair(tc.pub, tc.priv); err == nil {
			t.Errorf("%s: expected validation error, got nil", name)
		}
	}
}

func TestNewServiceValidatesKeys(t *testing.T) {
	// No keys => push disabled, no error.
	if _, err := NewService(nil, VAPID{}); err != nil {
		t.Fatalf("empty VAPID should not error: %v", err)
	}

	pub, priv, _ := GenerateVAPIDKeys()

	// Valid keys but a bad subject must fail.
	if _, err := NewService(nil, VAPID{Public: pub, Private: priv, Subject: "you@example.com"}); err == nil {
		t.Fatal("a subject without mailto:/https: should error")
	}
	// Valid keys + subject succeed.
	svc, err := NewService(nil, VAPID{Public: pub, Private: priv, Subject: "mailto:you@example.com"})
	if err != nil {
		t.Fatalf("valid config should not error: %v", err)
	}
	if !svc.Enabled() {
		t.Fatal("service with keys should be Enabled")
	}
}

func TestLibrarySubscriberAndSubjectClaim(t *testing.T) {
	// The library re-adds exactly one mailto:, so we must hand it the bare
	// address (this is the iOS 403 BadJwtToken fix).
	if got := librarySubscriber("mailto:you@example.com"); got != "you@example.com" {
		t.Fatalf("librarySubscriber should strip one mailto:, got %q", got)
	}
	if got := SubjectClaim("mailto:you@example.com"); got != "mailto:you@example.com" {
		t.Fatalf("SubjectClaim should yield a single mailto:, got %q", got)
	}
	if got := SubjectClaim("https://example.com"); got != "https://example.com" {
		t.Fatalf("https subject should pass through, got %q", got)
	}
}

func TestValidateVAPIDSubject(t *testing.T) {
	valid := []string{
		"mailto:you@example.com",
		"mailto:you@example.com?subject=x",
		"https://example.com",
		"https://example.com/contact",
	}
	for _, s := range valid {
		if err := validateVAPIDSubject(s); err != nil {
			t.Errorf("expected %q to be valid, got: %v", s, err)
		}
	}

	invalid := []string{
		"",
		"mailto:",
		"mailto:   ",
		"https:",
		"https://",
		"https:///path",                 // no host
		"you@example.com",               // no scheme
		"ftp://example.com",             // wrong scheme
		"HTTPS://example.com",           // uppercase: send path only matches lowercase
		" https://example.com",          // leading space breaks the JWT sub
		"https://example.com ",          // trailing space
		"mailto:you@example.com ",       // trailing space
		"mailto: you@example.com",       // embedded space
		"mailto:mailto:you@example.com", // doubled scheme breaks the sub
		"mailto:https://example.com",    // nested https scheme
	}
	for _, s := range invalid {
		if err := validateVAPIDSubject(s); err == nil {
			t.Errorf("expected %q to be rejected", s)
		}
	}
}

func TestSubscribeEnforcesInvariant(t *testing.T) {
	// nil db: validation must reject these before any DB access, so a nil pool
	// must never be dereferenced. A passing case would panic here, which is the
	// point - the guard runs first.
	svc := &Service{}

	full := func() Subscription {
		s := Subscription{Endpoint: "https://fcm.googleapis.com/fcm/send/abc"}
		s.Keys.P256dh = "p"
		s.Keys.Auth = "a"
		return s
	}

	// Missing keys.
	if err := svc.Subscribe(context.Background(), 1, Subscription{Endpoint: full().Endpoint}); err == nil {
		t.Error("Subscribe should reject a subscription with missing keys")
	}

	// Non-public endpoint (SSRF) supplied by a programmatic caller, bypassing
	// the HTTP handler's check.
	bad := full()
	bad.Endpoint = "https://169.254.169.254/latest/meta-data"
	if err := svc.Subscribe(context.Background(), 1, bad); err == nil {
		t.Error("Subscribe should reject a non-public endpoint")
	}
}

func TestValidatePushEndpoint(t *testing.T) {
	ok := []string{
		"https://fcm.googleapis.com/fcm/send/abc",
		"https://web.push.apple.com/xyz",
		"https://updates.push.services.mozilla.com/wpush/v2/abc",
	}
	for _, e := range ok {
		if err := validatePushEndpoint(e); err != nil {
			t.Errorf("expected %q to be allowed, got: %v", e, err)
		}
	}

	bad := []string{
		"http://fcm.googleapis.com/x", // not https
		"https://localhost/x",
		"https://127.0.0.1/x",
		"https://10.0.0.5/x",
		"https://192.168.1.1/x",
		"https://[::1]/x",
		"https://169.254.169.254/latest/meta-data", // cloud metadata
		"https://localhost./x",                     // trailing DNS dot
		"https://127.0.0.1./x",                     // trailing dot on IP literal
		"https://[fe80::1%25eth0]/x",               // link-local IPv6 with zone
		"https://100.64.0.1/x",                     // RFC 6598 CGNAT / Tailscale
		"https://[::ffff:100.64.0.1]/x",            // IPv4-mapped CGNAT
		"ftp://example.com/x",
		"not a url at all ::::",
		"https://",
	}
	for _, e := range bad {
		if err := validatePushEndpoint(e); err == nil {
			t.Errorf("expected %q to be rejected", e)
		}
	}
}

func TestClassifySendStatus(t *testing.T) {
	const badJwt = `{"reason":"BadJwtToken"}`

	cases := []struct {
		name     string
		status   int
		body     string
		wantGone bool
		wantErr  bool
		// wantMsgContains, when set, must appear in the error text.
		wantMsgContains string
	}{
		{name: "200 delivered", status: 200, wantGone: false, wantErr: false},
		{name: "201 delivered", status: 201, wantGone: false, wantErr: false},
		{name: "404 gone", status: 404, wantGone: true, wantErr: true},
		{name: "410 gone", status: 410, wantGone: true, wantErr: true},
		// The reviewer's case: a sub-200 informational status must not be
		// mistaken for delivery.
		{name: "101 not delivered", status: 101, wantGone: false, wantErr: true},
		{name: "100 not delivered", status: 100, wantGone: false, wantErr: true},
		{name: "500 error", status: 500, wantGone: false, wantErr: true},
		{
			name:            "403 BadJwtToken is actionable",
			status:          403,
			body:            badJwt,
			wantErr:         true,
			wantMsgContains: "different VAPID public key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			read := func() string { return tc.body }
			gone, err := classifySendStatus(tc.status, read)
			if gone != tc.wantGone {
				t.Errorf("gone = %v, want %v", gone, tc.wantGone)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantMsgContains != "" && (err == nil || !strings.Contains(err.Error(), tc.wantMsgContains)) {
				t.Errorf("err = %v, want it to contain %q", err, tc.wantMsgContains)
			}
		})
	}
}

// testSubscriptionKeys returns a p256dh/auth pair in the shape a browser's
// PushSubscription produces: an uncompressed P-256 point and a 16-byte auth
// secret. They have to be real, because webpush-go decodes both and checks the
// point is on the curve *before* it builds any HTTP request - junk keys would
// make the "no request reached the endpoint" assertion below pass for entirely
// the wrong reason.
func testSubscriptionKeys(t *testing.T) (p256dh, auth string) {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate subscription key: %v", err)
	}
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		t.Fatalf("generate auth secret: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
		base64.RawURLEncoding.EncodeToString(secret)
}

// A row persisted before the endpoint guard existed must never be fetched by
// the send path. Send is a thin wrapper around sendAll (query + marshal), so
// exercising sendAll covers the persisted rows without needing a database.
func TestSendSkipsInvalidPersistedEndpoint(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	// httptest listens on loopback, so srv.URL is "http://127.0.0.1:PORT" -
	// plain http *and* a loopback host, exactly what the guard rejects. It
	// stands in for a legacy row written before that guard existed.
	var sub webpush.Subscription
	sub.Endpoint = srv.URL + "/wpush/v2/legacy"
	sub.Keys.P256dh, sub.Keys.Auth = testSubscriptionKeys(t)
	if err := validatePushEndpoint(sub.Endpoint); err == nil {
		t.Fatalf("test setup: %q should be rejected by the endpoint guard", sub.Endpoint)
	}

	pub, priv, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("generate VAPID keys: %v", err)
	}
	// nil pool: nothing on this path may touch the database. Pruning a
	// subscription would need it, so a regression that decided to delete the
	// invalid row panics loudly instead of passing quietly.
	svc, err := NewService(nil, VAPID{Public: pub, Private: priv, Subject: "mailto:test@example.com"})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	body := []byte(`{"title":"t","body":"b"}`)

	// Control. sendOne carries no guard, so this proves the endpoint really is
	// reachable and webpush-go really does POST to it. Without it, "no request
	// arrived" below could be true for an unrelated reason.
	if _, err := svc.sendOne(context.Background(), body, sub); err != nil {
		t.Fatalf("control: sendOne should have reached the test server: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("control: want the endpoint fetched once, got %d", got)
	}

	// The guard itself.
	hits.Store(0)
	err = svc.sendAll(context.Background(), 1, body, []webpush.Subscription{sub})
	if got := hits.Load(); got != 0 {
		t.Errorf("SSRF: the invalid endpoint was fetched %d time(s); it must never be requested", got)
	}
	if err == nil {
		t.Fatal("an all-invalid subscription set must be reported as a failure, not a silent success")
	}
	if !strings.Contains(err.Error(), "endpoint") {
		t.Errorf("error should name the endpoint problem, got: %v", err)
	}
	// The endpoint is a capability URL - its path carries the subscription's
	// token - so it must not be echoed back out in an error either.
	if strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error must not contain the raw endpoint, got: %v", err)
	}
}
