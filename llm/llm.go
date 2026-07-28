// Package llm calls a hosted LLM - OpenAI, Anthropic, or xAI - so apps don't
// each rebuild the HTTP plumbing.
//
// The seam matches the one the mcp package uses: this package owns *transport*
// (auth headers, request/response marshaling, provider selection, error shape)
// and nothing else. Prompts, and what an app does with the text it gets back,
// stay in the app. There is no prompt templating here and no domain logic.
//
// Wiring in an app:
//
//	client, err := llm.New(llm.ConfigFromEnv())
//	if err != nil { /* no provider configured: fail fast at startup */ }
//
//	resp, err := client.Complete(ctx, llm.Request{
//	    Messages:  []llm.Message{{Role: llm.User, Content: "Say hi."}},
//	    MaxTokens: 512,
//	})
//
// An app that switches providers sets Request.Provider per call:
//
//	resp, err := client.Complete(ctx, llm.Request{
//	    Provider:  llm.Anthropic,
//	    Messages:  msgs,
//	    MaxTokens: 1024,
//	})
//
// Stream is the same call delivered incrementally, for a UI that shows text as
// it arrives:
//
//	resp, err := client.Stream(ctx, req, func(delta string) error {
//	    return send(delta) // e.g. write one SSE frame and flush
//	})
//
// The one thing to internalize about Stream: a delta is provisional until
// Stream returns nil. Complete can withhold text it doesn't like - a refusal or
// a completion the provider's content filter cut off - but a streamed delta is
// already gone, so all Stream can do is return an error afterward. On any error
// the caller must discard what it already emitted.
//
// Because switching providers is the point, requests are validated locally
// before any HTTP so a given Request means the same thing everywhere: MaxTokens
// is required, there is no default model, Temperature (when set at all) must be
// in 0-1, and the conversation must be an optional leading system message
// followed by strictly alternating user/assistant turns starting and ending
// with user, and no message may be empty. See validate for why each rule earns
// its place.
//
// Deliberately not here: tool calling, embeddings, retries, and token/cost
// accounting. Each is additive later; none has a caller today. A non-2xx
// response - a 429 or a 529, say - surfaces as an *Error with Status set, which
// is the seam an app uses if it ever wants to retry. A provider failure
// reported mid-stream, after the 200 headers are already out, can't be one; it
// comes back as a plain error.
package llm

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/robert-crandall-org/go-home-server/config"
)

// ProviderID names a supported backend.
type ProviderID string

const (
	OpenAI    ProviderID = "openai"
	Anthropic ProviderID = "anthropic"
	XAI       ProviderID = "xai"
)

// Role is a message author. These are the only three accepted values; a
// request carrying anything else is rejected before any HTTP happens.
type Role string

const (
	System    Role = "system"
	User      Role = "user"
	Assistant Role = "assistant"
)

// defaultTimeout bounds one call. LLM calls are slow, so this is much longer
// than a normal API timeout; callers who want tighter control either inject
// their own client with WithHTTPClient or set a deadline on the context.
//
// http.Client.Timeout covers every body read, so for Stream this caps the whole
// stream rather than just the time to first token. An app that expects longer
// generations injects a client with a longer or zero Timeout and bounds the
// call with the context instead.
const defaultTimeout = 2 * time.Minute

// Message is one turn of a conversation.
type Message struct {
	Role    Role
	Content string
}

// Request is a single completion, streaming or not.
type Request struct {
	// Provider selects the backend. Empty uses the client's default (see New).
	Provider ProviderID
	// Model is the provider's model name. Empty falls back to the model
	// configured for that provider; if neither is set, Complete errors rather
	// than guessing - see ProviderConfig.Model.
	Model string
	// Messages is the conversation. At most one System message, and it must be
	// first.
	Messages []Message
	// MaxTokens caps the completion. Required: Anthropic's API demands it, and
	// letting it default per-provider would silently truncate on one provider
	// but not another for the same Request.
	MaxTokens int
	// Temperature is sampling temperature, 0 to 1. Optional: nil sends no
	// temperature at all and takes the provider's default. Use Temp to set one.
	//
	// It's a pointer because 0 is a meaningful temperature - the near-greedy
	// setting a caller parsing structured output wants - so it has to be
	// distinguishable from unset. Leaving it nil is load-bearing too:
	// OpenAI's reasoning models reject any non-default temperature outright, so
	// a request that never asked for one must not carry the field.
	//
	// The 0-1 ceiling is Anthropic's; OpenAI and xAI accept up to 2. Capping at
	// the tighter of the two keeps the cross-provider promise the rest of
	// validate makes, and above 1 is noise-generation territory anyway.
	Temperature *float64
}

