package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Default endpoints. These are package constants rather than configuration:
// neither app talks to a proxy, Azure, or a self-hosted endpoint, so there's no
// user-controlled URL here and no SSRF surface. Tests override the unexported
// field below.
const (
	openAIBaseURL = "https://api.openai.com"
	xaiBaseURL    = "https://api.x.ai"
)

// openAIDone is the sentinel payload that terminates a stream. Both OpenAI and
// xAI document it.
const openAIDone = "[DONE]"

// finishContentFilter is the finish_reason for a completion the provider
// withheld. It's a declined request like a refusal, not a broken response.
const finishContentFilter = "content_filter"

// openAICompatible speaks the OpenAI Chat Completions wire format, which xAI
// implements too - so one transport serves both providers.
//
// Why chat/completions and not OpenAI's newer Responses API: OpenAI now
// recommends /v1/responses for new projects and calls chat/completions
// "legacy", but it's supported with no announced shutdown, and it's the common
// denominator that lets these two providers share a transport. If OpenAI ever
// sunsets it, that's a one-place fix here that every app picks up with
// `go get -u` - which is the argument for this package living in the module.
type openAICompatible struct {
	id      ProviderID
	baseURL string
	apiKey  string
	http    *http.Client
}

func newOpenAI(cfg ProviderConfig, h *http.Client) provider {
	return &openAICompatible{id: OpenAI, baseURL: openAIBaseURL, apiKey: strings.TrimSpace(cfg.APIKey), http: h}
}

func newXAI(cfg ProviderConfig, h *http.Client) provider {
	return &openAICompatible{id: XAI, baseURL: xaiBaseURL, apiKey: strings.TrimSpace(cfg.APIKey), http: h}
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
	// MaxCompletionTokens is the current field name for both providers:
	// OpenAI's reasoning models require it over the older max_tokens, and xAI's
	// docs mark max_tokens deprecated in its favor.
	MaxCompletionTokens int `json:"max_completion_tokens"`
	// Temperature is omitted when the caller didn't set one, which is
	// load-bearing rather than cosmetic: OpenAI's reasoning models reject a
	// non-default temperature with a 400, so a request that never asked for
	// one must not carry the field. A pointer to 0 still marshals.
	Temperature *float64 `json:"temperature,omitempty"`
	// Stream is omitted on the blocking path, so that request body is
	// byte-for-byte what it was before streaming existed.
	Stream bool `json:"stream,omitempty"`
}

type openAIResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			// Refusal is populated instead of Content when the model declines.
			// It's a distinct outcome from a broken response, so it gets a
			// distinct error rather than collapsing into "no text".
			Refusal string `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// openAIStreamChunk is one `data:` payload of a streaming response. It mirrors
// openAIResponse except that text arrives in delta rather than message, and an
// error can be delivered inside an already-200 stream.
type openAIStreamChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			Refusal string `json:"refusal"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	// Error carries a failure that happened after the 200 headers - a
	// mid-stream capacity problem, say. postJSON can't see those, so the status
	// seam doesn't apply and it's handled here.
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (p *openAICompatible) setAuth(h http.Header) {
	h.Set("Authorization", "Bearer "+p.apiKey)
}

func (p *openAICompatible) url() string { return p.baseURL + "/v1/chat/completions" }

func (p *openAICompatible) requestBody(req Request, model string) openAIRequest {
	// Both providers accept a system role in the messages array, so the
	// conversation maps across one-for-one.
	msgs := make([]openAIMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, openAIMessage{Role: string(m.Role), Content: m.Content})
	}
	return openAIRequest{
		Model:               model,
		Messages:            msgs,
		MaxCompletionTokens: req.MaxTokens,
		Temperature:         req.Temperature,
	}
}

func (p *openAICompatible) complete(ctx context.Context, req Request, model string) (Response, error) {
	var out openAIResponse
	err := doJSON(ctx, p.http, p.id, p.url(), p.apiKey, p.setAuth, p.requestBody(req, model), &out)
	if err != nil {
		return Response{}, err
	}

	if len(out.Choices) == 0 {
		// A 200 carrying no choices is a parser or provider-shape problem, not
		// a model with nothing to say. Returning an empty Response would hide
		// the difference from the caller.
		return Response{}, fmt.Errorf("llm: %s: response contained no text", p.id)
	}
	choice := out.Choices[0]

	// A refusal or a filtered completion is a real answer from the model, just
	// not a usable one - so name which it was instead of reporting the generic
	// shape error. Both are checked before the text because either can arrive
	// alongside partial content, and returning that as a successful completion
	// would hand the caller content the provider declined to give.
	if choice.Message.Refusal != "" {
		return Response{}, fmt.Errorf("llm: %s: model refused the request: %s", p.id, choice.Message.Refusal)
	}
	if choice.FinishReason == finishContentFilter {
		return Response{}, fmt.Errorf("llm: %s: response was withheld by the provider's content filter", p.id)
	}
	if choice.Message.Content == "" {
		return Response{}, fmt.Errorf("llm: %s: response contained no text", p.id)
	}

	respModel := out.Model
	if respModel == "" {
		respModel = model
	}
	return Response{
		Provider: p.id,
		Model:    respModel,
		Text:     choice.Message.Content,
	}, nil
}

func (p *openAICompatible) stream(ctx context.Context, req Request, model string, onText func(string) error) (Response, error) {
	body := p.requestBody(req, model)
	body.Stream = true

	var (
		text      strings.Builder
		refusal   strings.Builder
		respModel string
		filtered  bool
	)

	err := doStream(ctx, p.http, p.id, p.url(), p.apiKey, p.setAuth, body, func(data string) error {
		// This wire format doesn't name its events; the stream ends with a
		// sentinel payload instead.
		if data == openAIDone {
			return errStreamDone
		}

		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("llm: %s: decode stream chunk: %w", p.id, err)
		}
		if chunk.Error != nil {
			msg := chunk.Error.Message
			if msg == "" {
				msg = chunk.Error.Type
			}
			return fmt.Errorf("llm: %s: stream failed: %s", p.id, msg)
		}
		if chunk.Model != "" {
			respModel = chunk.Model
		}

		// Only the first choice, exactly like complete. This package never
		// sends "n", so there is only ever one - and concatenating alternate
		// completions into a single answer would be worse than ignoring them.
		if len(chunk.Choices) == 0 {
			return nil
		}
		c := chunk.Choices[0]
		if c.FinishReason == finishContentFilter {
			filtered = true
		}
		refusal.WriteString(c.Delta.Refusal)
		// Role-only and finish-only chunks carry no content. Those are
		// ordinary, not malformed.
		if c.Delta.Content == "" {
			return nil
		}
		text.WriteString(c.Delta.Content)
		return onText(c.Delta.Content)
	})
	if err != nil {
		return Response{}, err
	}

	// Same precedence as complete: a declined request is named as one, and only
	// then is a textless stream reported as a shape problem.
	if refusal.Len() > 0 {
		return Response{}, fmt.Errorf("llm: %s: model refused the request: %s", p.id, refusal.String())
	}
	if filtered {
		return Response{}, fmt.Errorf("llm: %s: response was withheld by the provider's content filter", p.id)
	}
	if text.Len() == 0 {
		return Response{}, fmt.Errorf("llm: %s: response contained no text", p.id)
	}

	if respModel == "" {
		respModel = model
	}
	return Response{Provider: p.id, Model: respModel, Text: text.String()}, nil
}
