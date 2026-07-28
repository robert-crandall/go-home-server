package llm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// errStreamDone is how a provider's event handler says "I saw my documented
// end-of-stream marker, stop scanning". It never reaches a caller.
var errStreamDone = errors.New("llm: stream complete")

// doStream performs one streaming JSON request and hands each dispatched
// Server-Sent Event's data payload to onEvent. It is deliberately the sibling of
// doJSON and nothing more: it owns transport and SSE *framing*, while what the
// events mean - which one carries text, which one ends the stream, which one is
// an error - stays in each provider's own handler. The two wire formats disagree
// enough on all three that a shared "streaming framework" would hide more than
// it saved.
//
// Only the data field is passed on. Every other SSE field is parsed past and
// dropped: the OpenAI-compatible stream doesn't name its events at all, and
// Anthropic repeats the event name inside the JSON as "type", which is the
// single source of truth its handler switches on. The id and retry fields exist
// for EventSource's automatic reconnect, and this package does not reconnect
// (see the package doc on retries).
//
// onEvent returning errStreamDone ends the scan successfully. Any other error
// from onEvent is returned as-is, which is also how an onText callback's error
// reaches the caller.
//
// A stream that ends without its provider's terminator is an error: a dropped
// connection or a proxy reset partway through a long generation is an ordinary
// thing to happen, and handing back a truncated completion as a success is the
// worst available outcome. A read failure is reported as itself rather than as
// "truncated", so a network drop isn't misfiled as a protocol violation.
func doStream(
	ctx context.Context,
	httpClient *http.Client,
	id ProviderID,
	url string,
	apiKey string,
	setAuth func(h http.Header),
	body any,
	onEvent func(data string) error,
) error {
	resp, err := postJSON(ctx, httpClient, id, url, apiKey, "text/event-stream", setAuth, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// bufio.Scanner caps a single line at 64 KiB. That's a limit on one SSE
	// line, not on one token - but for the text-only completions this package
	// makes, both providers emit token-sized events, and the largest structured
	// one (Anthropic's message_start) is a few hundred bytes. scanner.Err() is
	// checked below, so if that assumption is ever wrong it surfaces as a real
	// error instead of a silently truncated completion.
	scanner := bufio.NewScanner(resp.Body)

	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()

		// A blank line dispatches whatever has accumulated. An event with no
		// data is not dispatched at all, per the SSE spec - that's what makes a
		// bare comment or a stray field harmless.
		if line == "" {
			if data.Len() == 0 {
				continue
			}
			// Each data field appended a "\n"; the last one is not part of the
			// payload.
			payload := strings.TrimSuffix(data.String(), "\n")
			data.Reset()

			if err := onEvent(payload); err != nil {
				if errors.Is(err, errStreamDone) {
					return nil
				}
				return err
			}
			continue
		}

		// A line starting with a colon is a comment, used as a keep-alive.
		if strings.HasPrefix(line, ":") {
			continue
		}

		field, value, _ := strings.Cut(line, ":")
		if field != "data" {
			continue
		}
		// Per the SSE rule exactly one optional leading space is framing, not
		// value. It happens not to matter for JSON payloads, but TrimSpace here
		// would be a latent bug the day a payload isn't JSON.
		data.WriteString(strings.TrimPrefix(value, " "))
		data.WriteString("\n")
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("llm: %s: read stream: %w", id, err)
	}
	return fmt.Errorf("llm: %s: stream ended before the provider signaled completion", id)
}
