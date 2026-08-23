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
)

// journalSchema is the shape the calling app actually needs: an array of
// objects, integer ids, an enum, and nesting two levels deep. If the validated
// subset ever stops accepting this, the subset is wrong.
const journalSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["summary", "entries"],
  "properties": {
    "summary": {"type": "string", "description": "one sentence"},
    "entries": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["id", "mood", "tags", "score"],
        "properties": {
          "id": {"type": "integer"},
          "mood": {"type": "string", "enum": ["good", "bad"]},
          "score": {"type": "number"},
          "tags": {
            "type": "object",
            "additionalProperties": false,
            "required": ["names", "pinned"],
            "properties": {
              "names": {"type": "array", "items": {"type": "string"}},
              "pinned": {"type": "boolean"}
            }
          }
        }
      }
    }
  }
}`

func testSchema() *Schema {
	return &Schema{
		Name:        "journal_synopsis",
		Description: "a synopsis of one entry",
		JSON:        json.RawMessage(journalSchema),
	}
}

func schemaRequest(p ProviderID) Request {
	return Request{
		Provider:  p,
		Messages:  []Message{{Role: User, Content: "summarize"}},
		MaxTokens: 1024,
		Schema:    testSchema(),
	}
}

// captureBody serves one canned reply and records the request body as raw
// generic JSON, so the assertions are against literal wire keys - decoding into
// the request structs would let a typo'd json tag be mirrored by the test and
// pass anyway.
func captureBody(t *testing.T, reply string) (*httptest.Server, *map[string]any) {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// --- request bodies -------------------------------------------------------

func TestAnthropicSendsTheSchemaAsAForcedToolCall(t *testing.T) {
	srv, got := captureBody(t, `{"model":"claude-test","stop_reason":"tool_use",
		"content":[{"type":"tool_use","id":"tu_1","name":"journal_synopsis","input":{"summary":"ok","entries":[]}}]}`)

	c := testClient(t, Config{Anthropic: ProviderConfig{APIKey: "sk-ant", Model: "claude-test"}}, srv)
	resp := mustComplete(t, c, schemaRequest(Anthropic))

	tools, _ := (*got)["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want exactly one", (*got)["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "journal_synopsis" {
		t.Errorf("tools[0].name = %v", tool["name"])
	}
	if tool["description"] != "a synopsis of one entry" {
		t.Errorf("tools[0].description = %v", tool["description"])
	}
	if tool["strict"] != true {
		t.Errorf("tools[0].strict = %v, want true", tool["strict"])
	}
	// The schema has to survive into the body; a test that only checks the
	// parsed reply would pass with the schema silently dropped.
	schema, _ := tool["input_schema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["entries"]; !ok {
		t.Errorf("input_schema did not carry the schema: %v", tool["input_schema"])
	}

	choice, _ := (*got)["tool_choice"].(map[string]any)
	if choice["type"] != "tool" || choice["name"] != "journal_synopsis" {
		t.Errorf("tool_choice = %v", (*got)["tool_choice"])
	}
	if choice["disable_parallel_tool_use"] != true {
		t.Errorf("tool_choice.disable_parallel_tool_use = %v, want true", choice["disable_parallel_tool_use"])
	}

	// The tool input comes back as Text, not through a second field - which is
	// what keeps a stubbed completer and a Text-logging decorator working.
	var out struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(resp.Text), &out); err != nil {
		t.Fatalf("Text is not JSON: %v (%q)", err, resp.Text)
	}
	if out.Summary != "ok" {
		t.Errorf("Text = %q", resp.Text)
	}
}

func TestOpenAICompatibleSendsTheSchemaAsResponseFormat(t *testing.T) {
	for _, tc := range []struct {
		id  ProviderID
		cfg Config
	}{
		{OpenAI, Config{OpenAI: ProviderConfig{APIKey: "sk-oai", Model: "gpt-test"}}},
		{XAI, Config{XAI: ProviderConfig{APIKey: "xai-key", Model: "grok-test"}}},
	} {
		t.Run(string(tc.id), func(t *testing.T) {
			srv, got := captureBody(t, `{"model":"m","choices":[{"finish_reason":"stop","message":{"content":"{\"summary\":\"ok\",\"entries\":[]}"}}]}`)
			c := testClient(t, tc.cfg, srv)
			resp := mustComplete(t, c, schemaRequest(tc.id))

			rf, _ := (*got)["response_format"].(map[string]any)
			if rf["type"] != "json_schema" {
				t.Fatalf("response_format = %v", (*got)["response_format"])
			}
			js, _ := rf["json_schema"].(map[string]any)
			if js["name"] != "journal_synopsis" {
				t.Errorf("json_schema.name = %v", js["name"])
			}
			if js["description"] != "a synopsis of one entry" {
				t.Errorf("json_schema.description = %v", js["description"])
			}
			if js["strict"] != true {
				t.Errorf("json_schema.strict = %v, want true", js["strict"])
			}
			schema, _ := js["schema"].(map[string]any)
			props, _ := schema["properties"].(map[string]any)
			if _, ok := props["entries"]; !ok {
				t.Errorf("json_schema.schema did not carry the schema: %v", js["schema"])
			}
			if resp.Text != `{"summary":"ok","entries":[]}` {
				t.Errorf("Text = %q", resp.Text)
			}
		})
	}
}

// A request without a Schema must be byte-for-byte the one this package sent
// before schemas existed.
func TestRequestsWithoutASchemaCarryNoSchemaFields(t *testing.T) {
	for _, tc := range []struct {
		id    ProviderID
		cfg   Config
		reply string
		keys  []string
	}{
		{
			Anthropic,
			Config{Anthropic: ProviderConfig{APIKey: "sk-ant", Model: "claude-test"}},
			`{"content":[{"type":"text","text":"hi"}]}`,
			[]string{"tools", "tool_choice"},
		},
		{
			OpenAI,
			Config{OpenAI: ProviderConfig{APIKey: "sk-oai", Model: "gpt-test"}},
			`{"choices":[{"message":{"content":"hi"}}]}`,
			[]string{"response_format"},
		},
	} {
		t.Run(string(tc.id), func(t *testing.T) {
			srv, got := captureBody(t, tc.reply)
			c := testClient(t, tc.cfg, srv)
			mustComplete(t, c, Request{
				Provider:  tc.id,
				Messages:  []Message{{Role: User, Content: "hi"}},
				MaxTokens: 64,
			})
			for _, k := range tc.keys {
				if _, ok := (*got)[k]; ok {
					t.Errorf("body carries %q for a request with no Schema: %v", k, *got)
				}
			}
		})
	}
}

// --- Anthropic response handling ------------------------------------------

// completeSchema runs one schema-constrained Anthropic call against a canned
// reply and returns whatever came back.
func completeSchema(t *testing.T, id ProviderID, cfg Config, reply string) (Response, error) {
	t.Helper()
	srv, _ := captureBody(t, reply)
	c := testClient(t, cfg, srv)
	return c.Complete(context.Background(), schemaRequest(id))
}

var anthropicCfg = Config{Anthropic: ProviderConfig{APIKey: "sk-ant", Model: "claude-test"}}
var openAICfg = Config{OpenAI: ProviderConfig{APIKey: "sk-oai", Model: "gpt-test"}}

// The measured failure: the model stops mid-object and the turn ends with an
// ordinary-looking reason. Every reason but tool_use has to be an error, and an
// unfamiliar one has to be an error too - a silent partial object is exactly
// what this mechanism exists to remove.
func TestAnthropicSchemaRejectsEveryStopReasonButToolUse(t *testing.T) {
	for _, reason := range []string{"end_turn", "max_tokens", "stop_sequence", "pause_turn", "model_context_window_exceeded", "something_new", ""} {
		t.Run(reason, func(t *testing.T) {
			reply := fmt.Sprintf(`{"stop_reason":%q,"content":[{"type":"tool_use","name":"journal_synopsis","input":{"summary":"partial"}}]}`, reason)
			_, err := completeSchema(t, Anthropic, anthropicCfg, reply)
			if err == nil {
				t.Fatal("Complete succeeded on an unfinished structured response")
			}
			if !strings.Contains(err.Error(), "did not complete") {
				t.Errorf("error = %v", err)
			}
		})
	}
}

func TestAnthropicSchemaRejectsZeroMatchingToolBlocks(t *testing.T) {
	for name, reply := range map[string]string{
		// tool_choice should make both impossible. "Should" is not "did".
		"answered with text": `{"stop_reason":"tool_use","content":[{"type":"text","text":"{\"summary\":\"ok\"}"}]}`,
		"different tool":     `{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"other","input":{"summary":"ok"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := completeSchema(t, Anthropic, anthropicCfg, reply)
			if err == nil || !strings.Contains(err.Error(), "no \"tool_use\" block") {
				t.Fatalf("error = %v, want a missing-tool_use error", err)
			}
		})
	}
}

