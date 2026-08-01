// Google sign-in, as a plain server-side OAuth 2.0 authorization code flow.
//
// Two browser redirects and nothing else: /api/auth/google/start bounces to
// Google's consent screen, /api/auth/google/callback comes back with a code,
// trades it for an ID token, and issues exactly the same session cookie a
// password login issues. Middleware, RequireUser, sliding expiry and API tokens
// are therefore untouched by this file - once the cookie is set the two login
// routes are indistinguishable.
//
// It is deliberately not the Google Identity Services button, which hands the
// browser an ID token to POST. That would put a Google script tag and a
// client-side integration in every app's SPA; this way an app needs only
//
//	<a href="/api/auth/google/start">Sign in with Google</a>
package auth

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/oauth2"

	"github.com/robert-crandall/go-home-server/internal/apisecurity"
)

const (
	// stateCookieName is deliberately not cookieName: the session cookie is the
	// credential and must never be overwritten by the OAuth handshake.
	stateCookieName = "oauth_state"
	// stateTTL bounds how long a started sign-in stays valid. Long enough to
	// pick an account and type a password, short enough that an abandoned tab
	// doesn't leave a usable state lying around.
	stateTTL = 10 * time.Minute
)

// googleEndpoint is spelled out rather than taken from golang.org/x/oauth2/google,
// which pulls in cloud.google.com/go/compute/metadata to discover GCE service
// account credentials this will never use.
var googleEndpoint = oauth2.Endpoint{
	AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
	TokenURL: "https://oauth2.googleapis.com/token",
}

// googleIssuers are the two `iss` values Google is documented to use.
var googleIssuers = []string{"https://accounts.google.com", "accounts.google.com"}

// Failure codes appended to FailurePath as ?error=. They are a fixed, internal
// set: Google's own error strings are never passed through, so the SPA has a
// closed vocabulary to switch on and nothing reflects an attacker's input.
const (
	errCodeDenied        = "oauth_denied"
	errCodeInvalidState  = "invalid_state"
	errCodeExchange      = "token_exchange_failed"
	errCodeInvalidToken  = "invalid_id_token"
	errCodeRegistrations = "registration_closed"
)

// GoogleConfig holds the OAuth client an app registered in the Google Cloud
// console, plus where to send the browser afterwards.
type GoogleConfig struct {
	ClientID     string
	ClientSecret string
	// RedirectURL is the absolute URL Google sends the browser back to, e.g.
	// https://app.example.com/api/auth/google/callback. It has to byte-match
	// the console entry, so it is configured rather than derived from the
	// request's Host - which behind a proxy would mean trusting a header the
	// client controls.
	RedirectURL string

	// SuccessPath is where the browser lands once the session cookie is set.
	// Defaults to "/".
	SuccessPath string
	// FailurePath is where the browser lands when sign-in doesn't happen,
	// with ?error=<code> appended. Defaults to "/login".
	FailurePath string
}

// googleAuth is the validated form of GoogleConfig plus the OAuth client.
type googleAuth struct {
	oauth       *oauth2.Config
	successPath string
	failurePath string
}

func newGoogleAuth(cfg GoogleConfig) (*googleAuth, error) {
	switch {
	case cfg.ClientID == "":
		return nil, errors.New("auth: google client ID is required")
	case cfg.ClientSecret == "":
		return nil, errors.New("auth: google client secret is required")
	case cfg.RedirectURL == "":
		return nil, errors.New("auth: google redirect URL is required")
	}

	success, err := redirectPath(cfg.SuccessPath, "/")
	if err != nil {
		return nil, fmt.Errorf("auth: google success path: %w", err)
	}
	failure, err := redirectPath(cfg.FailurePath, "/login")
	if err != nil {
		return nil, fmt.Errorf("auth: google failure path: %w", err)
	}

	return &googleAuth{
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			// openid is what makes Google return an id_token at all; email is
			// what puts the email and email_verified claims in it. profile is
			// still left off even though users carry a display name: it widens
			// the consent screen, and PATCH /api/auth/me sets the name.
			Scopes:   []string{"openid", "email"},
			Endpoint: googleEndpoint,
		},
		successPath: success,
		failurePath: failure,
	}, nil
}

// redirectPath validates an app-supplied landing path. "//host" is rejected
// because a browser reads it as protocol-relative and leaves the site; "?" and
// "#" are rejected so appending "?error=..." can be plain concatenation.
func redirectPath(p, def string) (string, error) {
	if p == "" {
		return def, nil
	}
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return "", fmt.Errorf("%q must be a site-relative path starting with a single /", p)
	}
	if strings.ContainsAny(p, "?#") {
		return "", fmt.Errorf("%q must not contain a query string or fragment", p)
	}
	return p, nil
}

