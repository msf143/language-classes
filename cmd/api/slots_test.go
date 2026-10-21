package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"
)

func TestBookableStarts(t *testing.T) {
	// Times on one example day, written as "19:00". UTC keeps the arithmetic
	// plain; the function never looks at time zones.
	day := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	at := func(hhmm string) time.Time {
		c, err := time.Parse("15:04", hhmm)
		if err != nil {
			t.Fatal(err)
		}
		return day.Add(time.Duration(c.Hour())*time.Hour + time.Duration(c.Minute())*time.Minute)
	}
	sp := func(from, to string) span { return span{at(from), at(to)} }
	clock := func(ts []time.Time) []string {
		out := []string{}
		for _, t := range ts {
			out = append(out, t.Format("15:04"))
		}
		return out
	}

	tests := []struct {
		name      string
		blocks    []span
		busy      []span
		minutes   int
		notBefore string // "" = long ago
		want      []string
	}{
		{"no blocks, no slots", nil, nil, 60, "", []string{}},
		{"a free 2-hour block: every quarter hour that still fits a 60-minute class",
			[]span{sp("19:00", "21:00")}, nil, 60, "",
			[]string{"19:00", "19:15", "19:30", "19:45", "20:00"}},
		{"7-8 PM booked: the next class starts right at 8 (break is inside the class)",
			[]span{sp("19:00", "21:00")}, []span{sp("19:00", "20:00")}, 60, "",
			[]string{"20:00"}},
		{"7-9 PM block holds exactly two back-to-back 1-hour classes",
			[]span{sp("19:00", "21:00")}, []span{sp("19:00", "20:00"), sp("20:00", "21:00")}, 60, "",
			[]string{}},
		{"the design example: block 9-12, 10-11 booked, 60 minutes",
			[]span{sp("09:00", "12:00")}, []span{sp("10:00", "11:00")}, 60, "",
			[]string{"09:00", "11:00"}},
		{"same, 45 minutes",
			[]span{sp("09:00", "12:00")}, []span{sp("10:00", "11:00")}, 45, "",
			[]string{"09:00", "09:15", "11:00", "11:15"}},
		{"a booking that started before the block still blocks its start",
			[]span{sp("19:00", "21:00")}, []span{sp("18:30", "19:30")}, 60, "",
			[]string{"19:30", "19:45", "20:00"}},
		{"minimum notice: nothing before notBefore, rounded up to the grid",
			[]span{sp("19:00", "21:00")}, nil, 60, "19:10",
			[]string{"19:15", "19:30", "19:45", "20:00"}},
		{"minimum notice exactly on the grid: that time is allowed",
			[]span{sp("19:00", "21:00")}, nil, 60, "19:15",
			[]string{"19:15", "19:30", "19:45", "20:00"}},
		{"a block shorter than the class: no slots",
			[]span{sp("19:00", "19:30")}, nil, 60, "",
			[]string{}},
		{"touching blocks count as one (9:30 fits across 10:00)",
			[]span{sp("10:00", "12:00"), sp("09:00", "10:00")}, nil, 60, "",
			[]string{"09:00", "09:15", "09:30", "09:45", "10:00", "10:15", "10:30", "10:45", "11:00"}},
		{"separate blocks, given out of order: slots come back in time order",
			[]span{sp("19:00", "20:00"), sp("09:00", "10:00")}, nil, 60, "",
			[]string{"09:00", "19:00"}},
		{"a block that starts off the grid: first slot on the next quarter hour",
			[]span{sp("19:05", "20:30")}, nil, 60, "",
			[]string{"19:15", "19:30"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			notBefore := day.Add(-24 * time.Hour)
			if tt.notBefore != "" {
				notBefore = at(tt.notBefore)
			}
			got := clock(bookableStarts(tt.blocks, tt.busy, time.Duration(tt.minutes)*time.Minute, notBefore, 15*time.Minute))
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %v\nwant %v", got, tt.want)
			}
		})
	}
}

