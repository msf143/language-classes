package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The cancellation policy, in one place. The policies page will show these
// same numbers (roadmap step 6).
const (
	fullRefundNotice        = 24 * time.Hour // cancel MORE than this before the start: full refund
	lateCancelRefundPercent = 50             // otherwise (until the start): this share back
)

// refundFor is the credit given back for cancelling a booking that cost
// totalCents, untilStart before the class begins. Pure arithmetic in whole
// cents: a percentage that doesn't come out even rounds up, in the student's
// favour (50% of 1525 is 762.5, so 763).
func refundFor(totalCents int, untilStart time.Duration) int {
	if untilStart > fullRefundNotice {
		return totalCents
	}
	return (totalCents*lateCancelRefundPercent + 99) / 100 // integer ceiling of total × percent / 100
}

// cancelBooking handles POST /bookings/{id}/cancel (wrapped in requireSignIn).
// Only the person who paid can cancel. Cancelling an already cancelled booking
// returns it unchanged, so a double click or a retry never refunds twice.
func cancelBooking(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		id := r.PathValue("id") // the {id} in the route pattern
		if !isUUID(id) {
			writeError(w, r, http.StatusBadRequest, "the booking ID in the path is not valid")
			return
		}

		b, err := cancelBookingTx(r.Context(), pool, me.ID, id)
		var be *bookingError
		switch {
		case errors.As(err, &be):
			writeErrorCode(w, r, be.status, be.code, be.msg)
		case err != nil:
			logFrom(r).Error("cancel booking failed", "err", err, "booking_id", id)
			writeError(w, r, http.StatusInternalServerError, "could not cancel the booking")
		default:
			logFrom(r).Info("booking cancelled", "booking_id", b.ID, "refund_cents", b.RefundCents)
			writeJSON(w, http.StatusOK, b)
		}
	}
}

