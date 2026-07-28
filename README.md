# go-home-server

Shared foundation for my single-user apps. It exists so I stop re-solving the
same problems (auth, notifications, Postgres wiring, the embedded-SPA server) in
every new app.

This repo is **the Go module and nothing else** - the part that can physically be
an imported dependency. Apps `go get` it and pick up fixes with `go get -u`.

The layer you can't import - the Svelte SPA, the service worker, the PWA manifest
and iOS icons, the Vite/Tailwind config, the Dockerfile, the compose file, the
CI/CD workflows - used to live here under `template/`. It has been extracted so
this repo stays a clean vendorable dependency; it will get its own repo. The last
commit that contained it is `6a88e3f`, so until then:

```bash
git checkout 6a88e3f -- template scripts/new-app.sh
```

What's left here still can't silently rot: `examples/minimal` is a complete app
that CI compiles, and `internal/wiring` mounts every endpoint this module offers
onto one huma API so a cross-package break fails the build.

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
  cmd/vapid/         # generate a VAPID key pair for web push
  examples/minimal/  # smallest complete app built on all of the above
  internal/wiring/   # test-only: every endpoint on one huma API
```

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
  VAPID. `notify.Send(ctx, userID, payload)` from anywhere. The frontend half
  (service worker + subscribe flow) is the app's to write.
- **Database** - a pgx pool and a goose migration runner. Shared migrations and
  app migrations track separate goose version tables, so both can number from
  00001 without colliding.
- **Server** - a chi router with a huma API (so handlers are typed and the
  OpenAPI spec is generated from Go), serving the embedded SPA with a deep-link
  fallback (and a JSON 404 for unknown `/api` paths), plus graceful shutdown.
  Set `server.Options.HealthCheck` (pass `pool.Ping`) to expose a
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
  desktop client, or use `list`/`call` to exercise tools from a shell. Install it
  wherever you like (`$HOME/bin/<app>-mcp` is the convention `mcp.AppName`
  assumes); it reads `~/.config/<app>.json` for the app's URL and token (env vars
  still override).
  Tools are thin clients of the app's own HTTP API via the `apiclient` package
  (authed with a `/api/tokens` personal access token), so they reuse the app's
  auth, validation, and business logic instead of forking it - and never touch
  the database directly. The rule: an MCP tool never owns domain logic - if a
  tool needs new behavior, add or adjust the app's API first. See
  [MCP servers](#mcp-servers) for the config/Claude Desktop wiring.

## Use it

```bash
go get github.com/robert-crandall/go-home-server@latest
```

`examples/minimal/main.go` is a complete app in ~90 lines - config, migrations,
pool, auth, notifications, files, server - and CI compiles it, so it can't drift.
Copy it and start adding your own routes and migrations.

Web push needs a VAPID key pair:

```bash
go run github.com/robert-crandall/go-home-server/cmd/vapid@latest
```

### Configuration

`config.Load` reads these from the environment (and an optional `.env` in the
working directory). It covers the app-server side only - `llm` reads its own
provider keys (see [Calling an LLM](#calling-an-llm)) and `apiclient` reads the
MCP settings (see [MCP servers](#mcp-servers)).

| Variable | Default | What it does |
|---|---|---|
| `DATABASE_URL` | *required* | Postgres connection string for `db.New`. |
| `ADDR` | `:8080` | Listen address for `server.Run`. |
| `APP_ENV` | `development` | `production` turns on secure cookies (`cfg.IsProduction()`). |
| `SESSION_SECRET` | *(empty)* | Read and exposed on `Config`, but **no foundation package uses it**. It's there for an app that wants signing/derivation material. Safe to leave unset. |
| `ALLOW_OPEN_REGISTRATION` | `false` | Exactly `true` lets anyone register; anything else means first account only. |
| `VAPID_PUBLIC_KEY` | *(empty)* | Web push public key. Empty disables push. |
| `VAPID_PRIVATE_KEY` | *(empty)* | Web push private key. Must pair with the public one. |
| `VAPID_SUBJECT` | `mailto:admin@example.com` | `mailto:` or URL identifying the sender to the push service. |
| `UPLOAD_DIR` | *(empty)* | Directory for uploaded bytes. No default on purpose - see `files`. Required if you register the file routes; the directory must already exist. |
| `UPLOAD_MAX_BYTES` | *(unset)* | Cap on a single upload request body. Unset or `<= 0` means `files.DefaultMaxBytes` (25 MiB). |

The `mcp`/`apiclient` side reads `MCP_APP_URL` and `MCP_APP_TOKEN` (or
`~/.config/<app>.json`) - see [MCP servers](#mcp-servers). `llm.ConfigFromEnv`
reads `LLM_PROVIDER` and the per-provider key/model vars - see
[Calling an LLM](#calling-an-llm).

## The API-first loop

The OpenAPI spec is the contract. `server.New` hands you `srv.API`, and
`srv.API.OpenAPI()` marshals the spec straight out of your Go handlers and types
- so if your frontend generates a typed client from that spec, the client can
never guess request/response shapes. Regenerating it and committing the result
is your app repo's job; this module just makes sure it's derived from the code.

## MCP servers

Every app can expose its data to Claude (or any MCP client) through a small MCP
server, built on the shared `mcp` harness.

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
go run ./cmd/mcp list                 # list tools (no token needed)
go run ./cmd/mcp call <tool> '{...}'  # call a tool (needs a token)
go run ./cmd/mcp                      # serve MCP over stdio
```

