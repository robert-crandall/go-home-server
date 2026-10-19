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

func TestLMStudioRequests(t *testing.T) {
	for _, key := range []string{"", "local-token"} {
		for _, ttl := range []int{0, 300} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("key=%t/ttl=%d/stream=%t", key != "", ttl, stream), func(t *testing.T) {
					var got map[string]any
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
							t.Errorf("request = %s %s", r.Method, r.URL.Path)
						}
						wantAuth := ""
						if key != "" {
							wantAuth = "Bearer " + key
						}
						if r.Header.Get("Authorization") != wantAuth || (key == "" && len(r.Header.Values("Authorization")) != 0) {
							t.Errorf("Authorization = %q, want %q", r.Header.Values("Authorization"), wantAuth)
						}
						if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
							t.Errorf("decode request: %v", err)
						}
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							for _, event := range openAIStream("local ", "reply") {
								_, _ = w.Write([]byte(event))
							}
						} else {
							_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"local reply"}}]}`))
						}
					}))
					defer srv.Close()

					// Use the public constructor, not testClient's endpoint override.
					c, err := New(Config{LMStudio: LMStudioConfig{
						BaseURL: "  " + srv.URL + "/  ", APIKey: " " + key + " ",
						Model: " local-model ", TTLSeconds: ttl,
					}}, WithHTTPClient(srv.Client()))
					if err != nil {
						t.Fatalf("New: %v", err)
					}
					req := Request{
						Messages:  []Message{{Role: System, Content: "be terse"}, {Role: User, Content: "hi"}},
						MaxTokens: 64, Temperature: Temp(0),
					}
					resp := Response{}
					if stream {
						chunks, streamed := mustStream(t, c, req)
						resp = streamed
						if !reflect.DeepEqual(chunks, []string{"local ", "reply"}) {
							t.Errorf("chunks = %q", chunks)
						}
					} else {
						resp = mustComplete(t, c, req)
					}
					if resp != (Response{Provider: LMStudio, Model: "local-model", Text: "local reply"}) {
						t.Errorf("response = %+v", resp)
					}
					want := map[string]any{
						"model": "local-model", "max_tokens": float64(64), "temperature": float64(0),
						"messages": []any{
							map[string]any{"role": "system", "content": "be terse"},
							map[string]any{"role": "user", "content": "hi"},
						},
					}
					if ttl > 0 {
						want["ttl"] = float64(ttl)
					}
					if stream {
						want["stream"] = true
					}
					if !reflect.DeepEqual(got, want) {
						t.Errorf("body = %#v, want %#v", got, want)
					}
				})
			}
		}
	}
}

func TestLMStudioRoutingAndModelOverride(t *testing.T) {
	srv, got := captureBody(t, `{"choices":[{"message":{"content":"ok"}}]}`)
	c, err := New(Config{
		Default:  LMStudio,
		LMStudio: LMStudioConfig{BaseURL: srv.URL, Model: "configured"},
		OpenAI:   ProviderConfig{APIKey: "hosted-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 64}
	mustComplete(t, c, req)
	if (*got)["model"] != "configured" {
		t.Errorf("default model = %v", (*got)["model"])
	}
	req.Provider, req.Model = LMStudio, "override"
	mustComplete(t, c, req)
	if (*got)["model"] != "override" {
		t.Errorf("override model = %v", (*got)["model"])
	}
	c, err = New(Config{LMStudio: LMStudioConfig{BaseURL: srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	req.Model = ""
	if _, err := c.Complete(context.Background(), req); err == nil || !strings.Contains(err.Error(), "LMSTUDIO_MODEL") {
		t.Fatalf("missing model error = %v", err)
	}
	c, err = New(Config{
		LMStudio: LMStudioConfig{BaseURL: srv.URL},
		OpenAI:   ProviderConfig{APIKey: "hosted-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req.Provider = ""
	if _, err := c.Complete(context.Background(), req); err == nil || !strings.Contains(err.Error(), "openai, lmstudio") {
		t.Fatalf("ambiguous provider error = %v", err)
	}
}

func TestLMStudioConfigFromEnv(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
		bad  bool
	}{
		{"", 0, false}, {"  ", 0, false}, {"0", 0, false}, {" 300 ", 300, false},
		{"-1", 0, true}, {"abc", 0, true}, {"1.5", 0, true}, {"999999999999999999999999", 0, true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			for _, name := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "XAI_API_KEY"} {
				t.Setenv(name, "")
			}
			t.Setenv("LLM_PROVIDER", " lmstudio ")
			t.Setenv("LMSTUDIO_BASE_URL", "http://192.168.1.50:1234")
			t.Setenv("LMSTUDIO_MODEL", "local-model")
			t.Setenv("LMSTUDIO_API_KEY", "local-token")
			t.Setenv("LMSTUDIO_TTL_SECONDS", tc.raw)
			cfg := ConfigFromEnv()
			c, err := New(cfg)
			if tc.bad {
				if err == nil || !strings.Contains(err.Error(), "LMSTUDIO_TTL_SECONDS") {
					t.Fatalf("New: %v, want TTL configuration error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.LMStudio != (LMStudioConfig{
				BaseURL: "http://192.168.1.50:1234", APIKey: "local-token", Model: "local-model", TTLSeconds: tc.want,
			}) || c.def != LMStudio {
				t.Errorf("configuration = %+v, default = %q", cfg.LMStudio, c.def)
			}
		})
	}
}

func TestLMStudioRejectsInvalidConfig(t *testing.T) {
	for _, base := range []string{
		"localhost:1234", "ftp://localhost:1234", "http://", "http://localhost:bad",
		"http://user:password@localhost:1234", "http://localhost:1234/v1",
		"http://localhost:1234?key=value", "http://localhost:1234#fragment",
	} {
		t.Run(base, func(t *testing.T) {
			if _, err := New(Config{LMStudio: LMStudioConfig{BaseURL: base}}); err == nil || !strings.Contains(err.Error(), "BaseURL") {
				t.Fatalf("New: %v, want address error", err)
			}
		})
	}
	if _, err := New(Config{LMStudio: LMStudioConfig{BaseURL: "http://localhost:1234", TTLSeconds: -1}}); err == nil {
		t.Fatal("New accepted a negative TTL")
	}
	if _, err := New(Config{Default: LMStudio}); err == nil {
		t.Fatal("New accepted LM Studio without an address")
	}
	if _, err := New(Config{LMStudio: LMStudioConfig{BaseURL: "https://lmstudio.lan:1234"}}); err != nil {
		t.Fatalf("New rejected HTTPS: %v", err)
	}
}

func TestLMStudioStructuredFailures(t *testing.T) {
	for _, tc := range []struct {
		name, reply, want string
		status            int
	}{
		{"truncated", `{"choices":[{"finish_reason":"length","message":{"content":"{\"summary\":"}}]}`, "did not complete", 200},
		{"non-object", `{"choices":[{"finish_reason":"stop","message":{"content":"[]"}}]}`, "not a JSON object", 200},
		{"refusal", `{"choices":[{"message":{"refusal":"declined"}}]}`, "refused", 200},
		{"unsupported", `{"error":"schema unsupported"}`, `model "local-model" rejected`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.reply))
			}))
			defer srv.Close()
			c, err := New(Config{LMStudio: LMStudioConfig{BaseURL: srv.URL, Model: "local-model"}})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := c.Complete(context.Background(), schemaRequest(LMStudio))
			if err == nil || !strings.Contains(err.Error(), tc.want) || resp != (Response{}) || calls != 1 {
				t.Fatalf("response = %+v, error = %v, calls = %d", resp, err, calls)
			}
			if tc.status == 400 {
				var apiErr *Error
				if !errors.As(err, &apiErr) || apiErr.Status != 400 || apiErr.Provider != LMStudio ||
					!strings.Contains(err.Error(), "schema unsupported") {
					t.Errorf("provider error lost: %v", err)
				}
			}
		})
	}
}

func TestHostedRequestsIgnoreLMStudioConfig(t *testing.T) {
	for _, id := range []ProviderID{OpenAI, XAI, Anthropic} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", id, stream), func(t *testing.T) {
				var got map[string]any
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
						t.Error(err)
					}
					if id == Anthropic {
						if r.Header.Get("x-api-key") != "hosted-key" || r.Header.Get("Authorization") != "" || r.URL.Path != "/v1/messages" {
							t.Errorf("Anthropic request = %s, headers = %v", r.URL.Path, r.Header)
						}
					} else if r.Header.Get("Authorization") != "Bearer hosted-key" || r.URL.Path != "/v1/chat/completions" {
						t.Errorf("hosted request = %s, headers = %v", r.URL.Path, r.Header)
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						if id == Anthropic {
							_, _ = w.Write([]byte(anthropicEvent("content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"ok"}}`) +
								anthropicEvent("message_stop", `{"type":"message_stop"}`)))
						} else {
							for _, e := range openAIStream("ok") {
								_, _ = w.Write([]byte(e))
							}
						}
					} else if id == Anthropic {
						_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
					} else {
						_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
					}
				}))
				defer srv.Close()
				hosted := ProviderConfig{APIKey: "hosted-key", Model: "hosted-model"}
				c := testClient(t, Config{
					Default: id, OpenAI: hosted, XAI: hosted, Anthropic: hosted,
					LMStudio: LMStudioConfig{BaseURL: srv.URL, APIKey: "local-key", TTLSeconds: 300},
				}, srv)
				req := Request{Messages: []Message{{Role: User, Content: "hi"}}, MaxTokens: 64}
				if stream {
					mustStream(t, c, req)
				} else {
					mustComplete(t, c, req)
				}
				tokenKey := "max_completion_tokens"
				if id == Anthropic {
					tokenKey = "max_tokens"
				}
				want := map[string]any{
					"model": "hosted-model", tokenKey: float64(64),
					"messages": []any{map[string]any{"role": "user", "content": "hi"}},
				}
				if stream {
					want["stream"] = true
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("body = %#v, want unchanged hosted body %#v", got, want)
				}
			})
		}
	}
}