// cancelBookingTx does the cancellation in one transaction and returns the
// booking as it now is, or a *bookingError when it can't be cancelled.
//
// Locks, in the same order as booking (account first), so the two can't
// deadlock: the payer's account row serializes this person's money movements;
// the booking row makes two cancel clicks queue up, so the second one sees
// "already cancelled". The session isn't locked: freeing a seat can't oversell.
func cancelBookingTx(ctx context.Context, pool *pgxpool.Pool, payerID, bookingID string) (b bookingDTO, err error) {
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// 1. Lock the payer.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM account WHERE id = $1 FOR UPDATE`, payerID); err != nil {
			return err
		}

		// 2. Lock the booking, if it's mine. "FOR UPDATE OF b" locks just the
		//    booking row, not the session row it's joined to. now() is the
		//    database's clock, the one every timestamp here comes from.
		var bk struct {
			status, sessionID, sessionKind, sessionStatus, currency string
			totalCents                                              int
			startsAt, now                                           time.Time
		}
		err := tx.QueryRow(ctx, `
			SELECT b.status, b.total_cents, b.currency, s.id, s.kind, s.status, s.starts_at, now()
			FROM booking b JOIN session s ON s.id = b.session_id
			WHERE b.id = $1 AND b.payer_account_id = $2
			FOR UPDATE OF b`, bookingID, payerID).
			Scan(&bk.status, &bk.totalCents, &bk.currency, &bk.sessionID, &bk.sessionKind, &bk.sessionStatus, &bk.startsAt, &bk.now)
		if errors.Is(err, pgx.ErrNoRows) {
			// Someone else's booking looks the same as no booking: nothing to learn here.
			return &bookingError{http.StatusNotFound, "booking_not_found", "no such booking"}
		}
		if err != nil {
			return err
		}

		// 3. Already cancelled: answer as before, change nothing.
		if bk.status == "cancelled" {
			b, err = loadBooking(ctx, tx, bookingID, payerID)
			return err
		}

		// 4. Can it be cancelled?
		if bk.status != "confirmed" || bk.sessionStatus == "cancelled" {
			// attended / no_show, or the teacher cancelled the class (which refunds in full on its own).
			return notBookable("not_cancellable", "this booking can no longer be cancelled")
		}
		untilStart := bk.startsAt.Sub(bk.now)
		if untilStart <= 0 {
			return notBookable("too_late", "the class has already started")
		}

		// 5. How much comes back.
		refund := refundFor(bk.totalCents, untilStart)

		// 6. Write it all: the booking's new status, a refund entry in the
		//    ledger (the original spend stays; the ledger is append-only), the
		//    event for the worker, and for a private session the session itself.
		_, err = tx.Exec(ctx, `
			UPDATE booking SET status = 'cancelled', cancelled_at = now(), refund_cents = $2
			WHERE id = $1`, bookingID, refund)
		if err != nil {
			return err
		}
		if refund > 0 {
			_, err = tx.Exec(ctx, `
				INSERT INTO credit_ledger (id, account_id, entry_type, amount_cents, currency, booking_id)
				VALUES ($1, $2, 'refund', $3, $4, $5)`,
				newUUIDv7(), payerID, refund, bk.currency, bookingID)
			if err != nil {
				return err
			}
		}
		if bk.sessionKind == "private" {
			// A private session exists only for its one booking: free the teacher's time.
			_, err = tx.Exec(ctx, `
				UPDATE session SET status = 'cancelled', cancel_reason = 'booking_cancelled'
				WHERE id = $1 AND status <> 'cancelled'`, bk.sessionID)
			if err != nil {
				return err
			}
		}
		payload, err := json.Marshal(map[string]any{
			"booking_id": bookingID, "session_id": bk.sessionID, "payer_account_id": payerID,
			"total_cents": bk.totalCents, "refund_cents": refund, "currency": bk.currency,
			"cancelled_by": "student",
		})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO outbox_event (id, aggregate_type, aggregate_id, event_type, payload)
			VALUES ($1, 'booking', $2, 'booking_cancelled', $3)`,
			newUUIDv7(), bookingID, payload)
		if err != nil {
			return err
		}

		b, err = loadBooking(ctx, tx, bookingID, payerID)
		return err
	})
	return b, err
}

// myBookingDTO is one entry in GET /bookings: a booking with what the
// "My bookings" list shows about its class and who's attending.
type myBookingDTO struct {
	ID                     string        `json:"id"`
	PartySize              int           `json:"party_size"`
	TotalCents             int           `json:"total_cents"`
	Currency               string        `json:"currency"`
	CreatedAt              time.Time     `json:"created_at"`
	RefundIfCancelledCents int           `json:"refund_if_cancelled_cents"` // if cancelled right now
	Session                mySessionDTO  `json:"session"`
	Attendees              []attendeeDTO `json:"attendees"`
	OpenSeats              int           `json:"open_seats"` // paid seats waiting for a friend
	Seats                  []seatDTO     `json:"seats"`      // every seat: who's in it, or open
	Role                   string        `json:"role"`       // "payer", or "guest" (my people are in a friend's class)
	PaidBy                 string        `json:"paid_by"`    // the payer's name (for guests)
	MySeats                []string      `json:"my_seats"`   // seats my people sit in (a guest's Leave button)
}

// seatDTO is one place in a booking: filled by a learner (status "joined"),
// or open for a friend ("invited"; display_name empty).
type seatDTO struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	DisplayName string `json:"display_name"`
}

type mySessionDTO struct {
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	Title           *string   `json:"title"`
	TargetLanguage  string    `json:"target_language"`
	Audience        string    `json:"audience"`
	StartsAt        time.Time `json:"starts_at"`
	DurationMinutes int       `json:"duration_minutes"`
}

type attendeeDTO struct {
	LearnerID   string `json:"learner_id"`
	DisplayName string `json:"display_name"`
}

// listMyBookings handles GET /bookings (wrapped in requireSignIn): the signed-in
// person's upcoming bookings, soonest first: those they paid for (role
// "payer"), and friends' private classes their people joined (role "guest":
// no price or refund shown, that's the payer's). Still confirmed, for classes
// that haven't started and weren't cancelled.
func listMyBookings(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		rows, err := pool.Query(r.Context(), `
			SELECT b.id::text, b.party_size, b.total_cents, b.currency, b.created_at,
			       s.id::text, s.kind, s.title, s.target_language, s.audience, s.starts_at, s.ends_at,
			       -- who's attending, as a JSON array; pgx decodes it into []attendeeDTO
			       (SELECT COALESCE(json_agg(json_build_object(
			                   'learner_id', l.id, 'display_name', l.display_name)
			                   ORDER BY l.display_name), '[]')
			        FROM booking_attendee a JOIN learner l ON l.id = a.learner_id
			        WHERE a.booking_id = b.id AND a.status <> 'removed'),
			       (SELECT COALESCE(json_agg(json_build_object(
			                   'id', a.id, 'status', a.status, 'display_name', COALESCE(l.display_name, ''))
			                   ORDER BY a.status DESC, a.created_at), '[]')
			        FROM booking_attendee a LEFT JOIN learner l ON l.id = a.learner_id
			        WHERE a.booking_id = b.id AND a.status <> 'removed'),
			       now(),
			       CASE WHEN b.payer_account_id = $1 THEN 'payer' ELSE 'guest' END,
			       (SELECT display_name FROM account WHERE id = b.payer_account_id),
			       (SELECT COALESCE(array_agg(ma.id::text ORDER BY ma.id), '{}') FROM booking_attendee ma
			        JOIN learner ml ON ml.id = ma.learner_id
			        WHERE ma.booking_id = b.id AND ma.status = 'joined' AND ml.account_id = $1)
			FROM booking b JOIN session s ON s.id = b.session_id
			WHERE (b.payer_account_id = $1
			       OR EXISTS (SELECT 1 FROM booking_attendee ga JOIN learner gl ON gl.id = ga.learner_id
			                  WHERE ga.booking_id = b.id AND ga.status = 'joined' AND gl.account_id = $1))
			  AND b.status = 'confirmed'
			  AND s.status IN ('scheduled', 'confirmed') AND s.starts_at > now()
			ORDER BY s.starts_at
			LIMIT 100`, me.ID)
		if err != nil {
			logFrom(r).Error("list my bookings: query failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load your bookings")
			return
		}
		defer rows.Close()

		bookings := []myBookingDTO{} // [] rather than null when there are none
		for rows.Next() {
			var b myBookingDTO
			var endsAt, now time.Time
			if err := rows.Scan(&b.ID, &b.PartySize, &b.TotalCents, &b.Currency, &b.CreatedAt,
				&b.Session.ID, &b.Session.Kind, &b.Session.Title, &b.Session.TargetLanguage,
				&b.Session.Audience, &b.Session.StartsAt, &endsAt, &b.Attendees, &b.Seats, &now,
				&b.Role, &b.PaidBy, &b.MySeats); err != nil {
				logFrom(r).Error("list my bookings: scan failed", "err", err)
				writeError(w, r, http.StatusInternalServerError, "could not load your bookings")
				return
			}
			b.Session.StartsAt = b.Session.StartsAt.UTC()
			b.Session.DurationMinutes = int(endsAt.Sub(b.Session.StartsAt).Minutes())
			for _, seat := range b.Seats {
				if seat.Status == "invited" {
					b.OpenSeats++
				}
			}
			// The same rule POST /bookings/{id}/cancel applies, with the database's clock.
			b.RefundIfCancelledCents = refundFor(b.TotalCents, b.Session.StartsAt.Sub(now))
			if b.Role == "guest" { // the money is the payer's business
				b.TotalCents, b.RefundIfCancelledCents = 0, 0
			}
			bookings = append(bookings, b)
		}
		if err := rows.Err(); err != nil {
			logFrom(r).Error("list my bookings: row iteration failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load your bookings")
			return
		}
		writeJSON(w, http.StatusOK, bookings)
	}
}