// Temp returns a pointer to v, for setting Request.Temperature inline:
//
//	llm.Request{Messages: msgs, MaxTokens: 512, Temperature: llm.Temp(0.2)}
//
// Go has no way to take the address of a literal, and the alternative is a
// throwaway variable at every call site.
func Temp(v float64) *float64 { return &v }

// Response is a completed call. For Stream it is only meaningful when the error
// is nil; see Stream.
type Response struct {
	// Provider is the backend that answered. Useful when an app switches.
	Provider ProviderID
	// Model is the model name the provider reported, which can be more
	// specific than the one requested (e.g. a dated snapshot).
	Model string
	// Text is the assistant's reply. Never empty on success - a provider
	// returning no usable text is an error, not an empty completion. A refusal
	// is also an error, but a distinct one that names the refusal, so a caller
	// can tell a declined request from a broken response.
	Text string
}

// provider is one backend's transport. It's unexported on purpose: an app that
// wants to fake the LLM in a test declares its own narrow interface locally and
// *Client satisfies it, so exporting one here would be public API surface that
// buys nothing.
type provider interface {
	complete(ctx context.Context, req Request, model string) (Response, error)
	stream(ctx context.Context, req Request, model string, onText func(string) error) (Response, error)
}

// ProviderConfig is one backend's credentials and default model.
type ProviderConfig struct {
	// APIKey enables the provider. Empty means the provider is not configured.
	APIKey string
	// Model is the model used when a Request doesn't name one.
	//
	// There is deliberately no built-in default model. Model names churn every
	// few months, so a default baked into this module would rot into silently
	// calling a deprecated model. Requiring it is both less machinery and more
	// honest.
	Model string
}

func (p ProviderConfig) configured() bool { return strings.TrimSpace(p.APIKey) != "" }

// Config is the resolved LLM configuration.
//
// This is its own struct rather than fields on config.Config (the way
// notify.VAPID is), so apps that never call an LLM carry nothing.
type Config struct {
	// Default is the provider used when a Request doesn't name one. Optional;
	// see New for how the default is resolved.
	Default   ProviderID
	OpenAI    ProviderConfig
	Anthropic ProviderConfig
	XAI       ProviderConfig
}

// ConfigFromEnv reads configuration from the environment. It honors a local
// .env via config.LoadDotEnv (the same search the app uses) but does not
// require the full app config.
//
//   - LLM_PROVIDER:    default provider ("openai" | "anthropic" | "xai"). Optional.
//   - OPENAI_API_KEY / OPENAI_MODEL
//   - ANTHROPIC_API_KEY / ANTHROPIC_MODEL
//   - XAI_API_KEY / XAI_MODEL
func ConfigFromEnv() Config {
	config.LoadDotEnv()

	return Config{
		Default: ProviderID(strings.TrimSpace(os.Getenv("LLM_PROVIDER"))),
		OpenAI: ProviderConfig{
			APIKey: os.Getenv("OPENAI_API_KEY"),
			Model:  os.Getenv("OPENAI_MODEL"),
		},
		Anthropic: ProviderConfig{
			APIKey: os.Getenv("ANTHROPIC_API_KEY"),
			Model:  os.Getenv("ANTHROPIC_MODEL"),
		},
		XAI: ProviderConfig{
			APIKey: os.Getenv("XAI_API_KEY"),
			Model:  os.Getenv("XAI_MODEL"),
		},
	}
}

// Client routes completions to a configured provider. It's safe for concurrent
// use; the zero value is not usable - construct one with New.
type Client struct {
	providers map[ProviderID]provider
	models    map[ProviderID]string
	def       ProviderID
}

// Option customizes a Client.
type Option func(*clientOptions)

type clientOptions struct {
	http *http.Client
}

// WithHTTPClient injects a custom *http.Client (a different timeout, a
// transport with retries, or a test server's client). A nil client is ignored.
// One client is shared by every provider: http.Client is safe for concurrent
// use and pools connections per host.
func WithHTTPClient(h *http.Client) Option {
	return func(o *clientOptions) {
		if h != nil {
			o.http = h
		}
	}
}

