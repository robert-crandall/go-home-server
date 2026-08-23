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
	// anthropicToolUse is the stop_reason for a turn that ended in a tool
	// call, and the only one a schema-constrained response may carry. The
	// others - end_turn, max_tokens, stop_sequence, pause_turn,
	// model_context_window_exceeded - all mean the model stopped somewhere
	// other than the end of the tool input.
	anthropicToolUse = "tool_use"
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
	// Tools and ToolChoice carry a Request.Schema, and are omitted otherwise so
	// an unconstrained request is byte-for-byte what it was before schemas
	// existed.
	Tools      []anthropicTool      `json:"tools,omitempty"`
	ToolChoice *anthropicToolChoice `json:"tool_choice,omitempty"`
}

// anthropicTool is one tool definition. This is the only tool this package ever
// sends, and it is not a tool in the useful sense - it's a shape the model must
// fill in, used purely to get a constrained JSON object back.
//
// Why this rather than Anthropic's own output_config.format, which is the
// obvious fit: measured against claude-sonnet-5 on a real prompt, the native
// format truncated 23 times in 366 calls - it would degenerate into a run of
// closing braces and burn to max_tokens - while forced tool use failed 0 times
// in 174.
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	// Strict asks Anthropic to guarantee schema validation of the tool input.
	// Unlike OpenAI's flag of the same name it imposes no extra schema
	// requirements of its own.
	Strict bool `json:"strict"`
}

// anthropicToolChoice forces the model to answer by calling one named tool.
type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
	// DisableParallelToolUse makes Anthropic emit exactly one tool call.
	// complete still checks that, because a promise about the response is not
	// the same thing as having verified the response.
	DisableParallelToolUse bool `json:"disable_parallel_tool_use"`
}

type anthropicResponse struct {
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		// Name and Input are populated on a tool_use block. Input is kept raw
		// so a schema-constrained response is handed back exactly as the
		// provider wrote it rather than round-tripped through a Go map.
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
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

	if s := req.Schema; s != nil {
		body.Tools = []anthropicTool{{
			Name:        s.Name,
			Description: s.Description,
			InputSchema: s.JSON,
			Strict:      true,
		}}
		body.ToolChoice = &anthropicToolChoice{
			Type:                   "tool",
			Name:                   s.Name,
			DisableParallelToolUse: true,
		}
	}
	return body
}

func (p *anthropicProvider) complete(ctx context.Context, req Request, model string) (Response, error) {
	var out anthropicResponse
	err := doJSON(ctx, p.http, Anthropic, p.url(), p.apiKey, p.setAuth, p.requestBody(req, model), &out)
	if err != nil {
		return Response{}, schemaRequestError(Anthropic, model, req.Schema, err)
	}

	// A refusal is a real answer from the model, just not a usable completion.
	// This is checked before the text because a refusal can arrive after
	// partial output, and returning that partial text as a successful
	// completion would hand the caller content the provider declined to give.
	if out.StopReason == anthropicRefusal {
		return Response{}, fmt.Errorf("llm: %s: model refused the request", Anthropic)
	}

	respModel := out.Model
	if respModel == "" {
		respModel = model
	}

	if req.Schema != nil {
		text, err := anthropicToolInput(req.Schema.Name, out)
		if err != nil {
			return Response{}, err
		}
		return Response{Provider: Anthropic, Model: respModel, Text: text}, nil
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

	return Response{
		Provider: Anthropic,
		Model:    respModel,
		Text:     text.String(),
	}, nil
}

// anthropicToolInput pulls the constrained JSON object out of a forced tool
// call.
//
// Every check here fails closed, because the failure this whole path exists to
// remove is a half-written JSON object accepted as a successful answer:
//
//   - Any stop_reason other than tool_use means the model stopped somewhere
//     other than the end of the tool input, so whatever it wrote is truncated
//     and truncated JSON is invalid JSON. The reason is named rather than
//     matched against a list of known-bad ones, so a stop_reason Anthropic adds
//     later is loud instead of silently accepted.
//   - Zero blocks means the model answered with prose instead of calling the
//     tool. tool_choice should prevent that; if it doesn't, an error beats a
//     Response holding nothing.
//   - More than one means picking one would silently discard the rest.
//     disable_parallel_tool_use is meant to prevent this too. Counting every
//     tool_use block rather than only the ones named name is what makes that a
//     real check: filtering by name first would let an extra call to some other
//     tool through as "exactly one".
func anthropicToolInput(name string, out anthropicResponse) (string, error) {
	if out.StopReason != anthropicToolUse {
		return "", fmt.Errorf("llm: %s: structured response did not complete: stop_reason %q, want %q", Anthropic, out.StopReason, anthropicToolUse)
	}

	var called string
	var input json.RawMessage
	found := 0
	for _, block := range out.Content {
		if block.Type != anthropicToolUse {
			continue
		}
		found++
		called, input = block.Name, block.Input
	}
	switch {
	case found == 0:
		return "", fmt.Errorf("llm: %s: response contained no %q block", Anthropic, anthropicToolUse)
	case found > 1:
		return "", fmt.Errorf("llm: %s: response contained %d %q blocks, want exactly one", Anthropic, found, anthropicToolUse)
	case called != name:
		return "", fmt.Errorf("llm: %s: response called tool %q, want %q", Anthropic, called, name)
	}
	if _, ok := decodeJSONObject(input); !ok {
		return "", fmt.Errorf("llm: %s: structured response was not a JSON object", Anthropic)
	}
	return string(input), nil
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