func (g *googleAuth) failure(code string) string {
	return g.failurePath + "?error=" + code
}

// --- ID token --------------------------------------------------------------

type googleClaims struct {
	Issuer        string `json:"iss"`
	Audience      string `json:"aud"`
	Expiry        int64  `json:"exp"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
}

// parseIDToken reads and checks the claims of an ID token *that we just
// received from Google's token endpoint*, and deliberately does not verify its
// signature.
//
// That's the exception OpenID Connect Core 3.1.3.7 carves out: the token came
// back in the body of a request this process made, to a pinned HTTPS URL, with
// the client secret. TLS already answers "did this come from Google", so a JWKS
// client and its key-rotation cache would re-answer a question that isn't open.
// The one thing TLS doesn't answer is "was this token minted for us", which is
// what the audience check below is for.
//
// aud is typed as a string because Google issues a single audience. An array
// would fail to unmarshal, which is the right outcome: this doesn't handle it.
func parseIDToken(raw, clientID string) (googleClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return googleClaims{}, errors.New("id token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return googleClaims{}, fmt.Errorf("id token payload: %w", err)
	}
	var c googleClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return googleClaims{}, fmt.Errorf("id token claims: %w", err)
	}

	if !slices.Contains(googleIssuers, c.Issuer) {
		return googleClaims{}, fmt.Errorf("id token issuer %q is not Google", c.Issuer)
	}
	if !hmac.Equal([]byte(c.Audience), []byte(clientID)) {
		return googleClaims{}, errors.New("id token was issued for a different client")
	}
	if c.Expiry <= time.Now().Unix() {
		return googleClaims{}, errors.New("id token has expired")
	}
	if c.Email == "" {
		return googleClaims{}, errors.New("id token carries no email")
	}
	if !c.EmailVerified {
		return googleClaims{}, fmt.Errorf("email %q is not verified with Google", c.Email)
	}
	return c, nil
}

// --- user resolution -------------------------------------------------------

// userByEmail finds an active user, case-insensitively, and returns ErrNotFound
// when there isn't one.
func (s *Service) userByEmail(ctx context.Context, email string) (User, error) {
	var u User
	err := s.db.QueryRow(ctx,
		`SELECT id, email, name, created_at
		   FROM users
		  WHERE lower(email) = lower($1) AND deleted_at IS NULL`,
		email,
	).Scan(&u.ID, &u.Email, &u.Name, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// userForGoogle maps a verified Google email onto an account.
//
// Matching is on the email alone - no google_sub column, no identities table.
// That is what makes "either credential works" fall out for free: an account
// registered with a password is the same row a Google sign-in finds. It assumes
// the address is a Gmail one or on a domain the operator controls, so it can't
// be handed to someone else later. See the README.
//
// Registration follows OpenRegistration, and only OpenRegistration: Google
// never creates the *first* account even though password registration is
// first-user-open. Keeping that one door password-only guarantees the account a
// single-user app runs on always has a password to fall back on, so a broken
// Google client can't lock its owner out.
func (s *Service) userForGoogle(ctx context.Context, email string) (User, error) {
	u, err := s.userByEmail(ctx, email)
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return User{}, err
	}
	if !s.OpenRegistration {
		return User{}, errRegistrationClosed
	}

	u, err = s.createGoogleUser(ctx, email)
	if isUniqueViolation(err) {
		// A double-clicked callback can put two of these in flight. The loser
		// of the insert just reads the row the winner wrote and logs in.
		return s.userByEmail(ctx, email)
	}
	return u, err
}

// createGoogleUser inserts an account that can only ever be reached through
// Google. password_hash stays NOT NULL - relaxing it would change a table every
// downstream app vendors - so the row gets the bcrypt hash of a random value
// that is generated here and never stored. Nothing can present the plaintext,
// so password login fails for this account exactly like a wrong password does,
// and a future set-password flow can simply overwrite the hash.
//
// The account starts with no display name, since the ID token doesn't carry
// one at the scopes this asks for. PATCH /api/auth/me sets it afterwards.
func (s *Service) createGoogleUser(ctx context.Context, email string) (User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(randomToken()), bcrypt.DefaultCost)
	if err != nil {
		return User{}, err
	}
	var u User
	err = s.db.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, $2)
		 RETURNING id, email, name, created_at`,
		email, string(hash),
	).Scan(&u.ID, &u.Email, &u.Name, &u.CreatedAt)
	return u, err
}

// --- cookies ---------------------------------------------------------------

// stateCookie holds the CSRF state for one in-flight sign-in.
//
// SameSite=Lax is load-bearing, not a copy of the session cookie: the callback
// is a cross-site top-level navigation from accounts.google.com, and Strict
// would withhold the cookie on exactly that request.
func (s *Service) stateCookie(state string) http.Cookie {
	return http.Cookie{
		Name:     stateCookieName,
		Value:    state,
		Path:     "/",
		Expires:  time.Now().Add(stateTTL),
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	}
}

