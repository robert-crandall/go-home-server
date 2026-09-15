package notify

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/robert-crandall/go-home-server/auth"
	"github.com/robert-crandall/go-home-server/db"
	"github.com/robert-crandall/go-home-server/migrations"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
	}
	if err := db.Migrate(url, db.MigrationSource{
		FS: migrations.FS, Dir: migrations.Dir, TableName: migrations.TableName,
	}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := testContext(t)
	pool, err := db.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DELETE FROM users`); err != nil {
		t.Fatalf("clean: %v", err)
	}
	return pool
}

func testUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	user, err := auth.NewService(pool, false).CreateUser(ctx, name+"@example.invalid", "test-password")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return user.ID
}

func testSubscription(t *testing.T, path string) Subscription {
	t.Helper()
	sub := Subscription{Endpoint: "https://push.example.invalid/" + path}
	sub.Keys.P256dh, sub.Keys.Auth = testSubscriptionKeys(t)
	return sub
}

func assertEncryptedPush(t *testing.T, r *http.Request, v VAPID, payload Payload) {
	t.Helper()
	if r.Method != http.MethodPost || r.URL.Scheme != "https" || r.URL.Host != "push.example.invalid" {
		t.Fatalf("unexpected push request: %s %s", r.Method, r.URL)
	}
	if _, ok := r.Context().Deadline(); !ok {
		t.Fatal("push request lost its context deadline")
	}
	for header, want := range map[string]string{
		"Content-Type": "application/octet-stream", "Content-Encoding": "aes128gcm",
		"TTL": "60", "Urgency": "high",
	} {
		if got := r.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "vapid t=") || !strings.HasSuffix(authorization, ", k="+v.Public) {
		t.Fatal("missing VAPID authorization with the service's public key")
	}
	token := strings.Split(strings.TrimPrefix(authorization, "vapid t="), ",")[0]
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[2] == "" {
		t.Fatal("VAPID token must have three signed JWT parts")
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Audience string `json:"aud"`
		Subject  string `json:"sub"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Audience != "https://push.example.invalid" || claims.Subject != v.Subject {
		t.Fatalf("unexpected VAPID claims: %+v", claims)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	// RFC 8188: 16-byte salt, 4-byte record size, key length, sender key,
	// then ciphertext with a 16-byte authentication tag.
	if len(body) != 4096 || binary.BigEndian.Uint32(body[16:20]) != 4096 || body[20] != 65 {
		t.Fatal("push body must contain an aes128gcm record with a P-256 sender key")
	}
	if _, err := ecdh.P256().NewPublicKey(body[21:86]); err != nil {
		t.Fatalf("invalid sender key in encrypted record: %v", err)
	}
	if bytes.Contains(body, []byte(payload.Title)) || bytes.Contains(body, []byte(payload.Body)) || json.Valid(body) {
		t.Fatal("push payload must be encrypted, not plaintext JSON")
	}
}

func TestSendHTTPClientIsolation(t *testing.T) {
	pool := testPool(t)
	ctx := testContext(t)
	alice := testUser(t, ctx, pool, "alice")
	bob := testUser(t, ctx, pool, "bob")
	empty := testUser(t, ctx, pool, "empty")
	payload := Payload{Title: "Isolated notification", Body: "Synthetic encrypted content", URL: "/test", Tag: "test"}
	defaultClient, defaultTransport := http.DefaultClient, http.DefaultTransport

	var calls [2][]string
	var services [2]*Service
	for i := range services {
		v := testVAPID(t)
		client := &http.Client{Timeout: 2 * time.Second, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			assertEncryptedPush(t, r, v, payload)
			calls[i] = append(calls[i], r.URL.String())
			return &http.Response{StatusCode: http.StatusCreated, Body: http.NoBody, Request: r}, nil
		})}
		var err error
		services[i], err = NewServiceWithOptions(pool, v, WithHTTPClient(client))
		if err != nil {
			t.Fatal(err)
		}
	}
	subA, subB := testSubscription(t, "alice"), testSubscription(t, "bob")
	for _, fixture := range []struct {
		user int64
		sub  Subscription
	}{{alice, subA}, {bob, subB}} {
		if err := services[0].Subscribe(ctx, fixture.user, fixture.sub); err != nil {
			t.Fatal(err)
		}
	}
	for _, send := range []struct {
		service int
		user    int64
	}{{0, alice}, {1, bob}, {0, bob}, {1, alice}, {0, empty}, {1, empty}} {
		if err := services[send.service].Send(ctx, send.user, payload); err != nil {
			t.Fatalf("service %d Send: %v", send.service, err)
		}
	}
	for i, want := range [][]string{{subA.Endpoint, subB.Endpoint}, {subB.Endpoint, subA.Endpoint}} {
		if !slices.Equal(calls[i], want) {
			t.Errorf("client %d calls = %v, want %v", i, calls[i], want)
		}
	}
	if http.DefaultClient != defaultClient || http.DefaultTransport != defaultTransport {
		t.Fatal("per-service options must not replace HTTP globals")
	}
}

