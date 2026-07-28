package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseServer serves a canned Server-Sent Event stream and records the request
// body, so a test can assert both what went out and what came back.
func sseServer(t *testing.T, gotBody *map[string]any, events ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotBody != nil {
			if err := json.NewDecoder(r.Body).Decode(gotBody); err != nil {
				t.Errorf("decode request: %v", err)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			_, _ = w.Write([]byte(e))
		}
	}))
}

// collect streams a request into a slice of chunks, so a test can assert the
// deltas arrived separately rather than only that they concatenate.
func collect(t *testing.T, c *Client, req Request) ([]string, Response, error) {
	t.Helper()
	var chunks []string
	resp, err := c.Stream(context.Background(), req, func(s string) error {
		chunks = append(chunks, s)
		return nil
	})
	return chunks, resp, err
}

func mustStream(t *testing.T, c *Client, req Request) ([]string, Response) {
	t.Helper()
	chunks, resp, err := collect(t, c, req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	return chunks, resp
}

// openAIChunk renders one `data:` frame of an OpenAI-compatible stream.
func openAIChunk(body string) string { return "data: " + body + "\n\n" }

// anthropicEvent renders one named Anthropic SSE frame. The event name is
// included because a real stream sends it, even though the parser reads the
// type out of the JSON.
func anthropicEvent(name, body string) string {
	return "event: " + name + "\ndata: " + body + "\n\n"
}

func openAIStream(deltas ...string) []string {
	out := make([]string, 0, len(deltas)+1)
	for _, d := range deltas {
		out = append(out, openAIChunk(fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, d)))
	}
	return append(out, openAIChunk(openAIDone))
}

// --- OpenAI-compatible streaming (OpenAI + xAI) ---------------------------

