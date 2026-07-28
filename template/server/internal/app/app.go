// Package app holds wiring shared by the app's commands (server, migrate).
package app

import (
	"github.com/robert-crandall/go-home-server/db"
	shared "github.com/robert-crandall/go-home-server/migrations"

	"github.com/robert-crandall/example-app/internal/notes"
)

// Migrate applies the foundation's shared migrations followed by the app's own
// feature migrations. Each source tracks its own goose version history.
func Migrate(databaseURL string) error {
	return db.Migrate(databaseURL,
		db.MigrationSource{FS: shared.FS, Dir: shared.Dir, TableName: shared.TableName},
		db.MigrationSource{FS: notes.MigrationsFS, Dir: "migrations"},
	)
}
