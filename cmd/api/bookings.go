package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// bookingRequest is the body of POST /bookings.
type bookingRequest struct {
	SessionID  string   `json:"session_id"`
	LearnerIDs []string `json:"learner_ids"` // who attends: the payer's own learner profiles
}

// bookingDTO is what the API returns for a booking.
type bookingDTO struct {
	ID                  string    `json:"id"`
	SessionID           string    `json:"session_id"`
	LearnerIDs          []string  `json:"learner_ids"`
	PartySize           int       `json:"party_size"`
	PricePerPersonCents int       `json:"price_per_person_cents"`
	TotalCents          int       `json:"total_cents"`
	Currency            string    `json:"currency"`
	Status              string    `json:"status"`
	CreatedAt           time.Time `json:"created_at"`
	BalanceCents        int       `json:"balance_cents"` // the payer's credit after this booking
}

// bookingError is a "you can't book this" answer: an HTTP status, a code the
// front-end translates, and an English message for anyone reading the JSON.
type bookingError struct {
	status    int
	code, msg string
}

func (e *bookingError) Error() string { return e.code + ": " + e.msg }

func notBookable(code, msg string) error {
	return &bookingError{http.StatusConflict, code, msg}
}

// createBooking handles POST /bookings: book seats in a group session for some
// of the signed-in person's learners, paid from their credit balance.
//
// Send an Idempotency-Key header (any unique string per "Book" click). Sending
// the same request again with the same key returns the same booking (200)
// instead of booking and charging twice.
func createBooking(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r) // requireSignIn guarantees it's there

		key := r.Header.Get("Idempotency-Key")
		if key == "" || len(key) > 100 {
			writeError(w, r, http.StatusBadRequest, "send an Idempotency-Key header (a unique string per booking attempt, up to 100 characters)")
			return
		}
		var req bookingRequest
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, r, http.StatusBadRequest, `body must be JSON like {"session_id": "...", "learner_ids": ["..."]}`)
			return
		}
		if msg := req.normalize(); msg != "" {
			writeError(w, r, http.StatusBadRequest, msg)
			return
		}

		b, created, err := bookGroupSession(r.Context(), pool, me.ID, key, req)
		var be *bookingError
		switch {
		case errors.As(err, &be):
			writeErrorCode(w, r, be.status, be.code, be.msg)
		case err != nil:
			logFrom(r).Error("booking failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not complete the booking")
		case created:
			logFrom(r).Info("booking created", "booking_id", b.ID, "session_id", b.SessionID, "total_cents", b.TotalCents)
			writeJSON(w, http.StatusCreated, b)
		default: // a replay of an earlier request with the same key
			writeJSON(w, http.StatusOK, b)
		}
	}
}

// normalize checks the request and puts the learner IDs in a canonical form
// (lower case, sorted), so a replay can be compared with the original. It
// returns a message for the client, or "" if the request is fine.
func (req *bookingRequest) normalize() string {
	if !isUUID(req.SessionID) {
		return "session_id must be a session ID"
	}
	req.SessionID = strings.ToLower(req.SessionID)
	if len(req.LearnerIDs) < 1 || len(req.LearnerIDs) > 4 {
		return "learner_ids must list 1 to 4 learners"
	}
	for i, id := range req.LearnerIDs {
		if !isUUID(id) {
			return "learner_ids must be learner IDs"
		}
		req.LearnerIDs[i] = strings.ToLower(id)
	}
	slices.Sort(req.LearnerIDs)
	if len(slices.Compact(slices.Clone(req.LearnerIDs))) != len(req.LearnerIDs) {
		return "learner_ids lists the same learner twice"
	}
	return ""
}