// New builds a Client from cfg. It fails when no provider has an API key, so a
// misconfigured app dies at startup rather than on its first completion.
//
// The default provider (used when a Request leaves Provider empty) resolves as:
//
//  1. cfg.Default, which must have an API key.
//  2. Otherwise, if exactly one provider has a key, that one - so a
//     single-provider app never has to set LLM_PROVIDER.
//  3. Otherwise there is no default, and a Request without an explicit
//     Provider is an error. Adding a second key to an app that relied on rule 2
//     turns those calls into a clear "which one did you mean" error, which is
//     the right outcome: the config really did become ambiguous.
func New(cfg Config, opts ...Option) (*Client, error) {
	o := clientOptions{http: &http.Client{Timeout: defaultTimeout}}
	for _, opt := range opts {
		opt(&o)
	}

	c := &Client{
		providers: make(map[ProviderID]provider),
		models:    make(map[ProviderID]string),
	}

	for _, e := range []struct {
		id  ProviderID
		cfg ProviderConfig
		new func(ProviderConfig, *http.Client) provider
	}{
		{OpenAI, cfg.OpenAI, newOpenAI},
		{Anthropic, cfg.Anthropic, newAnthropic},
		{XAI, cfg.XAI, newXAI},
	} {
		if !e.cfg.configured() {
			continue
		}
		c.providers[e.id] = e.new(e.cfg, o.http)
		c.models[e.id] = strings.TrimSpace(e.cfg.Model)
	}

	if len(c.providers) == 0 {
		return nil, fmt.Errorf("llm: no provider configured (set one of OPENAI_API_KEY, ANTHROPIC_API_KEY, XAI_API_KEY)")
	}

	switch {
	case cfg.Default != "":
		if _, ok := c.providers[cfg.Default]; !ok {
			return nil, fmt.Errorf("llm: default provider %q has no API key (configured: %s)", cfg.Default, c.configuredList())
		}
		c.def = cfg.Default
	case len(c.providers) == 1:
		for id := range c.providers {
			c.def = id
		}
	}

	return c, nil
}

// Complete runs one non-streaming completion.
//
// The request is fully validated before any HTTP happens, so a request that
// couldn't work on some provider fails the same way on all of them - which
// matters for an app that switches providers at runtime.
func (c *Client) Complete(ctx context.Context, req Request) (Response, error) {
	p, model, err := c.resolve(req)
	if err != nil {
		return Response{}, err
	}
	return p.complete(ctx, req, model)
}

// Stream runs one completion and delivers the assistant's text incrementally.
//
// onText is called once per chunk, in order, on the calling goroutine - there
// is no goroutine, channel, or buffer behind this, so there's nothing to leak
// and backpressure is just onText taking its time. It receives assistant text
// only; a provider's reasoning and tool-call output is dropped the same way
// Complete drops it. Returning an error from onText stops the stream and that
// error comes back from Stream, which is how a caller abandons a generation
// whose reader has gone away.
//
// On success the returned Response is what Complete would have returned, with
// Text being every chunk concatenated - so a caller that also wants to persist
// the finished message doesn't have to accumulate it a second time.
//
// The one asymmetry with Complete: a chunk is provisional until Stream returns
// nil. Complete inspects a whole response before returning any of it, so it can
// refuse to hand back a refusal or a completion the provider's content filter
// cut off. Stream can't - those chunks are already gone by the time the
// provider says so. Every failure therefore returns a zero Response, and a
// caller must treat a non-nil error as "discard what I already emitted".
//
// A non-2xx response is an *Error, same as Complete. A failure the provider
// reports mid-stream, after its 200 headers are already out, is a plain error -
// there is no status to attach by then.
//
// Note that the client's timeout bounds the whole stream, not just the
// response headers - the default is two minutes (see defaultTimeout). An app
// expecting longer generations should inject a client with a longer or zero
// Timeout via WithHTTPClient and bound the call with ctx instead.
func (c *Client) Stream(ctx context.Context, req Request, onText func(text string) error) (Response, error) {
	if onText == nil {
		// Checked before validate so the message names the actual mistake
		// rather than whatever else might also be wrong.
		return Response{}, fmt.Errorf("llm: Stream requires a non-nil onText callback")
	}
	p, model, err := c.resolve(req)
	if err != nil {
		return Response{}, err
	}
	return p.stream(ctx, req, model, onText)
}

