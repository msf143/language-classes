package main

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// sessionDTO is the JSON shape of a session in API responses.
// Pointer fields are optional: they become null in JSON when empty.
type sessionDTO struct {
	ID              string     `json:"id"`
	Kind            string     `json:"kind"`            // "group" or "private"
	TargetLanguage  string     `json:"target_language"` // language being taught: "es" or "en"
	Audience        string     `json:"audience"`        // "adults", "kids" or "all"
	Title           *string    `json:"title"`
	StartsAt        time.Time  `json:"starts_at"` // always UTC; the front-end converts for display
	EndsAt          time.Time  `json:"ends_at"`
	DurationMinutes int        `json:"duration_minutes"`
	MaxSeats        *int       `json:"max_seats"` // null = no cap
	SeatsTaken      int        `json:"seats_taken"`
	SeatsLeft       *int       `json:"seats_left"` // null when there is no cap
	Status          string     `json:"status"`
	Prices          []priceDTO `json:"prices"` // price in force now, per party size; [] = not bookable
}

// priceDTO is the price per person for one party size. The total for a party
// is price_per_person_cents * party_size (the same rule POST /bookings uses).
type priceDTO struct {
	PartySize           int    `json:"party_size"`
	PricePerPersonCents int    `json:"price_per_person_cents"`
	Currency            string `json:"currency"`
}

// Upcoming, bookable sessions with seat counts.
// Seats taken = sum of party sizes on bookings that aren't cancelled.
// An empty-string filter parameter means "don't filter on this".
// Audience 'all' sessions are open to any age, so they match every audience filter.
//
// Group sessions are public. A private session is listed only for the
// accounts it belongs to ($4): its teacher, the payer of a live booking, or
// a friend whose learner joined that booking. When nobody is signed in, $4 is
// NULL, every "= $4" is unknown (not true), and only group sessions remain.
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
       s.status,
       -- The price tiers in force now for this teacher, kind and length, as a JSON
       -- array (one element per party size); pgx decodes it into []priceDTO.
       (SELECT COALESCE(json_agg(json_build_object(
                   'party_size', p.party_size,
                   'price_per_person_cents', p.price_per_person_cents,
                   'currency', p.currency) ORDER BY p.party_size), '[]')
        FROM price_tier p
        WHERE p.teacher_id = s.teacher_id AND p.kind = s.kind
          AND p.duration_minutes = EXTRACT(epoch FROM s.ends_at - s.starts_at)::int / 60
          AND p.valid @> now()) AS prices
FROM session s
LEFT JOIN booking b ON b.session_id = s.id
WHERE s.status IN ('scheduled', 'confirmed')
  AND s.starts_at > now()
  AND ($1::text = '' OR s.kind = $1)
  AND ($2::text = '' OR s.target_language = $2)
  AND ($3::text = '' OR s.audience IN ($3, 'all'))
  -- group classes of active teachers (an inactive teacher's are hidden, not cancelled);
  -- private ones only for their teacher or the people in them
  AND ((s.kind = 'group' AND EXISTS (SELECT 1 FROM teacher ta WHERE ta.id = s.teacher_id AND ta.status = 'active'))
       OR EXISTS (SELECT 1 FROM teacher t WHERE t.id = s.teacher_id AND t.account_id = $4)
       OR EXISTS (SELECT 1 FROM booking pb
                  LEFT JOIN booking_attendee ba ON ba.booking_id = pb.id AND ba.status = 'joined'
                  LEFT JOIN learner l ON l.id = ba.learner_id
                  WHERE pb.session_id = s.id AND pb.status <> 'cancelled'
                    AND (pb.payer_account_id = $4 OR l.account_id = $4)))
GROUP BY s.id
ORDER BY s.starts_at
LIMIT 100`

// listSessions handles GET /sessions?kind=group&language=es&audience=kids
// All filters are optional. Wrap it in signedIn: private sessions are only
// listed for the accounts they belong to, and kind=private needs a sign-in.
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

		var accountID *string // nil (SQL NULL) when signed out
		if a, ok := accountFrom(r); ok {
			accountID = &a.ID
		} else if kind == "private" {
			writeError(w, r, http.StatusUnauthorized, "please sign in to see your private sessions")
			return
		}

		rows, err := pool.Query(r.Context(), listSessionsSQL, kind, lang, audience, accountID)
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
				&s.StartsAt, &s.EndsAt, &s.MaxSeats, &s.SeatsTaken, &s.Status, &s.Prices); err != nil {
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
