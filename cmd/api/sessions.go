package main

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// sessionDTO is the JSON shape of a session in API responses.
// Pointer fields are optional: they become null in JSON when empty.
type sessionDTO struct {
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`            // "group" or "private"
	TargetLanguage  string    `json:"target_language"` // language being taught: "es" or "en"
	Audience        string    `json:"audience"`        // "adults", "kids" or "all"
	Title           *string   `json:"title"`
	StartsAt        time.Time `json:"starts_at"` // always UTC; the front-end converts for display
	EndsAt          time.Time `json:"ends_at"`
	DurationMinutes int       `json:"duration_minutes"`
	MaxSeats        *int      `json:"max_seats"` // null = no cap
	SeatsTaken      int       `json:"seats_taken"`
	SeatsLeft       *int      `json:"seats_left"` // null when there is no cap
	Status          string    `json:"status"`
}

// Upcoming, bookable sessions with seat counts.
// Seats taken = sum of party sizes on bookings that aren't cancelled.
// Empty filter parameters ('') mean "don't filter on this".
const listSessionsSQL = `
SELECT s.id::text,
       s.kind,
       s.target_language,
       s.audience,
       s.title,
       s.starts_at,
       s.ends_at,
       s.max_seats,
       COALESCE(SUM(b.party_size) FILTER (WHERE b.status <> 'cancelled'), 0)::int AS seats_taken,
       s.status
FROM session s
LEFT JOIN booking b ON b.session_id = s.id
WHERE s.status IN ('scheduled', 'confirmed')
  AND s.starts_at > now()
  AND ($1::text = '' OR s.kind = $1)
  AND ($2::text = '' OR s.target_language = $2)
  AND ($3::text = '' OR s.audience = $3)
GROUP BY s.id
ORDER BY s.starts_at
LIMIT 100`

// listSessions handles GET /sessions?kind=group&language=es&audience=kids
// All filters are optional.
func listSessions(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		kind, lang, audience := q.Get("kind"), q.Get("language"), q.Get("audience")

		// Reject bad filter values with a clear message instead of
		// silently returning an empty list.
		if !oneOf(kind, "", "group", "private") {
			writeError(w, r, http.StatusBadRequest, "kind must be 'group' or 'private'")
			return
		}
		if !oneOf(lang, "", "es", "en") {
			writeError(w, r, http.StatusBadRequest, "language must be 'es' or 'en'")
			return
		}
		if !oneOf(audience, "", "adults", "kids", "all") {
			writeError(w, r, http.StatusBadRequest, "audience must be 'adults', 'kids' or 'all'")
			return
		}

		rows, err := pool.Query(r.Context(), listSessionsSQL, kind, lang, audience)
		if err != nil {
			logFrom(r).Error("list sessions: query failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load sessions")
			return
		}
		defer rows.Close()

		sessions := []sessionDTO{} // empty JSON array [] rather than null when nothing matches
		for rows.Next() {
			var s sessionDTO
			if err := rows.Scan(&s.ID, &s.Kind, &s.TargetLanguage, &s.Audience, &s.Title,
				&s.StartsAt, &s.EndsAt, &s.MaxSeats, &s.SeatsTaken, &s.Status); err != nil {
				logFrom(r).Error("list sessions: scan failed", "err", err)
				writeError(w, r, http.StatusInternalServerError, "could not load sessions")
				return
			}
			s.StartsAt, s.EndsAt = s.StartsAt.UTC(), s.EndsAt.UTC()
			s.DurationMinutes = int(s.EndsAt.Sub(s.StartsAt).Minutes())
			if s.MaxSeats != nil {
				left := max(*s.MaxSeats-s.SeatsTaken, 0)
				s.SeatsLeft = &left
			}
			sessions = append(sessions, s)
		}
		if err := rows.Err(); err != nil { // errors that happen mid-stream show up here
			logFrom(r).Error("list sessions: row iteration failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load sessions")
			return
		}

		writeJSON(w, http.StatusOK, sessions)
	}
}

// writeError sends a JSON error body that includes the request ID,
// so a user or the front-end can quote it when reporting a problem.
func writeError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	id, _ := r.Context().Value(requestIDKey).(string)
	writeJSON(w, status, map[string]string{"error": msg, "request_id": id})
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}