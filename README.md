# go-home-server

Shared foundation for my single-user apps. It exists so I stop re-solving the
same problems (auth, notifications, Postgres wiring, the embedded-SPA server,
PWA/iOS boilerplate) in every new app.

It uses a **hybrid** reuse model, because not everything can be shared the same
way:

- **A shared Go module** (this repo's root) for backend logic that imports
  cleanly: config, database + migrations, session auth, web push, and the
  chi+huma server bootstrap. Apps `go get` it and pick up fixes with `go get -u`.
- **A template app** (`template/`) for the stuff you can't import: the Svelte
  SPA, the service worker, the PWA manifest + iOS icons, the Vite/Tailwind
  config, the Dockerfile, and CI. You copy this once to start a new app.

The template imports the module, so the module can't silently rot. If a change
breaks the foundation, the reference app stops compiling.

## Why not just one or the other?

The deciding factor is what can physically be an imported dependency.

You cannot `import` a service worker, a Dockerfile, or a Vite config. That whole
category has to come from a template. But auth logic, a push sender, and a
migration runner are exactly what Go modules are good at. So I do both, and push
as much logic as possible *down into the module* so the copied template layer
stays thin (mostly wiring and config). The thinner the copied layer, the less
drift I inherit when I fix something later.

The honest downside: once an app copies `template/`, later template fixes don't
flow back automatically. That's inherent to the foundation model. The mitigation
is the module - anything living there updates with `go get -u`.

## Layout

```
go-home-server/
  config/            # env config (+ .env)
  db/                # pgx pool + goose migration runner
  migrations/        # shared migrations: users, sessions, push_subscriptions, api_tokens, files
  auth/              # opaque server-side sessions, cookies, middleware, huma handlers; opt-in API tokens (bearer auth)
  files/             # per-user file uploads: bytes on a mounted disk, metadata in Postgres
  notify/            # web push (VAPID) + subscription store
  server/            # chi + huma bootstrap, embedded-SPA serving, graceful shutdown
  apiclient/         # tiny bearer-token HTTP client for scripts and MCP tools
  llm/               # OpenAI / Anthropic / xAI completions behind one interface
  mcp/               # dual-mode (CLI + MCP-over-stdio) harness wrapping the Go MCP SDK
  template/          # the copyable app (this is what `make new-app` clones)
    server/          # reference app: own go.mod (replace => ../../), cmd/{server,migrate,openapi,vapid,mcp}
      internal/notes # sample per-user feature (delete when starting real work)
      internal/web   # embeds the built SPA
    web/             # Svelte 5 + Vite + Tailwind v4 + DaisyUI + Lucide + PWA + openapi-fetch
    uploads/         # dev upload dir; bind-mounted to /data/uploads in compose
    Dockerfile       # multi-stage (web -> server -> distroless)
    docker-compose.yml
    .env.example
  scripts/new-app.sh # copies template/ + rewrites the module path/imports
  .github/workflows/ci.yml   # this repo's CI (foundation + reference app)
```

Note the app-level infra (Dockerfile, docker-compose, CI, env example) lives
under `template/`, so copying `template/` gives you a complete, self-contained
app. The root `.github/workflows/ci.yml` is this repo's own CI.

## What the foundation gives you

- **Auth** - register / login / logout / me, opaque session token hashed in
  Postgres, delivered as an httpOnly cookie. `auth.Middleware` resolves the user
  onto the request context; `auth.RequireUser` guards a handler. Registration is
  first-user-only by default (race-safe via a DB advisory lock); set
  `ALLOW_OPEN_REGISTRATION=true` for a multi-user app.
- **API tokens** (opt-in) - personal access tokens so scripts, cron jobs, or an
  MCP server can call the API with `Authorization: Bearer <token>` instead of a
  browser cookie. Call `authSvc.RegisterTokens(api)` to mount `/api/tokens`
  (create / list / revoke) and enable bearer auth. Only sha256(secret) is stored;
  the plaintext is shown once. A bearer request never falls back to the cookie,
  and token management itself is session-only, so a leaked token can't mint,
  list, or revoke tokens. Apps that don't call `RegisterTokens` expose no token
  endpoints and ignore bearer credentials entirely.
- **File uploads** - `POST/GET/DELETE /api/files` for per-user files, with the
  bytes on a directory you bind-mount into the container and the metadata in
  Postgres. Photos are the motivating case: the download endpoint streams
  through `http.ServeContent`, so `<img src="/api/files/{id}">` gets Range and
  conditional requests for free. Responses are `private, no-cache` - they're
  per-user and deletable, and browser caches key on URL rather than session, so
  caching one hard would leave it readable after logging out; revalidation
  re-runs the ownership check and still answers 304. The stored
  filename never touches the path (a random key plus a sanitized extension
  does), the content type is sniffed from the bytes rather than trusted from the
  client, and anything that isn't obviously safe to render is served
  `Content-Disposition: attachment` so an uploaded `.html` can't become stored
  XSS on your origin. `UPLOAD_DIR` is required and must already exist - the app
  refuses to create it, so forgetting the bind mount is a startup crash instead
  of photos quietly written to a container layer that's discarded on the next
  deploy. `UPLOAD_MAX_BYTES` caps a single upload request body (25 MiB by
  default). Every image the server can decode - JPEG, PNG, GIF, WebP - also
  gets a 512 px JPEG thumbnail written beside it at upload time, served from
  `GET /api/files/{id}/thumbnail`, so a photo grid loads kilobytes instead of
  megabytes; `hasThumbnail` on the file tells the client which URL to use.
  HEIC and AVIF don't get one (decoding them needs cgo or a wasm blob, and
  neither belongs in a distroless image) and neither do files that were
  uploaded before this existed - both fall back to the original. There's no
  dedup or quota - the volume's size is the quota.
