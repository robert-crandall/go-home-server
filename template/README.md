# Example App

A reference app built on the [go-home-server](https://github.com/robert-crandall/go-home-server)
foundation. This is what `scripts/new-app.sh` copies to start a new app.

## Layout

```
.
├── server/          # Go API: imports the foundation, adds app features
│   ├── cmd/         # server, migrate, openapi, vapid
│   └── internal/    # notes (sample feature), app wiring, embedded SPA
├── web/             # Svelte 5 + Vite + Tailwind v4 + DaisyUI + Lucide + PWA
├── Dockerfile       # multi-stage: web -> server -> distroless
├── docker-compose.yml
└── .env.example
```

## Run locally

Requires Go 1.26, Docker (for Postgres), and [bun](https://bun.sh) for the web
build (`make web-*` targets shell out to `bun`).

```bash
cp .env.example .env          # edit if needed
make dev-db                   # start Postgres
make run                      # API + embedded SPA on :8080 (migrates on boot)

# In another terminal, the frontend with hot reload:
make web-dev                  # http://localhost:5173 (proxies /api to :8080)
```

Registration is **first-user-only** by default: the first account you create
becomes your single user, then registration closes. Set
`ALLOW_OPEN_REGISTRATION=true` for a multi-user app.

Web push is optional. Generate keys once and put them in `.env`:

```bash
make vapid
```

## The API-first loop

The OpenAPI spec is the contract between server and client.

```bash
make gen-api   # regenerate openapi.json from Go, then schema.d.ts from the spec
```

Commit `server/openapi.json` and `web/src/lib/api/schema.d.ts` with your code.

## Icons

[Lucide](https://lucide.dev) is the icon set, via `@lucide/svelte`. One set on
purpose - every app built on this foundation should look like a sibling, and a
single coherent set is what gets you that. Import by name and pass a `size`:

```svelte
<script lang="ts">
  import { House, Trash2 } from '@lucide/svelte';
</script>

<House size={20} />
```

Icons render with `currentColor` and are `aria-hidden` unless you give them a
label, so they inherit DaisyUI button/navbar colours and stay out of the
accessibility tree. For an icon-only button, put the label on the button
(`aria-label="Delete note"`) - see `web/src/routes/Notes.svelte`.

Icons cost horizontal space, which is scarce in a phone-width navbar. `Nav.svelte`
keeps the icons at every width and drops the button *labels* below `sm` with
`sr-only sm:not-sr-only` - visually hidden, still announced. Use `sr-only`, not
`hidden`: `display: none` would strip the only accessible name those buttons have.

## Build

```bash
make build     # builds the SPA, embeds it, compiles ./bin/app
make docker    # production image
```

## MCP server

`server/cmd/mcp` exposes this app to Claude (or any MCP client). Every tool is a
thin client of this app's own HTTP API, authed with a personal access token, so
it reuses the app's auth and validation and never touches Postgres directly.

```bash
make mcp-list      # list the registered tools (no token needed)
make mcp-install   # build/refresh the CLI at $HOME/bin/<app>-mcp
```

The installed binary reads `~/.config/<app>.json`, where `<app>` is the Go module
base (`github.com/you/my-app` -> `my-app`):

```json
{
  "appUrl": "https://my-app.example.com",
  "token": "pat_1_..."
}
```

`chmod 600` it - that's a live credential. Mint the token with `POST /api/tokens`
while logged in. `appUrl` is the app origin, not the `/api` path. With
`XDG_CONFIG_HOME` set the file goes there instead; `make mcp-install` prints the
path it resolves to.

Settings resolve highest-first: real `MCP_APP_URL` / `MCP_APP_TOKEN` env vars,
then `~/.config/<app>.json`, then a local `.env`. Restart the MCP server after
editing the config; the client is built once per process.

Point Claude Desktop at the absolute path `make mcp-install` printed (it execs
the command directly, so `~` won't expand):

```json
{
  "mcpServers": {
    "my-app": { "command": "/Users/you/bin/my-app-mcp" }
  }
}
```

Delete the sample `list_notes`/`create_note` tools with the notes feature and
register your own.

## Auto-update (PWA behind a CDN)

Installed PWAs (iOS home-screen, Chrome) pick up a new deploy automatically - no
force refresh. Two things make that work, and both matter behind Cloudflare:

- The service worker (`web/src/sw.ts`) calls `skipWaiting()` + `clientsClaim()`, and
  `web/src/main.ts` registers it (with `registerType: 'autoUpdate'` set in
  `web/vite.config.ts`), so a new build takes over and reloads the page. It also
  re-checks for updates on focus and hourly (the app uses hash routing, and an open
  iOS PWA can sit for days without a real navigation to trigger the browser's own
  check).
- The Go server (from the foundation) sends `Cache-Control: immutable` for the
  content-hashed bundles under `/assets/` and `no-cache` for everything else
  (`index.html`, `sw.js`, `manifest.webmanifest`, icons). Cloudflare's default Origin
  Cache Control honors this, so the edge always revalidates the service worker and
  HTML shell instead of pinning old copies.

Only put content-hashed build output under `/assets/` - anything there is cached
forever. This assumes Cloudflare's default caching (no Cache Rule forcing "Cache
Everything" on HTML/JS).

First rollout only: if an app was already live before these cache headers existed,
Cloudflare may still be holding a header-less `sw.js`/`index.html` at the edge. Purge
the cache once on that first deploy. Already-open clients also won't pick up the new
update-check loop until they reload once; every deploy after that is automatic.

## What comes from the foundation

Auth (sessions, cookies, middleware), web push, the pgx pool + migration runner,
and the chi+huma server with embedded-SPA serving. Update it with:

```bash
cd server && go get github.com/robert-crandall/go-home-server@latest && go mod tidy
```

Delete `server/internal/notes` and build your own features the same way.