func (s *Service) clearStateCookie() http.Cookie {
	return http.Cookie{
		Name:     stateCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// --- huma endpoints --------------------------------------------------------

type googleRedirectOutput struct {
	// SetCookie is a slice because the callback emits two: the state cookie is
	// cleared and the session cookie is set in the same response.
	SetCookie []http.Cookie `header:"Set-Cookie"`
	Location  string        `header:"Location"`
}

type googleCallbackInput struct {
	Code  string `query:"code"`
	State string `query:"state"`
	// Error is Google's error parameter, present when the user cancels. It is
	// read to distinguish "declined" from "malformed", never echoed.
	Error string `query:"error"`
	// StateCookie is the other half of the state check. All four fields are
	// optional on purpose: a missing one means a failed sign-in, which belongs
	// on the login screen as a redirect, not as a 422 of problem+json in a
	// browser tab.
	StateCookie string `cookie:"oauth_state"`
}

// RegisterGoogle mounts Google sign-in on the given huma API, under
// /api/auth/google. Call it only when the app has a Google client configured;
// it returns an error for an incomplete or malformed config so a typo is a
// startup failure rather than a broken button.
//
//	if cfg.GoogleClientID != "" {
//	    if err := authSvc.RegisterGoogle(srv.API, auth.GoogleConfig{
//	        ClientID:     cfg.GoogleClientID,
//	        ClientSecret: cfg.GoogleClientSecret,
//	        RedirectURL:  cfg.GoogleRedirectURL,
//	    }); err != nil {
//	        log.Fatal(err)
//	    }
//	}
func (s *Service) RegisterGoogle(api huma.API, cfg GoogleConfig) error {
	g, err := newGoogleAuth(cfg)
	if err != nil {
		return err
	}
	s.registerGoogle(api, g)
	return nil
}

func (s *Service) registerGoogle(api huma.API, g *googleAuth) {
	huma.Register(api, huma.Operation{
		OperationID:   "google-login-start",
		Method:        http.MethodGet,
		Path:          "/api/auth/google/start",
		Summary:       "Begin Google sign-in",
		Tags:          []string{"auth"},
		DefaultStatus: http.StatusFound,
		Security:      apisecurity.Public(),
	}, func(_ context.Context, _ *struct{}) (*googleRedirectOutput, error) {
		state := randomToken()
		return &googleRedirectOutput{
			SetCookie: []http.Cookie{s.stateCookie(state)},
			Location:  g.oauth.AuthCodeURL(state),
		}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "google-login-callback",
		Method:        http.MethodGet,
		Path:          "/api/auth/google/callback",
		Summary:       "Complete Google sign-in",
		Tags:          []string{"auth"},
		DefaultStatus: http.StatusFound,
		Errors:        []int{http.StatusInternalServerError},
		Security:      apisecurity.Public(),
	}, func(ctx context.Context, in *googleCallbackInput) (*googleRedirectOutput, error) {
		// Every non-infrastructure outcome ends up here: the person is midway
		// through a browser flow and belongs back on the login screen, not
		// looking at a JSON error document.
		fail := func(code string) (*googleRedirectOutput, error) {
			return &googleRedirectOutput{
				SetCookie: []http.Cookie{s.clearStateCookie()},
				Location:  g.failure(code),
			}, nil
		}

		switch {
		case in.Error != "":
			return fail(errCodeDenied)
		case in.Code == "" || in.State == "" || in.StateCookie == "":
			return fail(errCodeInvalidState)
		case !hmac.Equal([]byte(in.State), []byte(in.StateCookie)):
			return fail(errCodeInvalidState)
		}

		// Exchange sends the configured RedirectURL along, which Google
		// re-checks against the one the code was issued for.
		tok, err := g.oauth.Exchange(ctx, in.Code)
		if err != nil {
			return fail(errCodeExchange)
		}
		raw, ok := tok.Extra("id_token").(string)
		if !ok {
			return fail(errCodeInvalidToken)
		}
		claims, err := parseIDToken(raw, g.oauth.ClientID)
		if err != nil {
			return fail(errCodeInvalidToken)
		}

		u, err := s.userForGoogle(ctx, claims.Email)
		if err != nil {
			if errors.Is(err, errRegistrationClosed) {
				return fail(errCodeRegistrations)
			}
			return nil, huma.Error500InternalServerError("could not resolve the Google account", err)
		}

		token, expires, err := s.createSession(ctx, u.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError("could not create session", err)
		}
		return &googleRedirectOutput{
			SetCookie: []http.Cookie{s.clearStateCookie(), s.cookie(token, expires)},
			Location:  g.successPath,
		}, nil
	})
}
