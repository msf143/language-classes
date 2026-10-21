package main

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	privateMinNotice  = time.Hour              // private classes can be booked from 1 hour ahead
	slotWindowDefault = 14 * 24 * time.Hour    // GET /private-slots looks this far ahead by default
	slotWindowMax     = 8 * 7 * 24 * time.Hour // and at most this far
)

// span is a half-open time range [start, end), the same convention as
// Postgres's tstzrange: the start is included, the end is not. So a class
// 7:00-8:00 and one 8:00-9:00 touch without overlapping.
type span struct {
	start, end time.Time
}

// overlaps reports whether two half-open ranges share any moment.
// (The same test Postgres's && does on ranges.)
func (a span) overlaps(b span) bool {
	return a.start.Before(b.end) && b.start.Before(a.end)
}

// bookableStarts returns the times a class of the given length could start:
//   - on the grid (every 15 minutes: :00, :15, :30, :45),
//   - not before notBefore (now + the minimum notice),
//   - with the whole class [start, start+length) inside the teacher's open
//     blocks and not overlapping any busy span (his booked sessions).
//
// The teacher's break is inside each class (it may end a few minutes early),
// so classes can sit back to back: no extra gap is added around busy spans.
//
// Pure arithmetic: no database, no clock. The database supplies the blocks and
// busy spans, and still has the last word when someone books (the exclusion
// constraint on session.occupied).
func bookableStarts(blocks, busy []span, length time.Duration, notBefore time.Time, grid time.Duration) []time.Time {
	var starts []time.Time
	for _, b := range mergeTouching(blocks) {
		t := ceilTo(maxTime(b.start, notBefore), grid)
		for ; !t.Add(length).After(b.end); t = t.Add(grid) { // the class must end by the block's end
			class := span{t, t.Add(length)}
			if !slices.ContainsFunc(busy, class.overlaps) {
				starts = append(starts, t)
			}
		}
	}
	return starts
}

// mergeTouching sorts blocks by start and joins any that touch or overlap, so
// 9:00-10:00 and 10:00-12:00 count as 9:00-12:00 (a 9:30 class fits).
func mergeTouching(blocks []span) []span {
	sorted := slices.Clone(blocks)
	slices.SortFunc(sorted, func(a, b span) int { return a.start.Compare(b.start) })
	var merged []span
	for _, b := range sorted {
		if n := len(merged); n > 0 && !b.start.After(merged[n-1].end) {
			merged[n-1].end = maxTime(merged[n-1].end, b.end) // touches the previous one: extend it
			continue
		}
		merged = append(merged, b)
	}
	return merged
}

// ceilTo rounds t up to the next multiple of d (t itself if it's already on it).
func ceilTo(t time.Time, d time.Duration) time.Time {
	if r := t.Truncate(d); !r.Equal(t) {
		return r.Add(d)
	}
	return t
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// teacherSlotsDTO is one teacher's bookable private-class times for one length.
type teacherSlotsDTO struct {
	TeacherID       string     `json:"teacher_id"`
	TeacherName     string     `json:"teacher_name"`
	DurationMinutes int        `json:"duration_minutes"`
	Prices          []priceDTO `json:"prices"` // per person, by party size (1-4)
	Slots           []slotDTO  `json:"slots"`
}

type slotDTO struct {
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
}

// listPrivateSlots handles GET /private-slots?duration=60&from=...&to=...
// (public, like the catalog): for each teacher who offers that length and has
// a private price for it, the times a class could start. from/to are RFC 3339
// and optional (default: now to 14 days ahead; at most 8 weeks).
func listPrivateSlots(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		minutes, err := strconv.Atoi(q.Get("duration"))
		if err != nil || minutes < 15 || minutes > 240 {
			writeError(w, r, http.StatusBadRequest, "duration must be the class length in minutes, e.g. duration=60")
			return
		}
		length := time.Duration(minutes) * time.Minute
		from, to := time.Now(), time.Now().Add(slotWindowDefault)
		if v := q.Get("from"); v != "" {
			if from, err = time.Parse(time.RFC3339, v); err != nil {
				writeError(w, r, http.StatusBadRequest, "from must be a time like 2026-11-03T00:00:00Z")
				return
			}
		}
		if v := q.Get("to"); v != "" {
			if to, err = time.Parse(time.RFC3339, v); err != nil {
				writeError(w, r, http.StatusBadRequest, "to must be a time like 2026-11-17T00:00:00Z")
				return
			}
		} else if q.Get("from") != "" {
			to = from.Add(slotWindowDefault)
		}
		if !to.After(from) || to.Sub(from) > slotWindowMax {
			writeError(w, r, http.StatusBadRequest, "to must be after from, and at most 8 weeks later")
			return
		}

		teachers, err := privateSlots(r.Context(), pool, minutes, length, from, to)
		if err != nil {
			logFrom(r).Error("private slots failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load the available times")
			return
		}
		writeJSON(w, http.StatusOK, teachers)
	}
}

