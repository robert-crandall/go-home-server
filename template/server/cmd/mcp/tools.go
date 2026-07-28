package main

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/robert-crandall-org/go-home-server/apiclient"
	"github.com/robert-crandall-org/go-home-server/mcp"
)

// apiClient lazily builds the authed HTTP client, once, from
// ~/.config/<app>.json (with MCP_APP_URL / MCP_APP_TOKEN as overrides).
// Building it lazily means `mcp help`/`list`/`serve` and tool registration all
// work with no config at all - only an actual tool call needs a token.
// OnceValues makes it safe for concurrent stdio tool calls to race on first use;
// it also means a config edit needs a restart of the MCP server to take effect.
var apiClient = sync.OnceValues(func() (*apiclient.Client, error) {
	return apiclient.FromConfig(mcp.AppName())
})

// note mirrors the app's notes.Note JSON shape. The MCP layer keeps its own tiny
// copy rather than importing the feature package, so deleting the feature and
// its tools stays a local edit.
type note struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

// Tool outputs must be object-shaped, so a list is wrapped in a struct.

type listNotesIn struct{}

type listNotesOut struct {
	Notes []note `json:"notes"`
}

type createNoteIn struct {
	Body string `json:"body" jsonschema:"the note text"`
}

type createNoteOut struct {
	Note note `json:"note"`
}

// registerNoteTools wires the sample tools. Delete this when you delete the
// notes feature.
func registerNoteTools(s *mcp.Server) {
	mcp.AddTool(s, "list_notes", "List your notes, newest first.",
		func(ctx context.Context, in listNotesIn) (listNotesOut, error) {
			c, err := apiClient()
			if err != nil {
				return listNotesOut{}, err
			}
			return listNotes(ctx, c, in)
		})

	mcp.AddTool(s, "create_note", "Add a note.",
		func(ctx context.Context, in createNoteIn) (createNoteOut, error) {
			c, err := apiClient()
			if err != nil {
				return createNoteOut{}, err
			}
			return createNote(ctx, c, in)
		})
}

// listNotes and createNote hold the tool logic independent of the lazy env
// client, so tests can drive them against an httptest server.

func listNotes(ctx context.Context, c *apiclient.Client, _ listNotesIn) (listNotesOut, error) {
	var notes []note
	if err := c.Do(ctx, http.MethodGet, "/api/notes", nil, &notes); err != nil {
		return listNotesOut{}, err
	}
	if notes == nil {
		notes = []note{}
	}
	return listNotesOut{Notes: notes}, nil
}

func createNote(ctx context.Context, c *apiclient.Client, in createNoteIn) (createNoteOut, error) {
	reqBody := map[string]string{"body": in.Body}
	var n note
	if err := c.Do(ctx, http.MethodPost, "/api/notes", reqBody, &n); err != nil {
		return createNoteOut{}, err
	}
	return createNoteOut{Note: n}, nil
}
