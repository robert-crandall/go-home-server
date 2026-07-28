.PHONY: help dev-db run build web-build web-check openapi gen-api vapid mcp-list mcp-install docker

# The app's short name comes from the Go module path (github.com/you/my-app ->
# my-app), so the installed binary, the MCP server name, and ~/.config/<app>.json
# all agree even if the checkout directory was renamed. The sed drops a /vN
# major-version suffix exactly like mcp.AppName() does at runtime (v0 and v1 are
# never module path elements, so a repo literally named "v1" keeps its name).
# Plain `=` (not `?=`): it's derived, not a knob, so a stray exported APP_NAME
# can't rename the binary out from under the handshake - but it stays lazy, so
# `go list -m` only runs for the targets that need it.
APP_NAME     = $(notdir $(shell cd server && go list -m | sed -E 's,/v([2-9][0-9]*|1[0-9]+)$$,,'))
MCP_BIN     ?= $(HOME)/bin/$(APP_NAME)-mcp
MCP_CONFIG  ?= $(if $(XDG_CONFIG_HOME),$(XDG_CONFIG_HOME),$(HOME)/.config)/$(APP_NAME).json
MCP_VERSION ?= dev

# mcp-install's mkdir and echo run from this directory, but the build runs after
# `cd server`, so a relative MCP_BIN would resolve against two different
# directories: the binary lands in server/ while the printed path points at an
# empty dir here. Resolve it once against the app root and use only this in the
# recipe. MCP_BIN is the binary's path, not a directory to drop it in - abspath
# is lexical, so it strips a trailing slash rather than treating it as one.
MCP_BIN_ABS  = $(abspath $(MCP_BIN))

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

dev-db: ## Start local Postgres
	docker compose up -d db

run: ## Run the server (migrates on boot; loads ../.env)
	cd server && go run ./cmd/server

web-dev: ## Run the Vite dev server (proxies /api to :8080)
	cd web && bun run dev

web-build: ## Build the Svelte SPA
	cd web && bun install --frozen-lockfile && bun run build

web-check: ## Typecheck and test the SPA
	cd web && bun run lint && bun run test

build: web-build ## Build the app binary with the SPA embedded
	# Clear stale hashed assets (keep the tracked fallback + .gitignore).
	find server/internal/web/dist -mindepth 1 -depth ! -name 'index.html' ! -name '.gitignore' -delete
	cp -r web/dist/* server/internal/web/dist/
	mkdir -p bin
	cd server && go build -o ../bin/app ./cmd/server
	# Restore the committed fallback so a build doesn't dirty tracked files.
	git -C server checkout -- internal/web/dist/index.html 2>/dev/null || true

openapi: ## Regenerate the OpenAPI spec from the Go handlers
	cd server && go run ./cmd/openapi -o openapi.json

gen-api: openapi ## Regenerate the spec and the TypeScript client types
	cd web && bun run gen:api

vapid: ## Generate a VAPID key pair for web push
	cd server && go run ./cmd/vapid

mcp-list: ## List the MCP server's tools (no token needed)
	cd server && go run ./cmd/mcp list

mcp-install: ## Build/refresh the MCP CLI in $HOME/bin
	mkdir -p "$(dir $(MCP_BIN_ABS))"
	cd server && go build -ldflags "-X main.version=$(MCP_VERSION)" -o "$(MCP_BIN_ABS)" ./cmd/mcp
	@echo "installed $(MCP_BIN_ABS) (config: $(MCP_CONFIG))"

docker: ## Build the production Docker image
	docker build -t app .
