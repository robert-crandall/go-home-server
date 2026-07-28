package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const (
	anthropicBaseURL = "https://api.anthropic.com"
	// anthropicVersion is the required API version header. 2023-06-01 is the
	// current stable version; it's the only non-deprecated one Anthropic lists.
	anthropicVersion = "2023-06-01"
	// anthropicRefusal is the stop_reason for a request the model declined.
	anthropicRefusal = "refusal"
)

// anthropicProvider speaks Anthropic's Messages API, which differs from the
// OpenAI shape in three ways that matter: x-api-key instead of a bearer token,
// the system prompt as a top-level field rather than a message, and a response
// whose content is an array of typed blocks.
type anthropicProvider struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func newAnthropic(cfg ProviderConfig, h *http.Client) provider {
	return &anthropicProvider{baseURL: anthropicBaseURL, apiKey: strings.TrimSpace(cfg.APIKey), http: h}
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model    string             `json:"model"`
	Messages []anthropicMessage `json:"messages"`
	// MaxTokens is required by this API, which is why llm.Request.MaxTokens is
	// required rather than defaulted.
	MaxTokens int `json:"max_tokens"`
	// System is the top-level system prompt. There is no system role in the
	// messages array.
	System string `json:"system,omitempty"`
	// Temperature is omitted when the caller didn't set one, so the provider's
	// own default applies. A pointer to 0 still marshals.
	Temperature *float64 `json:"temperature,omitempty"`
	// Stream is omitted on the blocking path, so that request body is
	// byte-for-byte what it was before streaming existed.
	Stream bool `json:"stream,omitempty"`
}

type anthropicResponse struct {
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	// StopReason is "refusal" when the model declines. Same distinction as the
	// OpenAI transport's refusal field: a declined request is a different
	// outcome from a response we failed to parse.
	StopReason string `json:"stop_reason"`
}

// anthropicStreamEvent is one `data:` payload of a streaming response.
//
// Every event carries a "type" that matches its SSE event name, so this is the
// single source of truth and the event name is ignored. The fields below are
// the union of the ones we act on: message.model arrives on message_start,
// delta.type/delta.text on content_block_delta, delta.stop_reason on
// message_delta, and error on the error event. Anthropic's own streaming
// errors - an overload partway through a long generation, say - arrive this way
// rather than as a status code, because the 200 headers are long gone.
type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Message struct {
		Model string `json:"model"`
	} `json:"message"`
	Delta struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (p *anthropicProvider) setAuth(h http.Header) {
	h.Set("x-api-key", p.apiKey)
	h.Set("anthropic-version", anthropicVersion)
}

func (p *anthropicProvider) url() string { return p.baseURL + "/v1/messages" }

func (p *anthropicProvider) requestBody(req Request, model string) anthropicRequest {
	body := anthropicRequest{Model: model, MaxTokens: req.MaxTokens, Temperature: req.Temperature}

	// validate() guarantees a system message is first if present, so hoisting
	// it out here can't reorder the conversation.
	for _, m := range req.Messages {
		if m.Role == System {
			body.System = m.Content
			continue
		}
		body.Messages = append(body.Messages, anthropicMessage{Role: string(m.Role), Content: m.Content})
	}
	return body
}

func (p *anthropicProvider) complete(ctx context.Context, req Request, model string) (Response, error) {
	var out anthropicResponse
	err := doJSON(ctx, p.http, Anthropic, p.url(), p.apiKey, p.setAuth, p.requestBody(req, model), &out)
	if err != nil {
		return Response{}, err
	}

	// A refusal is a real answer from the model, just not a usable completion.
	// This is checked before the text because a refusal can arrive after
	// partial output, and returning that partial text as a successful
	// completion would hand the caller content the provider declined to give.
	if out.StopReason == anthropicRefusal {
		return Response{}, fmt.Errorf("llm: %s: model refused the request", Anthropic)
	}

	// content is an array of typed blocks and the first one is not guaranteed
	// to be text - a thinking, redacted_thinking, or tool_use block can come
	// first. Reading content[0].text would be a real bug, so collect every text
	// block and ignore the rest.
	var text strings.Builder
	for _, block := range out.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	if text.Len() == 0 {
		return Response{}, fmt.Errorf("llm: %s: response contained no text blocks", Anthropic)
	}

	respModel := out.Model
	if respModel == "" {
		respModel = model
	}
	return Response{
		Provider: Anthropic,
		Model:    respModel,
		Text:     text.String(),
	}, nil
}

func (p *anthropicProvider) stream(ctx context.Context, req Request, model string, onText func(string) error) (Response, error) {
	body := p.requestBody(req, model)
	body.Stream = true

	var (
		text       strings.Builder
		respModel  string
		stopReason string
	)

	err := doStream(ctx, p.http, Anthropic, p.url(), p.apiKey, p.setAuth, body, func(data string) error {
		var e anthropicStreamEvent
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return fmt.Errorf("llm: %s: decode stream event: %w", Anthropic, err)
		}

		// Anthropic names its SSE events, but every payload repeats the name as
		// a "type" field, so switching on the JSON keeps one source of truth.
		//
		// Unrecognized event types are ignored rather than rejected: a real
		// stream carries ping keep-alives and content_block_start/stop, and
		// Anthropic documents that it may add more event types over time.
		switch e.Type {
		case "message_start":
			if e.Message.Model != "" {
				respModel = e.Message.Model
			}
		case "content_block_delta":
			// Only text blocks produce a text_delta - thinking blocks emit
			// thinking_delta and signature_delta, tool_use blocks emit
			// input_json_delta. So this one check is the streaming equivalent
			// of complete's "collect the text blocks, ignore the rest".
			if e.Delta.Type != "text_delta" || e.Delta.Text == "" {
				return nil
			}
			text.WriteString(e.Delta.Text)
			return onText(e.Delta.Text)
		case "message_delta":
			if e.Delta.StopReason != "" {
				stopReason = e.Delta.StopReason
			}
		case "message_stop":
			return errStreamDone
		case "error":
			msg := e.Error.Message
			if msg == "" {
				msg = e.Error.Type
			}
			return fmt.Errorf("llm: %s: stream failed: %s", Anthropic, msg)
		}
		return nil
	})
	if err != nil {
		return Response{}, err
	}

	// Unlike complete, this can't withhold the refused text - it was already
	// handed to onText. Returning the error and a zero Response is all that's
	// left; see Stream's doc for why a caller must treat chunks as provisional.
	if stopReason == anthropicRefusal {
		return Response{}, fmt.Errorf("llm: %s: model refused the request", Anthropic)
	}
	if text.Len() == 0 {
		return Response{}, fmt.Errorf("llm: %s: response contained no text blocks", Anthropic)
	}

	if respModel == "" {
		respModel = model
	}
	return Response{Provider: Anthropic, Model: respModel, Text: text.String()}, nil
}