func TestOpenAIStreamSendsStreamFlagAndDeliversDeltas(t *testing.T) {
	var gotBody map[string]any
	srv := sseServer(t, &gotBody,
		// A role-only opening chunk carries no text. Real streams send one, and
		// it must not count as an empty completion or a malformed chunk.
		openAIChunk(`{"model":"gpt-test-2026-01-01","choices":[{"delta":{"role":"assistant"}}]}`),
		openAIChunk(`{"choices":[{"delta":{"content":"Hello"}}]}`),
		openAIChunk(`{"choices":[{"delta":{"content":", world"}}]}`),
		openAIChunk(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`),
		openAIChunk(openAIDone),
	)
	defer srv.Close()

	c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "sk-test", Model: "gpt-test"}}, srv)
	chunks, resp := mustStream(t, c, Request{
		Messages:  []Message{{Role: User, Content: "hi"}},
		MaxTokens: 64,
	})

	if gotBody["stream"] != true {
		t.Errorf("body = %v, want stream:true", gotBody)
	}
	if gotBody["max_completion_tokens"] != float64(64) {
		t.Errorf("max_completion_tokens = %v", gotBody["max_completion_tokens"])
	}
	// Deltas must reach the caller one at a time - that's the whole point.
	if len(chunks) != 2 || chunks[0] != "Hello" || chunks[1] != ", world" {
		t.Errorf("chunks = %q, want two separate deltas", chunks)
	}
	if resp.Text != "Hello, world" {
		t.Errorf("Text = %q", resp.Text)
	}
	if resp.Provider != OpenAI {
		t.Errorf("Provider = %q", resp.Provider)
	}
	if resp.Model != "gpt-test-2026-01-01" {
		t.Errorf("Model = %q, want the model the provider reported", resp.Model)
	}
}

func TestCompleteStillOmitsTheStreamFlag(t *testing.T) {
	// Complete and Stream share a request builder, so this pins that the
	// blocking request body didn't gain a field.
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer srv.Close()

	c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "sk-test", Model: "gpt-test"}}, srv)
	mustComplete(t, c, Request{Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 32})

	if _, ok := gotBody["stream"]; ok {
		t.Errorf("blocking request carries a stream field: %v", gotBody)
	}
}

func TestStreamCarriesTemperature(t *testing.T) {
	// Complete and Stream share each provider's request builder, so this pins
	// that the streaming body picked the field up too.
	var gotBody map[string]any
	srv := sseServer(t, &gotBody, openAIStream("hi")...)
	defer srv.Close()

	c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "k", Model: "m"}}, srv)
	mustStream(t, c, Request{
		Messages:    []Message{{Role: User, Content: "hi"}},
		MaxTokens:   32,
		Temperature: Temp(0),
	})

	if gotBody["temperature"] != float64(0) {
		t.Errorf("temperature = %v, want 0 to reach the streaming request too (body: %v)", gotBody["temperature"], gotBody)
	}
}

func TestXAIStreamUsesTheOpenAIWireFormat(t *testing.T) {
	var gotPath, gotAuth, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotAccept = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Accept")
		for _, e := range openAIStream("grok ", "says hi") {
			_, _ = w.Write([]byte(e))
		}
	}))
	defer srv.Close()

	c := testClient(t, Config{XAI: ProviderConfig{APIKey: "xai-key", Model: "grok-test"}}, srv)
	_, resp := mustStream(t, c, Request{Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 32})

	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer xai-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream", gotAccept)
	}
	if resp.Provider != XAI || resp.Text != "grok says hi" {
		t.Errorf("resp = %+v", resp)
	}
	// The provider omitted "model", so the requested one is reported back.
	if resp.Model != "grok-test" {
		t.Errorf("Model = %q", resp.Model)
	}
}

func TestOpenAIStreamRefusalIsDistinguishableFromBrokenResponses(t *testing.T) {
	srv := sseServer(t, nil,
		openAIChunk(`{"choices":[{"delta":{"refusal":"I can't "}}]}`),
		openAIChunk(`{"choices":[{"delta":{"refusal":"help with that"}}]}`),
		openAIChunk(openAIDone),
	)
	defer srv.Close()

	c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "sk-test", Model: "gpt-test"}}, srv)
	_, resp, err := collect(t, c, Request{Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 32})

	if err == nil {
		t.Fatal("want an error for a refused request")
	}
	// The refusal is accumulated across deltas the same way text is.
	if !strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), "I can't help with that") {
		t.Errorf("err = %v, want the assembled refusal", err)
	}
	if resp != (Response{}) {
		t.Errorf("Response = %+v, want the zero value on error", resp)
	}
}

func TestContentFilterIsAnErrorOnBothPaths(t *testing.T) {
	// A filtered completion can still carry partial text. Returning it as a
	// success would hand back content the provider declined to give - and
	// Complete and Stream have to agree, or the same request would succeed one
	// way and fail the other.
	t.Run("complete", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"partial"},"finish_reason":"content_filter"}]}`))
		}))
		defer srv.Close()

		c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "sk-test", Model: "gpt-test"}}, srv)
		resp, err := c.Complete(context.Background(), Request{Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 32})
		if err == nil || !strings.Contains(err.Error(), "content filter") {
			t.Fatalf("err = %v, want a content filter error", err)
		}
		if resp != (Response{}) {
			t.Errorf("Response = %+v, want the zero value on error", resp)
		}
	})

	t.Run("stream", func(t *testing.T) {
		srv := sseServer(t, nil,
			openAIChunk(`{"choices":[{"delta":{"content":"partial"}}]}`),
			openAIChunk(`{"choices":[{"delta":{},"finish_reason":"content_filter"}]}`),
			openAIChunk(openAIDone),
		)
		defer srv.Close()

		c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "sk-test", Model: "gpt-test"}}, srv)
		chunks, resp, err := collect(t, c, Request{Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 32})
		if err == nil || !strings.Contains(err.Error(), "content filter") {
			t.Fatalf("err = %v, want a content filter error", err)
		}
		if resp != (Response{}) {
			t.Errorf("Response = %+v, want the zero value on error", resp)
		}
		// The partial text was already emitted - that's exactly why the
		// contract says chunks are provisional until Stream returns nil.
		if len(chunks) != 1 {
			t.Errorf("chunks = %q", chunks)
		}
	})
}

