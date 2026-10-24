package main

// Changing the seats of a private booking (step 9b). Only the payer, only on
// their own private booking, and never once the class has started.
//
//   More than 24 h before the class: adding or removing a seat reprices the
//   whole booking at the current price for the new party size; the payer pays
//   the difference, or gets it back (ledger 'spend' / 'refund').
//   Within 24 h (the party is locked): no new seats; removing one gives back
//   50% of that seat's price, rounded up in the payer's favour; the booking
//   keeps its price and party size (the seat is marked removed).
//
// Locks, always in this order: the payer's account (spending, as in booking),
// then the class's session, then the booking. The teacher's cancel also takes
// session before booking, so the two can't deadlock.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const partyLockNotice = 24 * time.Hour // the party size locks this long before the class

// seatChange is what the handlers share: the booking, locked, with its class.
type seatChange struct {
	bookingID, sessionID, currency string
	partySize, pricePerPerson      int
	totalCents                     int
	teacherID                      string
	minutes                        int
	locked                         bool // within 24 h: no repricing
}

// lockForSeatChange takes the locks (account → session → booking) and checks
// the booking can change at all.
func lockForSeatChange(ctx context.Context, tx pgx.Tx, payerID, bookingID string) (seatChange, error) {
	c := seatChange{bookingID: bookingID}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM account WHERE id = $1 FOR UPDATE`, payerID); err != nil {
		return c, err
	}
	var kind, bookingStatus, sessionStatus string
	var started bool
	var startsAt, endsAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT s.id::text, s.kind, s.status, s.teacher_id::text, s.starts_at, s.ends_at,
		       s.starts_at <= now(), s.starts_at - now() <= $3::int * interval '1 second'
		FROM booking b JOIN session s ON s.id = b.session_id
		WHERE b.id = $1 AND b.payer_account_id = $2
		FOR UPDATE OF s`, bookingID, payerID, int(partyLockNotice.Seconds())).
		Scan(&c.sessionID, &kind, &sessionStatus, &c.teacherID, &startsAt, &endsAt, &started, &c.locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, &bookingError{http.StatusNotFound, "booking_not_found", "no such booking"}
	}
	if err != nil {
		return c, err
	}
	if err := tx.QueryRow(ctx, `
		SELECT status, party_size, price_per_person_cents, total_cents, currency
		FROM booking WHERE id = $1 FOR UPDATE`, bookingID).
		Scan(&bookingStatus, &c.partySize, &c.pricePerPerson, &c.totalCents, &c.currency); err != nil {
		return c, err
	}
	c.minutes = int(endsAt.Sub(startsAt).Minutes())
	switch {
	case kind != "private":
		return c, notBookable("not_private", "only private classes have seats to change")
	case bookingStatus != "confirmed" || sessionStatus == "cancelled" || sessionStatus == "completed":
		return c, notBookable("not_changeable", "this booking can't be changed any more")
	case started:
		return c, notBookable("class_started", "the class has started")
	}
	return c, nil
}

