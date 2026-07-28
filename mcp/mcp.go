// Package mcp is a thin, dual-mode harness for building an app's MCP server on
// this foundation. It wraps the official Go SDK
// (github.com/modelcontextprotocol/go-sdk) so app code declares tools with a
// plain Go handler and never imports the SDK directly, and it gives every app
// the same binary shape: run it with no arguments (or "serve") to speak MCP over
// stdio to a desktop client, or use the "list"/"call" subcommands to exercise
// tools from a shell without a client.
//
// A tool is just a typed function. Input and output are Go structs; the SDK
// infers the JSON schema from the input struct's `jsonschema:"..."` tags and
// validates arguments before the handler runs. Tool outputs must be object
// shaped (a struct), so wrap slices in a struct rather than returning a bare
// slice.
//
// For the rare tool that needs something the simple Handler shape can't express
// - progress notifications, non-JSON content, explicit tool-vs-protocol errors,
// resources or prompts - drop to the raw SDK server via SDK().
package mcp

import (
	"context"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Server is an MCP server for one app. Construct it with New, register tools
// with AddTool, then run it with Run.
type Server struct {
	sdk     *sdkmcp.Server
	name    string
	version string
}

// New creates a server identified by name and version (surfaced to clients in
// the MCP initialize handshake).
func New(name, version string) *Server {
	return &Server{
		sdk:     sdkmcp.NewServer(&sdkmcp.Implementation{Name: name, Version: version}, nil),
		name:    name,
		version: version,
	}
}

// Handler is the shape an app implements for a tool: take a typed, already
// validated input and return a typed output (or an error). A returned error is
// reported to the client as a tool error (CallToolResult.IsError), not a
// protocol error, so the model can see it and self-correct.
type Handler[In, Out any] func(ctx context.Context, in In) (Out, error)

// AddTool registers a tool. In's `jsonschema:"..."` struct tags become the
// tool's input schema (and are validated automatically); Out must be an object
// (a struct) so the SDK can describe the structured result.
func AddTool[In, Out any](s *Server, name, description string, h Handler[In, Out]) {
	sdkmcp.AddTool(s.sdk, &sdkmcp.Tool{Name: name, Description: description},
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in In) (*sdkmcp.CallToolResult, Out, error) {
			out, err := h(ctx, in)
			if err != nil {
				var zero Out
				return nil, zero, err
			}
			return nil, out, nil
		})
}

// SDK returns the underlying SDK server for advanced needs not covered by the
// simple Handler shape. Prefer AddTool; reach for this only when you must.
func (s *Server) SDK() *sdkmcp.Server { return s.sdk }
