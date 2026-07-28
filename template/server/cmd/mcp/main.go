// Command mcp is the app's MCP server. Run it with no arguments (or "serve") to
// speak MCP over stdio to a desktop client like Claude Desktop; use the
// "list"/"call" subcommands to exercise tools from a shell.
//
// The tools here (list_notes, create_note) are samples over the sample notes
// feature - delete them when you start a real app and register your own. They
// show the shape: each tool is a thin client of the app's own HTTP API (so it
// reuses the app's auth, validation, and business logic) authed with a personal
// access token from POST /api/tokens.
//
// Install it with `make mcp-install`, which builds $HOME/bin/<app>-mcp. It reads
// ~/.config/<app>.json for the app's origin and token.
package main

import (
	"context"
	"log"
	"os"

	"github.com/robert-crandall/go-home-server/mcp"
)

// version is stamped into the MCP initialize handshake; override at build time
// with -ldflags "-X main.version=..." (which `make mcp-install` does).
var version = "dev"

func main() {
	s := mcp.New(mcp.AppName()+"-mcp", version)
	registerNoteTools(s)

	if err := s.Run(context.Background(), os.Args[1:]); err != nil {
		log.Fatalf("mcp: %v", err)
	}
}
