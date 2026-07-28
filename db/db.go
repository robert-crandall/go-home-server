// Package db wires up Postgres access and migrations for the foundation.
//
// Runtime queries use a pgx connection pool (no ORM). Migrations run through
// database/sql using pgx's stdlib adapter, so goose can manage them.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
)

// goose keeps dialect/base-FS/table-name in package-global state, so serialize
// Migrate calls to keep concurrent callers from interleaving those settings.
var migrateMu sync.Mutex

// New opens and verifies a pgx connection pool.
func New(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("db: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// MigrationSource is one embedded set of goose migrations to apply. TableName
// lets each source track its own version history so shared and app-specific
// migrations can both start numbering at 00001 without colliding.
type MigrationSource struct {
	FS        fs.FS
	Dir       string
	TableName string // optional; defaults to goose's "goose_db_version"
}

// Migrate applies each migration source in order against the given database
// URL. It is safe to call on every boot; goose only runs pending migrations.
func Migrate(url string, sources ...MigrationSource) error {
	// Serialize because goose configuration below is package-global.
	migrateMu.Lock()
	defer migrateMu.Unlock()

	sqldb, err := sql.Open("pgx", url)
	if err != nil {
		return fmt.Errorf("db: open for migrate: %w", err)
	}
	defer sqldb.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("db: goose dialect: %w", err)
	}

	for _, s := range sources {
		table := s.TableName
		if table == "" {
			table = "goose_db_version"
		}
		goose.SetTableName(table)
		goose.SetBaseFS(s.FS)

		dir := s.Dir
		if dir == "" {
			dir = "."
		}
		if err := goose.Up(sqldb, dir); err != nil {
			return fmt.Errorf("db: migrate %q: %w", table, err)
		}
	}
	return nil
}
