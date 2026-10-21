package main

import (
	"slices"
	"time"
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
