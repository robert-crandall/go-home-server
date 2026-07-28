// Package apiclient is a small bearer-token HTTP client for talking to an app
// built on this foundation. It's the plumbing a script, cron job, or MCP server
// uses to reach the app's own API with a personal access token (the pat_ tokens
// minted by POST /api/tokens), so those callers reuse the app's real business
// logic instead of forking it against the database.
//
// It's deliberately MCP-agnostic: nothing here imports the mcp package, so any
// out-of-process tool can use it.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is the app origin used when nothing configures one. It's the
// origin (scheme + host + port), not the /api prefix - callers pass paths like
// "/api/notes" to Do.
const DefaultBaseURL = "http://localhost:8080"

// defaultTimeout bounds a single request so a hung app can't wedge a tool call
// indefinitely. Callers who need something different can inject their own
// *http.Client via WithHTTPClient.
const defaultTimeout = 30 * time.Second

// maxErrorBodyBytes caps how much of a non-2xx response body we read into an
// APIError, so a pathological error page can't balloon memory or a log line.
const maxErrorBodyBytes = 4 << 10 // 4 KiB

// redacted stands in for a token scrubbed out of an error snippet.
const redacted = "[REDACTED]"

// Client calls an app's API with a bearer token. It's safe for concurrent use;
// the zero value is not usable - construct one with New, FromConfig, or FromEnv.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// Option customizes a Client.
type Option func(*Client)

// WithHTTPClient injects a custom *http.Client (for a different timeout, a
// transport with retries, or a test server's client). A nil client is ignored.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// New builds a Client for baseURL (the app origin) authenticating with token.
// A trailing slash on baseURL is trimmed so paths join cleanly.
func New(baseURL, token string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: defaultTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Do performs a JSON request. body, if non-nil, is marshaled as the request
// payload; out, if non-nil, is unmarshaled from a 2xx response body. A nil out
// with an empty or 204 response is fine (e.g. a DELETE). Non-2xx responses map
// to a typed *APIError.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	sendingBody := body != nil
	if sendingBody {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("apiclient: marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("apiclient: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if sendingBody {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("apiclient: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{
			Method: method,
			Path:   path,
			Status: resp.StatusCode,
			Body:   errorSnippet(resp.Body, c.token),
		}
	}

	if out == nil {
		// Fully drain the (successful, app-bounded) body so the connection can be
		// reused, then done. The client timeout still caps a pathological body.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	// A 204 (or otherwise empty) 2xx with an out target: leave out unchanged
	// rather than erroring on EOF. Callers pass a fresh zero value, so this reads
	// as "no data" without a spurious decode error.
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(out); err != nil {
		if err == io.EOF {
			return nil
		}
		return fmt.Errorf("apiclient: decode %s %s response: %w", method, path, err)
	}
	return nil
}

// APIError is a non-2xx response from the app. It deliberately carries only the
// request method/path, the status, and a bounded body snippet - never the token.
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

// Error formats the failure. It never includes the bearer token.
func (e *APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("apiclient: %s %s: unexpected status %d", e.Method, e.Path, e.Status)
	}
	return fmt.Sprintf("apiclient: %s %s: unexpected status %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// errorSnippet reads a bounded, token-free snippet of an error body, for
// diagnostics. The token is scrubbed in case the app echoes request headers
// back: an APIError can land in logs, a shell, or an LLM client, and must never
// carry the secret.
//
// The read is capped first and everything else happens inside that fixed
// window. That ordering is load-bearing: redaction changes the string's length,
// so scrubbing before truncating would let bytes from beyond the cap slide into
// view unexamined. Capping first means the only way a token fragment can
// survive is a token straddling the cap, which leaves a prefix at the very end
// - so trim that when the read actually truncated, and cap once more at the end
// because replacing a short token with the placeholder grows the string.
func errorSnippet(r io.Reader, token string) string {
	// One byte past the cap, purely to detect truncation. It is discarded
	// immediately, so the display window is still exactly maxErrorBodyBytes.
	raw, _ := io.ReadAll(io.LimitReader(r, maxErrorBodyBytes+1))
	truncated := len(raw) > maxErrorBodyBytes
	if truncated {
		raw = raw[:maxErrorBodyBytes]
	}

	body := string(raw)
	if token != "" {
		body = strings.ReplaceAll(body, token, redacted)
		// Only a truncated read can slice a token in half. In a complete body a
		// trailing prefix match is coincidence, and trimming it would quietly
		// eat the last character of an ordinary error message.
		if truncated {
			body = trimTrailingTokenPrefix(body, token)
		}
	}
	body = strings.TrimSpace(body)
	if len(body) > maxErrorBodyBytes {
		body = body[:maxErrorBodyBytes]
	}
	return body
}

// trimTrailingTokenPrefix removes a partial token left dangling at the end of a
// snippet by the read cap. Whole tokens are already gone by this point, so only
// a proper prefix can remain, and only in the final bytes. The fragment is just
// dropped: the snippet is a truncated tail already, so there's nothing useful
// to mark.
func trimTrailingTokenPrefix(body, token string) string {
	for n := len(token) - 1; n > 0; n-- {
		if strings.HasSuffix(body, token[:n]) {
			return body[:len(body)-n]
		}
	}
	return body
}
