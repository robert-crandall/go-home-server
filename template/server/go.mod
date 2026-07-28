// This module is the reference application shipped with the go-home-server
// foundation. scripts/new-app.sh generates new apps from it, rewriting the
// module path (github.com/robert-crandall-org/example-app) and replacing the
// in-repo `replace` below with a real dependency on the foundation.
module github.com/robert-crandall-org/example-app

go 1.26

require (
	github.com/danielgtaylor/huma/v2 v2.39.0
	github.com/jackc/pgx/v5 v5.10.0
	github.com/robert-crandall-org/go-home-server v0.0.0
)

require (
	github.com/SherClockHolmes/webpush-go v1.4.0 // indirect
	github.com/go-chi/chi/v5 v5.3.1 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/joho/godotenv v1.5.1 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/modelcontextprotocol/go-sdk v1.6.1 // indirect
	github.com/pressly/goose/v3 v3.27.3 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/sethvargo/go-retry v0.4.0 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
)

// Develop the app against the foundation living in this same repository.
// Remove this when the app lives in its own repo.
replace github.com/robert-crandall-org/go-home-server => ../../
