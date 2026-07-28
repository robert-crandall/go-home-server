.PHONY: help foundation-check app-build web-build web-check openapi gen-api dev-db vapid new-app

WEB := template/web
SRV := template/server

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

foundation-check: ## Build, vet and test the shared foundation module
	# -p 1: the integration tests in auth/ and files/ share one TEST_DATABASE_URL
	# and each wipe `users` on setup, so their packages must not run in parallel.
	go build ./... && go vet ./... && go test -p 1 ./...

app-build: web-build ## Build & validate the reference app (embeds the SPA)
	# Clear stale hashed assets (keep the tracked fallback + .gitignore).
	find $(SRV)/internal/web/dist -mindepth 1 -depth ! -name 'index.html' ! -name '.gitignore' -delete
	cp -r $(WEB)/dist/* $(SRV)/internal/web/dist/
	mkdir -p $(SRV)/bin
	cd $(SRV) && go build -o bin/app ./cmd/server && go vet ./... && go test ./...
	# Restore the committed fallback so a build doesn't dirty tracked files.
	git -C $(SRV) checkout -- internal/web/dist/index.html 2>/dev/null || true

web-build: ## Build the Svelte SPA
	cd $(WEB) && bun install --frozen-lockfile && bun run gen:api && bun run build

web-check: ## Typecheck and test the SPA
	cd $(WEB) && bun run lint && bun run test

openapi: ## Regenerate the OpenAPI spec from the Go handlers
	cd $(SRV) && go run ./cmd/openapi -o openapi.json

gen-api: openapi ## Regenerate the spec and the TypeScript client types
	cd $(WEB) && bun run gen:api

vapid: ## Generate a VAPID key pair for web push
	cd $(SRV) && go run ./cmd/vapid

dev-db: ## Start a local Postgres (via the template's docker-compose)
	cd template && docker compose up -d db

new-app: ## Scaffold a new app: make new-app DEST=../my-app MODULE=github.com/you/my-app
	scripts/new-app.sh "$(DEST)" "$(MODULE)"

