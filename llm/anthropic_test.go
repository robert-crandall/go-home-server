package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

var sonnet55Cfg = Config{Anthropic: ProviderConfig{APIKey: "sk-ant", Model: "claude-sonnet-5-5"}}

func TestAnthropicSchemaTransportUsesTheRequestedModel(t *testing.T) {
	for _, tc := range []struct {
		name, configured, override string
		native                     bool
	}{
		{"sonnet 5.5 default", "claude-sonnet-5-5", "", true},
		{"sonnet 5.5 override", "claude-sonnet-5", "claude-sonnet-5-5", true},
		{"sonnet 5 default", "claude-sonnet-5", "", false},
		{"sonnet 5 override", "claude-sonnet-5-5", "claude-sonnet-5", false},
		{"unknown snapshot", "claude-sonnet-5-5-20990101", "", false},
		{"similar name", "claude-sonnet-5-50", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply := `{"model":"reported-model","stop_reason":"tool_use","content":[{"type":"tool_use","name":"journal_synopsis","input":{"summary":"ok","entries":[]}}]}`
			if tc.native {
				reply = `{"model":"reported-model","stop_reason":"end_turn","content":[{"type":"thinking","text":"not JSON"},{"type":"text","text":"{\"summary\":\"ok\",\"entries\":[]}"}]}`
			}
			srv, got := captureBody(t, reply)
			c := testClient(t, Config{Anthropic: ProviderConfig{APIKey: "k", Model: tc.configured}}, srv)
			req := schemaRequest(Anthropic)
			req.Model = tc.override
			req.Messages = append([]Message{{Role: System, Content: "be terse"}}, req.Messages...)
			resp := mustComplete(t, c, req)
			if resp != (Response{Provider: Anthropic, Model: "reported-model", Text: `{"summary":"ok","entries":[]}`}) {
				t.Fatalf("response = %+v", resp)
			}
			model := tc.configured
			if tc.override != "" {
				model = tc.override
			}
			if (*got)["model"] != model || (*got)["max_tokens"] != float64(1024) || (*got)["system"] != "be terse" {
				t.Errorf("request fields changed: %v", *got)
			}
			for _, key := range []string{"thinking", "temperature", "stream"} {
				if _, ok := (*got)[key]; ok {
					t.Errorf("unexpected %s: %v", key, (*got)[key])
				}
			}
			if tc.native {
				for _, key := range []string{"tools", "tool_choice"} {
					if _, ok := (*got)[key]; ok {
						t.Errorf("native output carries %s", key)
					}
				}
				config, _ := (*got)["output_config"].(map[string]any)
				format, _ := config["format"].(map[string]any)
				var wantSchema map[string]any
				if err := json.Unmarshal(req.Schema.JSON, &wantSchema); err != nil {
					t.Fatal(err)
				}
				wantSchema["description"] = req.Schema.Description
				want := map[string]any{"type": "json_schema", "schema": wantSchema}
				if !reflect.DeepEqual(format, want) {
					t.Errorf("output_config.format = %v, want %v", format, want)
				}
			} else {
				if _, ok := (*got)["output_config"]; ok {
					t.Error("forced tool output carries output_config")
				}
				tools, _ := (*got)["tools"].([]any)
				if len(tools) != 1 {
					t.Fatalf("tools = %v, want one strict tool", tools)
				}
				tool, _ := tools[0].(map[string]any)
				if tool["strict"] != true || tool["name"] != req.Schema.Name {
					t.Errorf("tool = %v", tool)
				}
				want := map[string]any{"type": "tool", "name": req.Schema.Name, "disable_parallel_tool_use": true}
				if !reflect.DeepEqual((*got)["tool_choice"], want) {
					t.Errorf("tool_choice = %v, want %v", (*got)["tool_choice"], want)
				}
			}
			if string(req.Schema.JSON) != journalSchema {
				t.Fatal("request mutated the caller's schema")
			}
		})
	}
}

func TestAnthropicNativeSchemaPreservesDescriptionsAndConstraints(t *testing.T) {
	for _, description := range []string{"", "format description"} {
		for _, root := range []string{"", `,"description":"root description"`} {
			t.Run(description+root, func(t *testing.T) {
				var body map[string]json.RawMessage
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode request: %v", err)
					}
					_, _ = w.Write([]byte(`{"stop_reason":"end_turn","content":[{"type":"text","text":"{\"id\":9007199254740993}"}]}`))
				}))
				defer srv.Close()
				c := testClient(t, sonnet55Cfg, srv)
				req := schemaRequest(Anthropic)
				original := `{"type":"object","properties":{"id":{"type":"integer","enum":[9007199254740993]}},"required":["id"],"additionalProperties":false` + root + `}`
				req.Schema.JSON = json.RawMessage(original)
				req.Schema.Description = description
				mustComplete(t, c, req)
				var config map[string]any
				if err := json.Unmarshal(body["output_config"], &config); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(body["output_config"]), "9007199254740993") {
					t.Errorf("native schema rounded the integer enum: %s", body["output_config"])
				}
				format, _ := config["format"].(map[string]any)
				schema, _ := format["schema"].(map[string]any)
				wantDescription := description
				if root != "" {
					if wantDescription != "" {
						wantDescription += "\n\n"
					}
					wantDescription += "root description"
				}
				if wantDescription == "" {
					if _, ok := schema["description"]; ok {
						t.Error("added an empty description")
					}
				} else if schema["description"] != wantDescription {
					t.Errorf("description = %v, want %q", schema["description"], wantDescription)
				}
				if string(req.Schema.JSON) != original {
					t.Fatal("request mutated the caller's schema")
				}
			})
		}
	}
}

