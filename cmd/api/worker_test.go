package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/msf143/language-classes/migrations"
)

// migratedSchemaPool is a pool whose connections work in a new schema with all
// migrations applied: a private, empty copy of our tables (dropped afterwards).
// Worker tests need it because the dev database's own outbox is full of other
// tests' events. "public" stays on the search_path for the btree_gist extension.
func migratedSchemaPool(t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()
	admin := testPool(t)
	ctx := context.Background()
	schema := "test_worker_" + strings.ReplaceAll(newUUIDv7(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })

	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ", public"
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := runMigrations(ctx, pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	return pool
}

// addEvent queues an outbox event created `age` ago and returns its ID.
func addEvent(t *testing.T, pool *pgxpool.Pool, eventType string, age time.Duration) string {
	t.Helper()
	id := newUUIDv7()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO outbox_event (id, aggregate_type, aggregate_id, event_type, payload, created_at)
		VALUES ($1, 'booking', $2, $3, '{}', now() - $4::int * interval '1 second')`,
		id, newUUIDv7(), eventType, int(age.Seconds()))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

type eventRow struct {
	processed bool
	attempts  int
	lastError *string
	dueIn     time.Duration // available_at - now()
}

func eventState(t *testing.T, pool *pgxpool.Pool, id string) eventRow {
	t.Helper()
	var r eventRow
	var dueSeconds float64
	err := pool.QueryRow(context.Background(), `
		SELECT processed_at IS NOT NULL, attempts, last_error, extract(epoch FROM available_at - now())
		FROM outbox_event WHERE id = $1`, id).Scan(&r.processed, &r.attempts, &r.lastError, &dueSeconds)
	if err != nil {
		t.Fatal(err)
	}
	r.dueIn = time.Duration(dueSeconds * float64(time.Second))
	return r
}

func TestDrainOutbox(t *testing.T) {
	ctx := context.Background()

	t.Run("handles due events oldest first, then marks them processed", func(t *testing.T) {
		pool := migratedSchemaPool(t, 4)
		ids := []string{addEvent(t, pool, "a", 3*time.Minute), addEvent(t, pool, "b", 2*time.Minute), addEvent(t, pool, "c", time.Minute)}
		var seen []string
		stats, err := drainOutbox(ctx, pool, func(_ context.Context, ev outboxEvent) error {
			seen = append(seen, ev.EventType)
			return nil
		}, 100)
		if err != nil || stats.Processed != 3 || strings.Join(seen, "") != "abc" {
			t.Fatalf("stats %+v, err %v, order %v; want 3 processed in order a b c", stats, err, seen)
		}
		for _, id := range ids {
			if r := eventState(t, pool, id); !r.processed || r.lastError != nil {
				t.Errorf("event %+v, want processed without an error", r)
			}
		}
		if again, _ := drainOutbox(ctx, pool, func(context.Context, outboxEvent) error { return nil }, 100); again.Processed != 0 {
			t.Errorf("a second drain processed %d, want 0", again.Processed)
		}
	})

	t.Run("a failure is retried later, with the error kept", func(t *testing.T) {
		pool := migratedSchemaPool(t, 4)
		id := addEvent(t, pool, "x", time.Minute)
		fail := func(context.Context, outboxEvent) error { return errors.New("mail server down") }

		if stats, _ := drainOutbox(ctx, pool, fail, 100); stats.Failed != 1 {
			t.Fatalf("stats %+v, want 1 failed", stats)
		}
		r := eventState(t, pool, id)
		if r.processed || r.attempts != 1 || r.lastError == nil || *r.lastError != "mail server down" ||
			r.dueIn < 50*time.Second || r.dueIn > 70*time.Second {
			t.Errorf("after one failure: %+v; want unprocessed, 1 attempt, the error, due in about 1 minute", r)
		}
		// Not due yet: the next drain leaves it alone.
		if stats, _ := drainOutbox(ctx, pool, fail, 100); stats.Failed+stats.Processed != 0 {
			t.Errorf("retried before it was due: %+v", stats)
		}
		// Once due, it's tried again, and success clears the error.
		pool.Exec(ctx, `UPDATE outbox_event SET available_at = now() WHERE id = $1`, id)
		if stats, _ := drainOutbox(ctx, pool, func(context.Context, outboxEvent) error { return nil }, 100); stats.Processed != 1 {
			t.Fatalf("retry: %+v, want 1 processed", stats)
		}
		if r := eventState(t, pool, id); !r.processed || r.lastError != nil {
			t.Errorf("after the successful retry: %+v", r)
		}
	})

	t.Run("gives up after the last attempt", func(t *testing.T) {
		pool := migratedSchemaPool(t, 4)
		id := addEvent(t, pool, "x", time.Minute)
		pool.Exec(ctx, `UPDATE outbox_event SET attempts = $2 WHERE id = $1`, id, maxAttempts-1)
		drainOutbox(ctx, pool, func(context.Context, outboxEvent) error { return errors.New("still down") }, 100)
		pool.Exec(ctx, `UPDATE outbox_event SET available_at = now() WHERE id = $1`, id)
		called := false
		drainOutbox(ctx, pool, func(context.Context, outboxEvent) error { called = true; return nil }, 100)
		if r := eventState(t, pool, id); called || r.processed || r.attempts != maxAttempts {
			t.Errorf("called again: %v, event %+v; want left alone at %d attempts", called, r, maxAttempts)
		}
	})

	t.Run("stale events and skips are marked processed without retrying", func(t *testing.T) {
		pool := migratedSchemaPool(t, 4)
		stale := addEvent(t, pool, "old", 3*24*time.Hour)
		unknown := addEvent(t, pool, "unknown_type", time.Minute)
		var handled []string
		stats, _ := drainOutbox(ctx, pool, func(_ context.Context, ev outboxEvent) error {
			handled = append(handled, ev.EventType)
			return skip("no handler for " + ev.EventType)
		}, 100)
		if stats.Skipped != 2 || strings.Join(handled, ",") != "unknown_type" {
			t.Errorf("stats %+v, handler saw %v; want 2 skipped, the stale one never handed to the handler", stats, handled)
		}
		if r := eventState(t, pool, stale); !r.processed || r.lastError == nil || *r.lastError != "skipped: stale" {
			t.Errorf("stale event: %+v", r)
		}
		if r := eventState(t, pool, unknown); !r.processed || r.lastError == nil || *r.lastError != "skipped: no handler for unknown_type" {
			t.Errorf("unknown event: %+v", r)
		}
	})
}

// Two workers draining at once work side by side, each on a different event
// (SKIP LOCKED), and every event is handled exactly once.
//
// What SKIP LOCKED changes is concurrency, not correctness: without it, worker
// B waits for the row worker A has locked; when A commits, Postgres re-checks
// the row for B, sees it's processed, and moves on to the next one. So both
// still get events, but strictly taking turns: two handlers never run at the
// same time. That's what this test measures (checked: without SKIP LOCKED the
// most handlers running at once is 1).
func TestDrainOutboxTwoWorkers(t *testing.T) {
	pool := migratedSchemaPool(t, 8)
	const events = 20
	for i := range events {
		addEvent(t, pool, "e", time.Duration(events-i)*time.Second)
	}

	var mu sync.Mutex
	handled := map[string]int{} // event ID → times handled
	running, mostAtOnce := 0, 0
	handler := func(_ context.Context, ev outboxEvent) error {
		mu.Lock()
		handled[ev.ID]++
		running++
		mostAtOnce = max(mostAtOnce, running)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond) // like sending an email
		mu.Lock()
		running--
		mu.Unlock()
		return nil
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); drainOutbox(context.Background(), pool, handler, 100) }()
	}
	wg.Wait()

	if len(handled) != events {
		t.Errorf("%d events handled, want %d", len(handled), events)
	}
	for id, n := range handled {
		if n != 1 {
			t.Errorf("event %s handled %d times, want exactly once", id, n)
		}
	}
	if mostAtOnce != 2 {
		t.Errorf("at most %d handler(s) ran at once; want 2 (the workers should work side by side)", mostAtOnce)
	}
}

func TestRetryDelay(t *testing.T) {
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute,
		32 * time.Minute, time.Hour, time.Hour, time.Hour, time.Hour}
	for i, w := range want {
		if got := retryDelay(i + 1); got != w {
			t.Errorf("retryDelay(%d) = %v, want %v", i+1, got, w)
		}
	}
}
