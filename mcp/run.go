package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// cliTimeout bounds the in-memory list/call subcommands so a bug fails fast
// instead of hanging a shell.
const cliTimeout = 30 * time.Second

// Run is the binary's entry point. It dispatches on args (typically os.Args[1:]):
//
//   - "" / "serve": serve MCP over stdio (blocks until the client disconnects or
//     the process is signaled). stdout is the JSON-RPC channel here, so this mode
//     writes nothing but protocol to stdout.
//   - "list":       list registered tools (add --json for machine output).
//   - "call NAME":  invoke a tool; pass arguments with --input '<json>'. Exits
//     nonzero if the tool reports an error.
//   - "help":       usage.
//
// All human-facing logging goes to stderr so it never corrupts the stdio
// protocol stream in serve mode.
func (s *Server) Run(ctx context.Context, args []string) error {
	return s.run(ctx, args, os.Stdout, os.Stderr)
}

// run is the testable core of Run with explicit output streams. In serve mode
// the SDK owns the real stdin/stdout; stdout here is only used by list/call.
func (s *Server) run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}

	switch cmd {
	case "", "serve":
		return s.serve(ctx, stderr)
	case "list":
		return s.list(ctx, args[1:], stdout)
	case "call":
		return s.call(ctx, args[1:], stdout)
	case "help", "-h", "--help":
		s.usage(stderr)
		return nil
	default:
		s.usage(stderr)
		return fmt.Errorf("mcp: unknown command %q", cmd)
	}
}

func (s *Server) serve(ctx context.Context, stderr io.Writer) error {
	// Mirror the app server: a desktop client killing the process (or Ctrl-C)
	// cancels the context so the session closes cleanly.
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(stderr, "%s %s: serving MCP over stdio\n", s.name, s.version)
	// A signal (or the parent cancelling) closes the session cleanly; that's a
	// normal shutdown, not a failure, so don't surface context.Canceled as an
	// error the caller would log.Fatal on.
	if err := s.sdk.Run(sigCtx, &sdkmcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func (s *Server) list(ctx context.Context, args []string, stdout io.Writer) error {
	asJSON := hasFlag(args, "--json")

	cctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()
	cs, cleanup, err := s.connectInMemory(cctx)
	if err != nil {
		return err
	}
	defer cleanup()

	// Follow the pagination cursor so every tool is listed even if the SDK
	// chunks the response. Start non-nil so `list --json` emits [] not null.
	tools := []*sdkmcp.Tool{}
	params := &sdkmcp.ListToolsParams{}
	for {
		res, err := cs.ListTools(cctx, params)
		if err != nil {
			return err
		}
		tools = append(tools, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		params.Cursor = res.NextCursor
	}

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(tools)
	}
	if len(tools) == 0 {
		fmt.Fprintln(stdout, "(no tools registered)")
		return nil
	}
	for _, t := range tools {
		if t.Description != "" {
			fmt.Fprintf(stdout, "%s\t%s\n", t.Name, t.Description)
		} else {
			fmt.Fprintln(stdout, t.Name)
		}
	}
	return nil
}

func (s *Server) call(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] == "" {
		return fmt.Errorf("mcp: call requires a tool name")
	}
	name := args[0]
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("mcp: call requires a tool name before any flags")
	}

	// Parse the remaining args strictly. The only accepted flag is
	// "--input <json>". Silently ignoring an unrecognized token (e.g. a typo
	// like "--inpt") would let a mutating tool run with zero/default arguments
	// instead of failing, so reject anything we don't recognize.
	var (
		arguments any
		sawInput  bool
	)
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--input":
			if sawInput {
				return fmt.Errorf("mcp: --input specified more than once")
			}
			if i+1 >= len(rest) {
				return fmt.Errorf("mcp: --input requires a JSON value")
			}
			sawInput = true
			raw := rest[i+1]
			i++
			// Reject an empty value outright: omit --input entirely for a tool
			// that takes no arguments. Accepting "" here would re-introduce the
			// footgun this strict parsing exists to close - a mutating tool
			// running with defaults because a shell variable expanded to "".
			if raw == "" {
				return fmt.Errorf("mcp: --input requires non-empty JSON; omit --input for no arguments or pass '{}'")
			}
			// MCP tool arguments are a JSON object, so require object shape (this
			// also rejects null, arrays, and scalars with a clear message). Hand
			// the SDK the exact bytes rather than round-tripping through `any`
			// here, so the CLI path itself never coerces the JSON. (The SDK still
			// normalizes numbers during schema validation, so integers beyond
			// 2^53 can lose precision - an upstream limitation, not one this CLI
			// adds.)
			var obj map[string]json.RawMessage
			if err := json.Unmarshal([]byte(raw), &obj); err != nil || obj == nil {
				return fmt.Errorf("mcp: --input must be a JSON object")
			}
			arguments = json.RawMessage(raw)
		default:
			return fmt.Errorf("mcp: unexpected argument %q", rest[i])
		}
	}

	cctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()
	cs, cleanup, err := s.connectInMemory(cctx)
	if err != nil {
		return err
	}
	defer cleanup()

	res, err := cs.CallTool(cctx, &sdkmcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		return err
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		return err
	}
	// A tool-reported error is real output (already printed above), but the
	// command must still exit nonzero so a caller/script notices.
	if res.IsError {
		return fmt.Errorf("mcp: tool %q reported an error", name)
	}
	return nil
}

// connectInMemory wires an in-process client to this server over a paired
// transport. Order matters: the server must Connect before the client, because
// the client's Connect drives the initialize handshake.
func (s *Server) connectInMemory(ctx context.Context) (*sdkmcp.ClientSession, func(), error) {
	clientT, serverT := sdkmcp.NewInMemoryTransports()

	ss, err := s.sdk.Connect(ctx, serverT, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp: connect server: %w", err)
	}

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "cli", Version: s.version}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		_ = ss.Close()
		return nil, nil, fmt.Errorf("mcp: connect client: %w", err)
	}

	cleanup := func() {
		_ = cs.Close()
		_ = ss.Close()
	}
	return cs, cleanup, nil
}

func (s *Server) usage(w io.Writer) {
	fmt.Fprintf(w, `%s %s - MCP server

Usage:
  (no args) | serve      Serve MCP over stdio (for a desktop client)
  list [--json]          List registered tools
  call NAME [--input '<json>']
                         Call a tool with JSON arguments
  help                   Show this help
`, s.name, s.version)
}

// hasFlag reports whether flag appears anywhere in args.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}