// GET /private-slots reads blocks, booked sessions and prices from the
// database and returns what bookableStarts computes.
func TestListPrivateSlots(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	teacher := newTeacher(t, pool) // offers 30/45/60/90 (the default); has group prices only
	for size, cents := range []int{2500, 2000, 1700, 1500} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO price_tier (id, teacher_id, kind, duration_minutes, party_size, price_per_person_cents)
			VALUES ($1, $2, 'private', 60, $3, $4)`, newUUIDv7(), teacher, size+1, cents); err != nil {
			t.Fatal(err)
		}
	}
	// Three days from now: a block 19:00-22:00 UTC, and a class already booked 20:00-21:00.
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 3)
	at := func(h int) time.Time { return day.Add(time.Duration(h) * time.Hour) }
	if _, err := pool.Exec(ctx, `INSERT INTO availability_block (id, teacher_id, period) VALUES ($1, $2, tstzrange($3, $4))`,
		newUUIDv7(), teacher, at(19), at(22)); err != nil {
		t.Fatal(err)
	}
	booked := newUUIDv7()
	if _, err := pool.Exec(ctx, `
		INSERT INTO session (id, teacher_id, kind, target_language, starts_at, ends_at, occupied)
		VALUES ($1, $2, 'private', 'es', $3, $4, tstzrange($3, $4))`, booked, teacher, at(20), at(21)); err != nil {
		t.Fatal(err)
	}
	cancelWhenDone(t, pool, booked)

	get := func(query string) (int, []teacherSlotsDTO) {
		rec := call(listPrivateSlots(pool), "GET", "/private-slots?"+query, nil)
		var got []teacherSlotsDTO
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		return rec.Code, got
	}
	window := "&from=" + day.Format(time.RFC3339) + "&to=" + day.AddDate(0, 0, 1).Format(time.RFC3339)
	mine := func(all []teacherSlotsDTO) *teacherSlotsDTO {
		for i := range all {
			if all[i].TeacherID == teacher {
				return &all[i]
			}
		}
		return nil
	}

	t.Run("60 minutes: 19:00 and 21:00, around the booked class", func(t *testing.T) {
		code, all := get("duration=60" + window)
		me := mine(all)
		if code != http.StatusOK || me == nil {
			t.Fatalf("status %d, teacher listed: %v", code, me != nil)
		}
		var got []string
		for _, s := range me.Slots {
			got = append(got, s.StartsAt.Format("15:04")+"-"+s.EndsAt.Format("15:04"))
		}
		if !slices.Equal(got, []string{"19:00-20:00", "21:00-22:00"}) {
			t.Errorf("slots %v, want [19:00-20:00 21:00-22:00]", got)
		}
		if len(me.Prices) != 4 || me.Prices[0].PricePerPersonCents != 2500 || me.Prices[3].PricePerPersonCents != 1500 {
			t.Errorf("prices %+v, want 2500/2000/1700/1500", me.Prices)
		}
	})

	t.Run("a length he offers but has no private price for: not listed", func(t *testing.T) {
		if _, all := get("duration=90" + window); mine(all) != nil {
			t.Error("teacher listed for 90 minutes without a 90-minute private price")
		}
	})

	t.Run("a length he doesn't offer: not listed, even with a price for it", func(t *testing.T) {
		// A 20-minute price exists, so "not in offered_durations" is the only reason left.
		if _, err := pool.Exec(ctx, `
			INSERT INTO price_tier (id, teacher_id, kind, duration_minutes, party_size, price_per_person_cents)
			VALUES ($1, $2, 'private', 20, 1, 1000)`, newUUIDv7(), teacher); err != nil {
			t.Fatal(err)
		}
		if _, all := get("duration=20" + window); mine(all) != nil {
			t.Error("teacher listed for 20 minutes, which isn't in offered_durations")
		}
	})

	t.Run("bad requests", func(t *testing.T) {
		for _, q := range []string{"", "duration=abc", "duration=5", "duration=60&from=yesterday",
			"duration=60&from=2026-11-01T00:00:00Z&to=2027-03-01T00:00:00Z"} {
			if code, _ := get(q); code != http.StatusBadRequest {
				t.Errorf("?%s: status %d, want 400", q, code)
			}
		}
	})
}
