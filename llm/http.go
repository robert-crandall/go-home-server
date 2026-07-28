package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxErrorBodyBytes caps how much of a non-2xx response body we keep in an
// Error, so a pathological error page can't balloon memory or a log line.
const maxErrorBodyBytes = 4 << 10 // 4 KiB

// redacted stands in for an API key scrubbed out of an error snippet.
const redacted = "[REDACTED]"

// Error is a non-2xx response from a provider.
//
// Status is exported deliberately: it's the seam an app uses to decide whether
// to retry (429 everywhere, plus 529 "overloaded" from Anthropic and 503 from
// OpenAI). This package ships no retry policy of its own - these are
// single-user apps making occasional calls, so a failure surfaces to the human,
// who retries.
type Error struct {
	Provider ProviderID
	Status   int
	// Body is a bounded snippet of the response, with the API key scrubbed.
	Body string
}

func (e *Error) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("llm: %s: unexpected status %d", e.Provider, e.Status)
	}
	return fmt.Sprintf("llm: %s: unexpected status %d: %s", e.Provider, e.Status, e.Body)
}

// postJSON performs one JSON POST and hands back the response on a 2xx.
//
// setAuth is the per-provider header work (OpenAI and xAI use a bearer token,
// Anthropic uses x-api-key plus a version header), and apiKey is passed
// separately so a non-2xx body can be scrubbed of it. accept differs between
// the blocking and streaming paths, which is the only reason this is split out
// of doJSON.
//
// The caller owns resp.Body on success; every error path closes it here.
func postJSON(
	ctx context.Context,
	httpClient *http.Client,
	id ProviderID,
	url string,
	apiKey string,
	accept string,
	setAuth func(h http.Header),
	body any,
) (*http.Response, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llm: %s: marshal request: %w", id, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("llm: %s: build request: %w", id, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	setAuth(req.Header)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: %s: %w", id, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, &Error{
			Provider: id,
			Status:   resp.StatusCode,
			Body:     errorSnippet(resp.Body, apiKey),
		}
	}
	return resp, nil
}

// doJSON performs one JSON request and decodes a 2xx body into out.
func doJSON(
	ctx context.Context,
	httpClient *http.Client,
	id ProviderID,
	url string,
	apiKey string,
	setAuth func(h http.Header),
	body any,
	out any,
) error {
	resp, err := postJSON(ctx, httpClient, id, url, apiKey, "application/json", setAuth, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("llm: %s: decode response: %w", id, err)
	}
	return nil
}

// errorSnippet reads a bounded, key-free snippet of an error body.
//
// A provider API key is a long-lived billable secret, and an Error string can
// land in a log, a terminal, or another LLM's context - so whole keys are
// scrubbed out.
//
// The read is capped first and everything else happens inside that fixed
// window. That ordering is load-bearing: redaction changes the string's length,
// so scrubbing before truncating would let bytes from beyond the cap slide into
// view unexamined. Capping first means the only way a key fragment can survive
// is a key straddling the cap, which leaves a prefix at the very end - so trim
// that when the read actually truncated, and cap once more at the end because
// replacing a short key with the placeholder grows the string.
//
// What this deliberately does not do is hunt for arbitrary key *prefixes* mid
// body. The only partial keys that show up in practice are the ones providers
// mask themselves ("Incorrect API key provided: sk-proj-****abcd"), and that
// masking is both safe by their design and the single most useful thing in the
// message - it tells you which key was rejected.
func errorSnippet(r io.Reader, apiKey string) string {
	// One byte past the cap, purely to detect truncation. It is discarded
	// immediately, so the display window is still exactly maxErrorBodyBytes.
	raw, _ := io.ReadAll(io.LimitReader(r, maxErrorBodyBytes+1))
	truncated := len(raw) > maxErrorBodyBytes
	if truncated {
		raw = raw[:maxErrorBodyBytes]
	}

	body := string(raw)
	if apiKey != "" {
		body = strings.ReplaceAll(body, apiKey, redacted)
		// Only a truncated read can slice a key in half. In a complete body a
		// trailing prefix match is coincidence, and trimming it would quietly
		// eat the last character of an ordinary error message.
		if truncated {
			body = trimTrailingKeyPrefix(body, apiKey)
		}
	}
	body = strings.TrimSpace(body)
	if len(body) > maxErrorBodyBytes {
		body = body[:maxErrorBodyBytes]
	}
	return body
}

// trimTrailingKeyPrefix removes a partial key left dangling at the end of a
// snippet by the read cap. Whole keys are already gone by this point, so only a
// proper prefix can remain, and only in the final bytes. The fragment is just
// dropped: the snippet is a truncated tail already, so there's nothing useful
// to mark.
func trimTrailingKeyPrefix(body, apiKey string) string {
	for n := len(apiKey) - 1; n > 0; n-- {
		if strings.HasSuffix(body, apiKey[:n]) {
			return body[:len(body)-n]
		}
	}
	return body
}
