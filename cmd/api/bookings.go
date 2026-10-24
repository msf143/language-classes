package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// bookingRequest is the body of POST /bookings: either a group session
// ("session_id") or a private class at a time from GET /private-slots
// ("private"), never both.
type bookingRequest struct {
	SessionID  string          `json:"session_id"`
	Private    *privateRequest `json:"private"`
	LearnerIDs []string        `json:"learner_ids"` // who attends: the payer's own learner profiles
	// Seats (private classes only): how many places to book and pay for, at
	// least one per learner; the rest are open for friends (step 9). 0 = one
	// per learner, no open seats.
	Seats int `json:"seats"`
}

// bookingDTO is what the API returns for a booking.
type bookingDTO struct {
	ID                  string     `json:"id"`
	SessionID           string     `json:"session_id"`
	LearnerIDs          []string   `json:"learner_ids"`
	PartySize           int        `json:"party_size"`
	PricePerPersonCents int        `json:"price_per_person_cents"`
	TotalCents          int        `json:"total_cents"`
	Currency            string     `json:"currency"`
	Status              string     `json:"status"`
	CreatedAt           time.Time  `json:"created_at"`
	CancelledAt         *time.Time `json:"cancelled_at"`  // null unless cancelled
	RefundCents         int        `json:"refund_cents"`  // credit given back when cancelled
	BalanceCents        int        `json:"balance_cents"` // the payer's credit after this booking
	OpenSeats           int        `json:"open_seats"`    // paid seats waiting for a friend (private only)
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

// createBooking handles POST /bookings: book seats in a group session, or a
// private class, for some of the signed-in person's learners, paid from their
// credit balance.
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
			writeError(w, r, http.StatusBadRequest, `body must be JSON like {"session_id": "...", "learner_ids": ["..."]} or {"private": {...}, "learner_ids": ["..."]}`)
			return
		}
		if msg := req.normalize(); msg != "" {
			writeError(w, r, http.StatusBadRequest, msg)
			return
		}

		book := bookGroupSession
		if req.Private != nil {
			book = bookPrivateSession
		}
		b, created, err := book(r.Context(), pool, me.ID, key, req)
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
	switch {
	case req.Private != nil && req.SessionID != "":
		return "send either session_id (a group class) or private (a private class), not both"
	case req.Private != nil:
		if msg := req.Private.check(); msg != "" {
			return msg
		}
	case !isUUID(req.SessionID):
		return "session_id must be a session ID"
	default:
		req.SessionID = strings.ToLower(req.SessionID)
	}
	if len(req.LearnerIDs) < 1 || len(req.LearnerIDs) > maxPartySize {
		return fmt.Sprintf("learner_ids must list 1 to %d learners", maxPartySize)
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
	switch {
	case req.Seats == 0:
		req.Seats = len(req.LearnerIDs)
	case req.Private == nil:
		return "seats is only for private classes"
	case req.Seats < len(req.LearnerIDs) || req.Seats > maxPartySize:
		return fmt.Sprintf("seats must be from the number of learner_ids up to %d", maxPartySize)
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
		// 1-2. Lock the payer; seen this key before? Then answer as before.
		existing, err := lockPayerAndFindKey(ctx, tx, payerID, key)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.SessionID != req.SessionID || !slices.Equal(existing.LearnerIDs, req.LearnerIDs) {
				return errKeyReused
			}
			b = *existing
			return nil
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
		if active, err := teacherActiveShared(ctx, tx, s.teacherID); err != nil {
			return err
		} else if !active {
			return notBookable("session_not_open", "this class's teacher isn't taking bookings")
		}

		// 4. The learners: the payer's own, active, and allowed in this session.
		minors, err := checkLearners(ctx, tx, payerID, req.LearnerIDs)
		if err != nil {
			return err
		}
		switch {
		case s.audience == "adults" && minors > 0:
			return notBookable("audience_mismatch", "this session is for adults only")
		case s.audience == "kids" && minors < len(req.LearnerIDs):
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
		pricePerPerson, currency, err := priceFor(ctx, tx, s.teacherID, "group", minutes, partySize)
		if err != nil {
			return err
		}

		// 7-8. Enough credit? Then write it all.
		b = bookingDTO{
			SessionID: req.SessionID, LearnerIDs: req.LearnerIDs, PartySize: partySize,
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

// ---------- steps shared by group and private bookings ----------

var errKeyReused = &bookingError{http.StatusUnprocessableEntity, "idempotency_key_reused",
	"this Idempotency-Key was already used for a different booking"}

// lockPayerAndFindKey locks the payer's account row (serializing this person's
// spending, so two tabs can't spend the same credit) and returns the booking
// already made with this Idempotency-Key, if there is one (nil if not). The
// lock is what makes the lookup safe: two identical requests can't both miss.
func lockPayerAndFindKey(ctx context.Context, tx pgx.Tx, payerID, key string) (*bookingDTO, error) {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM account WHERE id = $1 FOR UPDATE`, payerID); err != nil {
		return nil, err
	}
	var existingID string
	err := tx.QueryRow(ctx, `SELECT id FROM booking WHERE payer_account_id = $1 AND idempotency_key = $2`,
		payerID, key).Scan(&existingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b, err := loadBooking(ctx, tx, existingID, payerID)
	return &b, err
}

// checkLearners makes sure every learner is the payer's own and active, and
// returns how many of them are minors (for audience rules).
func checkLearners(ctx context.Context, tx pgx.Tx, payerID string, learnerIDs []string) (minors int, err error) {
	rows, err := tx.Query(ctx, `
		SELECT is_minor FROM learner
		WHERE id = ANY($1::uuid[]) AND account_id = $2 AND status = 'active'`,
		learnerIDs, payerID)
	if err != nil {
		return 0, err
	}
	defer rows.Close() // already closed after the loop; this covers the early returns
	found := 0
	for rows.Next() {
		var isMinor bool
		if err := rows.Scan(&isMinor); err != nil {
			return 0, err
		}
		found++
		if isMinor {
			minors++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if found != len(learnerIDs) {
		return 0, &bookingError{http.StatusBadRequest, "unknown_learner", "learner_ids must be your own active learners"}
	}
	return minors, nil
}

// priceFor is the price per person in force right now for this teacher, kind
// ("group" or "private"), length and party size.
func priceFor(ctx context.Context, tx pgx.Tx, teacherID, kind string, minutes, partySize int) (cents int, currency string, err error) {
	err = tx.QueryRow(ctx, `
		SELECT price_per_person_cents, currency FROM price_tier
		WHERE teacher_id = $1 AND kind = $2 AND duration_minutes = $3
		  AND party_size = $4 AND valid @> now()`,
		teacherID, kind, minutes, partySize).Scan(&cents, &currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", notBookable("no_price", "this class has no price set for that many people")
	}
	return cents, currency, err
}

// teacherActiveShared takes a SHARED lock on the teacher's row and says whether
// he's active. Any number of bookings can hold it at once (they don't wait for
// each other); deactivating a teacher takes FOR UPDATE (admin.go), which waits
// for bookings in flight, and they wait for it. So a booking can never slip in
// between a deactivation's "no bookings?" check and its status change. Lock
// order stays account → session → teacher; deactivate takes only the teacher.
func teacherActiveShared(ctx context.Context, tx pgx.Tx, teacherID string) (bool, error) {
	var active bool
	err := tx.QueryRow(ctx, `SELECT status = 'active' FROM teacher WHERE id = $1 FOR SHARE`, teacherID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return active, err
}

// chargeAndWrite checks the payer can afford b.TotalCents (the account lock
// from lockPayerAndFindKey makes the balance read safe), then writes it all:
// the booking, its attendees, the ledger debit, and the outbox event a worker
// will later turn into a confirmation email. b must have SessionID,
// LearnerIDs, PartySize, the prices and Currency set; it gets its ID,
// CreatedAt, Status and BalanceCents here.
func chargeAndWrite(ctx context.Context, tx pgx.Tx, payerID, key string, b *bookingDTO) error {
	var balance int
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_cents), 0) FROM credit_ledger
		WHERE account_id = $1 AND currency = $2`, payerID, b.Currency).Scan(&balance)
	if err != nil {
		return err
	}
	if balance < b.TotalCents {
		return notBookable("insufficient_credit", "not enough credit for this booking")
	}

	b.ID, b.Status, b.BalanceCents = newUUIDv7(), "confirmed", balance-b.TotalCents
	err = tx.QueryRow(ctx, `
		INSERT INTO booking (id, session_id, payer_account_id, party_size,
		                     price_per_person_cents, total_cents, currency, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING created_at`,
		b.ID, b.SessionID, payerID, b.PartySize, b.PricePerPersonCents, b.TotalCents, b.Currency, key).Scan(&b.CreatedAt)
	if err != nil {
		return err
	}
	for _, learnerID := range b.LearnerIDs {
		_, err = tx.Exec(ctx, `
			INSERT INTO booking_attendee (id, booking_id, learner_id, status) VALUES ($1, $2, $3, 'joined')`,
			newUUIDv7(), b.ID, learnerID)
		if err != nil {
			return err
		}
	}
	// Open seats (private classes, step 9): paid places waiting for a friend.
	for range b.OpenSeats {
		if _, err := tx.Exec(ctx, `
			INSERT INTO booking_attendee (id, booking_id, status, invite_token) VALUES ($1, $2, 'invited', $3)`,
			newUUIDv7(), b.ID, unusableInviteToken()); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO credit_ledger (id, account_id, entry_type, amount_cents, currency, booking_id)
		VALUES ($1, $2, 'spend', $3, $4, $5)`,
		newUUIDv7(), payerID, -b.TotalCents, b.Currency, b.ID)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"booking_id": b.ID, "session_id": b.SessionID, "payer_account_id": payerID,
		"learner_ids": b.LearnerIDs, "total_cents": b.TotalCents, "currency": b.Currency,
	})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO outbox_event (id, aggregate_type, aggregate_id, event_type, payload)
		VALUES ($1, 'booking', $2, 'booking_created', $3)`,
		newUUIDv7(), b.ID, payload)
	return err
}

// unusableInviteToken is what an open seat holds until the payer asks for a
// link: the hash (hex) of a secret that's thrown away at once, so no link can
// match it. Asking for a link (step 9c) replaces it with the hash of a real one.
func unusableInviteToken() string {
	return fmt.Sprintf("%x", hashSecret(newSecret()))
}

// loadBooking reads a booking (and the payer's current balance), for a replay
// or after a cancellation. learner_ids are the payer's own learners only (a
// friend who joined later mustn't make a replay look different).
func loadBooking(ctx context.Context, tx pgx.Tx, id, payerID string) (bookingDTO, error) {
	b := bookingDTO{ID: id}
	err := tx.QueryRow(ctx, `
		SELECT b.session_id, b.party_size, b.price_per_person_cents, b.total_cents, b.currency,
		       b.status, b.created_at, b.cancelled_at, b.refund_cents,
		       COALESCE((SELECT array_agg(a.learner_id::text ORDER BY a.learner_id::text)
		                 FROM booking_attendee a JOIN learner l ON l.id = a.learner_id AND l.account_id = $2
		                 WHERE a.booking_id = b.id AND a.status = 'joined'), '{}'),
		       COALESCE((SELECT SUM(amount_cents) FROM credit_ledger
		                 WHERE account_id = $2 AND currency = b.currency), 0),
		       (SELECT count(*) FROM booking_attendee a WHERE a.booking_id = b.id AND a.status = 'invited')::int
		FROM booking b WHERE b.id = $1`, id, payerID).
		Scan(&b.SessionID, &b.PartySize, &b.PricePerPersonCents, &b.TotalCents, &b.Currency,
			&b.Status, &b.CreatedAt, &b.CancelledAt, &b.RefundCents, &b.LearnerIDs, &b.BalanceCents, &b.OpenSeats)
	return b, err
}
