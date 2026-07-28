package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrateIsolatesSourcesByTableName applies two migration sources whose
// version numbers deliberately collide.
//
// That collision is the whole design: shared migrations and an app's own
// migrations both start at 00001, and stay independent only because Migrate
// gives each source its own goose version table. Migrate configures that per
// source inside its loop, mutating goose's package-global state each time.
// Hoisting SetTableName out of the loop - an inviting cleanup - makes the
// second source read as already-applied and silently skip; hoisting SetBaseFS
// makes it re-run the first source's files. This test catches either.
//
// Every other test in this module passes a single source, which can't catch
// that. This one can.
func TestMigrateIsolatesSourcesByTableName(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
	}

	// Unique per run so repeated runs don't collide, and so a failure leaves
	// the previous run's tables alone.
	suffix := fmt.Sprintf("%d", os.Getpid())
	tableA := "migrate_test_version_a_" + suffix
	tableB := "migrate_test_version_b_" + suffix
	rowsA := "migrate_test_rows_a_" + suffix
	rowsB := "migrate_test_rows_b_" + suffix

	migration := func(table string) string {
		return "-- +goose Up\nCREATE TABLE " + table + " (id int primary key);\n"
	}

	// Note both sources number their first migration 00001.
	sourceA := fstest.MapFS{
		"00001_init.sql": &fstest.MapFile{Data: []byte(migration(rowsA))},
	}
	sourceB := fstest.MapFS{
		"00001_init.sql": &fstest.MapFile{Data: []byte(migration(rowsB))},
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Registered first so it runs last: cleanups are LIFO, and the drops below
	// need the pool open.
	t.Cleanup(pool.Close)

	t.Cleanup(func() {
		for _, table := range []string{rowsA, rowsB, tableA, tableB} {
			if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
				t.Errorf("cleanup %s: %v", table, err)
			}
		}
	})

	if err := Migrate(url,
		MigrationSource{FS: sourceA, Dir: ".", TableName: tableA},
		MigrationSource{FS: sourceB, Dir: ".", TableName: tableB},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Both sources ran despite sharing version 00001...
	for _, table := range []string{rowsA, rowsB} {
		if !tableExists(ctx, t, pool, table) {
			t.Errorf("table %s missing: its migration source did not run", table)
		}
	}

	// ...because each tracked its history in its own version table.
	for _, table := range []string{tableA, tableB} {
		if !tableExists(ctx, t, pool, table) {
			t.Errorf("version table %s missing: sources are not isolated", table)
		}
	}
}

func tableExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists)
	if err != nil {
		t.Fatalf("check %s: %v", name, err)
	}
	return exists
}