func TestOpenAIStreamErrorInsideA200IsAnError(t *testing.T) {
	// Once the 200 headers are out the status seam is gone, so a mid-stream
	// failure arrives as a payload instead.
	srv := sseServer(t, nil,
		openAIChunk(`{"choices":[{"delta":{"content":"partial"}}]}`),
		openAIChunk(`{"error":{"type":"server_error","message":"upstream exploded"}}`),
		openAIChunk(openAIDone),
	)
	defer srv.Close()

	c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "sk-test", Model: "gpt-test"}}, srv)
	_, resp, err := collect(t, c, Request{Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 32})

	if err == nil || !strings.Contains(err.Error(), "upstream exploded") {
		t.Fatalf("err = %v, want the provider's stream error", err)
	}
	if resp != (Response{}) {
		t.Errorf("Response = %+v, want the zero value on error", resp)
	}
}

func TestOpenAIStreamWithNoTextIsAnError(t *testing.T) {
	srv := sseServer(t, nil, openAIChunk(`{"choices":[{"delta":{"role":"assistant"}}]}`), openAIChunk(openAIDone))
	defer srv.Close()

	c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "sk-test", Model: "gpt-test"}}, srv)
	_, _, err := collect(t, c, Request{Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 32})
	if err == nil || !strings.Contains(err.Error(), "no text") {
		t.Fatalf("err = %v, want a no-text error", err)
	}
}

// --- Anthropic streaming --------------------------------------------------

func TestAnthropicStreamSendsExpectedRequestAndDeliversDeltas(t *testing.T) {
	var gotBody map[string]any
	var gotVersion, gotKey string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotVersion = r.Header.Get("x-api-key"), r.Header.Get("anthropic-version")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		for _, e := range []string{
			anthropicEvent("message_start", `{"type":"message_start","message":{"model":"claude-test-20260101"}}`),
			// A real stream keeps the connection warm with these.
			anthropicEvent("ping", `{"type":"ping"}`),
			anthropicEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`),
			anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":", world"}}`),
			anthropicEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
			anthropicEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`),
			anthropicEvent("message_stop", `{"type":"message_stop"}`),
		} {
			_, _ = w.Write([]byte(e))
		}
	}))
	defer srv.Close()

	c := testClient(t, Config{Anthropic: ProviderConfig{APIKey: "ant-key", Model: "claude-test"}}, srv)
	chunks, resp := mustStream(t, c, Request{
		Messages: []Message{
			{Role: System, Content: "be terse"},
			{Role: User, Content: "hi"},
		},
		MaxTokens: 128,
	})

	if gotKey != "ant-key" || gotVersion != anthropicVersion {
		t.Errorf("headers: x-api-key=%q anthropic-version=%q", gotKey, gotVersion)
	}
	if gotBody["stream"] != true {
		t.Errorf("body = %v, want stream:true", gotBody)
	}
	// The system prompt is still hoisted out of the messages array.
	if gotBody["system"] != "be terse" {
		t.Errorf("system = %v", gotBody["system"])
	}
	if msgs, _ := gotBody["messages"].([]any); len(msgs) != 1 {
		t.Errorf("messages = %v, want just the user turn", gotBody["messages"])
	}
	if len(chunks) != 2 || chunks[0] != "Hello" || chunks[1] != ", world" {
		t.Errorf("chunks = %q, want two separate deltas", chunks)
	}
	if resp.Text != "Hello, world" || resp.Provider != Anthropic {
		t.Errorf("resp = %+v", resp)
	}
	if resp.Model != "claude-test-20260101" {
		t.Errorf("Model = %q, want the model from message_start", resp.Model)
	}
}

func TestAnthropicStreamSkipsNonTextDeltas(t *testing.T) {
	// Thinking and tool_use blocks stream their own delta types. Only
	// text_delta is the assistant's answer, which mirrors how the blocking path
	// collects text blocks and ignores the rest.
	//
	// The thinking_delta below deliberately carries a "text" field that a real
	// one wouldn't. That's the point: the gate has to be the delta's declared
	// type, not the mere presence of text, or a delta type Anthropic adds later
	// would leak straight into the answer.
	srv := sseServer(t, nil,
		anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm","text":"leaked reasoning"}}`),
		anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`),
		anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"the answer"}}`),
		anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`),
		anthropicEvent("message_stop", `{"type":"message_stop"}`),
	)
	defer srv.Close()

	c := testClient(t, Config{Anthropic: ProviderConfig{APIKey: "ant-key", Model: "claude-test"}}, srv)
	chunks, resp := mustStream(t, c, Request{Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 32})

	if len(chunks) != 1 || chunks[0] != "the answer" {
		t.Errorf("chunks = %q, want only the text delta", chunks)
	}
	if resp.Text != "the answer" {
		t.Errorf("Text = %q", resp.Text)
	}
}

func TestAnthropicStreamRefusalIsAnError(t *testing.T) {
	srv := sseServer(t, nil,
		anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`),
		anthropicEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"refusal"}}`),
		anthropicEvent("message_stop", `{"type":"message_stop"}`),
	)
	defer srv.Close()

	c := testClient(t, Config{Anthropic: ProviderConfig{APIKey: "ant-key", Model: "claude-test"}}, srv)
	chunks, resp, err := collect(t, c, Request{Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 32})

	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err = %v, want a refusal error", err)
	}
	if resp != (Response{}) {
		t.Errorf("Response = %+v, want the zero value on error", resp)
	}
	// The refusal only arrives after the text, so the caller has already seen
	// it. Nothing can be done about that except say so - hence "provisional".
	if len(chunks) != 1 {
		t.Errorf("chunks = %q", chunks)
	}
}

func TestAnthropicStreamErrorEventIsAnError(t *testing.T) {
	srv := sseServer(t, nil,
		anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`),
		anthropicEvent("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
	)
	defer srv.Close()

	c := testClient(t, Config{Anthropic: ProviderConfig{APIKey: "ant-key", Model: "claude-test"}}, srv)
	_, _, err := collect(t, c, Request{Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 32})

	if err == nil || !strings.Contains(err.Error(), "Overloaded") {
		t.Fatalf("err = %v, want the provider's stream error", err)
	}
}

