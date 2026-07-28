// Package notify solves web push notifications once, so apps don't have to.
//
// The backend stores browser PushSubscription objects in Postgres and sends
// notifications to them using the Web Push protocol with VAPID. The matching
// frontend half - a service worker plus a subscribe flow - is the app's to
// write; this package covers the server side of it.
//
// Wiring in an app:
//
//	nsvc, err := notify.NewService(pool, notify.VAPID{Public: ..., Private: ..., Subject: ...})
//	if err != nil { /* malformed/mismatched VAPID keys: fail fast */ }
//	notify.Register(api, nsvc, func(ctx context.Context) (int64, error) {
//	    u, err := auth.RequireUser(ctx)
//	    return u.ID, err
//	})
//
// Then anywhere in the app: nsvc.Send(ctx, userID, notify.Payload{...}).
package notify

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

// VAPID P-256 key sizes (raw bytes, before base64url): the public key is an
// uncompressed EC point (0x04 || X || Y = 65 bytes), the private key is the
// 32-byte scalar.
const (
	vapidPublicKeyBytes  = 65
	vapidPrivateKeyBytes = 32
)

// VAPID holds the application server's Web Push identity keys.
type VAPID struct {
	Public  string
	Private string
	Subject string // "mailto:you@example.com" or an https: site URL
}

// Service sends web push notifications and manages subscriptions.
type Service struct {
	db    *pgxpool.Pool
	vapid VAPID
}

// NewService constructs a notify service. When VAPID keys are provided it
// validates them up front (base64url shape, P-256 sizes, and that the public
// key actually derives from the private key) so a malformed or mismatched pair
// fails loudly at startup instead of turning into an iOS-only 403 at send time.
// Empty keys mean push is disabled, which is not an error.
func NewService(db *pgxpool.Pool, v VAPID) (*Service, error) {
	if v.Public != "" || v.Private != "" {
		if err := validateVAPIDKeys(v.Public, v.Private); err != nil {
			return nil, err
		}
		if err := validateVAPIDSubject(v.Subject); err != nil {
			return nil, err
		}
	}
	return &Service{db: db, vapid: v}, nil
}

// Enabled reports whether VAPID keys are configured.
func (s *Service) Enabled() bool {
	return s.vapid.Public != "" && s.vapid.Private != ""
}

// PublicKey returns the VAPID public key the frontend needs to subscribe.
func (s *Service) PublicKey() string { return s.vapid.Public }

// Subscription mirrors the browser PushSubscription JSON shape.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// Payload is a notification's content.
type Payload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url,omitempty"`
	// Tag collapses notifications: a new one with the same tag replaces the
	// previous on the lock screen instead of stacking. It only takes effect if
	// the app's service worker passes tag through to showNotification, which is
	// not something this package can do for you.
	Tag string `json:"tag,omitempty"`
}

// Subscribe stores (or refreshes) a push subscription for a user. It enforces
// the storage invariant itself - non-empty endpoint/keys and a public-HTTPS
// endpoint (the SSRF guard) - so foundation consumers that call it directly get
// the same protection as the HTTP handler, not only the handler path.
func (s *Service) Subscribe(ctx context.Context, userID int64, sub Subscription) error {
	if sub.Endpoint == "" || sub.Keys.P256dh == "" || sub.Keys.Auth == "" {
		return errors.New("notify: subscription endpoint and keys are required")
	}
	if err := validatePushEndpoint(sub.Endpoint); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx,
		`INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (endpoint)
		 DO UPDATE SET p256dh = EXCLUDED.p256dh, auth = EXCLUDED.auth, user_id = EXCLUDED.user_id`,
		userID, sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth,
	)
	return err
}

// Unsubscribe removes a user's subscription by its endpoint. It is scoped to
// the user so one account can't delete another account's subscription.
func (s *Service) Unsubscribe(ctx context.Context, userID int64, endpoint string) error {
	_, err := s.db.Exec(ctx,
		`DELETE FROM push_subscriptions WHERE user_id = $1 AND endpoint = $2`, userID, endpoint)
	return err
}

// Send pushes a notification to every subscription belonging to userID. Stale
// subscriptions (410 Gone / 404) are pruned automatically. It returns an error
// if there were subscriptions but none were accepted, so callers that care
// (e.g. a "send test notification" endpoint) can surface the failure. The
// returned error carries a representative per-subscription failure, so a VAPID
// key mismatch surfaces as an actionable message rather than a bare status.
func (s *Service) Send(ctx context.Context, userID int64, p Payload) error {
	if !s.Enabled() {
		return errors.New("notify: VAPID keys not configured")
	}

	subs, err := s.subscriptionsFor(ctx, userID)
	if err != nil {
		return err
	}

	body, err := json.Marshal(p)
	if err != nil {
		return err
	}

	return s.sendAll(ctx, userID, body, subs)
}

