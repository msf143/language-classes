package main

// `api teacher -email ...`: make an account a teacher. There's deliberately no
// web page that can do this: on production it runs as a one-off execution of
// the Cloud Run job "migrate" with these arguments instead of "migrate"
// (docs/deploy.md), so who may run it is decided by Google Cloud permissions.
//
// Safe to run again: an existing account is reused, an existing teacher is
// reactivated, nothing is duplicated.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Lengths a new teacher offers for private classes, matching the prices the
// teacher page asks for (the column's own default also has 90).
var defaultOfferedDurations = []int{30, 45, 60}

// runMakeTeacher parses the command's flags and makes the account a teacher.
func runMakeTeacher(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	fs := flag.NewFlagSet("teacher", flag.ContinueOnError)
	email := fs.String("email", "", "the teacher's email (required); they sign in with it")
	name := fs.String("name", "", "display name (optional; a new account otherwise gets the part before @)")
	zone := fs.String("timezone", "", `time zone, e.g. "America/Bogota" (optional; the teacher page shows times in it)`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	t, err := makeTeacher(ctx, pool, *email, *name, *zone)
	if err != nil {
		return err
	}
	slog.Info("teacher ready", "email", t.email, "account_id", t.accountID, "teacher_id", t.teacherID,
		"new_account", t.newAccount, "new_teacher", t.newTeacher)
	return nil
}

type madeTeacher struct {
	email, accountID, teacherID string
	newAccount, newTeacher      bool
}

// makeTeacher finds or creates the account for email, then its teacher row,
// in one transaction (all or nothing).
func makeTeacher(ctx context.Context, pool *pgxpool.Pool, email, name, zone string) (madeTeacher, error) {
	t := madeTeacher{email: strings.ToLower(strings.TrimSpace(email))}
	if !strings.Contains(t.email, "@") {
		return t, errors.New("-email is required and must be an email address")
	}
	if zone != "" {
		if _, err := time.LoadLocation(zone); err != nil || !strings.Contains(zone, "/") {
			return t, fmt.Errorf("-timezone %q is not a time zone name like America/Bogota", zone)
		}
	}
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		t.accountID, t.newAccount, err = findOrCreateAccount(ctx, tx, t.email, name)
		if err != nil {
			return err
		}
		if zone != "" {
			if _, err := tx.Exec(ctx, `UPDATE account SET timezone = $2 WHERE id = $1`, t.accountID, zone); err != nil {
				return err
			}
		}
		// The teacher row: new, or an existing one made active again.
		// (xmax = 0) is Postgres's way of telling, in the RETURNING of an
		// upsert, that the row was inserted rather than updated.
		return tx.QueryRow(ctx, `
			INSERT INTO teacher (id, account_id, offered_durations) VALUES ($1, $2, $3)
			ON CONFLICT (account_id) DO UPDATE SET status = 'active'
			RETURNING id::text, (xmax = 0)`, newUUIDv7(), t.accountID, defaultOfferedDurations).
			Scan(&t.teacherID, &t.newTeacher)
	})
	return t, err
}

// findOrCreateAccount returns the account for email (already lower-cased),
// creating it if this person never signed in: one account per email, whatever
// the capitals, like sign-in. A non-empty name replaces the display name.
func findOrCreateAccount(ctx context.Context, tx pgx.Tx, email, name string) (id string, created bool, err error) {
	displayName := name
	if displayName == "" {
		displayName = email[:strings.Index(email, "@")]
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO account (id, email, display_name) VALUES ($1, $2, $3)
		ON CONFLICT ((lower(email))) DO NOTHING
		RETURNING id::text`, newUUIDv7(), email, displayName).Scan(&id)
	created = err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT id::text FROM account WHERE lower(email) = $1`, email).Scan(&id)
	}
	if err != nil {
		return "", false, err
	}
	if name != "" && !created {
		if _, err := tx.Exec(ctx, `UPDATE account SET display_name = $2 WHERE id = $1`, id, name); err != nil {
			return "", false, err
		}
	}
	return id, created, nil
}

// `api admin -email ... [-revoke]`: make an account an admin, or stop it
// being one. Like `api teacher`, only through a one-off job execution.
func runMakeAdmin(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	fs := flag.NewFlagSet("admin", flag.ContinueOnError)
	email := fs.String("email", "", "the admin's email (required); they sign in with it")
	revoke := fs.Bool("revoke", false, "remove admin rights instead of granting them")
	if err := fs.Parse(args); err != nil {
		return err
	}
	accountID, created, err := setAdmin(ctx, pool, *email, !*revoke)
	if err != nil {
		return err
	}
	slog.Info("admin updated", "email", strings.ToLower(strings.TrimSpace(*email)), "account_id", accountID,
		"is_admin", !*revoke, "new_account", created)
	return nil
}

// setAdmin sets account.is_admin for email, creating the account when
// granting (revoking from someone without an account is an error).
func setAdmin(ctx context.Context, pool *pgxpool.Pool, email string, admin bool) (accountID string, created bool, err error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") {
		return "", false, errors.New("-email is required and must be an email address")
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if admin {
			var err error
			if accountID, created, err = findOrCreateAccount(ctx, tx, email, ""); err != nil {
				return err
			}
		} else if err := tx.QueryRow(ctx, `SELECT id::text FROM account WHERE lower(email) = $1`, email).Scan(&accountID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("no account for %s", email)
			}
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE account SET is_admin = $2 WHERE id = $1`, accountID, admin)
		return err
	})
	return accountID, created, err
}
