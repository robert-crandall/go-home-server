package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testClient builds a Client whose providers all point at srv. Base URLs are
// package constants rather than config (there's no proxy or self-hosted
// endpoint in play), so the test seam is the unexported field - which is
// reachable because these tests live in the package.
func testClient(t *testing.T, cfg Config, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(cfg, WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, p := range c.providers {
		switch v := p.(type) {
		case *openAICompatible:
			v.baseURL = srv.URL
		case *anthropicProvider:
			v.baseURL = srv.URL
		default:
			t.Fatalf("unknown provider type %T", p)
		}
	}
	return c
}

func mustComplete(t *testing.T, c *Client, req Request) Response {
	t.Helper()
	resp, err := c.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return resp
}

// --- OpenAI-compatible transport (OpenAI + xAI) ---------------------------

func TestOpenAISendsExpectedRequestAndParsesResponse(t *testing.T) {
	var gotAuth, gotPath string
	// Decoded raw so the assertions are against literal wire keys. Decoding
	// into openAIRequest would let a typo'd json tag be mirrored by the test
	// type and pass anyway.
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-test-2026-01-01","choices":[{"message":{"content":"hello there"}}]}`))
	}))
	defer srv.Close()

	c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "sk-test", Model: "gpt-test"}}, srv)
	resp := mustComplete(t, c, Request{
		Messages: []Message{
			{Role: System, Content: "be terse"},
			{Role: User, Content: "hi"},
		},
		MaxTokens: 256,
	})

	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want bearer token", gotAuth)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q", gotPath)
	}
	if gotBody["model"] != "gpt-test" {
		t.Errorf("model = %v", gotBody["model"])
	}
	// The verified wire detail: OpenAI's reasoning models reject max_tokens,
	// and xAI's docs deprecate it in favor of max_completion_tokens.
	if gotBody["max_completion_tokens"] != float64(256) {
		t.Errorf("max_completion_tokens = %v, want 256", gotBody["max_completion_tokens"])
	}
	if _, ok := gotBody["max_tokens"]; ok {
		t.Errorf("body carries the deprecated max_tokens field: %v", gotBody)
	}
	// The system prompt stays in the messages array for this wire format.
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v, want system then user", gotBody["messages"])
	}
	for i, want := range []string{"system", "user"} {
		m, _ := msgs[i].(map[string]any)
		if m["role"] != want {
			t.Errorf("messages[%d].role = %v, want %q", i, m["role"], want)
		}
	}
	if resp.Text != "hello there" {
		t.Errorf("Text = %q", resp.Text)
	}
	if resp.Provider != OpenAI {
		t.Errorf("Provider = %q", resp.Provider)
	}
	// The provider's reported model can be more specific than the request's.
	if resp.Model != "gpt-test-2026-01-01" {
		t.Errorf("Model = %q, want the model the provider reported", resp.Model)
	}
}

func TestXAIUsesTheOpenAIWireFormat(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"grok says hi"}}]}`))
	}))
	defer srv.Close()

	c := testClient(t, Config{XAI: ProviderConfig{APIKey: "xai-key", Model: "grok-test"}}, srv)
	resp := mustComplete(t, c, Request{Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 32})

	if gotAuth != "Bearer xai-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q", gotPath)
	}
	if resp.Provider != XAI || resp.Text != "grok says hi" {
		t.Errorf("resp = %+v", resp)
	}
	// The provider omitted "model", so the requested one is reported back.
	if resp.Model != "grok-test" {
		t.Errorf("Model = %q, want the requested model when the provider omits it", resp.Model)
	}
}

func TestOpenAIEmptyTextIsAnError(t *testing.T) {
	// A 200 with no usable text means a parser or provider-shape problem. An
	// empty Response would make that indistinguishable from a real answer.
	cases := map[string]string{
		"no choices":    `{"choices":[]}`,
		"null content":  `{"choices":[{"message":{"content":null}}]}`,
		"empty content": `{"choices":[{"message":{"content":""}}]}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(payload))
			}))
			defer srv.Close()

			c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "k", Model: "m"}}, srv)
			_, err := c.Complete(context.Background(), Request{
				Messages:  []Message{{Role: User, Content: "x"}},
				MaxTokens: 10,
			})
			if err == nil {
				t.Fatal("expected an error for a 200 with no text")
			}
			if !strings.Contains(err.Error(), "no text") {
				t.Errorf("error = %v, want it to mention missing text", err)
			}
		})
	}
}

