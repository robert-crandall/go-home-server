package auth

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/robert-crandall/go-home-server/db"
	"github.com/robert-crandall/go-home-server/migrations"
)

// These tests need a real Postgres. They skip cleanly when TEST_DATABASE_URL is
// unset so unit tests still run anywhere.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
	}
	if err := db.Migrate(url, db.MigrationSource{
		FS: migrations.FS, Dir: migrations.Dir, TableName: migrations.TableName,
	}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `DELETE FROM users`); err != nil {
		t.Fatalf("clean: %v", err)
	}
	return pool
}

// The core of the security fix: with OpenRegistration off, many concurrent
// registrations must produce exactly one user (first-user-only, race-safe).
func TestFirstUserRegistrationIsSerialized(t *testing.T) {
	pool := testPool(t)
	svc := NewService(pool, false) // OpenRegistration defaults to false

	const n = 10
	var wg sync.WaitGroup
	results := make(chan error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, _, _, err := svc.registerUser(
				context.Background(), fmt.Sprintf("user%d@example.com", i), "supersecret")
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)

	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("expected exactly 1 successful registration, got %d", success)
	}

	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 user row, got %d", count)
	}
}

// With OpenRegistration on, multiple users can register.
func TestOpenRegistrationAllowsMany(t *testing.T) {
	pool := testPool(t)
	svc := NewService(pool, false)
	svc.OpenRegistration = true

	for i := 0; i < 3; i++ {
		if _, _, _, err := svc.registerUser(
			context.Background(), fmt.Sprintf("open%d@example.com", i), "supersecret"); err != nil {
			t.Fatalf("registration %d failed: %v", i, err)
		}
	}

	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("expected 3 user rows, got %d", count)
	}
}
