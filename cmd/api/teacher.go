package main

// The teacher's side of the API: publishing the times he's available for
// private classes (availability blocks). Students' bookable slots are derived
// from these blocks (minus booked sessions), never stored.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	slotGrid         = 15 * time.Minute // blocks and slots start and end on a quarter hour
	maxBlockLength   = 12 * time.Hour
	maxRepeatWeeks   = 12
	exclusionViolate = "23P01" // Postgres error code: an EXCLUDE constraint said no
)

// teacherKey is where requireTeacher stores the teacher's ID in the context.
const teacherKey ctxKey = "teacher_id"

// requireTeacher wraps a handler for teachers only: signed in (else 401) and
// with an active teacher profile (else 403). The teacher's ID goes in the
// request context; read it with teacherFrom.
func requireTeacher(pool *pgxpool.Pool, next http.HandlerFunc) http.HandlerFunc {
	return requireSignIn(pool, func(w http.ResponseWriter, r *http.Request) {
		a, _ := accountFrom(r)
		var teacherID string
		err := pool.QueryRow(r.Context(), `
			SELECT id::text FROM teacher WHERE account_id = $1 AND status = 'active'`, a.ID).Scan(&teacherID)
		if errors.Is(err, pgx.ErrNoRows) {
			writeErrorCode(w, r, http.StatusForbidden, "not_teacher", "this is for teachers only")
			return
		}
		if err != nil {
			logFrom(r).Error("teacher lookup failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not check your teacher profile")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), teacherKey, teacherID)))
	})
}

func teacherFrom(r *http.Request) string {
	id, _ := r.Context().Value(teacherKey).(string)
	return id
}