func TestAnthropicNativeSchemaResponse(t *testing.T) {
	for name, tc := range map[string]struct {
		reason, content, wantText, wantError string
	}{
		"thinking before split text": {"end_turn", `[{"type":"thinking","text":"not JSON"},{"type":"text","text":" {\"summary\":"},{"type":"redacted_thinking","text":"ignore"},{"type":"text","text":"\"ok\",\"entries\":[]} "}]`, ` {"summary":"ok","entries":[]} `, ""},
		"no blocks":                  {"end_turn", `[]`, "", "no text blocks"},
		"empty text":                 {"end_turn", `[{"type":"text","text":""}]`, "", "no text blocks"},
		"thinking only":              {"end_turn", `[{"type":"thinking","text":"{}"}]`, "", "no text blocks"},
		"tool only":                  {"end_turn", `[{"type":"tool_use","name":"journal_synopsis","input":{}}]`, "", "no text blocks"},
		"refusal after JSON":         {"refusal", `[{"type":"text","text":"{}"}]`, "", "refused"},
	} {
		t.Run(name, func(t *testing.T) {
			reply := fmt.Sprintf(`{"stop_reason":%q,"content":%s}`, tc.reason, tc.content)
			resp, err := completeSchema(t, Anthropic, sonnet55Cfg, reply)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) || resp != (Response{}) {
					t.Fatalf("response = %+v, error = %v, want zero response and %q", resp, err, tc.wantError)
				}
			} else if err != nil || resp != (Response{Provider: Anthropic, Model: "claude-sonnet-5-5", Text: tc.wantText}) {
				t.Fatalf("response = %+v, error = %v", resp, err)
			}
		})
	}
}

func TestAnthropicNativeSchemaRequiresEndTurn(t *testing.T) {
	for _, reason := range []string{"tool_use", "max_tokens", "stop_sequence", "pause_turn", "model_context_window_exceeded", "something_new", ""} {
		t.Run(reason, func(t *testing.T) {
			// Valid JSON isolates the stop-reason guard from the object check.
			reply := fmt.Sprintf(`{"stop_reason":%q,"content":[{"type":"text","text":"{\"summary\":\"ok\",\"entries\":[]}"}]}`, reason)
			resp, err := completeSchema(t, Anthropic, sonnet55Cfg, reply)
			if err == nil || !strings.Contains(err.Error(), "did not complete") || resp != (Response{}) {
				t.Fatalf("response = %+v, error = %v, want unfinished-response error", resp, err)
			}
		})
	}
}

func TestAnthropicNativeSchemaRequiresOneJSONObject(t *testing.T) {
	for _, text := range []string{`null`, `[]`, `"text"`, `42`, `true`, ` `, `{"summary":`, `{} {}`, "```json\n{}\n```", `{} trailing`} {
		t.Run(text, func(t *testing.T) {
			encoded, err := json.Marshal(text)
			if err != nil {
				t.Fatal(err)
			}
			reply := fmt.Sprintf(`{"stop_reason":"end_turn","content":[{"type":"text","text":%s}]}`, encoded)
			resp, err := completeSchema(t, Anthropic, sonnet55Cfg, reply)
			if err == nil || !strings.Contains(err.Error(), "not a JSON object") || resp != (Response{}) {
				t.Fatalf("response = %+v, error = %v, want non-object error", resp, err)
			}
		})
	}
}

func TestAnthropicNativeSchemaProviderErrorDoesNotRetry(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"schema rejected"}}`))
	}))
	defer srv.Close()
	c := testClient(t, sonnet55Cfg, srv)
	resp, err := c.Complete(context.Background(), schemaRequest(Anthropic))
	var apiErr *Error
	if resp != (Response{}) || !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("response = %+v, error = %v", resp, err)
	}
	for _, want := range []string{"claude-sonnet-5-5", "journal_synopsis", "schema rejected"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want %q", err, want)
		}
	}
	if calls != 1 {
		t.Errorf("requests = %d, want 1", calls)
	}
}