// reprice moves the booking to newSize seats at the current price for that
// size, and settles the difference with the payer: a 'spend' entry if it costs
// more (refused if the credit isn't there), a 'refund' entry if less.
func (c *seatChange) reprice(ctx context.Context, tx pgx.Tx, payerID string, newSize int) (deltaCents int, err error) {
	price, currency, err := priceFor(ctx, tx, c.teacherID, "private", c.minutes, newSize)
	if err != nil {
		return 0, err
	}
	if currency != c.currency {
		return 0, notBookable("no_price", "no price in this booking's currency")
	}
	newTotal := price * newSize
	deltaCents = newTotal - c.totalCents
	if deltaCents > 0 {
		var balance int
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(amount_cents), 0) FROM credit_ledger WHERE account_id = $1 AND currency = $2`,
			payerID, c.currency).Scan(&balance); err != nil {
			return 0, err
		}
		if balance < deltaCents {
			return 0, notBookable("insufficient_credit", "not enough credit for another seat")
		}
	}
	if deltaCents != 0 {
		entry := "spend"
		if deltaCents < 0 {
			entry = "refund"
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO credit_ledger (id, account_id, entry_type, amount_cents, currency, booking_id, note)
			VALUES ($1, $2, $3, $4, $5, $6, 'seats changed')`,
			newUUIDv7(), payerID, entry, -deltaCents, c.currency, c.bookingID); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE booking SET party_size = $2, price_per_person_cents = $3, total_cents = $4 WHERE id = $1`,
		c.bookingID, newSize, price, newTotal); err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `UPDATE session SET max_seats = $2 WHERE id = $1`, c.sessionID, newSize)
	return deltaCents, err
}

// addSeat handles POST /bookings/{id}/seats (wrapped in requireSignIn): one
// more open seat, before the lock, repriced.
func addSeat(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		id := r.PathValue("id")
		if !isUUID(id) {
			writeError(w, r, http.StatusBadRequest, "the booking ID in the path is not valid")
			return
		}
		var delta int
		err := pgx.BeginFunc(r.Context(), pool, func(tx pgx.Tx) error {
			c, err := lockForSeatChange(r.Context(), tx, me.ID, id)
			if err != nil {
				return err
			}
			switch {
			case c.locked:
				return notBookable("seats_locked", "seats can't be added within 24 hours of the class")
			case c.partySize >= maxPartySize:
				return notBookable("party_full", "a private class has at most 4 seats")
			}
			if delta, err = c.reprice(r.Context(), tx, me.ID, c.partySize+1); err != nil {
				return err
			}
			_, err = tx.Exec(r.Context(), `
				INSERT INTO booking_attendee (id, booking_id, status, invite_token) VALUES ($1, $2, 'invited', $3)`,
				newUUIDv7(), id, unusableInviteToken())
			return err
		})
		writeSeatResult(w, r, pool, me.ID, id, delta, err)
	}
}

// removeSeat handles POST /bookings/{id}/seats/{seat}/remove (wrapped in
// requireSignIn): any seat (open, one of the payer's people, or a friend's),
// but never the last one (cancelling the booking is for that).
func removeSeat(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		id, seatID := r.PathValue("id"), r.PathValue("seat")
		if !isUUID(id) || !isUUID(seatID) {
			writeError(w, r, http.StatusBadRequest, "the booking or seat ID in the path is not valid")
			return
		}
		var delta int
		err := pgx.BeginFunc(r.Context(), pool, func(tx pgx.Tx) error {
			c, err := lockForSeatChange(r.Context(), tx, me.ID, id)
			if err != nil {
				return err
			}
			var seats int
			if err := tx.QueryRow(r.Context(), `
				SELECT count(*) FROM booking_attendee WHERE booking_id = $1 AND status <> 'removed'`, id).Scan(&seats); err != nil {
				return err
			}
			tag, err := tx.Exec(r.Context(), `
				UPDATE booking_attendee SET status = 'removed'
				WHERE id = $1 AND booking_id = $2 AND status <> 'removed'`, seatID, id)
			switch {
			case err != nil:
				return err
			case tag.RowsAffected() == 0:
				return &bookingError{http.StatusNotFound, "seat_not_found", "no such seat in this booking"}
			case seats <= 1:
				return notBookable("last_seat", "this is the only seat; cancel the booking instead")
			}
			if !c.locked {
				delta, err = c.reprice(r.Context(), tx, me.ID, c.partySize-1)
				return err
			}
			// Locked: half of that seat's price back, rounded up; the booking keeps
			// its price and party size (the seat stays paid for, now removed).
			back := (c.pricePerPerson*lateCancelRefundPercent + 99) / 100
			if back > 0 {
				if _, err := tx.Exec(r.Context(), `
					INSERT INTO credit_ledger (id, account_id, entry_type, amount_cents, currency, booking_id, note)
					VALUES ($1, $2, 'refund', $3, $4, $5, 'seat removed within 24 hours')`,
					newUUIDv7(), me.ID, back, c.currency, id); err != nil {
					return err
				}
				if _, err := tx.Exec(r.Context(), `UPDATE booking SET refund_cents = refund_cents + $2 WHERE id = $1`, id, back); err != nil {
					return err
				}
			}
			delta = -back
			return nil
		})
		writeSeatResult(w, r, pool, me.ID, id, delta, err)
	}
}

// writeSeatResult answers a seat change: the booking as it is now, and what
// it cost (delta_cents > 0) or gave back (< 0).
func writeSeatResult(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, payerID, bookingID string, delta int, err error) {
	var be *bookingError
	switch {
	case errors.As(err, &be):
		writeErrorCode(w, r, be.status, be.code, be.msg)
		return
	case err != nil:
		logFrom(r).Error("seat change failed", "err", err, "booking_id", bookingID)
		writeError(w, r, http.StatusInternalServerError, "could not change the seats")
		return
	}
	var b bookingDTO
	err = pgx.BeginFunc(r.Context(), pool, func(tx pgx.Tx) error {
		var err error
		b, err = loadBooking(r.Context(), tx, bookingID, payerID)
		return err
	})
	if err != nil {
		logFrom(r).Error("seat change: reload failed", "err", err, "booking_id", bookingID)
		writeError(w, r, http.StatusInternalServerError, "changed, but could not load the booking")
		return
	}
	logFrom(r).Info("seats changed", "booking_id", bookingID, "party_size", b.PartySize, "delta_cents", delta)
	writeJSON(w, http.StatusOK, map[string]any{"booking": b, "delta_cents": delta})
}