// Picking the first would silently discard the rest, which is the same class of
// bug as accepting a truncated object.
func TestAnthropicSchemaRejectsMultipleMatchingToolBlocks(t *testing.T) {
	reply := `{"stop_reason":"tool_use","content":[
		{"type":"tool_use","name":"journal_synopsis","input":{"summary":"first"}},
		{"type":"tool_use","name":"journal_synopsis","input":{"summary":"second"}}]}`
	_, err := completeSchema(t, Anthropic, anthropicCfg, reply)
	if err == nil || !strings.Contains(err.Error(), "want exactly one") {
		t.Fatalf("error = %v, want a too-many-blocks error", err)
	}
}

// JSON null unmarshals into a Go struct without error and leaves every field
// zero, so a null input accepted as success would be silent data loss.
func TestAnthropicSchemaRejectsANonObjectToolInput(t *testing.T) {
	for _, input := range []string{`null`, `"text"`, `[1,2]`} {
		t.Run(input, func(t *testing.T) {
			reply := fmt.Sprintf(`{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"journal_synopsis","input":%s}]}`, input)
			_, err := completeSchema(t, Anthropic, anthropicCfg, reply)
			if err == nil || !strings.Contains(err.Error(), "not a JSON object") {
				t.Fatalf("error = %v, want a non-object error", err)
			}
		})
	}
}