func TestAnthropicStreamWithNoTextIsAnError(t *testing.T) {
	srv := sseServer(t, nil,
		anthropicEvent("message_start", `{"type":"message_start","message":{"model":"claude-test"}}`),
		anthropicEvent("message_stop", `{"type":"message_stop"}`),
	)
	defer srv.Close()

	c := testClient(t, Config{Anthropic: ProviderConfig{APIKey: "ant-key", Model: "claude-test"}}, srv)
	_, _, err := collect(t, c, Request{Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 32})
	if err == nil || !strings.Contains(err.Error(), "no text") {
		t.Fatalf("err = %v, want a no-text error", err)
	}
}

// --- Stream contract ------------------------------------------------------

func TestTruncatedStreamIsAnError(t *testing.T) {
	// A dropped connection partway through leaves a well-formed prefix. Both
	// providers document an explicit terminator, so its absence is the only
	// signal that the completion is short - and returning the prefix as a
	// success would be silently wrong.
	cases := map[string][]string{
		"openai": {
			openAIChunk(`{"choices":[{"delta":{"content":"half an ans"}}]}`),
		},
		"anthropic": {
			anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half an ans"}}`),
		},
	}

	for name, events := range cases {
		t.Run(name, func(t *testing.T) {
			srv := sseServer(t, nil, events...)
			defer srv.Close()

			cfg := Config{OpenAI: ProviderConfig{APIKey: "sk-test", Model: "gpt-test"}}
			if name == "anthropic" {
				cfg = Config{Anthropic: ProviderConfig{APIKey: "ant-key", Model: "claude-test"}}
			}
			c := testClient(t, cfg, srv)
			_, resp, err := collect(t, c, Request{Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 32})

			if err == nil || !strings.Contains(err.Error(), "ended before") {
				t.Fatalf("err = %v, want a truncated-stream error", err)
			}
			if resp != (Response{}) {
				t.Errorf("Response = %+v, want the zero value on error", resp)
			}
		})
	}
}

