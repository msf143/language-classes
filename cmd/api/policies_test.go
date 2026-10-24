package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// GET /policies states the numbers the code enforces. In a fresh schema the
// credit policy is the migration's (24 months, 30 days); the break is the
// longest of the active teachers' (a retired teacher's doesn't count).
func TestGetPolicies(t *testing.T) {
	pool := migratedSchemaPool(t, 2)
	ctx := context.Background()
	for _, tc := range []struct {
		buffer int
		status string
	}{{5, "active"}, {3, "active"}, {20, "inactive"}} {
		id := newTeacher(t, pool)
		if _, err := pool.Exec(ctx, `UPDATE teacher SET buffer_minutes = $2, status = $3 WHERE id = $1`, id, tc.buffer, tc.status); err != nil {
			t.Fatal(err)
		}
	}

	rec := call(getPolicies(pool), "GET", "/policies", nil)
	var got policiesDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s (%v)", rec.Code, rec.Body, err)
	}
	want := policiesDTO{
		FullRefundHours: 24, LateRefundPercent: 50, GroupMinStudents: 2, GroupDecisionHours: 24,
		PrivateNoticeMinutes: 60, MaxPartySize: 4, BreakMinutes: 5, CreditExpireMonths: 24, CreditWarnDays: 30,
	}
	if got != want {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

// The page's numbers must match what the database enforces too: these
// constants have twins in CHECK constraints, and this fails if one changes
// without the other.
func TestPolicyConstantsMatchSchema(t *testing.T) {
	pool := migratedSchemaPool(t, 2)
	ctx := context.Background()
	teacher := newTeacher(t, pool)
	tryGroup := func(minSeats int) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO session (id, teacher_id, kind, target_language, audience, starts_at, ends_at, occupied, min_seats, status)
			VALUES ($1, $2, 'group', 'es', 'adults', now() + interval '3 days', now() + interval '3 days 1 hour',
			        tstzrange(now() + interval '3 days', now() + interval '3 days 1 hour'), $3, 'scheduled')`,
			newUUIDv7(), teacher, minSeats)
		return err
	}
	if err := tryGroup(groupMinStudents - 1); err == nil {
		t.Errorf("a group class requiring %d was accepted; groupMinStudents (%d) is below the constraint", groupMinStudents-1, groupMinStudents)
	}
	if err := tryGroup(groupMinStudents); err != nil {
		t.Errorf("a group class requiring %d was refused (%v); groupMinStudents is above the constraint", groupMinStudents, err)
	}
	var partyCheck string
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = 'booking'::regclass AND pg_get_constraintdef(oid) LIKE '%party_size <=%'`).Scan(&partyCheck); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("CHECK (((party_size >= 1) AND (party_size <= %d)))", maxPartySize); partyCheck != want {
		t.Errorf("booking party_size constraint %q, want %q (maxPartySize)", partyCheck, want)
	}
}