// sendAll delivers body to each of the user's stored subscriptions and does the
// delivered/failed accounting. It is split out of Send so the delivery rules can
// be tested against a real HTTP server without a database.
//
// It re-applies the endpoint guard to every stored row. Subscribe validates on
// the way in, but rows written before that guard existed (or by any other
// writer) can still hold a loopback/private/CGNAT/plain-http endpoint, and this
// is the code that would fetch it - so validating here is what actually closes
// the SSRF hole. An invalid row is skipped and logged, never fetched, and never
// silently counted as delivered.
//
// It skips rather than deletes on purpose. A 404/410 is the push service itself
// saying the subscription is gone, which is authoritative; failing our endpoint
// policy is only a local judgement, and policy changes (this foundation runs on
// CGNAT-ish networks where a self-hosted relay could one day be legitimate).
// Skipping is reversible, deleting is not, and a skipped row is inert.
func (s *Service) sendAll(ctx context.Context, userID int64, body []byte, subs []webpush.Subscription) error {
	var (
		stale     []string
		delivered int
		failed    int
		lastErr   error
		// Failures are ranked - gone < invalid endpoint < real send failure - so
		// the (nondeterministic) order rows come back in can't decide which
		// diagnostic the caller sees. lastErrIsGone records whether lastErr is
		// still only the lowest rank and may be replaced.
		lastErrIsGone bool
	)
	for _, sub := range subs {
		if err := validatePushEndpoint(sub.Endpoint); err != nil {
			failed++
			log.Printf("notify: skipping push subscription with invalid endpoint (user %d, host %q): %v",
				userID, endpointHost(sub.Endpoint), err)
			if lastErr == nil || lastErrIsGone {
				lastErr = fmt.Errorf("notify: stored subscription endpoint is not allowed: %w", err)
				lastErrIsGone = false
			}
			continue
		}

		gone, sendErr := s.sendOne(ctx, body, sub)
		switch {
		case gone:
			stale = append(stale, sub.Endpoint)
			failed++
			// A pruned-stale subscription ("subscription gone") is the least
			// actionable failure, so only let it seed lastErr when nothing
			// better has been recorded. A real send failure below always wins,
			// regardless of the (nondeterministic) order subs come back in, so
			// the key-mismatch diagnostic isn't clobbered by a trailing 410.
			if lastErr == nil {
				lastErr = sendErr
				lastErrIsGone = true
			}
		case sendErr != nil:
			failed++
			lastErr = sendErr
			lastErrIsGone = false
		default:
			delivered++
		}
	}

	for _, endpoint := range stale {
		_ = s.Unsubscribe(ctx, userID, endpoint)
	}

	if delivered == 0 && failed > 0 {
		if lastErr != nil {
			return fmt.Errorf("notify: all %d push send(s) failed: %w", failed, lastErr)
		}
		return fmt.Errorf("notify: all %d push send(s) failed", failed)
	}
	return nil
}

