package main

// Friends joining private classes (step 9c). The payer makes a link for an
// open seat; a friend opens it, signs in, and fills the seat with one of their
// own learners. A friend can leave again: the seat opens (still paid by the
// payer; only the payer ever moves money).
//
// Links: only the SHA-256 hash (hex) of the secret is stored, in
// booking_attendee.invite_token; making a new link replaces it, so an older
// link stops working. The secret travels after "#" (never in a request line).
//
// Locks: booking, then seat, the same order as removing a seat (9b), so the
// two can't deadlock. Joining finds the seat by its token first without a
// lock, locks the booking, then locks the seat and checks the token again.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seatTokenHash is what invite_token holds for a link's secret.
func seatTokenHash(secret string) string { return fmt.Sprintf("%x", hashSecret(secret)) }

// seatLink handles POST /bookings/{id}/seats/{seat}/link (wrapped in
// requireSignIn): a fresh link for one of the payer's open seats.
func seatLink(pool *pgxpool.Pool, siteURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		id, seatID := r.PathValue("id"), r.PathValue("seat")
		if !isUUID(id) || !isUUID(seatID) {
			writeError(w, r, http.StatusBadRequest, "the booking or seat ID in the path is not valid")
			return
		}
		secret := newSecret()
		tag, err := pool.Exec(r.Context(), `
			UPDATE booking_attendee a SET invite_token = $4
			FROM booking b JOIN session s ON s.id = b.session_id
			WHERE a.id = $1 AND a.booking_id = $2 AND a.status = 'invited'
			  AND b.id = a.booking_id AND b.payer_account_id = $3 AND b.status = 'confirmed'
			  AND s.status IN ('scheduled', 'confirmed') AND s.starts_at > now()`,
			seatID, id, me.ID, seatTokenHash(secret))
		switch {
		case err != nil:
			logFrom(r).Error("seat link failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not make a link")
		case tag.RowsAffected() == 0:
			writeErrorCode(w, r, http.StatusNotFound, "seat_not_open", "no open seat like that in an upcoming booking of yours")
		default:
			writeJSON(w, http.StatusOK, map[string]string{"link": siteURL + "/join.html#token=" + secret})
		}
	}
}

// seatPreviewDTO: what the join page shows before anyone signs in.
type seatPreviewDTO struct {
	Kind            string    `json:"kind"`
	Title           *string   `json:"title"`
	TargetLanguage  string    `json:"target_language"`
	StartsAt        time.Time `json:"starts_at"`
	DurationMinutes int       `json:"duration_minutes"`
	TeacherName     string    `json:"teacher_name"`
	InvitedBy       string    `json:"invited_by"` // the payer's display name
}

func decodeToken(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(into); err != nil {
		writeError(w, r, http.StatusBadRequest, `body must be JSON like {"token": "..."}`)
		return false
	}
	return true
}

// seatPreview handles POST /seats/preview {"token"} (public: the link is the
// proof). 404 seat_link_invalid for a link that's unknown, used, replaced,
// or for a class that's cancelled or has started.
func seatPreview(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Token string `json:"token"`
		}
		if !decodeToken(w, r, &body) {
			return
		}
		var p seatPreviewDTO
		var endsAt time.Time
		err := pool.QueryRow(r.Context(), `
			SELECT s.kind, s.title, s.target_language, s.starts_at, s.ends_at, ta.display_name, pa.display_name
			FROM booking_attendee a
			JOIN booking b ON b.id = a.booking_id JOIN session s ON s.id = b.session_id
			JOIN account pa ON pa.id = b.payer_account_id
			JOIN teacher t ON t.id = s.teacher_id JOIN account ta ON ta.id = t.account_id
			WHERE a.invite_token = $1 AND a.status = 'invited' AND b.status = 'confirmed'
			  AND s.status IN ('scheduled', 'confirmed') AND s.starts_at > now()`, seatTokenHash(body.Token)).
			Scan(&p.Kind, &p.Title, &p.TargetLanguage, &p.StartsAt, &endsAt, &p.TeacherName, &p.InvitedBy)
		if errors.Is(err, pgx.ErrNoRows) {
			writeErrorCode(w, r, http.StatusNotFound, "seat_link_invalid", "this link isn't valid any more")
			return
		}
		if err != nil {
			logFrom(r).Error("seat preview failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load the invitation")
			return
		}
		p.StartsAt = p.StartsAt.UTC()
		p.DurationMinutes = int(endsAt.Sub(p.StartsAt).Minutes())
		writeJSON(w, http.StatusOK, p)
	}
}

