package main

import (
	"context"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Version nibble must be 7; variant nibble must be 8, 9, a or b.
var uuidV7Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewUUIDv7Format(t *testing.T) {
	seen := make(map[string]bool)
	for range 1000 {
		id := newUUIDv7()
		if !uuidV7Pattern.MatchString(id) {
			t.Fatalf("not a v7 UUID: %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate ID: %q", id)
		}
		seen[id] = true
	}
}

func TestNewUUIDv7SortsByTime(t *testing.T) {
	first := newUUIDv7()
	time.Sleep(2 * time.Millisecond) // move to a later millisecond
	second := newUUIDv7()
	if first >= second {
		t.Fatalf("expected %q < %q", first, second)
	}
}

// Postgres must accept the ID as a uuid and give back the same text.
func TestNewUUIDv7AcceptedByPostgres(t *testing.T) {
	pool := testPool(t)
	id := newUUIDv7()
	var got string
	if err := pool.QueryRow(context.Background(), "SELECT $1::uuid::text", id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != id {
		t.Fatalf("round trip changed the ID: sent %q, got %q", id, got)
	}
}

// testPool connects to the real dev database, or skips the test when
// DATABASE_URL isn't set (for example, outside the devcontainer).
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping database test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
