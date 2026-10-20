package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/msf143/language-classes/migrations"
)

// The real migration files must be named so runMigrations can order them.
func TestEmbeddedMigrationFiles(t *testing.T) {
	list, err := migrationFiles(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Fatal("no migrations embedded")
	}
	for i, m := range list {
		if m.version != int64(i+1) {
			t.Fatalf("migration %d is %s; versions should run 1, 2, 3... with no gaps", i+1, m.name)
		}
	}
}

func TestMigrationFilesRejectsBadNames(t *testing.T) {
	tests := []struct {
		name  string
		files fstest.MapFS
	}{
		{"no number", fstest.MapFS{"init.up.sql": {}}},
		{"duplicate version", fstest.MapFS{"1_a.up.sql": {}, "0001_b.up.sql": {}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := migrationFiles(tt.files); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// TestRunMigrations runs fake migrations in a throwaway Postgres schema, so it
// never touches the real tables or the real schema_migrations.
func TestRunMigrations(t *testing.T) {
	pool := schemaPool(t)
	ctx := context.Background()

	files := fstest.MapFS{
		"000001_create.up.sql": {Data: []byte("CREATE TABLE t (x int);")},
		// Several statements in one file, like real migrations.
		"000002_fill.up.sql": {Data: []byte("INSERT INTO t VALUES (1);\nINSERT INTO t VALUES (2);")},
	}
	if err := runMigrations(ctx, pool, files); err != nil {
		t.Fatal(err)
	}
	assertVersion(t, pool, 2)
	assertRows(t, pool, 2)

	// Running again changes nothing (idempotent).
	if err := runMigrations(ctx, pool, files); err != nil {
		t.Fatal(err)
	}
	assertRows(t, pool, 2)

	// A failing migration rolls back completely: no table u, version still 2.
	files["000003_bad.up.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE u (x int);\nSELECT 1/0;")}
	err := runMigrations(ctx, pool, files)
	if err == nil || !strings.Contains(err.Error(), "000003_bad.up.sql") {
		t.Fatalf("want an error naming the bad file, got %v", err)
	}
	assertVersion(t, pool, 2)
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('u') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("table u exists: the failed migration was not rolled back")
	}

	// Fixed and rerun, it applies.
	files["000003_bad.up.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE u (x int);")}
	if err := runMigrations(ctx, pool, files); err != nil {
		t.Fatal(err)
	}
	assertVersion(t, pool, 3)
}

// schemaPool returns a pool whose connections work in a new, empty schema
// (dropped when the test ends) instead of the dev database's real one.
func schemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := testPool(t) // skips the test when DATABASE_URL isn't set
	ctx := context.Background()
	schema := "test_migrate_" + strings.ReplaceAll(newUUIDv7(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })

	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close) // registered last, so it runs before the DROP SCHEMA
	return pool
}

func assertVersion(t *testing.T, pool *pgxpool.Pool, want int64) {
	t.Helper()
	var v int64
	var dirty bool
	if err := pool.QueryRow(context.Background(), "SELECT version, dirty FROM schema_migrations").Scan(&v, &dirty); err != nil {
		t.Fatal(err)
	}
	if v != want || dirty {
		t.Fatalf("schema_migrations = (%d, dirty=%v), want (%d, false)", v, dirty, want)
	}
}

func assertRows(t *testing.T, pool *pgxpool.Pool, want int) {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Fatalf("t has %d rows, want %d", n, want)
	}
}