// joinSeat handles POST /seats/join {"token", "learner_id"} (wrapped in
// requireSignIn): fill the seat with one of my own learners.
func joinSeat(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		var body struct {
			Token     string `json:"token"`
			LearnerID string `json:"learner_id"`
		}
		if !decodeToken(w, r, &body) {
			return
		}
		if !isUUID(body.LearnerID) {
			writeError(w, r, http.StatusBadRequest, "learner_id must be one of your learners")
			return
		}
		hash := seatTokenHash(body.Token)
		var bookingID, seatID string
		err := pgx.BeginFunc(r.Context(), pool, func(tx pgx.Tx) error {
			// Which booking? (no lock yet: booking first, then seat)
			if err := tx.QueryRow(r.Context(), `SELECT booking_id::text FROM booking_attendee WHERE invite_token = $1`, hash).
				Scan(&bookingID); err != nil {
				return err
			}
			open, err := lockOpenBooking(r.Context(), tx, bookingID)
			if err != nil {
				return err
			}
			if !open {
				return notBookable("not_changeable", "this class is cancelled or has started")
			}
			// The seat, locked, and still the one this link is for.
			var status string
			if err := tx.QueryRow(r.Context(), `
				SELECT id::text, status FROM booking_attendee WHERE invite_token = $1 AND booking_id = $2 FOR UPDATE`,
				hash, bookingID).Scan(&seatID, &status); err != nil {
				return err
			}
			if status != "invited" {
				return notBookable("seat_taken", "someone has just taken this seat")
			}
			var mine bool
			if err := tx.QueryRow(r.Context(), `
				SELECT EXISTS (SELECT 1 FROM learner WHERE id = $1 AND account_id = $2 AND status = 'active')`,
				body.LearnerID, me.ID).Scan(&mine); err != nil {
				return err
			}
			if !mine {
				return &bookingError{http.StatusBadRequest, "unknown_learner", "learner_id must be one of your own active learners"}
			}
			_, err = tx.Exec(r.Context(), `
				UPDATE booking_attendee SET status = 'joined', learner_id = $2, invite_token = NULL WHERE id = $1`,
				seatID, body.LearnerID)
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" { // booking_attendee_learner_uq
				return notBookable("already_in_class", "this person is already in the class")
			}
			if err != nil {
				return err
			}
			var name string
			if err := tx.QueryRow(r.Context(), `SELECT display_name FROM learner WHERE id = $1`, body.LearnerID).Scan(&name); err != nil {
				return err
			}
			return queueSeatEvent(r.Context(), tx, "seat_joined", bookingID, seatEvent{SeatID: seatID, AccountID: me.ID, LearnerName: name})
		})
		var be *bookingError
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			writeErrorCode(w, r, http.StatusNotFound, "seat_link_invalid", "this link isn't valid any more")
		case errors.As(err, &be):
			writeErrorCode(w, r, be.status, be.code, be.msg)
		case err != nil:
			logFrom(r).Error("join seat failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not join the class")
		default:
			logFrom(r).Info("friend joined a private class", "booking_id", bookingID, "seat_id", seatID, "account_id", me.ID)
			writeJSON(w, http.StatusOK, map[string]string{"booking_id": bookingID, "seat_id": seatID})
		}
	}
}

// lockOpenBooking takes a shared lock on the booking (a cancellation, which
// takes FOR UPDATE, waits for us and we for it) and says whether its class is
// still upcoming and confirmed.
func lockOpenBooking(ctx context.Context, tx pgx.Tx, bookingID string) (bool, error) {
	var open bool
	err := tx.QueryRow(ctx, `
		SELECT b.status = 'confirmed' AND s.status IN ('scheduled', 'confirmed') AND s.starts_at > now()
		FROM booking b JOIN session s ON s.id = b.session_id
		WHERE b.id = $1 FOR SHARE OF b`, bookingID).Scan(&open)
	return open, err
}

// leaveSeat handles POST /seats/{seat}/leave (wrapped in requireSignIn): my
// learner leaves someone's private class; the seat opens again, still paid
// for (the payer can invite someone else, or remove the seat).
func leaveSeat(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		seatID := r.PathValue("seat")
		if !isUUID(seatID) {
			writeError(w, r, http.StatusBadRequest, "the seat ID in the path is not valid")
			return
		}
		var bookingID, learnerName string
		err := pgx.BeginFunc(r.Context(), pool, func(tx pgx.Tx) error {
			if err := tx.QueryRow(r.Context(), `
				SELECT a.booking_id::text, l.display_name FROM booking_attendee a JOIN learner l ON l.id = a.learner_id
				WHERE a.id = $1 AND l.account_id = $2 AND a.status = 'joined'`, seatID, me.ID).Scan(&bookingID, &learnerName); err != nil {
				return err
			}
			open, err := lockOpenBooking(r.Context(), tx, bookingID)
			if err != nil {
				return err
			}
			if !open {
				return notBookable("not_changeable", "this class is cancelled or has started")
			}
			tag, err := tx.Exec(r.Context(), `
				UPDATE booking_attendee SET status = 'invited', learner_id = NULL, invite_token = $2
				WHERE id = $1 AND status = 'joined'`, seatID, unusableInviteToken())
			if err == nil && tag.RowsAffected() == 0 {
				return pgx.ErrNoRows // changed meanwhile (removed by the payer)
			}
			if err != nil {
				return err
			}
			return queueSeatEvent(r.Context(), tx, "seat_left", bookingID, seatEvent{SeatID: seatID, AccountID: me.ID, LearnerName: learnerName})
		})
		var be *bookingError
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			writeErrorCode(w, r, http.StatusNotFound, "seat_not_found", "you're not in this seat")
		case errors.As(err, &be):
			writeErrorCode(w, r, be.status, be.code, be.msg)
		case err != nil:
			logFrom(r).Error("leave seat failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not leave the class")
		default:
			logFrom(r).Info("friend left a private class", "booking_id", bookingID, "seat_id", seatID, "account_id", me.ID)
			writeJSON(w, http.StatusOK, map[string]string{"seat_id": seatID, "status": "invited"})
		}
	}
}
