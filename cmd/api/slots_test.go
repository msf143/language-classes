package main

import (
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
