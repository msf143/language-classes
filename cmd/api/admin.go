package main

// Admin routes (step 7f): seeing and managing teachers. An admin is an account
// with is_admin set (by the command `api admin`, never through the website).

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// requireAdmin wraps a handler for admins only: signed out → 401 (from
// requireSignIn), signed in but not an admin → 403 not_admin. The flag comes
// with the signed-in account (accountColumns), read fresh on every request,
// so revoking it takes effect at once.
func requireAdmin(pool *pgxpool.Pool, next http.HandlerFunc) http.HandlerFunc {
	return requireSignIn(pool, func(w http.ResponseWriter, r *http.Request) {
		if a, _ := accountFrom(r); a == nil || !a.IsAdmin {
			writeErrorCode(w, r, http.StatusForbidden, "not_admin", "this is for admins only")
			return
		}
		next(w, r)
	})
}

// adminTeacherDTO is one row of the admin's teacher list.
type adminTeacherDTO struct {
	ID              string    `json:"id"`
	DisplayName     string    `json:"display_name"`
	Email           string    `json:"email"`
	Status          string    `json:"status"` // "pending", "active" or "inactive"
	CreatedAt       time.Time `json:"created_at"`
	UpcomingClasses int       `json:"upcoming_classes"` // scheduled or confirmed, not yet ended
	BookedClasses   int       `json:"booked_classes"`   // of those, with at least one booking
	Prices          int       `json:"prices"`           // price cells valid now
}

// listAdminTeachers handles GET /admin/teachers: pending first (they're
// waiting for an admin), then active, then inactive; by name within each.
func listAdminTeachers(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := pool.Query(r.Context(), `
			SELECT t.id::text, a.display_name, a.email, t.status, t.created_at,
			       (SELECT count(*) FROM session s WHERE s.teacher_id = t.id
			          AND s.status IN ('scheduled', 'confirmed') AND s.ends_at > now())::int,
			       (SELECT count(*) FROM session s WHERE s.teacher_id = t.id
			          AND s.status IN ('scheduled', 'confirmed') AND s.ends_at > now()
			          AND EXISTS (SELECT 1 FROM booking b WHERE b.session_id = s.id AND b.status = 'confirmed'))::int,
			       (SELECT count(*) FROM price_tier p WHERE p.teacher_id = t.id AND p.valid @> now())::int
			FROM teacher t JOIN account a ON a.id = t.account_id
			ORDER BY array_position(ARRAY['pending', 'active', 'inactive'], t.status), lower(a.display_name)`)
		if err != nil {
			logFrom(r).Error("admin teachers: query failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load the teachers")
			return
		}
		list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (adminTeacherDTO, error) {
			var t adminTeacherDTO
			err := row.Scan(&t.ID, &t.DisplayName, &t.Email, &t.Status, &t.CreatedAt,
				&t.UpcomingClasses, &t.BookedClasses, &t.Prices)
			return t, err
		})
		if err != nil {
			logFrom(r).Error("admin teachers: scan failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load the teachers")
			return
		}
		if list == nil {
			list = []adminTeacherDTO{}
		}
		writeJSON(w, http.StatusOK, list)
	}
}

// classSummaryDTO: an upcoming class, as the admin sees it.
type classSummaryDTO struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Title      *string   `json:"title"`
	StartsAt   time.Time `json:"starts_at"`
	EndsAt     time.Time `json:"ends_at"`
	SeatsTaken int       `json:"seats_taken"`
}

// upcomingClasses: the teacher's classes not yet ended (scheduled or
// confirmed), soonest first; bookedOnly keeps those with a confirmed booking.
func upcomingClasses(ctx context.Context, q querier, teacherID string, bookedOnly bool) ([]classSummaryDTO, error) {
	rows, err := q.Query(ctx, `
		SELECT s.id::text, s.kind, s.title, s.starts_at, s.ends_at,
		       COALESCE((SELECT SUM(b.party_size) FROM booking b
		                 WHERE b.session_id = s.id AND b.status = 'confirmed'), 0)::int AS taken
		FROM session s
		WHERE s.teacher_id = $1 AND s.status IN ('scheduled', 'confirmed') AND s.ends_at > now()
		ORDER BY s.starts_at`, teacherID)
	if err != nil {
		return nil, err
	}
	all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (classSummaryDTO, error) {
		var c classSummaryDTO
		err := row.Scan(&c.ID, &c.Kind, &c.Title, &c.StartsAt, &c.EndsAt, &c.SeatsTaken)
		return c, err
	})
	list := []classSummaryDTO{}
	for _, c := range all {
		if !bookedOnly || c.SeatsTaken > 0 {
			list = append(list, c)
		}
	}
	return list, err
}