func TestSubscriptionOwnershipWithHTTPClient(t *testing.T) {
	pool := testPool(t)
	ctx := testContext(t)
	alice := testUser(t, ctx, pool, "alice")
	bob := testUser(t, ctx, pool, "bob")
	hits := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits++
		return &http.Response{StatusCode: http.StatusCreated, Body: http.NoBody, Request: r}, nil
	})}
	svc, err := NewServiceWithOptions(pool, testVAPID(t), WithHTTPClient(client))
	if err != nil {
		t.Fatal(err)
	}
	sub := testSubscription(t, "shared")
	for _, endpoint := range []string{"http://push.example.invalid/plain", "https://127.0.0.1/private"} {
		bad := sub
		bad.Endpoint = endpoint
		if err := svc.Subscribe(ctx, alice, bad); err == nil {
			t.Fatalf("custom client must not allow endpoint %q", endpoint)
		}
	}
	assertStored := func(owner int64) {
		t.Helper()
		var userID int64
		var p256dh, authKey string
		if err := pool.QueryRow(ctx, `SELECT user_id, p256dh, auth FROM push_subscriptions WHERE endpoint = $1`,
			sub.Endpoint).Scan(&userID, &p256dh, &authKey); err != nil {
			t.Fatal(err)
		}
		if userID != owner || p256dh != sub.Keys.P256dh || authKey != sub.Keys.Auth {
			t.Fatal("upsert must store the current owner and refreshed keys")
		}
	}
	send := func(owner int64, wantHits int) {
		t.Helper()
		if err := svc.Send(ctx, owner, Payload{Title: "Ownership"}); err != nil {
			t.Fatal(err)
		}
		if hits != wantHits {
			t.Fatalf("transport calls = %d, want %d", hits, wantHits)
		}
	}
	if err := svc.Subscribe(ctx, alice, sub); err != nil {
		t.Fatal(err)
	}
	sub.Keys.P256dh, sub.Keys.Auth = testSubscriptionKeys(t)
	if err := svc.Subscribe(ctx, alice, sub); err != nil {
		t.Fatal(err)
	}
	assertStored(alice)
	if err := svc.Unsubscribe(ctx, bob, sub.Endpoint); err != nil {
		t.Fatal(err)
	}
	send(alice, 1)
	send(bob, 1)

	sub.Keys.P256dh, sub.Keys.Auth = testSubscriptionKeys(t)
	if err := svc.Subscribe(ctx, bob, sub); err != nil {
		t.Fatal(err)
	}
	assertStored(bob)
	send(alice, 1)
	send(bob, 2)
	if err := svc.Unsubscribe(ctx, bob, sub.Endpoint); err != nil {
		t.Fatal(err)
	}
	send(bob, 2)
}

func TestSendProviderResults(t *testing.T) {
	pool := testPool(t)
	transportErr := errors.New("synthetic transport failure")
	for _, tc := range []struct {
		name     string
		statuses []int
		wantErr  string
	}{
		{"accepted", []int{201, 204}, ""},
		{"bad-vapid", []int{403}, "different VAPID public key"},
		{"rate-limited", []int{429}, "unexpected status 429"},
		{"unavailable", []int{503}, "unexpected status 503"},
		{"transport", []int{0}, transportErr.Error()},
		{"not-found", []int{404}, "subscription gone (404)"},
		{"gone", []int{410}, "subscription gone (410)"},
		{"all-failed", []int{403, 404, 410}, "different VAPID public key"},
		{"partly-accepted", []int{201, 403, 404, 410}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testContext(t)
			user := testUser(t, ctx, pool, tc.name)
			responses := make(map[string]int)
			calls := make(map[string]int)
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				endpoint := r.URL.String()
				status, ok := responses[endpoint]
				if !ok {
					t.Fatalf("unexpected push endpoint: %s", endpoint)
				}
				calls[endpoint]++
				if status == 0 {
					return nil, transportErr
				}
				return &http.Response{
					StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"reason":"BadJwtToken"}`)), Request: r,
				}, nil
			})}
			svc, err := NewServiceWithOptions(pool, testVAPID(t), WithHTTPClient(client))
			if err != nil {
				t.Fatal(err)
			}
			for i, status := range tc.statuses {
				sub := testSubscription(t, fmt.Sprintf("%s/%d", tc.name, i))
				responses[sub.Endpoint] = status
				if err := svc.Subscribe(ctx, user, sub); err != nil {
					t.Fatal(err)
				}
			}
			err = svc.Send(ctx, user, Payload{Title: "Provider response"})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Send: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) ||
				!strings.Contains(err.Error(), fmt.Sprintf("all %d push send(s) failed", len(tc.statuses))) {
				t.Fatalf("Send = %v, want all-failed error containing %q", err, tc.wantErr)
			}
			if tc.name == "transport" && !errors.Is(err, transportErr) {
				t.Fatalf("Send must wrap transport error, got %v", err)
			}
			for endpoint, status := range responses {
				if calls[endpoint] != 1 {
					t.Errorf("endpoint %s sent %d times, want 1", endpoint, calls[endpoint])
				}
				var stored bool
				if err := pool.QueryRow(ctx,
					`SELECT EXISTS (SELECT 1 FROM push_subscriptions WHERE user_id = $1 AND endpoint = $2)`,
					user, endpoint).Scan(&stored); err != nil {
					t.Fatal(err)
				}
				wantStored := status != 404 && status != 410
				if stored != wantStored {
					t.Errorf("status %d: subscription stored = %t, want %t", status, stored, wantStored)
				}
			}
		})
	}
}