func TestOnTextErrorStopsTheStream(t *testing.T) {
	srv := sseServer(t, nil, openAIStream("one", "two", "three")...)
	defer srv.Close()

	c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "sk-test", Model: "gpt-test"}}, srv)

	// The realistic case: the SSE writer this feeds has gone away.
	sentinel := errors.New("client disconnected")
	var seen int
	resp, err := c.Stream(context.Background(), Request{
		Messages:  []Message{{Role: User, Content: "hi"}},
		MaxTokens: 32,
	}, func(string) error {
		seen++
		if seen == 2 {
			return sentinel
		}
		return nil
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the callback's own error unwrapped", err)
	}
	if seen != 2 {
		t.Errorf("callback ran %d times, want it to stop at the failing chunk", seen)
	}
	if resp != (Response{}) {
		t.Errorf("Response = %+v, want the zero value on error", resp)
	}
}

func TestStreamClosesTheBodyWhenOnTextFails(t *testing.T) {
	// Abandoning a generation is the whole point of an onText error, so the
	// connection has to actually go away - otherwise a browser that navigated
	// off leaves the app paying for tokens until the provider finishes. The
	// server here keeps the response open after its first chunk and reports
	// when its request context is cancelled, which only happens once the client
	// closes the body.
	closed := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(openAIChunk(`{"choices":[{"delta":{"content":"one"}}]}`)))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(closed)
		case <-release:
		}
	}))
	// LIFO: release the handler first so Close never waits on it.
	defer srv.Close()
	defer close(release)

	c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "sk-test", Model: "gpt-test"}}, srv)
	sentinel := errors.New("client disconnected")
	_, err := c.Stream(context.Background(), Request{
		Messages:  []Message{{Role: User, Content: "hi"}},
		MaxTokens: 32,
	}, func(string) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the callback's own error", err)
	}

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("response body was still open after onText failed")
	}
}

func TestOpenAIStreamUsesOnlyTheFirstChoice(t *testing.T) {
	// complete reads Choices[0] and this package never sends "n", so stream has
	// to agree. Splicing alternate completions into one answer would be worse
	// than ignoring them, and would make Stream and Complete disagree about
	// what the same request returned.
	srv := sseServer(t, nil,
		openAIChunk(`{"choices":[{"index":0,"delta":{"content":"first"}},{"index":1,"delta":{"content":"second"}}]}`),
		openAIChunk(openAIDone),
	)
	defer srv.Close()

	c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "sk-test", Model: "gpt-test"}}, srv)
	chunks, resp := mustStream(t, c, Request{Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 32})

	if len(chunks) != 1 || chunks[0] != "first" {
		t.Errorf("chunks = %q, want only the first choice", chunks)
	}
	if resp.Text != "first" {
		t.Errorf("Text = %q, want only the first choice", resp.Text)
	}
}

func TestStreamRejectsNilCallback(t *testing.T) {
	srv := sseServer(t, nil, openAIStream("hi")...)
	defer srv.Close()

	c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "sk-test", Model: "gpt-test"}}, srv)
	_, err := c.Stream(context.Background(), Request{
		Messages:  []Message{{Role: User, Content: "hi"}},
		MaxTokens: 32,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "onText") {
		t.Fatalf("err = %v, want a nil-callback error", err)
	}
}

func TestStreamValidatesAndRoutesLikeComplete(t *testing.T) {
	// Stream and Complete share resolve(), so the same request must be accepted
	// or rejected identically. A request that reaches the server here would
	// mean the two paths had drifted.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached the server; want a local failure")
	}))
	defer srv.Close()

	c := testClient(t, Config{
		OpenAI:    ProviderConfig{APIKey: "sk-test", Model: "gpt-test"},
		Anthropic: ProviderConfig{APIKey: "ant-key", Model: "claude-test"},
	}, srv)

	cases := map[string]Request{
		"invalid conversation": {Messages: []Message{{Role: Assistant, Content: "hi"}}, MaxTokens: 32},
		"missing max tokens":   {Messages: []Message{{Role: User, Content: "hi"}}},
		"unconfigured provider": {
			Provider: XAI, Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 32,
		},
		// Two keys and no LLM_PROVIDER: genuinely ambiguous.
		"no default provider": {Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 32},
	}

	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := collect(t, c, req); err == nil {
				t.Error("Stream accepted a request Complete would reject")
			}
			if _, err := c.Complete(context.Background(), req); err == nil {
				t.Error("Complete accepted it either; the test case is wrong")
			}
		})
	}
}