// A refusal is still reported as a refusal, not as a shape problem.
func TestAnthropicSchemaRefusalKeepsItsOwnError(t *testing.T) {
	_, err := completeSchema(t, Anthropic, anthropicCfg, `{"stop_reason":"refusal","content":[]}`)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("error = %v, want a refusal error", err)
	}
}

// --- OpenAI-compatible response handling ----------------------------------

func TestOpenAISchemaRejectsEveryFinishReasonButStop(t *testing.T) {
	for _, reason := range []string{"length", "tool_calls", "function_call", "something_new", ""} {
		t.Run(reason, func(t *testing.T) {
			reply := fmt.Sprintf(`{"choices":[{"finish_reason":%q,"message":{"content":"{\"summary\":\"partial"}}]}`, reason)
			_, err := completeSchema(t, OpenAI, openAICfg, reply)
			if err == nil || !strings.Contains(err.Error(), "did not complete") {
				t.Fatalf("error = %v, want an unfinished-response error", err)
			}
		})
	}
}

// content_filter is also "not stop", but it gets the more specific error.
func TestOpenAISchemaContentFilterKeepsItsOwnError(t *testing.T) {
	_, err := completeSchema(t, OpenAI, openAICfg, `{"choices":[{"finish_reason":"content_filter","message":{"content":""}}]}`)
	if err == nil || !strings.Contains(err.Error(), "content filter") {
		t.Fatalf("error = %v, want a content-filter error", err)
	}
}

func TestOpenAISchemaRejectsNonObjectContent(t *testing.T) {
	for _, content := range []string{`null`, `[1,2]`, `not json at all`} {
		t.Run(content, func(t *testing.T) {
			body, _ := json.Marshal(content)
			reply := fmt.Sprintf(`{"choices":[{"finish_reason":"stop","message":{"content":%s}}]}`, body)
			_, err := completeSchema(t, OpenAI, openAICfg, reply)
			if err == nil || !strings.Contains(err.Error(), "not a JSON object") {
				t.Fatalf("error = %v, want a non-object error", err)
			}
		})
	}
}

// --- Stream ---------------------------------------------------------------

// Anthropic streams tool input as input_json_delta, not text_delta, so a stream
// that accepted a Schema would call onText zero times and hand back nothing.
func TestStreamRejectsASchemaBeforeAnyHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("Stream made an HTTP request for a schema-constrained request")
	}))
	defer srv.Close()

	c := testClient(t, anthropicCfg, srv)
	calls := 0
	_, err := c.Stream(context.Background(), schemaRequest(Anthropic), func(string) error {
		calls++
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "does not support Request.Schema") {
		t.Fatalf("error = %v, want a Schema rejection", err)
	}
	if calls != 0 {
		t.Errorf("onText called %d times", calls)
	}
}

// --- schema validation ----------------------------------------------------

