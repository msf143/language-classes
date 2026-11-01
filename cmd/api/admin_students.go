package main

// The admin's list of students: POST /admin/students/search {"q"} (wrapped in
// requireAdmin). A student here is any account that isn't a teacher. Children
// aren't accounts: only how many a parent has is shown, never their names.
//
// POST rather than GET: the request log records query strings, and what an admin
// searches for is someone's name or email.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const adminStudentsLimit = 100 // newest first; search to find anyone beyond these

type adminStudentDTO struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	Email       string    `json:"email"`
	Status      string    `json:"status"` // "active" or "disabled"
	JoinedAt    time.Time `json:"joined_at"`
	PhotoID     *string   `json:"photo_id"` // shown at /photos/{id}; null = none
	Bookings    int       `json:"bookings"` // not cancelled: paid for, or someone on the account attending
	Children    int       `json:"children"` // a count only
}

// searchAdminStudents answers {students, total}: the newest matches (at most
// adminStudentsLimit) and how many match in all. q finds any part of a name or
// email, ignoring case; position() rather than LIKE, so "%" and "_" are just
// characters. An empty q matches everyone.
func searchAdminStudents(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Q string `json:"q"`
		}
		err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body)
		if err != nil && !errors.Is(err, io.EOF) { // an empty body means no search
			writeError(w, r, http.StatusBadRequest, `body must be JSON like {"q": "ana"}`)
			return
		}
		q := strings.TrimSpace(body.Q)
		if utf8.RuneCountInString(q) > 100 {
			writeError(w, r, http.StatusBadRequest, "the search can be at most 100 characters")
			return
		}
		rows, err := pool.Query(r.Context(), `
			SELECT a.id::text, a.display_name, a.email, a.status, a.created_at,
			       (SELECT ph.id::text FROM profile_photo ph WHERE ph.account_id = a.id),
			       (SELECT count(DISTINCT b.id) FROM booking b
			        LEFT JOIN booking_attendee ba ON ba.booking_id = b.id AND ba.status = 'joined'
			        LEFT JOIN learner l ON l.id = ba.learner_id
			        WHERE b.status <> 'cancelled' AND (b.payer_account_id = a.id OR l.account_id = a.id))::int,
			       (SELECT count(*) FROM learner c WHERE c.account_id = a.id AND c.is_minor AND c.status = 'active')::int,
			       count(*) OVER ()::int
			FROM account a
			WHERE NOT EXISTS (SELECT 1 FROM teacher t WHERE t.account_id = a.id)
			  AND ($1::text = '' OR position(lower($1::text) IN lower(a.display_name)) > 0
			                     OR position(lower($1::text) IN lower(a.email)) > 0)
			ORDER BY a.created_at DESC, a.id DESC
			LIMIT $2`, q, adminStudentsLimit)
		if err != nil {
			logFrom(r).Error("admin students: query failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load the students")
			return
		}
		total := 0
		students, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (adminStudentDTO, error) {
			var s adminStudentDTO
			err := row.Scan(&s.ID, &s.DisplayName, &s.Email, &s.Status, &s.JoinedAt, &s.PhotoID, &s.Bookings, &s.Children, &total)
			return s, err
		})
		if err != nil {
			logFrom(r).Error("admin students: scan failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load the students")
			return
		}
		if students == nil {
			students = []adminStudentDTO{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"students": students, "total": total})
	}
}
