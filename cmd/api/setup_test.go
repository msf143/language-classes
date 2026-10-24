package main

import (
	"context"
	"testing"
)

// `api teacher`: a new person, the same person again, an existing student, a
// retired teacher, and bad input.
func TestMakeTeacher(t *testing.T) {
	pool := migratedSchemaPool(t, 2)
	ctx := context.Background()

	first, err := makeTeacher(ctx, pool, " Carlos@Example.com ", "Carlos", "America/Bogota")
	if err != nil {
		t.Fatal(err)
	}
	var name, zone string
	var durations []int
	if err := pool.QueryRow(ctx, `
		SELECT a.display_name, a.timezone, t.offered_durations
		FROM teacher t JOIN account a ON a.id = t.account_id WHERE t.id = $1`, first.teacherID).
		Scan(&name, &zone, &durations); err != nil {
		t.Fatal(err)
	}
	if !first.newAccount || !first.newTeacher || first.email != "carlos@example.com" || name != "Carlos" ||
		zone != "America/Bogota" || len(durations) != 3 || durations[2] != 60 {
		t.Errorf("first run: %+v, name %q, zone %q, lengths %v", first, name, zone, durations)
	}

	t.Run("again: same account, same teacher, nothing new", func(t *testing.T) {
		again, err := makeTeacher(ctx, pool, "carlos@example.com", "", "")
		if err != nil || again.accountID != first.accountID || again.teacherID != first.teacherID || again.newAccount || again.newTeacher {
			t.Errorf("%+v (err %v)", again, err)
		}
		if n := count(t, pool, `SELECT count(*) FROM teacher`); n != 1 {
			t.Errorf("%d teachers, want 1", n)
		}
	})

	t.Run("an existing student becomes a teacher on the same account", func(t *testing.T) {
		id, email := newAccount(t, pool)
		got, err := makeTeacher(ctx, pool, email, "", "")
		if err != nil || got.accountID != id || got.newAccount || !got.newTeacher {
			t.Errorf("%+v (err %v), want account %s reused", got, err, id)
		}
	})

	t.Run("a retired teacher is made active again", func(t *testing.T) {
		pool.Exec(ctx, `UPDATE teacher SET status = 'inactive' WHERE id = $1`, first.teacherID)
		if _, err := makeTeacher(ctx, pool, "carlos@example.com", "", ""); err != nil {
			t.Fatal(err)
		}
		if n := count(t, pool, `SELECT count(*) FROM teacher WHERE id = $1 AND status = 'active'`, first.teacherID); n != 1 {
			t.Error("still inactive")
		}
	})

	t.Run("bad input changes nothing", func(t *testing.T) {
		for _, tc := range []struct{ email, zone string }{{"", ""}, {"not-an-email", ""}, {"x@example.com", "Bogota"}, {"x@example.com", "Mars/Olympus"}} {
			if _, err := makeTeacher(ctx, pool, tc.email, "", tc.zone); err == nil {
				t.Errorf("email %q zone %q accepted", tc.email, tc.zone)
			}
		}
		if n := count(t, pool, `SELECT count(*) FROM account WHERE lower(email) = 'x@example.com'`); n != 0 {
			t.Errorf("bad input created an account")
		}
	})
}