// bookGroupSession does the booking in one transaction. It returns the
// booking, whether it was created now (false = replay of the same key), or a
// *bookingError when the booking isn't possible.
//
// Concurrency: READ COMMITTED plus two row locks, always taken in this order
// (account, then session) so two bookings can never deadlock:
//   - the payer's account row serializes this person's spending, so two tabs
//     can't both spend the same credit, and makes the idempotency check safe;
//   - the session row serializes seat-taking, so two people can't both get the
//     last seat.
func bookGroupSession(ctx context.Context, pool *pgxpool.Pool, payerID, key string, req bookingRequest) (b bookingDTO, created bool, err error) {
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// 1. Lock the payer.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM account WHERE id = $1 FOR UPDATE`, payerID); err != nil {
			return err
		}

		// 2. Seen this key before? Then answer as before; don't book again.
		var existingID string
		err := tx.QueryRow(ctx, `SELECT id FROM booking WHERE payer_account_id = $1 AND idempotency_key = $2`,
			payerID, key).Scan(&existingID)
		if err == nil {
			b, err = loadBooking(ctx, tx, existingID, payerID)
			if err != nil {
				return err
			}
			if b.SessionID != req.SessionID || !slices.Equal(b.LearnerIDs, req.LearnerIDs) {
				return &bookingError{http.StatusUnprocessableEntity, "idempotency_key_reused",
					"this Idempotency-Key was already used for a different booking"}
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		// 3. Lock the session and check it can be booked.
		var s struct {
			teacherID, kind, audience, status string
			starts, ends                      time.Time
			maxSeats                          *int // nil = no cap
			started                           bool
		}
		err = tx.QueryRow(ctx, `
			SELECT teacher_id, kind, audience, status, starts_at, ends_at, max_seats, starts_at <= now()
			FROM session WHERE id = $1 FOR UPDATE`, req.SessionID).
			Scan(&s.teacherID, &s.kind, &s.audience, &s.status, &s.starts, &s.ends, &s.maxSeats, &s.started)
		if errors.Is(err, pgx.ErrNoRows) {
			return &bookingError{http.StatusNotFound, "session_not_found", "no such session"}
		}
		if err != nil {
			return err
		}
		if s.kind != "group" {
			return notBookable("session_not_open", "only group sessions can be booked this way")
		}
		if (s.status != "scheduled" && s.status != "confirmed") || s.started {
			return notBookable("session_not_open", "this session is no longer open for booking")
		}

		// 4. The learners: the payer's own, active, and allowed in this session.
		rows, err := tx.Query(ctx, `
			SELECT is_minor FROM learner
			WHERE id = ANY($1::uuid[]) AND account_id = $2 AND status = 'active'`,
			req.LearnerIDs, payerID)
		if err != nil {
			return err
		}
		defer rows.Close() // already closed after the loop; this covers the early returns
		minors, found := 0, 0
		for rows.Next() {
			var isMinor bool
			if err := rows.Scan(&isMinor); err != nil {
				return err
			}
			found++
			if isMinor {
				minors++
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if found != len(req.LearnerIDs) {
			return &bookingError{http.StatusBadRequest, "unknown_learner", "learner_ids must be your own active learners"}
		}
		switch {
		case s.audience == "adults" && minors > 0:
			return notBookable("audience_mismatch", "this session is for adults only")
		case s.audience == "kids" && minors < found:
			return notBookable("audience_mismatch", "this session is for children only")
		}

		var alreadyBooked bool
		err = tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM booking_attendee a JOIN booking b ON b.id = a.booking_id
				WHERE b.session_id = $1 AND b.status = 'confirmed'
				  AND a.status <> 'removed' AND a.learner_id = ANY($2::uuid[]))`,
			req.SessionID, req.LearnerIDs).Scan(&alreadyBooked)
		if err != nil {
			return err
		}
		if alreadyBooked {
			return notBookable("already_booked", "a learner in this booking already has a seat in this session")
		}

		// 5. Seats, derived from the bookings themselves (never a stored counter).
		partySize := len(req.LearnerIDs)
		var taken int
		err = tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(party_size), 0) FROM booking
			WHERE session_id = $1 AND status = 'confirmed'`, req.SessionID).Scan(&taken)
		if err != nil {
			return err
		}
		if s.maxSeats != nil && taken+partySize > *s.maxSeats {
			return notBookable("session_full", "not enough seats left in this session")
		}

		// 6. The price in force right now, snapshotted onto the booking.
		minutes := int(s.ends.Sub(s.starts).Minutes())
		var pricePerPerson int
		var currency string
		err = tx.QueryRow(ctx, `
			SELECT price_per_person_cents, currency FROM price_tier
			WHERE teacher_id = $1 AND kind = 'group' AND duration_minutes = $2
			  AND party_size = $3 AND valid @> now()`,
			s.teacherID, minutes, partySize).Scan(&pricePerPerson, &currency)
		if errors.Is(err, pgx.ErrNoRows) {
			return notBookable("no_price", "this session has no price set for that many people")
		}
		if err != nil {
			return err
		}
		total := pricePerPerson * partySize

		// 7. Enough credit? The account lock from step 1 makes this read safe.
		var balance int
		err = tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(amount_cents), 0) FROM credit_ledger
			WHERE account_id = $1 AND currency = $2`, payerID, currency).Scan(&balance)
		if err != nil {
			return err
		}
		if balance < total {
			return notBookable("insufficient_credit", "not enough credit for this booking")
		}

		// 8. Write it all: booking, attendees, the ledger debit, and the outbox
		//    event a worker will later turn into a confirmation email.
		b = bookingDTO{
			ID: newUUIDv7(), SessionID: req.SessionID, LearnerIDs: req.LearnerIDs,
			PartySize: partySize, PricePerPersonCents: pricePerPerson, TotalCents: total,
			Currency: currency, Status: "confirmed", BalanceCents: balance - total,
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO booking (id, session_id, payer_account_id, party_size,
			                     price_per_person_cents, total_cents, currency, idempotency_key)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING created_at`,
			b.ID, b.SessionID, payerID, partySize, pricePerPerson, total, currency, key).Scan(&b.CreatedAt)
		if err != nil {
			return err
		}
		for _, learnerID := range req.LearnerIDs {
			_, err = tx.Exec(ctx, `
				INSERT INTO booking_attendee (id, booking_id, learner_id, status) VALUES ($1, $2, $3, 'joined')`,
				newUUIDv7(), b.ID, learnerID)
			if err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO credit_ledger (id, account_id, entry_type, amount_cents, currency, booking_id)
			VALUES ($1, $2, 'spend', $3, $4, $5)`,
			newUUIDv7(), payerID, -total, currency, b.ID)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(map[string]any{
			"booking_id": b.ID, "session_id": b.SessionID, "payer_account_id": payerID,
			"learner_ids": b.LearnerIDs, "total_cents": total, "currency": currency,
		})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO outbox_event (id, aggregate_type, aggregate_id, event_type, payload)
			VALUES ($1, 'booking', $2, 'booking_created', $3)`,
			newUUIDv7(), b.ID, payload)
		if err != nil {
			return err
		}
		created = true
		return nil
	})
	return b, created, err
}

// loadBooking reads a booking (and the payer's current balance) for a replay.
func loadBooking(ctx context.Context, tx pgx.Tx, id, payerID string) (bookingDTO, error) {
	b := bookingDTO{ID: id}
	err := tx.QueryRow(ctx, `
		SELECT b.session_id, b.party_size, b.price_per_person_cents, b.total_cents, b.currency,
		       b.status, b.created_at,
		       COALESCE((SELECT array_agg(a.learner_id::text ORDER BY a.learner_id::text)
		                 FROM booking_attendee a
		                 WHERE a.booking_id = b.id AND a.learner_id IS NOT NULL), '{}'),
		       COALESCE((SELECT SUM(amount_cents) FROM credit_ledger
		                 WHERE account_id = $2 AND currency = b.currency), 0)
		FROM booking b WHERE b.id = $1`, id, payerID).
		Scan(&b.SessionID, &b.PartySize, &b.PricePerPersonCents, &b.TotalCents, &b.Currency,
			&b.Status, &b.CreatedAt, &b.LearnerIDs, &b.BalanceCents)
	return b, err
}
