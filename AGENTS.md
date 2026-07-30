# AGENTS.md

`github.com/robert-crandall/go-home-server` is an importable Go module - the shared
foundation (auth, API tokens, file uploads, web push, Postgres wiring, chi+huma server,
LLM client, MCP harness) that the author's single-user homelab apps `go get`. It is not
an app. The SPA, service worker, Dockerfile, compose file, and deploy workflows belong to
each app and are deliberately absent here; don't add them. See `README.md` for what each
package does and why.

## Setup

A Go toolchain matching `go.mod` is enough to build, vet, and run the unit tests. The
integration tests additionally need a Postgres:

```bash
export TEST_DATABASE_URL='postgres://app:app@127.0.0.1:5432/<scratch-db>?sslmode=disable'
```

Use a **scratch database you don't care about**: test setup runs `DELETE FROM users`.
`make dev-db` will start one in Docker, but it binds host port 5432 and fails if
something already owns that port - point `TEST_DATABASE_URL` at your own Postgres
instead.

## Commands

```bash
make foundation-check          # exactly what CI runs: go build ./... && go vet ./... && go test -p 1 ./...
go test ./files                # one package
go test ./auth -run TestFirstUserRegistrationIsSerialized   # one test
gofmt -l .                     # must print nothing
```

`-p 1` is load-bearing: the `auth` and `files` integration tests share the one
`TEST_DATABASE_URL` and each wipe `users` on setup, so their packages must not run
concurrently.

There is no linter in CI and no `.golangci.yml`. Gofmt-clean plus `go vet` is the bar.

## Gotchas

1. **A green `go test ./...` does not mean the database paths ran.** Integration tests
   are gated at runtime, not by build tag: when `TEST_DATABASE_URL` is unset they
   `t.Skip` and the package still reports `ok`. Set it before you claim the tests pass.
   Exemplar: `testPool` in `auth/auth_integration_test.go`.
2. **Don't hoist `goose.SetTableName` / `goose.SetBaseFS` out of the loop in
   `db.Migrate`.** goose keeps them in package-global state, and configuring them per
   source *inside* the loop is the only reason shared migrations and an app's own
   migrations can both start numbering at 00001. Hoisting them reads as an obvious
   cleanup and silently breaks every app downstream.
   `db/migrate_integration_test.go` catches it - but only with `TEST_DATABASE_URL` set,
   so gotcha 1 and this one compound.
3. **Adding, renaming, or removing a huma operation fails `internal/wiring`** until you
   update the `want` map in `internal/wiring/wiring_test.go`. That's an intentional
   change detector: the HTTP surface of a module other repos vendor shouldn't move by
   accident.
4. **This module is somebody else's dependency.** A changed exported signature lands in
   every app on the next `go get -u`. Prefer additive changes.
5. **`README.md` has an "Acknowledged, not fixed" section.** The bar for this repo is
   single-user homelab software on a private network. Those entries are accepted
   decisions, not oversights - if you think one has become a real problem, say so and
   make the case; don't quietly add a defense.
6. **`UPLOAD_DIR` must already exist.** `files.NewService` refuses to create it, so a
   forgotten bind mount is a startup crash instead of uploads written to a discarded
   container layer.

## Layout notes

`README.md` maps every package. The parts that aren't self-evident:

- `internal/wiring/` - test-only. It mounts every endpoint the module offers onto one
  huma API to catch cross-package collisions. It exists to be run, never imported.
- `examples/minimal/main.go` - a complete ~90-line app, compiled by CI so it can't drift.
  It is the copy-me starting point for a new app.
- `migrations/` - goose SQL, `NNNNN_name.sql`, always with an `-- +goose Down`. Shared
  migrations track their own version table (`goose_shared_version`). Never edit a
  migration that has been applied; add a new one.
- There is no codegen step and no vendor directory: nothing has to be re-run after an edit.

## Conventions

Read the exemplar rather than a rule:

- **HTTP handlers** - `huma.Register` with an explicit `OperationID`, `Summary`, and
  `Tags`; `auth/auth.go` `Register` is the plain case. huma can't infer a body schema for
  a `StreamResponse`, so binary endpoints declare `Responses` by hand - see `download-file`
  in `files/files.go`.
- **Errors** - infrastructure failures are wrapped with a package prefix
  (`fmt.Errorf("db: connect: %w", err)` in `db/db.go`). Handler-facing validation is not
  uniform: `notify.validatePushEndpoint` returns plain messages that the handler wraps in
  `huma.Error422UnprocessableEntity`, while `auth.RequireUser` returns a `huma` 401
  directly. Match the neighbouring code.
- **Tests** - standard library `testing` only, no testify or other assertion package;
  `httptest` for HTTP. Integration tests live in `*_integration_test.go` and skip
  themselves rather than using a build tag.
- **Comments** - the non-obvious decisions carry their reasoning inline.
  `migrations/00007_sessions_drop_expires_at_index.sql` is the house style.

## PRs

- CI is a single job: `go build ./...`, `go vet ./...`, `go test -p 1 ./...` against a
  `postgres:16-alpine` service, on pushes and PRs to `main`. That is the entire gate, so
  run `make foundation-check` with `TEST_DATABASE_URL` set before calling anything done.
- No enforced commit-message convention; history mixes plain imperative subjects
  ("Slide session expiry on every authenticated request") with scoped prefixes
  ("files: generate a thumbnail for every decodable image").
- Dependabot PRs squash-merge themselves once CI is green.