// privateSlots gathers each eligible teacher's blocks and busy sessions in
// [from, to) and lets bookableStarts do the arithmetic.
func privateSlots(ctx context.Context, pool *pgxpool.Pool, minutes int, length time.Duration, from, to time.Time) ([]teacherSlotsDTO, error) {
	// Teachers who offer this length and have private prices for it right now,
	// with those prices (as JSON, like GET /sessions), and the database's clock.
	rows, err := pool.Query(ctx, `
		SELECT t.id::text, a.display_name,
		       (SELECT json_agg(json_build_object(
		                   'party_size', p.party_size,
		                   'price_per_person_cents', p.price_per_person_cents,
		                   'currency', p.currency) ORDER BY p.party_size)
		        FROM price_tier p
		        WHERE p.teacher_id = t.id AND p.kind = 'private'
		          AND p.duration_minutes = $1 AND p.valid @> now()),
		       now()
		FROM teacher t JOIN account a ON a.id = t.account_id
		WHERE t.status = 'active' AND $1 = ANY (t.offered_durations)
		  AND EXISTS (SELECT 1 FROM price_tier p
		              WHERE p.teacher_id = t.id AND p.kind = 'private'
		                AND p.duration_minutes = $1 AND p.valid @> now())
		ORDER BY a.display_name`, minutes)
	if err != nil {
		return nil, err
	}
	teachers := []teacherSlotsDTO{}
	var now time.Time
	for rows.Next() {
		ts := teacherSlotsDTO{DurationMinutes: minutes, Slots: []slotDTO{}}
		if err := rows.Scan(&ts.TeacherID, &ts.TeacherName, &ts.Prices, &now); err != nil {
			rows.Close()
			return nil, err
		}
		teachers = append(teachers, ts)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	notBefore := maxTime(now.Add(privateMinNotice), from)
	window := span{from, to.Add(length)} // a class starting just before "to" ends after it
	for i := range teachers {
		blocks, err := spans(ctx, pool, `
			SELECT lower(period), upper(period) FROM availability_block
			WHERE teacher_id = $1 AND status = 'open' AND period && tstzrange($2, $3)`,
			teachers[i].TeacherID, window.start, window.end)
		if err != nil {
			return nil, err
		}
		// Busy = any session of his that isn't cancelled: group or private.
		busy, err := spans(ctx, pool, `
			SELECT lower(occupied), upper(occupied) FROM session
			WHERE teacher_id = $1 AND status <> 'cancelled' AND occupied && tstzrange($2, $3)`,
			teachers[i].TeacherID, window.start, window.end)
		if err != nil {
			return nil, err
		}
		for _, t := range bookableStarts(blocks, busy, length, notBefore, slotGrid) {
			if t.Before(to) {
				teachers[i].Slots = append(teachers[i].Slots, slotDTO{t.UTC(), t.Add(length).UTC()})
			}
		}
	}
	return teachers, nil
}

// spans runs a query whose rows are (start, end) pairs.
func spans(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) ([]span, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []span
	for rows.Next() {
		var s span
		if err := rows.Scan(&s.start, &s.end); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
