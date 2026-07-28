// Command migrate applies all database migrations (foundation + app) and exits.
// The server also migrates on boot; this exists for CI and manual runs.
package main

import (
	"log"

	"github.com/robert-crandall/go-home-server/config"

	"github.com/robert-crandall/example-app/internal/app"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := app.Migrate(cfg.DatabaseURL); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	log.Println("migrations applied")
}