`list`, `help`, and `serve` need no token; only an actual tool `call` does.

### Install it

The MCP server is a CLI you keep around, so build it somewhere on your PATH and
rebuild whenever you change a tool. `mcp.AppName` assumes the convention
`$HOME/bin/<app>-mcp`:

```bash
go build -o "$HOME/bin/my-app-mcp" ./cmd/mcp
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
under that directory instead.

Settings resolve highest-first: `MCP_APP_URL` / `MCP_APP_TOKEN` in the real
environment, then `~/.config/<app>.json`, then a local `.env`. So a desktop
client's `env` block or a one-off `MCP_APP_TOKEN=... my-app-mcp call ...` still
wins, while a stale `.env` in whatever directory you're standing in doesn't
quietly hijack an installed binary. The client is built once per process, so
restart the MCP server after editing the config.

Wire it into Claude Desktop's `claude_desktop_config.json` (stdio transport).
Use an absolute path - desktop clients exec the command directly, so `~` won't
expand:

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

Copy `examples/minimal/main.go` into a fresh module and grow it:

```bash
mkdir -p ../my-app && cd ../my-app
go mod init github.com/you/my-app
go get github.com/robert-crandall/go-home-server@latest
# then copy examples/minimal/main.go from this repo
```

It registers the file routes, so `UPLOAD_DIR` is mandatory and the directory
must already exist - `files.NewService` refuses to create it (see the comment on
`files.NewService` for why). A minimal `.env` to get it booting:

```bash
mkdir -p uploads
cat > .env <<'ENV'
DATABASE_URL=postgres://app:app@localhost:5432/app?sslmode=disable
UPLOAD_DIR=./uploads
ENV
```

Then add your own migration `fs.FS` as a second `db.MigrationSource` (the
example shows where), register your own huma routes next to the foundation's,
and embed your own built SPA into `server.Options.SPA`. If you don't want file
uploads at all, drop the `files` service and its `Register` call instead of
setting `UPLOAD_DIR`.

The app-side layer - Svelte SPA, service worker, PWA icons, Dockerfile, compose,
CI/CD - will live in a separate repo. That repo doesn't exist yet; for now,
recover the old in-repo template from git history using the command at the top
of this file.

## Validation

```bash
make foundation-check   # go build/vet/test on the module
```

Integration tests are gated on `TEST_DATABASE_URL`; `make dev-db` starts a
Postgres and prints the export line.

## Status / honesty

Verified locally against a real Postgres: migrations (including that two
migration sources with colliding version numbers stay isolated by version
table), register -> me -> logout, the push endpoints, the first-user-only
registration gate (including a concurrent-registration test), the file
upload/download/thumbnail endpoints, unknown `/api` paths returning JSON 404,
and the embedded SPA (real build served, deep links fall back to index.html,
manifest / service worker / icons all served with correct content types).

`internal/wiring` mounts auth, API tokens, notifications, and files on one huma
API and marshals the resulting OpenAPI spec. That's the check that catches a
cross-package break - an operation-ID collision, say - which no single-package
test can see, and it needs no database. Fail-first verified: renaming one
operation to collide with another does fail it.

The MCP install path was verified when the app template lived here: scaffold an
app, install the binary, then drive it against the running app with nothing but
`~/.config/<app>.json` - `create_note` then `list_notes` round-tripped through
the real API. Precedence was checked live too: an `MCP_APP_URL` env var beats
the config file, a stale `.env` in the app checkout does not, and with no config
file that `.env` is still honored. Those runs predate the split; the `mcp` and
`apiclient` packages themselves are unchanged and still covered by their own
tests.

**Not** verified here: the `llm` package against live APIs. Its wire formats
(endpoints, auth headers, `max_completion_tokens` vs `max_tokens`, Anthropic's
top-level `system` field and typed content blocks, and both SSE streaming
formats) were checked against each vendor's current documentation, and the
request/response handling is covered by `httptest` servers - but no real call to
OpenAI, Anthropic, or xAI was made, because this environment has no API keys.
Make one real call per provider, blocking and streaming, before depending on it.