// endpointHost returns just the host of a push endpoint, for log lines. A push
// endpoint is a capability URL - its path carries the subscription's secret
// token - so the full endpoint must never be logged. The host is enough to
// recognise the offending row alongside the user ID. An unparseable URL, which
// validatePushEndpoint also rejects, yields "".
func endpointHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// sendOne delivers to a single subscription. gone is true when the push service
// reports the subscription no longer exists (404/410), so the caller prunes it.
func (s *Service) sendOne(ctx context.Context, body []byte, sub webpush.Subscription) (gone bool, err error) {
	resp, err := webpush.SendNotificationWithContext(ctx, body, &sub, &webpush.Options{
		// librarySubscriber strips our "mailto:" prefix so webpush-go re-adds
		// exactly one: passing the subject directly yields an invalid
		// "mailto:mailto:..." JWT sub that Apple rejects with 403 BadJwtToken
		// (FCM is lenient, which is why this only breaks iOS).
		Subscriber:      librarySubscriber(s.vapid.Subject),
		VAPIDPublicKey:  s.vapid.Public,
		VAPIDPrivateKey: s.vapid.Private,
		TTL:             60,
		// High urgency asks the push service (and APNs for iOS) to wake the
		// device promptly rather than batch the message. Delivery timing, not
		// user-facing escalation.
		Urgency: webpush.UrgencyHigh,
	})
	if err != nil {
		return false, fmt.Errorf("notify: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Only read the body when we need it for an error message.
	readBody := func() string {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return strings.TrimSpace(string(raw))
	}
	return classifySendStatus(resp.StatusCode, readBody)
}

// classifySendStatus turns a push service response status into (gone, err).
// gone is true for 404/410 (prune the subscription). Only a 2xx counts as
// delivered; everything else - including a stray sub-200 informational status -
// is a failure that must not be mistaken for delivery. body is read lazily so we
// only pull the response body when building an error message.
func classifySendStatus(status int, body func() string) (gone bool, err error) {
	switch {
	case status == http.StatusNotFound || status == http.StatusGone:
		return true, fmt.Errorf("notify: subscription gone (%d)", status)
	case status >= 200 && status < 300:
		return false, nil
	default:
		trimmed := body()
		// A 401/403 carrying "BadJwtToken" is the push service rejecting our
		// VAPID signature, almost always a key mismatch: the subscription was
		// created with a different application server key than we sign with now
		// (rotated keys, or reusing another app's keys). Spell out the fix.
		if (status == http.StatusUnauthorized || status == http.StatusForbidden) &&
			strings.Contains(trimmed, "BadJwtToken") {
			return false, fmt.Errorf("notify: push service rejected the VAPID token (%d: %s): the subscription was created with a different VAPID public key than this server signs with; re-subscribe the device or confirm VAPID_PUBLIC_KEY/VAPID_PRIVATE_KEY match", status, trimmed)
		}
		return false, fmt.Errorf("notify: unexpected status %d: %s", status, trimmed)
	}
}

func (s *Service) subscriptionsFor(ctx context.Context, userID int64) ([]webpush.Subscription, error) {
	rows, err := s.db.Query(ctx,
		`SELECT endpoint, p256dh, auth FROM push_subscriptions WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var subs []webpush.Subscription
	for rows.Next() {
		var sub webpush.Subscription
		if err := rows.Scan(&sub.Endpoint, &sub.Keys.P256dh, &sub.Keys.Auth); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

// GenerateVAPIDKeys returns a fresh VAPID key pair, for one-time setup.
func GenerateVAPIDKeys() (public, private string, err error) {
	private, public, err = webpush.GenerateVAPIDKeys()
	return public, private, err
}

// VerifyKeyPair reports whether the base64url public and private VAPID keys are
// a valid, matching P-256 pair. It's the same check NewService runs at startup,
// exported for setup/diagnostic tooling.
func VerifyKeyPair(publicKey, privateKey string) error {
	return validateVAPIDKeys(publicKey, privateKey)
}

// SubjectClaim returns the JWT `sub` claim that will actually be sent to the
// push service for the given configured subject, after accounting for
// webpush-go's mailto: prefixing. Use it to confirm the sub is a single,
// well-formed mailto:/https: URI.
func SubjectClaim(subject string) string {
	sub := librarySubscriber(subject)
	if strings.HasPrefix(sub, "https:") {
		return sub
	}
	return "mailto:" + sub
}

// librarySubscriber adapts our stored VAPID subject to the form webpush-go
// expects. The library unconditionally prefixes "mailto:" to any subscriber
// that isn't an https: URL, so handing it an already-"mailto:"-prefixed subject
// produces an invalid "mailto:mailto:..." JWT `sub`. Apple validates the sub
// strictly and rejects the malformed value with 403 BadJwtToken (FCM is
// lenient, which is why this only breaks iOS). Stripping a leading "mailto:"
// lets the library re-add exactly one; https: subjects pass through untouched.
func librarySubscriber(subject string) string {
	return strings.TrimPrefix(subject, "mailto:")
}

// validateVAPIDSubject checks the configured VAPID subject is a usable JWT `sub`:
// a mailto: with a non-empty address, or an https: URL with a non-empty host. A
// bare "mailto:" or "https:" would otherwise survive startup and produce an
// invalid `sub` the push service rejects, so catch it here at boot.
//
// It is intentionally strict about the exact form the send path handles: no
// whitespace, and a lowercase "mailto:"/"https:" prefix, because
// librarySubscriber and webpush-go both match a literal lowercase prefix
// (webpush-go re-adds "mailto:" to anything not starting with "https:"). A
// subject that differed only in case or padding would still validate here but
// sign a broken sub, so reject it up front instead.
func validateVAPIDSubject(subject string) error {
	// mailto:/https: URIs never carry raw whitespace; a stray space (a common
	// env-var mistake) would pass a prefix check but be copied verbatim into the
	// JWT sub, so reject any whitespace before anything else.
	if strings.IndexFunc(subject, unicode.IsSpace) >= 0 {
		return fmt.Errorf("notify: VAPID subject must not contain whitespace, got %q", subject)
	}
	switch {
	case strings.HasPrefix(subject, "mailto:"):
		addr := strings.TrimPrefix(subject, "mailto:")
		if addr == "" {
			return fmt.Errorf("notify: VAPID subject mailto: must include an address, got %q", subject)
		}
		// A doubled scheme (mailto:mailto:you@... or mailto:https://...) would
		// survive here but break the sub: librarySubscriber strips only the
		// first "mailto:", then webpush-go re-adds one, recreating exactly the
		// malformed claim this check exists to prevent. Reject a nested scheme.
		lowerAddr := strings.ToLower(addr)
		if strings.HasPrefix(lowerAddr, "mailto:") || strings.HasPrefix(lowerAddr, "https:") {
			return fmt.Errorf("notify: VAPID subject mailto: address must not contain another scheme, got %q", subject)
		}
	case strings.HasPrefix(subject, "https:"):
		u, err := url.Parse(subject)
		if err != nil || u.Hostname() == "" {
			return fmt.Errorf("notify: VAPID subject https: must include a host, got %q", subject)
		}
	default:
		return fmt.Errorf("notify: VAPID subject must be a mailto: or https: URL, got %q", subject)
	}
	return nil
}

// validateVAPIDKeys checks the keys decode as base64url to the expected P-256
// sizes and form a matching pair: the public key must be a 65-byte uncompressed
// EC point (leading 0x04), the private key the 32-byte scalar, and the public
// key must derive from the private key. Keys must decode under the same strict
// base64url forms webpush-go accepts (see decodeBase64URL), so a key that passes
// here also decodes at send time.
func validateVAPIDKeys(publicKey, privateKey string) error {
	if publicKey == "" || privateKey == "" {
		return fmt.Errorf("notify: VAPID public and private keys are both required")
	}
	pub, err := decodeBase64URL(publicKey)
	if err != nil {
		return fmt.Errorf("notify: VAPID public key is not valid base64url: %w", err)
	}
	if len(pub) != vapidPublicKeyBytes || pub[0] != 0x04 {
		return fmt.Errorf("notify: VAPID public key must be a %d-byte uncompressed P-256 point, got %d bytes", vapidPublicKeyBytes, len(pub))
	}
	priv, err := decodeBase64URL(privateKey)
	if err != nil {
		return fmt.Errorf("notify: VAPID private key is not valid base64url: %w", err)
	}
	if len(priv) != vapidPrivateKeyBytes {
		return fmt.Errorf("notify: VAPID private key must be %d bytes, got %d", vapidPrivateKeyBytes, len(priv))
	}
	// A mismatched pair yields a signature the push service can't verify, which
	// Apple reports as a 403 BadJwtToken at send time. Deriving and comparing
	// here turns a bad key rotation into a loud startup failure instead.
	key, err := ecdh.P256().NewPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("notify: VAPID private key is not a valid P-256 scalar: %w", err)
	}
	if !bytes.Equal(key.PublicKey().Bytes(), pub) {
		return fmt.Errorf("notify: VAPID public and private keys are not a matching pair (the public key does not derive from the private key); regenerate both and set them together")
	}
	return nil
}

// decodeBase64URL decodes a VAPID key exactly the way webpush-go's decodeVapidKey
// does: strict standard-padded base64url first, then strict raw (unpadded)
// base64url. Both attempts are strict, so an over- or mis-padded key (e.g. an
// extra "==") is rejected here rather than silently normalized. That keeps the
// startup fail-fast honest: any key this accepts, webpush-go can also decode at
// send time, and any key it rejects would have failed there anyway.
func decodeBase64URL(s string) ([]byte, error) {
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}

// validatePushEndpoint rejects anything that isn't a syntactically valid HTTPS
// URL with a public host. The endpoint is later fetched server-side (every
// Send POSTs to it), so this is the SSRF guard: it blocks loopback, private,
// link-local, multicast, broadcast and unspecified hosts (localhost, 127.0.0.1,
// 10.x, 192.168.x, ::1, 169.254.169.254 cloud metadata, 255.255.255.255, ...),
// plus RFC 6598 CGNAT space (100.64.0.0/10).
//
// It deliberately does not resolve DNS: a public hostname that resolves to a
// private address still passes here. Full egress protection (DNS-rebinding, per
// request IP pinning) belongs at the network layer. Real push services (FCM,
// Apple, Mozilla) are public HTTPS hosts.
func validatePushEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("endpoint must be a valid URL")
	}
	if u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("endpoint must be an https:// URL")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("endpoint must be an https:// URL")
	}
	// Canonicalize before the literal checks below: a single trailing DNS dot
	// ("localhost.") resolves identically to the undotted name, and an IPv6 zone
	// ("fe80::1%eth0") makes net.ParseIP return nil even though net/http can
	// dial that link-local address - either would otherwise slip past the checks.
	host = strings.TrimSuffix(host, ".")
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return fmt.Errorf("endpoint host is not allowed")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsMulticast() || ip.IsUnspecified() || ip.Equal(net.IPv4bcast) ||
			cgnatRange.Contains(ip) {
			return fmt.Errorf("endpoint host is not allowed")
		}
	}
	return nil
}

// cgnatRange is RFC 6598 shared address space (100.64.0.0/10). It is non-public
// and commonly reachable inside CGNAT / Tailscale networks - exactly the kind of
// deployment this foundation runs in - but net.IP.IsPrivate does not cover it,
// so block it explicitly to keep the public-host invariant.
var cgnatRange = func() *net.IPNet {
	_, n, err := net.ParseCIDR("100.64.0.0/10")
	if err != nil {
		panic(err)
	}
	return n
}()

// --- huma endpoints --------------------------------------------------------

// CurrentUserFunc resolves the acting user's ID from the request context,
// typically wrapping auth.RequireUser. Keeping it a function avoids a hard
// dependency from notify onto a specific auth package.
type CurrentUserFunc func(ctx context.Context) (int64, error)

// Register mounts push endpoints under /api/push: the VAPID public key,
// subscribe, unsubscribe, and a self-test notification.
func Register(api huma.API, svc *Service, currentUser CurrentUserFunc) {
	huma.Register(api, huma.Operation{
		OperationID: "push-vapid-key",
		Method:      http.MethodGet,
		Path:        "/api/push/vapid-public-key",
		Summary:     "Get the VAPID public key for push subscription",
		Tags:        []string{"push"},
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body struct {
			PublicKey string `json:"publicKey"`
			Enabled   bool   `json:"enabled"`
		}
	}, error) {
		out := &struct {
			Body struct {
				PublicKey string `json:"publicKey"`
				Enabled   bool   `json:"enabled"`
			}
		}{}
		out.Body.PublicKey = svc.PublicKey()
		out.Body.Enabled = svc.Enabled()
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "push-subscribe",
		Method:      http.MethodPost,
		Path:        "/api/push/subscribe",
		Summary:     "Register a browser push subscription",
		Tags:        []string{"push"},
	}, func(ctx context.Context, in *struct{ Body Subscription }) (*struct{}, error) {
		userID, err := currentUser(ctx)
		if err != nil {
			return nil, err
		}
		if in.Body.Endpoint == "" || in.Body.Keys.P256dh == "" || in.Body.Keys.Auth == "" {
			return nil, huma.Error422UnprocessableEntity("subscription endpoint and keys are required")
		}
		// SSRF guard: the endpoint is later fetched server-side by Send, so
		// reject anything that isn't a public https host before persisting it.
		if err := validatePushEndpoint(in.Body.Endpoint); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		if err := svc.Subscribe(ctx, userID, in.Body); err != nil {
			return nil, err
		}
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "push-unsubscribe",
		Method:      http.MethodPost,
		Path:        "/api/push/unsubscribe",
		Summary:     "Remove a browser push subscription",
		Tags:        []string{"push"},
	}, func(ctx context.Context, in *struct {
		Body struct {
			Endpoint string `json:"endpoint"`
		}
	}) (*struct{}, error) {
		userID, err := currentUser(ctx)
		if err != nil {
			return nil, err
		}
		if err := svc.Unsubscribe(ctx, userID, in.Body.Endpoint); err != nil {
			return nil, err
		}
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "push-test",
		Method:      http.MethodPost,
		Path:        "/api/push/test",
		Summary:     "Send a test notification to yourself",
		Tags:        []string{"push"},
	}, func(ctx context.Context, _ *struct{}) (*struct{}, error) {
		userID, err := currentUser(ctx)
		if err != nil {
			return nil, err
		}
		err = svc.Send(ctx, userID, Payload{
			Title: "It works",
			Body:  "This is a test notification from your app.",
			URL:   "/",
			Tag:   "notify-test",
		})
		if err != nil {
			return nil, huma.Error500InternalServerError("could not send notification", err)
		}
		return &struct{}{}, nil
	})
}