// getAdminTeacher handles GET /admin/teachers/{id}: one teacher in full.
func getAdminTeacher(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !isUUID(id) {
			writeError(w, r, http.StatusBadRequest, "the teacher ID in the path is not valid")
			return
		}
		var t struct {
			adminTeacherDTO
			Timezone       string            `json:"timezone"`
			BioEN          string            `json:"bio_en"`
			BioES          string            `json:"bio_es"`
			BreakMinutes   int               `json:"break_minutes"`
			OfferedLengths []int             `json:"offered_lengths"`
			PriceList      []priceCellDTO    `json:"price_list"`
			Classes        []classSummaryDTO `json:"classes"`
		}
		err := pool.QueryRow(r.Context(), `
			SELECT t.id::text, a.display_name, a.email, t.status, t.created_at, a.timezone,
			       COALESCE(t.bio_en, ''), COALESCE(t.bio_es, ''), t.buffer_minutes, t.offered_durations
			FROM teacher t JOIN account a ON a.id = t.account_id WHERE t.id = $1`, id).
			Scan(&t.ID, &t.DisplayName, &t.Email, &t.Status, &t.CreatedAt, &t.Timezone,
				&t.BioEN, &t.BioES, &t.BreakMinutes, &t.OfferedLengths)
		if errors.Is(err, pgx.ErrNoRows) {
			writeErrorCode(w, r, http.StatusNotFound, "teacher_not_found", "no such teacher")
			return
		}
		if err == nil {
			t.PriceList, err = currentPrices(r.Context(), pool, id)
		}
		if err == nil {
			t.Classes, err = upcomingClasses(r.Context(), pool, id, false)
		}
		if err != nil {
			logFrom(r).Error("admin teacher: load failed", "err", err, "teacher_id", id)
			writeError(w, r, http.StatusInternalServerError, "could not load the teacher")
			return
		}
		t.Prices = len(t.PriceList)
		for _, c := range t.Classes {
			t.UpcomingClasses++
			if c.SeatsTaken > 0 {
				t.BookedClasses++
			}
		}
		slices.Sort(t.OfferedLengths)
		writeJSON(w, http.StatusOK, t)
	}
}

// setTeacherStatus handles POST /admin/teachers/{id}/activate and
// /deactivate. The teacher row is locked FOR UPDATE: bookings hold a shared
// lock on it while they run (teacherActiveShared), so deactivating waits for
// any booking in flight, then sees it. Deactivating is refused (409
// has_booked_classes, with the list) while any upcoming class has bookings:
// those are cancelled first, with full refunds. Doing it twice changes nothing.
func setTeacherStatus(pool *pgxpool.Pool, active bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		id := r.PathValue("id")
		if !isUUID(id) {
			writeError(w, r, http.StatusBadRequest, "the teacher ID in the path is not valid")
			return
		}
		want := "inactive"
		if active {
			want = "active"
		}
		var from string
		var booked []classSummaryDTO
		err := pgx.BeginFunc(r.Context(), pool, func(tx pgx.Tx) error {
			if err := tx.QueryRow(r.Context(), `SELECT status FROM teacher WHERE id = $1 FOR UPDATE`, id).Scan(&from); err != nil {
				return err
			}
			if from == want {
				return nil
			}
			if !active {
				var err error
				if booked, err = upcomingClasses(r.Context(), tx, id, true); err != nil || len(booked) > 0 {
					return err
				}
			}
			_, err := tx.Exec(r.Context(), `UPDATE teacher SET status = $2 WHERE id = $1`, id, want)
			return err
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			writeErrorCode(w, r, http.StatusNotFound, "teacher_not_found", "no such teacher")
			return
		case err != nil:
			logFrom(r).Error("admin: teacher status change failed", "err", err, "teacher_id", id)
			writeError(w, r, http.StatusInternalServerError, "could not change the teacher's status")
			return
		case len(booked) > 0:
			reqID, _ := r.Context().Value(requestIDKey).(string)
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "this teacher has booked upcoming classes; cancel them first", "code": "has_booked_classes",
				"request_id": reqID, "classes": booked,
			})
			return
		}
		if from != want {
			// Who did what, in the log (the audit trail for admin actions).
			logFrom(r).Info("admin: teacher status changed", "admin_account_id", me.ID, "admin_email", me.Email,
				"teacher_id", id, "from", from, "to", want)
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": want})
	}
}