// blockDTO is one availability block in API responses. Times are UTC.
type blockDTO struct {
	ID       string    `json:"id"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
	Status   string    `json:"status"` // "open" or "withdrawn"
}

type publishRequest struct {
	StartsAt    time.Time `json:"starts_at"` // RFC 3339, e.g. "2026-10-27T14:00:00Z"
	EndsAt      time.Time `json:"ends_at"`
	RepeatWeeks int       `json:"repeat_weeks"` // how many weeks in a row (0 or 1 = just this one)
}

// check validates a publish request and returns a message for the client, or "".
func (p *publishRequest) check(now time.Time) string {
	if p.RepeatWeeks == 0 {
		p.RepeatWeeks = 1
	}
	switch {
	case p.StartsAt.IsZero() || p.EndsAt.IsZero():
		return `body must be JSON like {"starts_at": "2026-10-27T14:00:00Z", "ends_at": "2026-10-27T17:00:00Z"}`
	case !p.EndsAt.After(p.StartsAt):
		return "ends_at must be after starts_at"
	case !p.StartsAt.After(now):
		return "a block must start in the future"
	case p.EndsAt.Sub(p.StartsAt) > maxBlockLength:
		return "a block can be at most 12 hours long"
	case !p.StartsAt.Equal(p.StartsAt.Truncate(slotGrid)) || !p.EndsAt.Equal(p.EndsAt.Truncate(slotGrid)):
		return "blocks start and end on a quarter hour (:00, :15, :30, :45)"
	case p.RepeatWeeks < 1 || p.RepeatWeeks > maxRepeatWeeks:
		return "repeat_weeks must be between 1 and 12"
	}
	return ""
}

// publishAvailability handles POST /teacher/availability (wrapped in
// requireTeacher). With repeat_weeks = N it creates N blocks a week apart, at
// the same local time in the teacher's time zone, all in one transaction: if
// any of them overlaps an open block he already has, none are created.
func publishAvailability(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		teacherID := teacherFrom(r)

		var req publishRequest
		r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, r, http.StatusBadRequest, `body must be JSON like {"starts_at": "2026-10-27T14:00:00Z", "ends_at": "2026-10-27T17:00:00Z", "repeat_weeks": 1}`)
			return
		}
		if msg := req.check(time.Now()); msg != "" {
			writeError(w, r, http.StatusBadRequest, msg)
			return
		}

		// "Same time next week" means the same wall-clock time where the teacher
		// lives. AddDate in his zone keeps 9:00 at 9:00 across a daylight-saving
		// change (Bogotá has none, but a future teacher's zone might).
		loc, err := time.LoadLocation(me.Timezone)
		if err != nil {
			loc = time.UTC
		}
		start, end := req.StartsAt.In(loc), req.EndsAt.In(loc)

		var blocks []blockDTO
		err = pgx.BeginFunc(r.Context(), pool, func(tx pgx.Tx) error {
			for week := range req.RepeatWeeks {
				b := blockDTO{
					ID:       newUUIDv7(),
					StartsAt: start.AddDate(0, 0, 7*week).UTC(),
					EndsAt:   end.AddDate(0, 0, 7*week).UTC(),
					Status:   "open",
				}
				_, err := tx.Exec(r.Context(), `
					INSERT INTO availability_block (id, teacher_id, period) VALUES ($1, $2, tstzrange($3, $4))`,
					b.ID, teacherID, b.StartsAt, b.EndsAt)
				if err != nil {
					return err
				}
				blocks = append(blocks, b)
			}
			return nil
		})
		var pgErr *pgconn.PgError
		switch {
		case errors.As(err, &pgErr) && pgErr.Code == exclusionViolate:
			// The table's EXCLUDE constraint: no two open blocks may overlap.
			writeErrorCode(w, r, http.StatusConflict, "block_overlap", "you already have time open during part of this")
		case err != nil:
			logFrom(r).Error("publish availability failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not publish the availability")
		default:
			logFrom(r).Info("availability published", "blocks", len(blocks))
			writeJSON(w, http.StatusCreated, blocks)
		}
	}
}

// listAvailability handles GET /teacher/availability (wrapped in requireTeacher):
// the teacher's open blocks that haven't ended yet, soonest first.
func listAvailability(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := pool.Query(r.Context(), `
			SELECT id::text, lower(period), upper(period), status FROM availability_block
			WHERE teacher_id = $1 AND status = 'open' AND upper(period) > now()
			ORDER BY lower(period)
			LIMIT 500`, teacherFrom(r))
		if err != nil {
			logFrom(r).Error("list availability: query failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load your availability")
			return
		}
		defer rows.Close()
		blocks := []blockDTO{}
		for rows.Next() {
			var b blockDTO
			if err := rows.Scan(&b.ID, &b.StartsAt, &b.EndsAt, &b.Status); err != nil {
				logFrom(r).Error("list availability: scan failed", "err", err)
				writeError(w, r, http.StatusInternalServerError, "could not load your availability")
				return
			}
			b.StartsAt, b.EndsAt = b.StartsAt.UTC(), b.EndsAt.UTC()
			blocks = append(blocks, b)
		}
		if err := rows.Err(); err != nil {
			logFrom(r).Error("list availability: row iteration failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load your availability")
			return
		}
		writeJSON(w, http.StatusOK, blocks)
	}
}

// withdrawAvailability handles POST /teacher/availability/{id}/withdraw
// (wrapped in requireTeacher). The block stays in the table with status
// "withdrawn"; classes already booked in it are not affected. Withdrawing a
// withdrawn block just returns it. Another teacher's block is a 404.
func withdrawAvailability(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !isUUID(id) {
			writeError(w, r, http.StatusBadRequest, "the block ID in the path is not valid")
			return
		}
		var b blockDTO
		// One statement does it: the UPDATE only changes an open block, and the
		// CTE returns the row either way (changed or already withdrawn).
		err := pool.QueryRow(r.Context(), `
			WITH changed AS (
				UPDATE availability_block SET status = 'withdrawn'
				WHERE id = $1 AND teacher_id = $2 AND status = 'open'
				RETURNING id, period, status
			)
			SELECT id::text, lower(period), upper(period), status FROM changed
			UNION ALL
			SELECT id::text, lower(period), upper(period), status FROM availability_block
			WHERE id = $1 AND teacher_id = $2 AND NOT EXISTS (SELECT 1 FROM changed)`,
			id, teacherFrom(r)).Scan(&b.ID, &b.StartsAt, &b.EndsAt, &b.Status)
		if errors.Is(err, pgx.ErrNoRows) {
			writeErrorCode(w, r, http.StatusNotFound, "block_not_found", "no such availability block")
			return
		}
		if err != nil {
			logFrom(r).Error("withdraw availability failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not withdraw the availability")
			return
		}
		b.StartsAt, b.EndsAt = b.StartsAt.UTC(), b.EndsAt.UTC()
		writeJSON(w, http.StatusOK, b)
	}
}
