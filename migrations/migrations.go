// Package migrations holds the goose SQL migrations shared by every app built
// on this foundation: users, sessions, push subscriptions, and API tokens.
//
// Apps apply these alongside their own migrations via db.Migrate, giving the
// shared migrations a dedicated goose version table so version numbers never
// collide with the app's own migrations.
package migrations

import "embed"

// FS is the embedded set of shared migration files.
//
//go:embed *.sql
var FS embed.FS

// Dir is the directory within FS that contains the migrations. Because the
// files are embedded at the package root, this is ".".
const Dir = "."

// TableName is the goose version table the shared migrations track themselves
// in, kept separate from an app's own migration history.
const TableName = "goose_shared_version"
