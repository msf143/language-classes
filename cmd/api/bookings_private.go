package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// privateRequest is the "private" part of a POST /bookings body: a time from
// GET /private-slots. The session doesn't exist yet; booking creates it.
type privateRequest struct {
	TeacherID       string    `json:"teacher_id"`
	StartsAt        time.Time `json:"starts_at"`
	DurationMinutes int       `json:"duration_minutes"`
	TargetLanguage  string    `json:"target_language"` // "es" or "en": what the class teaches
}

// check validates a private request, returning a message for the client or "".
func (p *privateRequest) check() string {
	switch {
	case !isUUID(p.TeacherID):
		return "private.teacher_id must be a teacher ID"
	case p.StartsAt.IsZero() || !p.StartsAt.Equal(p.StartsAt.Truncate(slotGrid)):
		return "private.starts_at must be a time on the quarter hour, from GET /private-slots"
	case p.DurationMinutes < 15 || p.DurationMinutes > 240:
		return "private.duration_minutes must be the class length in minutes"
	case !oneOf(p.TargetLanguage, "es", "en"):
		return "private.target_language must be 'es' or 'en'"
	}
	p.TeacherID = strings.ToLower(p.TeacherID)
	p.StartsAt = p.StartsAt.UTC()
	return ""
}

// bookPrivateSession books a private class in one transaction: it creates the
// session and books it for the payer's learners. Same shape and same shared
// steps as bookGroupSession, with two differences:
//   - instead of seats, it checks the time is still bookable: a length the
//     teacher offers, at least 1 hour ahead, and inside his open availability;
//   - no app lock guards the time slot. Two students may race for 7 PM; the
//     exclusion constraint on session.occupied lets only one session in, and
//     the other transaction rolls back (no charge) with "slot_taken".
func bookPrivateSession(ctx context.Context, pool *pgxpool.Pool, payerID, key string, req bookingRequest) (b bookingDTO, created bool, err error) {
	p := req.Private
	start := p.StartsAt
	end := start.Add(time.Duration(p.DurationMinutes) * time.Minute)

	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// 1-2. Lock the payer; seen this key before? Then it must be this same class.
		existing, err := lockPayerAndFindKey(ctx, tx, payerID, key)
		if err != nil {
			return err
		}
		if existing != nil {
			var same bool
			err := tx.QueryRow(ctx, `
				SELECT kind = 'private' AND teacher_id = $2 AND starts_at = $3 AND ends_at = $4
				FROM session WHERE id = $1`, existing.SessionID, p.TeacherID, start, end).Scan(&same)
			if err != nil {
				return err
			}
			if !same || !slices.Equal(existing.LearnerIDs, req.LearnerIDs) {
				return errKeyReused
			}
			b = *existing
			return nil
		}

		// 3. Is this time still bookable?
		var offered, enoughNotice, inside bool
		err = tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM teacher
			               WHERE id = $1 AND status = 'active' AND $4 = ANY (offered_durations)),
			       $2 >= now() + interval '1 hour',
			       -- range_agg merges his open blocks into one multirange (touching
			       -- blocks join up, like mergeTouching in Go); @> = "contains".
			       COALESCE((SELECT range_agg(period) FROM availability_block
			                 WHERE teacher_id = $1 AND status = 'open' AND period && tstzrange($2, $3))
			                @> tstzrange($2, $3), false)`,
			p.TeacherID, start, end, p.DurationMinutes).Scan(&offered, &enoughNotice, &inside)
		if err != nil {
			return err
		}
		switch {
		case !offered:
			return notBookable("slot_unavailable", "this teacher doesn't offer classes of that length")
		case !enoughNotice:
			return notBookable("slot_unavailable", "private classes must be booked at least 1 hour ahead")
		case !inside:
			return notBookable("slot_unavailable", "the teacher isn't available at that time")
		}

		// 4. The learners: the payer's own and active. Any age: the class is theirs.
		minors, err := checkLearners(ctx, tx, payerID, req.LearnerIDs)
		if err != nil {
			return err
		}
		partySize := len(req.LearnerIDs)

		// 5. The private price for this length and party size.
		pricePerPerson, currency, err := priceFor(ctx, tx, p.TeacherID, "private", p.DurationMinutes, partySize)
		if err != nil {
			return err
		}

		// 6. Create the session. occupied = the class itself (the teacher's
		//    break is inside it). If another class already holds any of this
		//    time, the exclusion constraint refuses the insert: someone was faster.
		audience := "all" // a mix of ages
		switch minors {
		case 0:
			audience = "adults"
		case partySize:
			audience = "kids"
		}
		sessionID := newUUIDv7()
		_, err = tx.Exec(ctx, `
			INSERT INTO session (id, teacher_id, kind, target_language, audience,
			                     starts_at, ends_at, occupied, min_seats, max_seats)
			VALUES ($1, $2, 'private', $3, $4, $5, $6, tstzrange($5, $6), 1, $7)`,
			sessionID, p.TeacherID, p.TargetLanguage, audience, start, end, partySize)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == exclusionViolate {
			return notBookable("slot_taken", "sorry, that time was just taken")
		}
		if err != nil {
			return err
		}

		// 7. Enough credit? Then the booking, attendees, ledger and outbox.
		b = bookingDTO{
			SessionID: sessionID, LearnerIDs: req.LearnerIDs, PartySize: partySize,
			PricePerPersonCents: pricePerPerson, TotalCents: pricePerPerson * partySize, Currency: currency,
		}
		if err := chargeAndWrite(ctx, tx, payerID, key, &b); err != nil {
			return err
		}
		created = true
		return nil
	})
	return b, created, err
}