func TestRefusalsAreDistinguishableFromBrokenResponses(t *testing.T) {
	// A refusal is a real answer, just not a usable completion. Collapsing it
	// into the generic "no text" error would leave a caller unable to tell a
	// declined request from a client bug.
	cases := map[string]struct {
		cfg     Config
		payload string
		// detail is provider text the error must preserve, if the provider
		// gives any. Anthropic only reports the stop reason.
		detail string
	}{
		"openai refusal": {
			Config{OpenAI: ProviderConfig{APIKey: "k", Model: "m"}},
			`{"choices":[{"message":{"content":null,"refusal":"I can't help with that."}}]}`,
			"I can't help with that.",
		},
		"anthropic refusal": {
			Config{Anthropic: ProviderConfig{APIKey: "k", Model: "m"}},
			`{"content":[],"stop_reason":"refusal"}`,
			"",
		},
		// A refusal can land after the model already emitted some text.
		// Handing that partial text back as a successful completion would give
		// the caller content the provider explicitly declined to finish.
		"anthropic refusal after partial output": {
			Config{Anthropic: ProviderConfig{APIKey: "k", Model: "m"}},
			`{"content":[{"type":"text","text":"Sure, first you"}],"stop_reason":"refusal"}`,
			"",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.payload))
			}))
			defer srv.Close()

			c := testClient(t, tc.cfg, srv)
			_, err := c.Complete(context.Background(), Request{
				Messages:  []Message{{Role: User, Content: "x"}},
				MaxTokens: 10,
			})
			if err == nil {
				t.Fatal("expected an error for a refusal")
			}
			if !strings.Contains(err.Error(), "refused") {
				t.Errorf("error = %v, want it to say the model refused", err)
			}
			if strings.Contains(err.Error(), "contained no text") {
				t.Errorf("error = %v, want it distinct from the generic no-text error", err)
			}
			if tc.detail != "" && !strings.Contains(err.Error(), tc.detail) {
				t.Errorf("error = %v, want it to preserve the refusal text %q", err, tc.detail)
			}
		})
	}
}

// --- Anthropic transport --------------------------------------------------

func TestAnthropicSendsExpectedRequest(t *testing.T) {
	var gotKey, gotVersion, gotAuth, gotPath string
	// Decoded as raw JSON rather than into anthropicRequest: a typo'd struct tag
	// would be mirrored by the decode and the assertion would pass anyway. These
	// are the vendor's wire keys, so assert on the wire.
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(`{"model":"claude-test","content":[{"type":"text","text":"hi from claude"}]}`))
	}))
	defer srv.Close()

	c := testClient(t, Config{Anthropic: ProviderConfig{APIKey: "sk-ant-test", Model: "claude-test"}}, srv)
	resp := mustComplete(t, c, Request{
		Messages: []Message{
			{Role: System, Content: "be terse"},
			{Role: User, Content: "hi"},
		},
		MaxTokens: 512,
	})

	if gotKey != "sk-ant-test" {
		t.Errorf("x-api-key = %q", gotKey)
	}
	if gotVersion != anthropicVersion {
		t.Errorf("anthropic-version = %q, want %q", gotVersion, anthropicVersion)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want unset (Anthropic uses x-api-key)", gotAuth)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("path = %q", gotPath)
	}
	// Anthropic spells this max_tokens, unlike the max_completion_tokens the
	// OpenAI-compatible transport sends, and it is required.
	if gotBody["max_tokens"] != float64(512) {
		t.Errorf("max_tokens = %v, want 512 (body: %v)", gotBody["max_tokens"], gotBody)
	}
	if _, ok := gotBody["max_completion_tokens"]; ok {
		t.Errorf("body carries max_completion_tokens, which Anthropic does not accept: %v", gotBody)
	}
	if gotBody["model"] != "claude-test" {
		t.Errorf("model = %v", gotBody["model"])
	}
	// The system prompt is hoisted to a top-level field and must not remain in
	// the messages array - there is no system role in this API.
	if gotBody["system"] != "be terse" {
		t.Errorf("system = %v, want the system message hoisted out", gotBody["system"])
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v, want only the user message", gotBody["messages"])
	}
	if m, _ := msgs[0].(map[string]any); m["role"] != "user" || m["content"] != "hi" {
		t.Errorf("messages[0] = %v", msgs[0])
	}
	if resp.Provider != Anthropic || resp.Text != "hi from claude" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestAnthropicOmitsSystemWhenAbsent(t *testing.T) {
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer srv.Close()

	c := testClient(t, Config{Anthropic: ProviderConfig{APIKey: "k", Model: "m"}}, srv)
	mustComplete(t, c, Request{Messages: []Message{{Role: User, Content: "x"}}, MaxTokens: 10})

	if _, ok := raw["system"]; ok {
		t.Errorf("request body %v sent an empty system field", raw)
	}
}