- **Notifications** - store browser push subscriptions and send Web Push with
  VAPID. `notify.Send(ctx, userID, payload)` from anywhere. The template ships
  the frontend half (service worker + subscribe flow).
- **Database** - a pgx pool and a goose migration runner. Shared migrations and
  app migrations track separate goose version tables, so both can number from
  00001 without colliding.
- **Server** - a chi router with a huma API (so handlers are typed and the
  OpenAPI spec is generated from Go), serving the embedded SPA with a deep-link
  fallback (and a JSON 404 for unknown `/api` paths), plus graceful shutdown.
  Set `server.Options.HealthCheck` (the template passes `pool.Ping`) to expose a
  `GET /healthz` readiness probe for uptime monitors and container healthchecks;
  it reports 200 when the check passes and 503 when it fails.
- **LLM calls** - one small client for OpenAI, Anthropic, and xAI, so an app can
  switch providers with a field on the request instead of three sets of HTTP
  plumbing. `llm.New(llm.ConfigFromEnv())`, then
  `client.Complete(ctx, llm.Request{...})`, or `client.Stream(...)` to relay
  text as it's generated. It owns transport only - prompts stay in the app. See
  [Calling an LLM](#calling-an-llm).
- **MCP server** - a `mcp` harness so every app can expose its data to Claude
  (or any MCP client) the same way. It wraps the official Go MCP SDK: an app
  registers tools as plain typed Go functions (`mcp.AddTool`), and the resulting
  binary is dual-mode - run it bare (or `serve`) to speak MCP over stdio to a
  desktop client, or use `list`/`call` to exercise tools from a shell. `make
  mcp-install` builds it to `$HOME/bin/<app>-mcp`, and it reads
  `~/.config/<app>.json` for the app's URL and token (env vars still override).
  Tools are thin clients of the app's own HTTP API via the `apiclient` package
  (authed with a `/api/tokens` personal access token), so they reuse the app's
  auth, validation, and business logic instead of forking it - and never touch
  the database directly. The rule: an MCP tool never owns domain logic - if a
  tool needs new behavior, add or adjust the app's API first. The template ships
  a sample `cmd/mcp` with `list_notes`/`create_note` over the sample notes
  feature (delete and rebuild per app, like `notes`). See
  [MCP servers](#mcp-servers) for the install/config/Claude Desktop wiring.

## Run the reference app locally

Requires Go 1.26, Docker (for Postgres), and [bun](https://bun.sh) (the web
build/dev tooling - the Makefile's `web-*` targets shell out to `bun`).

```bash
cd template
cp .env.example .env              # edit DATABASE_URL if needed
make dev-db                       # start Postgres

# Backend: migrates on boot, serves API + embedded SPA on :8080
make run

# Frontend dev server (proxies /api to :8080, hot reload) in another terminal:
make web-dev                      # http://localhost:5173
```

The first account you register becomes your single user, then registration
closes. Web push is optional - generate keys with `make vapid`.

## The API-first loop

The OpenAPI spec is the contract. When an endpoint or type changes:

1. Edit the Go handlers/types.
2. `make openapi` - regenerate `template/server/openapi.json` from the Go code.
3. `make gen-api` - regenerate `template/web/src/lib/api/schema.d.ts`.
4. Commit the spec and generated types with the code.

CI's `contract` job fails if the committed spec or client types are stale, so
the web client can never guess request/response shapes.

## MCP servers

Every app can expose its data to Claude (or any MCP client) through a small MCP
server, built on the shared `mcp` harness. The template ships a reference one at
`template/server/cmd/mcp` with two sample tools (`list_notes`, `create_note`)
over the sample notes feature.

The design in one line: **an MCP tool is a thin client of the app's own HTTP
API.** Tools authenticate with a personal access token (`POST /api/tokens`) via
the `apiclient` package and call the same endpoints the SPA does, so they inherit
the app's auth, validation, and business logic. Domain logic never moves into the
MCP layer - if a tool needs new behavior, add or change the API first.

Register a tool as a plain typed function; the harness infers the input schema
from struct tags and validates arguments:

```go
type createNoteIn struct {
    Body string `json:"body" jsonschema:"the note text"`
}
type createNoteOut struct{ Note note `json:"note"` }

mcp.AddTool(s, "create_note", "Add a note.",
    func(ctx context.Context, in createNoteIn) (createNoteOut, error) {
        // call the app API via apiclient, return a struct
    })
```

Tool outputs must be object-shaped (a struct), so wrap slices in a struct rather
than returning a bare slice.

The binary is dual-mode:

```bash
cd server && go run ./cmd/mcp list                 # list tools (no token needed)
cd server && go run ./cmd/mcp call list_notes      # call a tool (needs a token)
cd server && go run ./cmd/mcp                      # serve MCP over stdio
```

`list`, `help`, and `serve` need no token; only an actual tool `call` does.

### Install it

The MCP server is a CLI you keep around, so build it into `$HOME/bin` and rerun
this whenever you change a tool. From the app's root (this target lives in the
app's Makefile, not the foundation's):

```bash
make mcp-install     # -> $HOME/bin/<app>-mcp
```

The name comes from the Go module path (`github.com/you/my-app` -> `my-app-mcp`),
which is also the name it reports in the MCP handshake.

### Configure it

An installed binary is launched by a desktop client from an arbitrary working
directory, so it doesn't get to rely on the app's `.env`. It reads
`~/.config/<app>.json` instead:

```bash
mkdir -p ~/.config
cat > ~/.config/my-app.json <<'JSON'
{
  "appUrl": "https://my-app.example.com",
  "token": "pat_1_..."
}
JSON
chmod 600 ~/.config/my-app.json
```

`appUrl` is the app's origin (default `http://localhost:8080`), not the `/api`
path. `token` is a personal access token from `POST /api/tokens` - the file holds
a live credential, hence the `chmod 600`. If you set `XDG_CONFIG_HOME` it goes
under that directory instead; `make mcp-install` prints the path it resolves to.

Settings resolve highest-first: `MCP_APP_URL` / `MCP_APP_TOKEN` in the real
environment, then `~/.config/<app>.json`, then a local `.env`. So a desktop
client's `env` block or a one-off `MCP_APP_TOKEN=... my-app-mcp call ...` still
wins, while a stale `.env` in whatever directory you're standing in doesn't
quietly hijack an installed binary. The client is built once per process, so
restart the MCP server after editing the config.

Wire it into Claude Desktop's `claude_desktop_config.json` (stdio transport).
Use the absolute path `make mcp-install` printed - desktop clients exec the
command directly, so `~` won't expand:

```json
{
  "mcpServers": {
    "my-app": {
      "command": "/Users/you/bin/my-app-mcp"
    }
  }
}
```

No `env` block needed - that's what `~/.config/my-app.json` is for.

Delete the sample tools and register your own the same way you replace
`internal/notes`.

## Calling an LLM

The `llm` package talks to OpenAI, Anthropic, and xAI through one interface, so
an app that wants to switch providers changes a field instead of maintaining
three HTTP clients.

The seam is the same one the MCP harness uses: **this package owns transport
only.** Auth headers, request/response shapes, provider selection, and the error
type live here. Prompts and what you do with the reply stay in the app - there's
no prompt templating and no domain logic in the foundation.

```go
client, err := llm.New(llm.ConfigFromEnv())
if err != nil {
    // no provider configured - fail at startup, not on the first call
}

resp, err := client.Complete(ctx, llm.Request{
    Messages: []llm.Message{
        {Role: llm.System, Content: "Answer in one sentence."},
        {Role: llm.User, Content: question},
    },
    MaxTokens: 512,
})
// resp.Text, resp.Provider, resp.Model
```

Switching provider is a per-request field:

```go
resp, err := client.Complete(ctx, llm.Request{
    Provider:  llm.Anthropic, // or llm.OpenAI, llm.XAI
    Model:     "some-model",  // optional; falls back to the configured one
    Messages:  msgs,
    MaxTokens: 1024,
})
```

`Temperature` is optional and set the same way. It's a `*float64` because 0 is a
real temperature - the near-greedy setting you want when parsing structured
output - so it has to be distinguishable from "unset". `llm.Temp` exists because
Go can't take the address of a literal:

```go
resp, err := client.Complete(ctx, llm.Request{
    Messages:    msgs,
    MaxTokens:   1024,
    Temperature: llm.Temp(0), // leave nil for the provider's default
})
```

Configuration is env-only, and separate from `config.Config` so apps that never
call an LLM carry nothing:

```bash
LLM_PROVIDER=anthropic        # default provider; optional (see below)
OPENAI_API_KEY=...            # OPENAI_MODEL=...
ANTHROPIC_API_KEY=...         # ANTHROPIC_MODEL=...
XAI_API_KEY=...               # XAI_MODEL=...
```

A provider with no key simply isn't available. `LLM_PROVIDER` is optional when
exactly one key is set - the single-provider case needs no default. With two
keys and no `LLM_PROVIDER`, a request that doesn't name a provider is an error,
because the config genuinely became ambiguous.

A few things that surprise people, all deliberate:

- **`MaxTokens` is required.** Anthropic's API demands it and the others don't.
  Defaulting it per-provider would silently truncate the same request on one
  provider but not another, which is exactly the trap for an app that switches.
- **There's no default model.** Model names churn every few months, so a default
  baked in here would rot into calling a deprecated model. Set `Request.Model`
  or `<PROVIDER>_MODEL`.
- **`Temperature` caps at 1, not 2.** OpenAI and xAI go to 2, Anthropic stops at
  1, so 1 is the ceiling a single request can portably mean. Leaving it nil
  sends no `temperature` field at all, which also keeps OpenAI's reasoning
  models working - they reject any non-default temperature outright.
- **Messages must alternate.** An optional system message first, then strictly
  alternating user/assistant turns, starting and ending with `user`. Anything
  else means different things to different providers: Anthropic hoists the
  system message to a top-level field (so one mid-conversation is meaningless),
  silently merges consecutive same-role turns into one where OpenAI keeps them
  distinct, and continues from a trailing `assistant` message as a prefill where
  OpenAI just answers it as history. Join your fragments before calling.
- **No message may be empty.** Anthropic rejects an empty text block outright
  while OpenAI accepts it, so an app that built a message from an empty variable
  would get a provider error on one backend and a wasted call on the other.
- **A refused or filtered completion is an error, not empty text.** Anthropic
  reports it as `stop_reason: "refusal"`, OpenAI and xAI as a `refusal` field or
  `finish_reason: "content_filter"`. All of them come back as an error so an app
  can't mistake a refusal for a successful empty answer. Hitting `MaxTokens` is
  not an error - that limit is yours, and the partial text is usable.

All of that is checked before any HTTP happens, so a malformed request fails the
same way no matter which provider would have answered it.

### Streaming

`Stream` runs the same request and calls you back with each chunk of assistant
text as it arrives, then returns the same assembled `Response` that `Complete`
would have:

```go
rc := http.NewResponseController(w)

resp, err := client.Stream(ctx, req, func(delta string) error {
    b, err := json.Marshal(delta) // deltas contain newlines
    if err != nil {
        return err
    }
    if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
        return err // the browser went away - stop generating
    }
    return rc.Flush()
})
// resp.Text is the whole thing, so you can persist it without re-accumulating
```

Return the write and flush errors rather than dropping them. That's what makes
abandoning a generation work: once the browser disconnects, the next write fails
and `Stream` stops instead of paying for tokens nobody will read.
`http.NewResponseController` is used instead of a `w.(http.Flusher)` assertion
because that assertion panics on a `ResponseWriter` some middleware has wrapped.

The callback runs synchronously on your goroutine - no goroutines, channels, or
buffers, so backpressure is free and there's nothing to leak. Returning an error
from it stops the stream and that error comes back from `Stream`.

Two things worth knowing:

- **Chunks are provisional until `Stream` returns `nil`.** This is the one real
  difference from `Complete`. `Complete` inspects a whole response before
  returning any of it, so it can withhold a refusal or a filtered completion;
  `Stream` can't, because those chunks are already gone. Every failure -
  refusal, a mid-stream error, a dropped connection - returns a zero `Response`
  and the caller must discard what it already emitted. Relaying to a browser,
  that means sending an explicit error event so the frontend can drop the
  partial message rather than leaving it on screen.
- **`http.Client.Timeout` bounds the entire stream, not just the headers.** The
  default is two minutes. For long generations, inject a client with a longer or
  zero `Timeout` via `llm.WithHTTPClient` and bound the call with the context
  instead.

A stream that ends without its provider's terminator is an error too, so a proxy
reset mid-answer can't quietly look like a complete one.

Not included: tool calling, embeddings, retries, and token/cost accounting. None
has a caller yet and each is additive later. A non-2xx response - a rate limit
or an overload, say - comes back as an `*llm.Error` with `Status` set, which is
the seam if an app ever wants to retry:

```go
var apiErr *llm.Error
if errors.As(err, &apiErr) && apiErr.Status == 429 {
    // back off and retry, if this app cares
}
```

That's a non-2xx response. A failure a provider reports mid-stream, after its
200 headers are already out, comes back as a plain error - there's no status
left to attach.

Note the package targets OpenAI's Chat Completions endpoint rather than the
newer Responses API. Chat Completions is what xAI implements, so it's the common
denominator that lets two providers share one transport. It's labeled legacy but
supported, with no announced shutdown - and if that changes, it's one fix here
that every app picks up with `go get -u`.

## Start a new app

Use the script - it copies `template/`, rewrites the module path and every
internal import, and points the app at the foundation as a real dependency:

```bash
make new-app DEST=../my-app MODULE=github.com/you/my-app
# then:
cd ../my-app && cp .env.example .env && make dev-db && make run
```

Until the foundation is published/tagged, pass a local checkout so the new app
resolves it via a `replace`:

```bash
scripts/new-app.sh ../my-app github.com/you/my-app .
```

Then delete `server/internal/notes`, build your own features the same way, and
replace the placeholder icons in `web/public/icons` (the PWA/iOS wiring around
them is already done).

## Validation

```bash
make foundation-check   # go build/vet/test on the module
make app-build          # build the SPA, embed it, build the app binary
make web-check          # svelte-check + vitest
```

## Status / honesty

Verified locally against a real Postgres: migrations (both version tables),
register -> me -> create note -> list -> logout, the push endpoints, the
first-user-only registration gate (including a concurrent-registration test),
unknown `/api` paths returning JSON 404, and the embedded SPA (real build
served, deep links fall back to index.html, manifest / service worker / icons
all served with correct content types). `scripts/new-app.sh` is tested by
generating an app and compiling it.

The MCP install path is verified the same way: scaffold an app, `make
mcp-install`, then drive the installed `$HOME/bin/<app>-mcp` against the running
app with nothing but `~/.config/<app>.json` - `create_note` then `list_notes`
round-tripped through the real API. Precedence was checked live too: an
`MCP_APP_URL` env var beats the config file, a stale `.env` in the app checkout
does not, and with no config file that `.env` is still honored.

**Not** verified here: the Docker image build (no Docker daemon was available in
this environment). `template/Dockerfile` builds standalone (foundation via
`go get`), so it only works once the app is generated by `scripts/new-app.sh`
(which drops the in-repo `replace`) and the foundation is published; give it a
run before relying on it.

Also **not** verified against live APIs: the `llm` package. Its wire formats
(endpoints, auth headers, `max_completion_tokens` vs `max_tokens`, Anthropic's
top-level `system` field and typed content blocks, and both SSE streaming
formats) were checked against each vendor's current documentation, and the
request/response handling is covered by `httptest` servers - but no real call to
OpenAI, Anthropic, or xAI was made, because this environment has no API keys.
Make one real call per provider, blocking and streaming, before depending on it.