func TestStreamNon2xxReturnsTypedErrorWithoutTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited for key sk-super-secret"}}`))
	}))
	defer srv.Close()

	c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "sk-super-secret", Model: "gpt-test"}}, srv)
	_, _, err := collect(t, c, Request{Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 32})

	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want *Error", err, err)
	}
	if apiErr.Status != http.StatusTooManyRequests {
		t.Errorf("Status = %d", apiErr.Status)
	}
	// Streaming reuses the blocking path's error handling, key scrubbing and
	// all, so the retry seam is the same on both.
	if strings.Contains(apiErr.Body, "sk-super-secret") {
		t.Errorf("Body leaked the API key: %q", apiErr.Body)
	}
	if !strings.Contains(apiErr.Body, redacted) {
		t.Errorf("Body = %q, want the key replaced", apiErr.Body)
	}
}

func TestStreamHonorsContextCancellation(t *testing.T) {
	srv := sseServer(t, nil, openAIStream("hi")...)
	defer srv.Close()

	c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "sk-test", Model: "gpt-test"}}, srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Stream(ctx, Request{Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 32}, func(string) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestStreamReportsAMidStreamReadFailureAsItself(t *testing.T) {
	// Bounding a long generation with ctx is what the docs tell an app to do,
	// so a deadline firing between chunks is ordinary. It has to come back as
	// the cancellation, not as "the provider never sent its terminator" - those
	// send a reader looking in completely different places.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(openAIChunk(`{"choices":[{"delta":{"content":"one"}}]}`)))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	// LIFO: release the handler first so Close never waits on it.
	defer srv.Close()
	defer close(release)

	c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "sk-test", Model: "gpt-test"}}, srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resp, err := c.Stream(ctx, Request{
		Messages:  []Message{{Role: User, Content: "hi"}},
		MaxTokens: 32,
	}, func(string) error {
		// The stream is live at this point, so this cancels mid-read rather
		// than before the request goes out.
		cancel()
		return nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation rather than a truncated-stream error", err)
	}
	if resp != (Response{}) {
		t.Errorf("Response = %+v, want the zero value on error", resp)
	}
}

// --- SSE framing ----------------------------------------------------------

func TestSSEFramingHandlesRealWorldStreams(t *testing.T) {
	// One event's payload split over several data lines, keep-alive comments,
	// and blank lines with nothing pending. All three are ordinary SSE that a
	// naive line-per-event reader would mangle.
	srv := sseServer(t, nil,
		": keep-alive\n\n",
		"\n",
		"data: {\"choices\":[{\"delta\":\n",
		"data: {\"content\":\"split payload\"}}]}\n",
		"\n",
		openAIChunk(openAIDone),
	)
	defer srv.Close()

	c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "sk-test", Model: "gpt-test"}}, srv)
	chunks, resp := mustStream(t, c, Request{Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 32})

	if len(chunks) != 1 || chunks[0] != "split payload" {
		t.Errorf("chunks = %q, want the rejoined payload", chunks)
	}
	if resp.Text != "split payload" {
		t.Errorf("Text = %q", resp.Text)
	}
}

func TestStreamPassesDeltasThroughVerbatim(t *testing.T) {
	// Nothing along the path may tidy a delta. Models emit leading and trailing
	// spaces as their own tokens, so trimming anywhere - the SSE field value,
	// the chunk handed to onText, or the accumulated Text - shows up as words
	// running together in the rendered output. Both the chunk and the assembled
	// text are checked, since they're built at different points.
	const delta = " leading and trailing "
	srv := sseServer(t, nil,
		openAIChunk(fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, delta)),
		openAIChunk(openAIDone),
	)
	defer srv.Close()

	c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "sk-test", Model: "gpt-test"}}, srv)
	chunks, resp := mustStream(t, c, Request{Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 32})

	if len(chunks) != 1 || chunks[0] != delta {
		t.Errorf("chunks = %q, want the delta untouched", chunks)
	}
	if resp.Text != delta {
		t.Errorf("Text = %q, want the surrounding spaces kept", resp.Text)
	}
}
