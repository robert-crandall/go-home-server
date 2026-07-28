# Multi-stage build for an app generated from this template.
#
# Build context is the APP ROOT (this directory). The app depends on the
# go-home-server foundation via `go get` (the `replace` directive is dropped by
# scripts/new-app.sh when the app is created), so this builds standalone.
#
#   docker build -t my-app .

# --- web build ---
FROM oven/bun:1.3.14-alpine AS web
WORKDIR /web
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY web/ ./
RUN bun run build

# --- server build ---
FROM golang:1.26-alpine AS server
WORKDIR /src
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
# Bake the built SPA into the embed directory before compiling.
COPY --from=web /web/dist ./internal/web/dist
RUN CGO_ENABLED=0 go build -o /app ./cmd/server

# --- runtime ---
FROM gcr.io/distroless/static
COPY --from=server /app /app
EXPOSE 8080
ENTRYPOINT ["/app"]
