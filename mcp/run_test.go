package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type greetIn struct {
	Name string `json:"name" jsonschema:"who to greet"`
}

type greetOut struct {
	Message string `json:"message"`
}

func newTestServer() *Server {
	s := New("test-app", "0.0.1")
	AddTool(s, "greet", "Greet someone by name.", func(_ context.Context, in greetIn) (greetOut, error) {
		return greetOut{Message: "hello " + in.Name}, nil
	})
	AddTool(s, "boom", "Always fails.", func(_ context.Context, _ greetIn) (greetOut, error) {
		return greetOut{}, errAlways
	})
	AddTool(s, "ping", "Takes no arguments.", func(_ context.Context, _ struct{}) (greetOut, error) {
		return greetOut{Message: "pong"}, nil
	})
	return s
}

var errAlways = &staticErr{"kaboom"}

type staticErr struct{ s string }

func (e *staticErr) Error() string { return e.s }

func TestListPrintsRegisteredTools(t *testing.T) {
	s := newTestServer()
	var out bytes.Buffer
	if err := s.run(context.Background(), []string{"list"}, &out, &out); err != nil {
		t.Fatalf("run list: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "greet") || !strings.Contains(got, "boom") {
		t.Errorf("list output missing tools:\n%s", got)
	}
	if !strings.Contains(got, "Greet someone by name.") {
		t.Errorf("list output missing description:\n%s", got)
	}
}

func TestListJSON(t *testing.T) {
	s := newTestServer()
	var out bytes.Buffer
	if err := s.run(context.Background(), []string{"list", "--json"}, &out, &out); err != nil {
		t.Fatalf("run list --json: %v", err)
	}
	var tools []map[string]any
	if err := json.Unmarshal(out.Bytes(), &tools); err != nil {
		t.Fatalf("list --json not valid JSON: %v\n%s", err, out.String())
	}
	if len(tools) != 3 {
		t.Errorf("got %d tools, want 3", len(tools))
	}
}

func TestListJSONEmptyIsArrayNotNull(t *testing.T) {
	s := New("empty-app", "0.0.1") // no tools registered
	var out bytes.Buffer
	if err := s.run(context.Background(), []string{"list", "--json"}, &out, &out); err != nil {
		t.Fatalf("run list --json: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "[]" {
		t.Errorf("list --json with no tools = %q, want %q (must not be null)", got, "[]")
	}
}

func TestCallSucceeds(t *testing.T) {
	s := newTestServer()
	var out bytes.Buffer
	err := s.run(context.Background(), []string{"call", "greet", "--input", `{"name":"sam"}`}, &out, &out)
	if err != nil {
		t.Fatalf("run call: %v", err)
	}
	if !strings.Contains(out.String(), "hello sam") {
		t.Errorf("call output missing result:\n%s", out.String())
	}
}

func TestCallToolErrorExitsNonzero(t *testing.T) {
	s := newTestServer()
	var out bytes.Buffer
	err := s.run(context.Background(), []string{"call", "boom", "--input", `{"name":"x"}`}, &out, &out)
	if err == nil {
		t.Fatal("expected a nonzero exit (error) when the tool reports IsError")
	}
	// The result JSON (with the error content) should still have been printed.
	if !strings.Contains(out.String(), "kaboom") {
		t.Errorf("call output should include the tool error content:\n%s", out.String())
	}
}

func TestCallMissingToolName(t *testing.T) {
	s := newTestServer()
	var out bytes.Buffer
	if err := s.run(context.Background(), []string{"call"}, &out, &out); err == nil {
		t.Fatal("expected error when no tool name is given")
	}
}

func TestCallInvalidInputJSON(t *testing.T) {
	s := newTestServer()
	var out bytes.Buffer
	err := s.run(context.Background(), []string{"call", "greet", "--input", "{not json"}, &out, &out)
	if err == nil {
		t.Fatal("expected error for invalid --input JSON")
	}
}

func TestCallRejectsUnknownFlag(t *testing.T) {
	s := newTestServer()
	var out bytes.Buffer
	// A typo like "--inpt" must fail loudly, not silently run greet with no
	// arguments - otherwise a mutating tool could execute with zero/defaults.
	err := s.run(context.Background(), []string{"call", "greet", "--inpt", `{"name":"sam"}`}, &out, &out)
	if err == nil {
		t.Fatal("expected error for an unrecognized flag")
	}
}

func TestCallRejectsExtraArgs(t *testing.T) {
	s := newTestServer()
	var out bytes.Buffer
	err := s.run(context.Background(), []string{"call", "greet", "extra"}, &out, &out)
	if err == nil {
		t.Fatal("expected error for an unexpected positional argument")
	}
}

func TestCallNoInputMeansNoArgs(t *testing.T) {
	s := newTestServer()
	var out bytes.Buffer
	// Omitting --input entirely is the "send no arguments" path and must work
	// for a tool that takes no arguments.
	if err := s.run(context.Background(), []string{"call", "ping"}, &out, &out); err != nil {
		t.Fatalf("run call ping: %v", err)
	}
	if !strings.Contains(out.String(), "pong") {
		t.Errorf("call output missing result:\n%s", out.String())
	}
}

func TestCallRejectsEmptyInput(t *testing.T) {
	s := newTestServer()
	var out bytes.Buffer
	// An explicit empty --input value must fail rather than silently sending no
	// arguments (a shell var expanding to "" should not run a tool with defaults).
	if err := s.run(context.Background(), []string{"call", "ping", "--input", ""}, &out, &out); err == nil {
		t.Fatal("expected error for an empty --input value")
	}
}

func TestCallRejectsNonObjectInput(t *testing.T) {
	s := newTestServer()
	for _, raw := range []string{"5", "true", "null", `"hi"`, "[1,2]"} {
		var out bytes.Buffer
		if err := s.run(context.Background(), []string{"call", "greet", "--input", raw}, &out, &out); err == nil {
			t.Errorf("expected error for non-object --input %q", raw)
		}
	}
}

func TestCallRejectsFlagAsToolName(t *testing.T) {
	s := newTestServer()
	var out bytes.Buffer
	// A flag in the tool-name position must not be treated as a tool name.
	if err := s.run(context.Background(), []string{"call", "--input", `{"name":"x"}`}, &out, &out); err == nil {
		t.Fatal("expected error when a flag appears where the tool name should be")
	}
}

func TestUnknownCommand(t *testing.T) {
	s := newTestServer()
	var out bytes.Buffer
	if err := s.run(context.Background(), []string{"frobnicate"}, &out, &out); err == nil {
		t.Fatal("expected error for unknown command")
	}
}

func TestHelp(t *testing.T) {
	s := newTestServer()
	var out bytes.Buffer
	if err := s.run(context.Background(), []string{"help"}, &out, &out); err != nil {
		t.Fatalf("run help: %v", err)
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("help missing usage:\n%s", out.String())
	}
}