func TestSchemaValidationAcceptsThePortableSubset(t *testing.T) {
	for name, schema := range map[string]string{
		"the app's real shape": journalSchema,
		"integer enum":         `{"type":"object","additionalProperties":false,"required":["n"],"properties":{"n":{"type":"integer","enum":[1,2,3]}}}`,
		"array of arrays":      `{"type":"object","additionalProperties":false,"required":["g"],"properties":{"g":{"type":"array","items":{"type":"array","items":{"type":"number"}}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			err := validateSchema(&Schema{Name: "ok_name", JSON: json.RawMessage(schema)})
			if err != nil {
				t.Fatalf("validateSchema: %v", err)
			}
		})
	}
}

func TestSchemaValidationRejectsWhatTheProvidersDisagreeAbout(t *testing.T) {
	const obj = `{"type":"object","additionalProperties":false,"required":["a"],"properties":{"a":%s}}`
	for name, tc := range map[string]struct {
		schema string
		want   string
	}{
		// OpenAI and xAI both reject a non-object root outright.
		"array root":  {`{"type":"array","items":{"type":"string"}}`, `root "type"`},
		"scalar root": {`{"type":"string"}`, `root "type"`},
		"not JSON":    {`nope`, "must be a JSON object"},
		"JSON null":   {`null`, `root "type"`},

		// OpenAI's strict mode rejects these; Anthropic's input_schema accepts
		// them and just doesn't enforce them. Same Request, three meanings.
		"missing additionalProperties": {`{"type":"object","required":["a"],"properties":{"a":{"type":"string"}}}`, "additionalProperties"},
		"additionalProperties true":    {`{"type":"object","additionalProperties":true,"required":["a"],"properties":{"a":{"type":"string"}}}`, "must set"},
		"optional property":            {`{"type":"object","additionalProperties":false,"required":[],"properties":{"a":{"type":"string"}}}`, "every property must be required"},
		"missing required":             {`{"type":"object","additionalProperties":false,"properties":{"a":{"type":"string"}}}`, `missing "required"`},
		"required names a ghost":       {`{"type":"object","additionalProperties":false,"required":["a","b"],"properties":{"a":{"type":"string"}}}`, "does not define as a property"},
		"required lists a name twice":  {`{"type":"object","additionalProperties":false,"required":["a","a"],"properties":{"a":{"type":"string"}}}`, "twice"},
		"missing properties":           {`{"type":"object","additionalProperties":false,"required":[]}`, `missing "properties"`},
		"no properties":                {`{"type":"object","additionalProperties":false,"required":[],"properties":{}}`, "no properties"},

		// Bounds are enforced by OpenAI, enforced up to a limit by xAI, and
		// silently ignored by Anthropic's tool input_schema.
		"minLength": {fmt.Sprintf(obj, `{"type":"string","minLength":3}`), `"minLength"`},
		"maxItems":  {fmt.Sprintf(obj, `{"type":"array","items":{"type":"string"},"maxItems":2}`), `"maxItems"`},
		"minimum":   {fmt.Sprintf(obj, `{"type":"integer","minimum":0}`), `"minimum"`},
		"format":    {fmt.Sprintf(obj, `{"type":"string","format":"email"}`), `"format"`},

		// Not in the checked subset. Loud beats "works on two of three".
		"anyOf":       {fmt.Sprintf(obj, `{"anyOf":[{"type":"string"}]}`), `missing "type"`},
		"union type":  {fmt.Sprintf(obj, `{"type":["string","null"]}`), "non-string"},
		"null type":   {fmt.Sprintf(obj, `{"type":"null"}`), "unsupported type"},
		"$ref":        {fmt.Sprintf(obj, `{"$ref":"#/$defs/x"}`), `missing "type"`},
		"no type":     {fmt.Sprintf(obj, `{"description":"hi"}`), `missing "type"`},
		"array items": {fmt.Sprintf(obj, `{"type":"array"}`), `no "items"`},

		// Enums: xAI 400s on a zero-variant enum, and a value that can't match
		// the declared type is a schema two providers reject outright.
		"empty enum":       {fmt.Sprintf(obj, `{"type":"string","enum":[]}`), `empty "enum"`},
		"mistyped enum":    {fmt.Sprintf(obj, `{"type":"string","enum":["a",2]}`), "not string"},
		"fractional enum":  {fmt.Sprintf(obj, `{"type":"integer","enum":[1.5]}`), "not integer"},
		"enum on a number": {fmt.Sprintf(obj, `{"type":"number","enum":[1]}`), `"enum"`},

		// JSON null unmarshals into anything without error and leaves the zero
		// value, so these two would otherwise read as a valid enum variant and
		// as additionalProperties: false.
		"null enum variant":         {fmt.Sprintf(obj, `{"type":"string","enum":["a",null]}`), "not string"},
		"null additionalProperties": {`{"type":"object","additionalProperties":null,"required":["a"],"properties":{"a":{"type":"string"}}}`, "must set"},

		// The recursion has to reach all the way down, not just the root.
		"bad node two deep": {
			`{"type":"object","additionalProperties":false,"required":["a"],"properties":{"a":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["b"],"properties":{"b":{"type":"string","pattern":"x"}}}}}}`,
			`"pattern"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateSchema(&Schema{Name: "ok_name", JSON: json.RawMessage(tc.schema)})
			if err == nil {
				t.Fatalf("validateSchema accepted %s", tc.schema)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// The error has to say which node is wrong; a schema nested two deep is
// otherwise a hunt.
func TestSchemaValidationNamesTheOffendingNode(t *testing.T) {
	bad := `{"type":"object","additionalProperties":false,"required":["a"],"properties":{"a":{"type":"array","items":{"type":"widget"}}}}`
	err := validateSchema(&Schema{Name: "ok_name", JSON: json.RawMessage(bad)})
	if err == nil || !strings.Contains(err.Error(), "#/properties/a/items") {
		t.Fatalf("error = %v, want it to name the node", err)
	}
}

// Both providers document the same name syntax and 64-byte cap, so one rule
// catches it locally instead of costing a round trip.
func TestSchemaNameIsValidated(t *testing.T) {
	for name, want := range map[string]bool{
		"journal_synopsis":      true,
		"a-b-9":                 true,
		"":                      false,
		"has space":             false,
		"has.dot":               false,
		strings.Repeat("x", 64): true,
		strings.Repeat("x", 65): false,
	} {
		err := validateSchema(&Schema{Name: name, JSON: json.RawMessage(journalSchema)})
		if got := err == nil; got != want {
			t.Errorf("Schema.Name %q: accepted = %v, want %v (%v)", name, got, want, err)
		}
	}
}

func TestSchemaJSONIsRequired(t *testing.T) {
	err := validateSchema(&Schema{Name: "ok_name"})
	if err == nil || !strings.Contains(err.Error(), "Schema.JSON is required") {
		t.Fatalf("error = %v", err)
	}
}

// Like every other rule in validate, a bad schema fails identically on every
// provider because it never reaches one.
func TestBadSchemaIsRejectedBeforeAnyHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("Complete made an HTTP request for an invalid schema")
	}))
	defer srv.Close()

	c := testClient(t, anthropicCfg, srv)
	req := schemaRequest(Anthropic)
	req.Schema.JSON = json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`)
	if _, err := c.Complete(context.Background(), req); err == nil {
		t.Fatal("Complete accepted an invalid schema")
	}
}

// --- provider rejection ---------------------------------------------------

// A 400 here means either "this model can't do structured output" or "this
// provider won't take this schema", and only the provider's own message can
// say which. So the error names the model and forwards the reason rather than
// claiming either - and never quietly retries without the schema.
func TestSchemaRejectionNamesTheModelAndKeepsTheProvidersReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"tools are not supported for this model"}}`))
	}))
	defer srv.Close()

	c := testClient(t, anthropicCfg, srv)
	_, err := c.Complete(context.Background(), schemaRequest(Anthropic))
	if err == nil {
		t.Fatal("Complete succeeded against a 400")
	}
	for _, want := range []string{`"claude-test"`, "journal_synopsis", "tools are not supported for this model"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
	// Status is the retry seam and wrapping must not hide it.
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Errorf("errors.As did not reach the *Error: %v", err)
	}
}

// A rate limit has nothing to do with the schema, so it's left alone.
func TestNonSchemaFailuresAreNotRelabeled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer srv.Close()

	c := testClient(t, anthropicCfg, srv)
	_, err := c.Complete(context.Background(), schemaRequest(Anthropic))
	if err == nil || strings.Contains(err.Error(), "rejected the request") {
		t.Fatalf("error = %v, want the plain *Error", err)
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests {
		t.Fatalf("errors.As did not reach the *Error: %v", err)
	}
}
