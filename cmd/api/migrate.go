package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrateLockKey is an arbitrary number naming the Postgres advisory lock that
// stops two migration runs (say, two deploys close together) from overlapping.
const migrateLockKey = 4_700_100_001

// runMigrations applies every *.up.sql file in files whose version is newer
// than the database's, in version order. It's what `api migrate` runs in the
// Cloud Run migration job before each deploy.
//
// It keeps its bookkeeping in schema_migrations, the same one-row table the
// golang-migrate CLI uses in dev, so the two stay interchangeable. One
// difference: each file runs in a transaction together with its version bump,
// so a failing migration rolls back completely (Postgres DDL is transactional)
// instead of leaving the database "dirty". The flip side: a migration can't use
// statements that refuse to run in a transaction (CREATE INDEX CONCURRENTLY).
// Only "up" is supported; down migrations are a dev tool.
func runMigrations(ctx context.Context, pool *pgxpool.Pool, files fs.FS) error {
	pending, err := migrationFiles(files)
	if err != nil {
		return err
	}

	// An advisory lock belongs to one database connection, so hold one
	// connection for the whole run.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLockKey); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrateLockKey)

	// Same shape as golang-migrate's table.
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations
		(version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var current int64
	var dirty bool
	err = conn.QueryRow(ctx, "SELECT version, dirty FROM schema_migrations LIMIT 1").Scan(&current, &dirty)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read schema version: %w", err)
	}
	if dirty {
		// Only golang-migrate can leave this behind (a failed run in dev).
		return fmt.Errorf("database is marked dirty at version %d: fix by hand, then `migrate force`", current)
	}

	applied := 0
	for _, m := range pending {
		if m.version <= current {
			continue
		}
		sql, err := fs.ReadFile(files, m.name)
		if err != nil {
			return err
		}
		if err := applyMigration(ctx, conn, m.version, string(sql)); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		slog.Info("migration applied", "file", m.name, "version", m.version)
		applied++
	}
	slog.Info("migrations done", "applied", applied, "version", max(current, lastVersion(pending)))
	return nil
}

// applyMigration runs one file and records its version, all or nothing.
func applyMigration(ctx context.Context, conn *pgxpool.Conn, version int64, sql string) error {
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		// With no arguments, pgx sends the text as-is (Postgres "simple query"),
		// which allows a whole file of statements in one call.
		if _, err := tx.Exec(ctx, sql); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM schema_migrations"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, dirty) VALUES ($1, false)", version)
		return err
	})
}

type migrationFile struct {
	version int64
	name    string
}

// migrationFiles lists the *.up.sql files, sorted by the number before the
// first "_" (000003_auth.up.sql is version 3). Duplicate versions are an error.
func migrationFiles(files fs.FS) ([]migrationFile, error) {
	names, err := fs.Glob(files, "*.up.sql")
	if err != nil {
		return nil, err
	}
	var list []migrationFile
	seen := map[int64]string{}
	for _, name := range names {
		prefix, _, _ := strings.Cut(name, "_")
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migration %q: name must start with a number and _", name)
		}
		if other, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %q and %q have the same version %d", other, name, v)
		}
		seen[v] = name
		list = append(list, migrationFile{v, name})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].version < list[j].version })
	return list, nil
}

func lastVersion(list []migrationFile) int64 {
	if len(list) == 0 {
		return 0
	}
	return list[len(list)-1].version
}