// resolve validates a request and picks the provider and model that will serve
// it. Shared by Complete and Stream so the two can't drift: the same Request
// must be accepted, rejected, and routed identically whichever one runs it.
func (c *Client) resolve(req Request) (provider, string, error) {
	if err := validate(req); err != nil {
		return nil, "", err
	}

	id := req.Provider
	if id == "" {
		if c.def == "" {
			return nil, "", fmt.Errorf("llm: no default provider; set Request.Provider or LLM_PROVIDER (configured: %s)", c.configuredList())
		}
		id = c.def
	}

	p, ok := c.providers[id]
	if !ok {
		return nil, "", fmt.Errorf("llm: provider %q is not configured (configured: %s)", id, c.configuredList())
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = c.models[id]
	}
	if model == "" {
		return nil, "", fmt.Errorf("llm: no model for provider %q; set Request.Model or %s_MODEL", id, strings.ToUpper(string(id)))
	}

	return p, model, nil
}

// configuredList renders the configured providers for an error message, in a
// stable order (map iteration order is random, and an error string that
// reshuffles between runs is miserable to read).
func (c *Client) configuredList() string {
	got := make([]string, 0, len(c.providers))
	for _, id := range []ProviderID{OpenAI, Anthropic, XAI} {
		if _, ok := c.providers[id]; ok {
			got = append(got, string(id))
		}
	}
	return strings.Join(got, ", ")
}

// validate enforces the cross-provider request contract. Anything a provider
// would reject - or would quietly handle differently from its peers - is caught
// here so behavior doesn't depend on which backend answered.
func validate(req Request) error {
	if len(req.Messages) == 0 {
		return fmt.Errorf("llm: at least one message is required")
	}
	if req.MaxTokens <= 0 {
		// Anthropic requires max_tokens outright; OpenAI and xAI don't. Rather
		// than invent a per-provider default (which would truncate the same
		// Request on one provider but not another), require it everywhere.
		return fmt.Errorf("llm: MaxTokens must be greater than zero")
	}
	// Anthropic caps temperature at 1 while OpenAI and xAI allow 2, so the
	// intersection is what a Request can portably mean. NaN is called out
	// separately because it compares false against both bounds, and letting it
	// through would surface as an opaque JSON marshaling error instead.
	if t := req.Temperature; t != nil {
		if math.IsNaN(*t) || *t < 0 || *t > 1 {
			return fmt.Errorf("llm: Temperature must be between 0 and 1 (got %v)", *t)
		}
	}

	for i, m := range req.Messages {
		switch m.Role {
		case System:
			// Anthropic hoists the system prompt to a top-level field, so a
			// system message in the middle of a conversation has no coherent
			// cross-provider meaning. Apps with several system fragments join
			// them into one.
			if i != 0 {
				return fmt.Errorf("llm: a system message must be the first message (found at index %d)", i)
			}
		case User, Assistant:
		default:
			return fmt.Errorf("llm: message %d has invalid role %q (want %q, %q or %q)", i, m.Role, System, User, Assistant)
		}
		// Anthropic rejects an empty text block outright while OpenAI happily
		// accepts one, so an app that built a message from an empty variable
		// gets a provider error on one backend and a wasted call on the other.
		if strings.TrimSpace(m.Content) == "" {
			return fmt.Errorf("llm: message %d (%s) has empty content", i, m.Role)
		}
	}

	// The rest of the conversation must strictly alternate user/assistant,
	// starting and ending with user. The providers documentably disagree about
	// anything else: Anthropic silently merges consecutive same-role turns into
	// one while OpenAI keeps them distinct, and Anthropic treats a trailing
	// assistant message as a prefill it continues from (newer Claude models
	// reject it outright) while OpenAI just answers it as history. Both are
	// exactly the sort of silent divergence an app that switches providers would
	// misread as a model quality difference, so refuse the shape rather than let
	// meaning depend on the backend. Apps with several consecutive user
	// fragments join them.
	rest := req.Messages
	if rest[0].Role == System {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		// Hoisting the system message out would leave Anthropic an empty
		// messages array.
		return fmt.Errorf("llm: at least one user message is required")
	}
	for i, m := range rest {
		want := User
		if i%2 == 1 {
			want = Assistant
		}
		if m.Role != want {
			return fmt.Errorf("llm: messages must alternate user/assistant after any system message (message %d is %q, want %q)", i+len(req.Messages)-len(rest), m.Role, want)
		}
	}
	if rest[len(rest)-1].Role != User {
		return fmt.Errorf("llm: the last message must be from the user, not the assistant")
	}
	return nil
}
