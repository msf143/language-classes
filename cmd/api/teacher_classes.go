package main

// The teacher's classes (step 7d): creating a group class, and cancelling any
// of his classes (group or private) with a full refund for everyone booked.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	groupCreateNotice = 48 * time.Hour // a new group class starts at least this far ahead (≥ a day of bookings before the 24 h decision)
	maxGroupSeats     = 30
	maxTitleChars     = 100
)

type newGroupClass struct {
	Title           string `json:"title"`
	TargetLanguage  string `json:"target_language"` // "es" or "en": the language taught
	Audience        string `json:"audience"`        // "adults", "kids" or "all"
	Date            string `json:"date"`            // "2026-11-03", in the teacher's time zone
	From            string `json:"from"`            // "18:00", in the teacher's time zone
	DurationMinutes int    `json:"duration_minutes"`
	MinSeats        int    `json:"min_seats"`
	MaxSeats        *int   `json:"max_seats"` // null = no cap
	startsAt        time.Time
}

// check validates the request (reading date + from in the teacher's zone) and
// returns a message for the client, or "".
func (c *newGroupClass) check(loc *time.Location, now time.Time) string {
	c.Title = strings.TrimSpace(c.Title)
	var err error
	c.startsAt, err = time.ParseInLocation("2006-01-02 15:04", c.Date+" "+c.From, loc)
	switch n := utf8.RuneCountInString(c.Title); {
	case n == 0 || n > maxTitleChars:
		return fmt.Sprintf("title is required, at most %d characters", maxTitleChars)
	case c.TargetLanguage != "es" && c.TargetLanguage != "en":
		return `target_language must be "es" or "en"`
	case !oneOf(c.Audience, "adults", "kids", "all"):
		return `audience must be "adults", "kids" or "all"`
	case err != nil:
		return `date and from look like {"date": "2026-11-03", "from": "18:00"}`
	case !slices.Contains(allowedLengths, c.DurationMinutes):
		return fmt.Sprintf("duration_minutes must be one of %v", allowedLengths)
	case c.MinSeats < groupMinStudents:
		return fmt.Sprintf("min_seats must be at least %d", groupMinStudents)
	case c.MaxSeats != nil && (*c.MaxSeats < c.MinSeats || *c.MaxSeats > maxGroupSeats):
		return fmt.Sprintf("max_seats must be empty (no cap) or between min_seats and %d", maxGroupSeats)
	}
	return ""
}

// createGroupClass handles POST /teacher/sessions (wrapped in requireTeacher).
func createGroupClass(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		teacherID := teacherFrom(r)
		var c newGroupClass
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			writeError(w, r, http.StatusBadRequest, "body must be JSON like {title, target_language, audience, date, from, duration_minutes, min_seats, max_seats}")
			return
		}
		if msg := c.check(zone(me.Timezone), time.Now()); msg != "" {
			writeErrorCode(w, r, http.StatusBadRequest, "invalid_class", msg)
			return
		}

		id := newUUIDv7()
		err := pgx.BeginFunc(r.Context(), pool, func(tx pgx.Tx) error {
			// Timing and price are checked against the database's clock and data.
			var tooSoon, priced bool
			if err := tx.QueryRow(r.Context(), `
				SELECT $2::timestamptz < now() + $3::int * interval '1 second',
				       EXISTS (SELECT 1 FROM price_tier WHERE teacher_id = $1 AND kind = 'group'
				               AND duration_minutes = $4 AND valid @> now())`,
				teacherID, c.startsAt, int(groupCreateNotice.Seconds()), c.DurationMinutes).Scan(&tooSoon, &priced); err != nil {
				return err
			}
			if tooSoon {
				return notBookable("too_soon", fmt.Sprintf("a group class must start at least %d hours from now", int(groupCreateNotice.Hours())))
			}
			if !priced {
				return notBookable("no_price", fmt.Sprintf("set a group price for %d minutes first", c.DurationMinutes))
			}
			ends := c.startsAt.Add(time.Duration(c.DurationMinutes) * time.Minute)
			_, err := tx.Exec(r.Context(), `
				INSERT INTO session (id, teacher_id, kind, target_language, audience, title,
				                     starts_at, ends_at, occupied, min_seats, max_seats)
				VALUES ($1, $2, 'group', $3, $4, $5, $6, $7, tstzrange($6, $7), $8, $9)`,
				id, teacherID, c.TargetLanguage, c.Audience, c.Title, c.startsAt, ends, c.MinSeats, c.MaxSeats)
			return err
		})
		var be *bookingError
		var pgErr *pgconn.PgError
		switch {
		case errors.As(err, &be):
			writeErrorCode(w, r, http.StatusConflict, be.code, be.msg)
			return
		case errors.As(err, &pgErr) && pgErr.Code == "23P01": // exclusion: overlaps one of his classes
			writeErrorCode(w, r, http.StatusConflict, "class_overlap", "you already have a class at that time")
			return
		case err != nil:
			logFrom(r).Error("create group class failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not create the class")
			return
		}
		logFrom(r).Info("group class created", "session_id", id, "teacher_id", teacherID, "starts_at", c.startsAt)
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "starts_at": c.startsAt.UTC()})
	}
}

// cancelTeacherSession handles POST /teacher/sessions/{id}/cancel (wrapped in
// requireTeacher): the teacher cancels one of his classes, group or private;
// everyone booked gets everything back. Cancelling twice changes nothing more.
// Another teacher's class, or no class at all, is a 404.
func cancelTeacherSession(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !isUUID(id) {
			writeError(w, r, http.StatusBadRequest, "the class ID in the path is not valid")
			return
		}
		var refunded int
		var already bool
		err := pgx.BeginFunc(r.Context(), pool, func(tx pgx.Tx) error {
			// The session lock first, like the group-minimum check: a booking
			// arriving now waits, then finds the class cancelled.
			var status string
			var started bool
			err := tx.QueryRow(r.Context(), `
				SELECT status, starts_at <= now() FROM session
				WHERE id = $1 AND teacher_id = $2 FOR UPDATE`, id, teacherFrom(r)).Scan(&status, &started)
			if errors.Is(err, pgx.ErrNoRows) {
				return notBookable("class_not_found", "no such class")
			}
			if err != nil {
				return err
			}
			switch {
			case status == "cancelled":
				already = true
				return nil
			case status == "completed" || started:
				return notBookable("class_started", "a class that has started can't be cancelled")
			}
			refunded, err = cancelWithRefunds(r.Context(), tx, id, "teacher_cancelled", "teacher")
			return err
		})
		var be *bookingError
		switch {
		case errors.As(err, &be) && be.code == "class_not_found":
			writeErrorCode(w, r, http.StatusNotFound, be.code, be.msg)
			return
		case errors.As(err, &be):
			writeErrorCode(w, r, http.StatusConflict, be.code, be.msg)
			return
		case err != nil:
			logFrom(r).Error("teacher cancel failed", "err", err, "session_id", id)
			writeError(w, r, http.StatusInternalServerError, "could not cancel the class")
			return
		}
		if !already {
			logFrom(r).Info("class cancelled by the teacher", "session_id", id, "bookings_refunded", refunded)
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "cancelled", "bookings_refunded": refunded})
	}
}
