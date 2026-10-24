package main

// My account (step 8): a student changes the name others see, and manages the
// learners on the account: the people who actually attend (themselves, their
// children, another adult in the household). Learners are never deleted:
// removing one archives it, so past bookings still say who came.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maxActiveLearners = 10

// cleanName trims a name and checks its length (in characters, not bytes).
func cleanName(name, field string) (string, string) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n == 0 || n > maxNameChars {
		return name, fmt.Sprintf("%s is required, at most %d characters", field, maxNameChars)
	}
	return name, ""
}

// putMe handles PUT /me {"display_name": "Mike"} (wrapped in requireSignIn).
// Your own learner (the one made at your first sign-in, with your name back
// then) is renamed with you, in the same transaction: otherwise "Who's
// attending?" would keep showing the old name. It's recognised by still
// having the account's old name; one you renamed yourself is left alone.
func putMe(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		var body struct {
			DisplayName string `json:"display_name"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeError(w, r, http.StatusBadRequest, `body must be JSON like {"display_name": "Mike"}`)
			return
		}
		name, msg := cleanName(body.DisplayName, "display_name")
		if msg != "" {
			writeErrorCode(w, r, http.StatusBadRequest, "invalid_name", msg)
			return
		}
		err := pgx.BeginFunc(r.Context(), pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(r.Context(), `
				UPDATE learner SET display_name = $3
				WHERE account_id = $1 AND status = 'active' AND display_name = $2`, me.ID, me.DisplayName, name); err != nil {
				return err
			}
			_, err := tx.Exec(r.Context(), `UPDATE account SET display_name = $2 WHERE id = $1`, me.ID, name)
			return err
		})
		if err != nil {
			logFrom(r).Error("put me failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not save your name")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"display_name": name})
	}
}

// learnerInput is the body of POST /me/learners and PUT /me/learners/{id}.
type learnerInput struct {
	DisplayName    string `json:"display_name"`
	IsMinor        bool   `json:"is_minor"`
	NativeLanguage string `json:"native_language"` // "en" or "es"
}

func decodeLearner(w http.ResponseWriter, r *http.Request) (learnerInput, bool) {
	var in learnerInput
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, r, http.StatusBadRequest, `body must be JSON like {"display_name": "Sofía", "is_minor": true, "native_language": "en"}`)
		return in, false
	}
	var msg string
	if in.DisplayName, msg = cleanName(in.DisplayName, "display_name"); msg == "" && in.NativeLanguage != "en" && in.NativeLanguage != "es" {
		msg = `native_language must be "en" or "es"`
	}
	if msg != "" {
		writeErrorCode(w, r, http.StatusBadRequest, "invalid_learner", msg)
		return in, false
	}
	return in, true
}

// addLearner handles POST /me/learners (wrapped in requireSignIn). At most
// maxActiveLearners per account; the account row is locked so two adds at
// once can't both squeeze past the limit.
func addLearner(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		in, ok := decodeLearner(w, r)
		if !ok {
			return
		}
		l := learnerDTO{ID: newUUIDv7(), DisplayName: in.DisplayName, IsMinor: in.IsMinor, NativeLanguage: in.NativeLanguage}
		err := pgx.BeginFunc(r.Context(), pool, func(tx pgx.Tx) error {
			var active int
			if err := tx.QueryRow(r.Context(), `
				SELECT (SELECT count(*) FROM learner WHERE account_id = a.id AND status = 'active')::int
				FROM account a WHERE a.id = $1 FOR UPDATE`, me.ID).Scan(&active); err != nil {
				return err
			}
			if active >= maxActiveLearners {
				return &bookingError{http.StatusConflict, "too_many_learners", fmt.Sprintf("an account can hold at most %d people", maxActiveLearners)}
			}
			_, err := tx.Exec(r.Context(), `
				INSERT INTO learner (id, account_id, display_name, is_minor, native_language)
				VALUES ($1, $2, $3, $4, $5)`, l.ID, me.ID, l.DisplayName, l.IsMinor, l.NativeLanguage)
			return err
		})
		var be *bookingError
		switch {
		case errors.As(err, &be):
			writeErrorCode(w, r, be.status, be.code, be.msg)
		case err != nil:
			logFrom(r).Error("add learner failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not add the person")
		default:
			writeJSON(w, http.StatusCreated, l)
		}
	}
}

// updateLearner handles PUT /me/learners/{id} (wrapped in requireSignIn):
// one of my own active learners; anyone else's (or none) is a 404.
func updateLearner(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		id := r.PathValue("id")
		if !isUUID(id) {
			writeError(w, r, http.StatusBadRequest, "the learner ID in the path is not valid")
			return
		}
		in, ok := decodeLearner(w, r)
		if !ok {
			return
		}
		tag, err := pool.Exec(r.Context(), `
			UPDATE learner SET display_name = $3, is_minor = $4, native_language = $5
			WHERE id = $1 AND account_id = $2 AND status = 'active'`,
			id, me.ID, in.DisplayName, in.IsMinor, in.NativeLanguage)
		switch {
		case err != nil:
			logFrom(r).Error("update learner failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not save the person")
		case tag.RowsAffected() == 0:
			writeErrorCode(w, r, http.StatusNotFound, "learner_not_found", "no such person on your account")
		default:
			writeJSON(w, http.StatusOK, learnerDTO{ID: id, DisplayName: in.DisplayName, IsMinor: in.IsMinor, NativeLanguage: in.NativeLanguage})
		}
	}
}

// archiveLearner handles POST /me/learners/{id}/archive (wrapped in
// requireSignIn): the person no longer appears for booking. Refused while
// they're booked into a class that hasn't ended (cancel that first). The
// account row is locked first, as booking does: a booking for this person
// can't commit between the "booked?" check and the archive.
func archiveLearner(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		id := r.PathValue("id")
		if !isUUID(id) {
			writeError(w, r, http.StatusBadRequest, "the learner ID in the path is not valid")
			return
		}
		err := pgx.BeginFunc(r.Context(), pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(r.Context(), `SELECT 1 FROM account WHERE id = $1 FOR UPDATE`, me.ID); err != nil {
				return err
			}
			var status string
			var booked bool
			err := tx.QueryRow(r.Context(), `
				SELECT l.status, EXISTS (
				         SELECT 1 FROM booking_attendee a
				         JOIN booking b ON b.id = a.booking_id
				         JOIN session s ON s.id = b.session_id
				         WHERE a.learner_id = l.id AND a.status = 'joined'
				           AND b.status = 'confirmed' AND s.ends_at > now())
				FROM learner l WHERE l.id = $1 AND l.account_id = $2`, id, me.ID).Scan(&status, &booked)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				return &bookingError{http.StatusNotFound, "learner_not_found", "no such person on your account"}
			case err != nil:
				return err
			case status == "archived":
				return nil // already removed: nothing to do
			case booked:
				return &bookingError{http.StatusConflict, "learner_booked", "this person is booked into an upcoming class; cancel that booking first"}
			}
			_, err = tx.Exec(r.Context(), `UPDATE learner SET status = 'archived' WHERE id = $1`, id)
			return err
		})
		var be *bookingError
		switch {
		case errors.As(err, &be):
			writeErrorCode(w, r, be.status, be.code, be.msg)
		case err != nil:
			logFrom(r).Error("archive learner failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not remove the person")
		default:
			writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "archived"})
		}
	}
}