func TestAnthropicSkipsNonTextBlocks(t *testing.T) {
	// content[0] is not guaranteed to be a text block: a thinking block can
	// come first. Reading content[0].text would be a real bug, so every text
	// block is collected and the rest ignored.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":[
			{"type":"thinking","thinking":"hmm"},
			{"type":"text","text":"first"},
			{"type":"tool_use","name":"x"},
			{"type":"text","text":" and second"}
		]}`))
	}))
	defer srv.Close()

	c := testClient(t, Config{Anthropic: ProviderConfig{APIKey: "k", Model: "m"}}, srv)
	resp := mustComplete(t, c, Request{Messages: []Message{{Role: User, Content: "x"}}, MaxTokens: 10})

	if resp.Text != "first and second" {
		t.Errorf("Text = %q, want the text blocks joined and others skipped", resp.Text)
	}
}

func TestAnthropicNoTextBlocksIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"thinking","thinking":"hmm"}]}`))
	}))
	defer srv.Close()

	c := testClient(t, Config{Anthropic: ProviderConfig{APIKey: "k", Model: "m"}}, srv)
	_, err := c.Complete(context.Background(), Request{
		Messages:  []Message{{Role: User, Content: "x"}},
		MaxTokens: 10,
	})
	if err == nil {
		t.Fatal("expected an error when no text block is present")
	}
	if !strings.Contains(err.Error(), "no text") {
		t.Errorf("error = %v, want it to mention missing text", err)
	}
}

// --- Routing and configuration -------------------------------------------

func TestExplicitProviderRouting(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		switch r.URL.Path {
		case "/v1/messages":
			_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"claude"}]}`))
		default:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"gpt"}}]}`))
		}
	}))
	defer srv.Close()

	c := testClient(t, Config{
		Default:   OpenAI,
		OpenAI:    ProviderConfig{APIKey: "k1", Model: "m1"},
		Anthropic: ProviderConfig{APIKey: "k2", Model: "m2"},
	}, srv)

	// Default routing.
	resp := mustComplete(t, c, Request{Messages: []Message{{Role: User, Content: "x"}}, MaxTokens: 10})
	if resp.Provider != OpenAI || gotPath != "/v1/chat/completions" {
		t.Errorf("default routed to %q via %q, want openai", resp.Provider, gotPath)
	}

	// Per-request override - the whole point for an app that switches.
	resp = mustComplete(t, c, Request{Provider: Anthropic, Messages: []Message{{Role: User, Content: "x"}}, MaxTokens: 10})
	if resp.Provider != Anthropic || gotPath != "/v1/messages" {
		t.Errorf("override routed to %q via %q, want anthropic", resp.Provider, gotPath)
	}
}

