.PHONY: help foundation-check vapid dev-db

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

foundation-check: ## Build, vet and test the module
	# -p 1: the integration tests in auth/ and files/ share one TEST_DATABASE_URL
	# and each wipe `users` on setup, so their packages must not run in parallel.
	go build ./... && go vet ./... && go test -p 1 ./...

vapid: ## Generate a VAPID key pair for web push
	go run ./cmd/vapid

dev-db: ## Start a local Postgres for the integration tests (no-op if already up)
	@docker container inspect -f '{{.State.Running}}' go-home-server-db 2>/dev/null | grep -qx true || \
		docker run -d --rm --name go-home-server-db \
			-e POSTGRES_USER=app -e POSTGRES_PASSWORD=app -e POSTGRES_DB=app \
			-p 5432:5432 postgres:16-alpine
	@echo 'export TEST_DATABASE_URL=postgres://app:app@localhost:5432/app?sslmode=disable'