func TestSingleConfiguredProviderBecomesDefault(t *testing.T) {
	c, err := New(Config{Anthropic: ProviderConfig{APIKey: "k", Model: "m"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.def != Anthropic {
		t.Errorf("default = %q, want the single configured provider", c.def)
	}
}

func TestTwoProvidersWithoutDefaultRequiresExplicitProvider(t *testing.T) {
	c, err := New(Config{
		OpenAI:    ProviderConfig{APIKey: "k1", Model: "m1"},
		Anthropic: ProviderConfig{APIKey: "k2", Model: "m2"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.def != "" {
		t.Fatalf("default = %q, want none when the config is ambiguous", c.def)
	}
	_, err = c.Complete(context.Background(), Request{
		Messages:  []Message{{Role: User, Content: "x"}},
		MaxTokens: 10,
	})
	if err == nil {
		t.Fatal("expected an error when no provider is named and there's no default")
	}
	if !strings.Contains(err.Error(), "openai, anthropic") {
		t.Errorf("error = %v, want it to list the configured providers", err)
	}
}

func TestNewRejectsNoProviders(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected an error when no provider has an API key")
	}
}

func TestNewRejectsDefaultWithoutKey(t *testing.T) {
	_, err := New(Config{Default: XAI, OpenAI: ProviderConfig{APIKey: "k", Model: "m"}})
	if err == nil {
		t.Fatal("expected an error when the default provider has no key")
	}
}

func TestUnconfiguredProviderIsRejected(t *testing.T) {
	c, err := New(Config{OpenAI: ProviderConfig{APIKey: "k", Model: "m"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Complete(context.Background(), Request{
		Provider:  XAI,
		Messages:  []Message{{Role: User, Content: "x"}},
		MaxTokens: 10,
	})
	if err == nil {
		t.Fatal("expected an error for a provider with no API key")
	}
}

func TestModelPrecedence(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body openAIRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "k", Model: "configured-model"}}, srv)

	mustComplete(t, c, Request{Messages: []Message{{Role: User, Content: "x"}}, MaxTokens: 10})
	if gotModel != "configured-model" {
		t.Errorf("model = %q, want the configured default", gotModel)
	}

	mustComplete(t, c, Request{Model: "per-call-model", Messages: []Message{{Role: User, Content: "x"}}, MaxTokens: 10})
	if gotModel != "per-call-model" {
		t.Errorf("model = %q, want the per-request override to win", gotModel)
	}
}

func TestMissingModelIsAnError(t *testing.T) {
	// No default model table ships with this package - model names churn, so a
	// baked-in default would rot into calling a deprecated model.
	c, err := New(Config{OpenAI: ProviderConfig{APIKey: "k"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Complete(context.Background(), Request{
		Messages:  []Message{{Role: User, Content: "x"}},
		MaxTokens: 10,
	})
	if err == nil {
		t.Fatal("expected an error when no model is configured or requested")
	}
	if !strings.Contains(err.Error(), "OPENAI_MODEL") {
		t.Errorf("error = %v, want it to name the env var to set", err)
	}
}

// --- Temperature ----------------------------------------------------------

func TestTemperatureIsSentOnlyWhenSet(t *testing.T) {
	// The two transports have separate request structs, so each is asserted
	// against the raw wire rather than trusting they share a field.
	for _, tc := range []struct {
		name string
		cfg  Config
		body string
	}{
		{"openai", Config{OpenAI: ProviderConfig{APIKey: "k", Model: "m"}}, `{"choices":[{"message":{"content":"ok"}}]}`},
		{"anthropic", Config{Anthropic: ProviderConfig{APIKey: "k", Model: "m"}}, `{"content":[{"type":"text","text":"ok"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody = nil
				if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
					t.Errorf("decode request: %v", err)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := testClient(t, tc.cfg, srv)
			msgs := []Message{{Role: User, Content: "hi"}}

			// Unset must send nothing at all: OpenAI's reasoning models reject
			// a non-default temperature outright, so a caller who never asked
			// for one must keep working.
			mustComplete(t, c, Request{Messages: msgs, MaxTokens: 10})
			if _, ok := gotBody["temperature"]; ok {
				t.Errorf("body %v carries temperature for a request that never set one", gotBody)
			}

			// Zero is the whole reason the field is a pointer: it's a real,
			// useful temperature that must not read as "unset".
			mustComplete(t, c, Request{Messages: msgs, MaxTokens: 10, Temperature: Temp(0)})
			if gotBody["temperature"] != float64(0) {
				t.Errorf("temperature = %v, want 0 to be transmitted (body: %v)", gotBody["temperature"], gotBody)
			}

			mustComplete(t, c, Request{Messages: msgs, MaxTokens: 10, Temperature: Temp(0.7)})
			if gotBody["temperature"] != 0.7 {
				t.Errorf("temperature = %v, want 0.7", gotBody["temperature"])
			}
		})
	}
}

// --- Validation -----------------------------------------------------------

func TestValidationRejectsBadRequestsBeforeAnyHTTP(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "k", Model: "m"}}, srv)

	cases := []struct {
		name string
		req  Request
	}{
		{"no messages", Request{MaxTokens: 10}},
		{"zero max tokens", Request{Messages: []Message{{Role: User, Content: "x"}}}},
		{"negative max tokens", Request{Messages: []Message{{Role: User, Content: "x"}}, MaxTokens: -1}},
		{"invalid role", Request{Messages: []Message{{Role: "developer", Content: "x"}}, MaxTokens: 10}},
		{"system not first", Request{Messages: []Message{
			{Role: User, Content: "x"},
			{Role: System, Content: "late"},
		}, MaxTokens: 10}},
		{"system only", Request{Messages: []Message{{Role: System, Content: "x"}}, MaxTokens: 10}},
		// Anthropic merges consecutive same-role turns into one; OpenAI keeps
		// them distinct. Same request, different conversation.
		{"consecutive user messages", Request{Messages: []Message{
			{Role: User, Content: "context"},
			{Role: User, Content: "question"},
		}, MaxTokens: 10}},
		{"consecutive assistant messages", Request{Messages: []Message{
			{Role: User, Content: "hi"},
			{Role: Assistant, Content: "a"},
			{Role: Assistant, Content: "b"},
			{Role: User, Content: "more"},
		}, MaxTokens: 10}},
		// Anthropic continues from a trailing assistant message (prefill) while
		// OpenAI answers it as history.
		{"trailing assistant message", Request{Messages: []Message{
			{Role: User, Content: "Q"},
			{Role: Assistant, Content: "The answer is ("},
		}, MaxTokens: 10}},
		{"starts with assistant", Request{Messages: []Message{
			{Role: Assistant, Content: "unprompted"},
			{Role: User, Content: "x"},
		}, MaxTokens: 10}},
		// Anthropic rejects an empty text block; OpenAI accepts one.
		{"empty user content", Request{Messages: []Message{
			{Role: User, Content: ""},
		}, MaxTokens: 10}},
		{"whitespace-only user content", Request{Messages: []Message{
			{Role: User, Content: "   \n\t "},
		}, MaxTokens: 10}},
		{"empty system content", Request{Messages: []Message{
			{Role: System, Content: ""},
			{Role: User, Content: "x"},
		}, MaxTokens: 10}},
		// OpenAI and xAI go to 2, Anthropic stops at 1, so 1 is the ceiling a
		// Request can portably mean.
		{"negative temperature", Request{Messages: []Message{{Role: User, Content: "x"}}, MaxTokens: 10, Temperature: Temp(-0.1)}},
		{"temperature above one", Request{Messages: []Message{{Role: User, Content: "x"}}, MaxTokens: 10, Temperature: Temp(1.5)}},
		// NaN compares false against both bounds, so it needs its own check -
		// otherwise it surfaces as an opaque JSON marshaling failure instead.
		{"NaN temperature", Request{Messages: []Message{{Role: User, Content: "x"}}, MaxTokens: 10, Temperature: Temp(math.NaN())}},
		{"infinite temperature", Request{Messages: []Message{{Role: User, Content: "x"}}, MaxTokens: 10, Temperature: Temp(math.Inf(1))}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Complete(context.Background(), tc.req); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}

	// The point of validating up front: a request that couldn't work on some
	// provider fails identically on all of them, without a round trip.
	if hits != 0 {
		t.Errorf("server received %d requests, want 0 - validation must precede HTTP", hits)
	}

	// The mirror image: well-formed conversations must survive validation.
	for _, tc := range []struct {
		name string
		msgs []Message
	}{
		{"single user message", []Message{{Role: User, Content: "x"}}},
		{"system then user", []Message{
			{Role: System, Content: "be brief"},
			{Role: User, Content: "x"},
		}},
		{"a full alternating exchange", []Message{
			{Role: System, Content: "be brief"},
			{Role: User, Content: "one"},
			{Role: Assistant, Content: "two"},
			{Role: User, Content: "three"},
		}},
	} {
		t.Run("accepts "+tc.name, func(t *testing.T) {
			if _, err := c.Complete(context.Background(), Request{Messages: tc.msgs, MaxTokens: 10}); err != nil {
				t.Fatalf("valid request rejected: %v", err)
			}
		})
	}

	// The bounds themselves are legal values, not off-by-one rejections.
	for _, temp := range []float64{0, 0.5, 1} {
		t.Run(fmt.Sprintf("accepts temperature %v", temp), func(t *testing.T) {
			req := Request{Messages: []Message{{Role: User, Content: "x"}}, MaxTokens: 10, Temperature: Temp(temp)}
			if _, err := c.Complete(context.Background(), req); err != nil {
				t.Fatalf("valid request rejected: %v", err)
			}
		})
	}
}

// --- Errors ---------------------------------------------------------------

func TestNon2xxReturnsTypedErrorWithStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded"}}`))
	}))
	defer srv.Close()

	c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "k", Model: "m"}}, srv)
	_, err := c.Complete(context.Background(), Request{
		Messages:  []Message{{Role: User, Content: "x"}},
		MaxTokens: 10,
	})

	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *Error", err)
	}
	// Status is the seam an app uses to decide whether to retry - this package
	// ships no retry policy of its own.
	if apiErr.Status != http.StatusTooManyRequests {
		t.Errorf("Status = %d, want 429", apiErr.Status)
	}
	if apiErr.Provider != OpenAI {
		t.Errorf("Provider = %q", apiErr.Provider)
	}
	if !strings.Contains(apiErr.Body, "rate limit exceeded") {
		t.Errorf("Body = %q, want the response snippet", apiErr.Body)
	}
}

func TestErrorNeverLeaksAPIKey(t *testing.T) {
	const secret = "sk-ant-supersecretvalue"
	// Simulate a provider that reflects the key back in its error body - the
	// worst case for a client carrying a long-lived billable secret.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("invalid key: " + r.Header.Get("x-api-key")))
	}))
	defer srv.Close()

	c := testClient(t, Config{Anthropic: ProviderConfig{APIKey: secret, Model: "m"}}, srv)
	_, err := c.Complete(context.Background(), Request{
		Messages:  []Message{{Role: User, Content: "x"}},
		MaxTokens: 10,
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Error() leaked the API key: %q", err.Error())
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if !strings.Contains(apiErr.Body, "[REDACTED]") {
		t.Errorf("Body = %q, want the key replaced", apiErr.Body)
	}
}

func TestErrorSnippetTrimsBoundaryKeyFragments(t *testing.T) {
	// Realistic length: provider keys run 50+ characters, which is what makes a
	// leaked prefix worth caring about.
	secret := "sk-proj-" + strings.Repeat("A", 48)

	cases := map[string]string{
		// The key straddles the display cap, so truncation alone slices it and
		// leaves a prefix behind.
		"straddling the cap": strings.Repeat("x", maxErrorBodyBytes-len(secret)/2) + secret + " trailing",
		// Leading whitespace pushes the key entirely past the cap. Trimming
		// before the window is fixed drags it back into view unscrubbed.
		"behind leading whitespace": strings.Repeat(" ", maxErrorBodyBytes+len(secret)-50) + secret,
		// Whole keys earlier in the body shorten the string when replaced, which
		// shifts a later partial key in from beyond the cap.
		"shifted in by earlier redactions": secret + secret +
			strings.Repeat("x", maxErrorBodyBytes+len(secret)-2*len(secret)-30) + secret,
		// The ordinary case: a short body that simply contains the key.
		"in a short body": `{"error":"bad key ` + secret + `"}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got := errorSnippet(strings.NewReader(body), secret)

			// Six characters is short enough to be a coincidence in arbitrary
			// text but long enough that a real leak trips it.
			for n := len(secret); n >= 6; n-- {
				if strings.Contains(got, secret[:n]) {
					t.Fatalf("leaked a %d-char key prefix: %q", n, got[max(0, len(got)-60):])
				}
			}
			if len(got) > maxErrorBodyBytes {
				t.Errorf("snippet is %d bytes, want it capped at %d", len(got), maxErrorBodyBytes)
			}
		})
	}
}

func TestErrorSnippetStaysCappedWhenRedactionGrowsTheBody(t *testing.T) {
	// A misconfigured key shorter than the placeholder makes every replacement
	// expand the string, so the cap has to be re-applied after scrubbing.
	const secret = "k"
	got := errorSnippet(strings.NewReader(strings.Repeat(secret, maxErrorBodyBytes)), secret)

	if len(got) > maxErrorBodyBytes {
		t.Errorf("snippet is %d bytes, want it capped at %d", len(got), maxErrorBodyBytes)
	}
}

func TestErrorSnippetLeavesCompleteBodiesIntact(t *testing.T) {
	// The trailing-fragment trim only makes sense when the read was cut short.
	// On a complete body a trailing prefix match is a coincidence, and trimming
	// it would silently eat the end of an ordinary error message.
	secret := "sk-proj-" + strings.Repeat("A", 48)
	body := `{"error":"rate limited, retry after 30s` // ends with "s", a one-character prefix of the key

	if got := errorSnippet(strings.NewReader(body), secret); got != body {
		t.Errorf("snippet = %q, want the body unchanged", got)
	}
}

func TestErrorRedactsKeyAcrossTruncationBoundary(t *testing.T) {
	const secret = "sk-supersecretvalue"
	// Place the key so it straddles the display cap: without boundary-aware
	// redaction, truncation would slice it and leak a prefix.
	pad := strings.Repeat("x", maxErrorBodyBytes-len(secret)/2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(pad + secret + " trailing"))
	}))
	defer srv.Close()

	c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: secret, Model: "m"}}, srv)
	_, err := c.Complete(context.Background(), Request{
		Messages:  []Message{{Role: User, Content: "x"}},
		MaxTokens: 10,
	})

	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *Error", err)
	}
	for n := len(secret); n >= 6; n-- {
		if strings.Contains(apiErr.Body, secret[:n]) {
			t.Fatalf("Body leaked a %d-char key prefix: %q", n, apiErr.Body)
		}
	}
	if len(apiErr.Body) > maxErrorBodyBytes {
		t.Errorf("Body is %d bytes, want it capped at %d", len(apiErr.Body), maxErrorBodyBytes)
	}
}

func TestContextCancellationIsHonored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := testClient(t, Config{OpenAI: ProviderConfig{APIKey: "k", Model: "m"}}, srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Complete(ctx, Request{Messages: []Message{{Role: User, Content: "x"}}, MaxTokens: 10})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

// --- Config ---------------------------------------------------------------

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "anthropic")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("OPENAI_MODEL", "gpt-x")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant")
	t.Setenv("ANTHROPIC_MODEL", "claude-x")
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("XAI_MODEL", "")

	cfg := ConfigFromEnv()
	if cfg.Default != Anthropic {
		t.Errorf("Default = %q", cfg.Default)
	}
	if cfg.OpenAI.APIKey != "sk-openai" || cfg.OpenAI.Model != "gpt-x" {
		t.Errorf("OpenAI = %+v", cfg.OpenAI)
	}
	if cfg.Anthropic.APIKey != "sk-ant" || cfg.Anthropic.Model != "claude-x" {
		t.Errorf("Anthropic = %+v", cfg.Anthropic)
	}
	if cfg.XAI.configured() {
		t.Errorf("XAI = %+v, want unconfigured", cfg.XAI)
	}
}
