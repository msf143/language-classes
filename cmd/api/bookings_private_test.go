package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// privateTeacher makes a teacher with private prices for 60 minutes only
// (per person: $25 / $20 / $17 / $15 for 1-4 people) and one open block,
// three days ahead, 19:00-22:00 UTC. It returns the teacher and that day.
func privateTeacher(t *testing.T, pool *pgxpool.Pool) (string, time.Time) {
	t.Helper()
	ctx := context.Background()
	teacher := newTeacher(t, pool)
	for size, cents := range []int{2500, 2000, 1700, 1500} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO price_tier (id, teacher_id, kind, duration_minutes, party_size, price_per_person_cents)
			VALUES ($1, $2, 'private', 60, $3, $4)`, newUUIDv7(), teacher, size+1, cents); err != nil {
			t.Fatal(err)
		}
	}
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 3)
	addBlock(t, pool, teacher, day.Add(19*time.Hour), day.Add(22*time.Hour))
	return teacher, day
}

func addBlock(t *testing.T, pool *pgxpool.Pool, teacher string, from, to time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO availability_block (id, teacher_id, period) VALUES ($1, $2, tstzrange($3, $4))`,
		newUUIDv7(), teacher, from, to); err != nil {
		t.Fatal(err)
	}
}

// bookPrivate sends POST /bookings with a "private" body.
func bookPrivate(pool *pgxpool.Pool, cookie *http.Cookie, key, teacher string, start time.Time, minutes int, learners ...string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{
		"private": map[string]any{
			"teacher_id": teacher, "starts_at": start, "duration_minutes": minutes, "target_language": "es",
		},
		"learner_ids": learners,
	})
	req := httptest.NewRequest("POST", "/bookings", strings.NewReader(string(body)))
	req.Header.Set("Idempotency-Key", key)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	requireSignIn(pool, createBooking(pool))(rec, req)
	return rec
}

// slotStarts lists the teacher's 60-minute slot start times on that day, as "19:00".
func slotStarts(t *testing.T, pool *pgxpool.Pool, teacher string, day time.Time) []string {
	t.Helper()
	q := "/private-slots?duration=60&from=" + day.Format(time.RFC3339) + "&to=" + day.AddDate(0, 0, 1).Format(time.RFC3339)
	var all []teacherSlotsDTO
	if err := json.Unmarshal(call(listPrivateSlots(pool), "GET", q, nil).Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, ts := range all {
		if ts.TeacherID == teacher {
			for _, s := range ts.Slots {
				out = append(out, s.StartsAt.Format("15:04"))
			}
		}
	}
	return out
}

func TestBookPrivate(t *testing.T) {
	pool := testPool(t)
	teacher, day := privateTeacher(t, pool)
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }

	t.Run("book 19:00 for two: priced per person, session created, the slot is gone", func(t *testing.T) {
		s := newStudent(t, pool, 10000, false, true)
		rec := bookPrivate(pool, s.cookie, "k1", teacher, at(19, 0), 60, s.learners...)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body)
		}
		b := decodeBooking(t, rec)
		cancelWhenDone(t, pool, b.SessionID)
		if b.TotalCents != 4000 || b.PricePerPersonCents != 2000 || b.BalanceCents != 6000 {
			t.Errorf("booking %+v; want 2 x 2000 = 4000, balance 6000", b)
		}
		var kind, audience string
		var starts, ends time.Time
		if err := pool.QueryRow(context.Background(), `
			SELECT kind, audience, lower(occupied), upper(occupied) FROM session WHERE id = $1`, b.SessionID).
			Scan(&kind, &audience, &starts, &ends); err != nil {
			t.Fatal(err)
		}
		if kind != "private" || audience != "all" || !starts.Equal(at(19, 0)) || !ends.Equal(at(20, 0)) {
			t.Errorf("session: %s %s %v-%v; want private, all (adult + child), occupied 19:00-20:00", kind, audience, starts, ends)
		}
		if got := slotStarts(t, pool, teacher, day); strings.Join(got, " ") != "20:00 20:15 20:30 20:45 21:00" {
			t.Errorf("slots after booking: %v; want 20:00 ... 21:00 (back to back right after)", got)
		}

		t.Run("same key again: same booking; same key, other time: refused", func(t *testing.T) {
			again := bookPrivate(pool, s.cookie, "k1", teacher, at(19, 0), 60, s.learners...)
			if again.Code != http.StatusOK || decodeBooking(t, again).ID != b.ID {
				t.Errorf("replay: %d %s", again.Code, again.Body)
			}
			other := bookPrivate(pool, s.cookie, "k1", teacher, at(21, 0), 60, s.learners...)
			if other.Code != http.StatusUnprocessableEntity || errorCode(t, other) != "idempotency_key_reused" {
				t.Errorf("reused key: %d %s", other.Code, other.Body)
			}
		})

		t.Run("someone else, same time or overlapping it: slot_taken, nothing charged", func(t *testing.T) {
			late := newStudent(t, pool, 10000, false)
			for _, start := range []time.Time{at(19, 0), at(19, 30)} {
				rec := bookPrivate(pool, late.cookie, newUUIDv7(), teacher, start, 60, late.learners...)
				if rec.Code != http.StatusConflict || errorCode(t, rec) != "slot_taken" {
					t.Errorf("%s: %d %s; want 409 slot_taken", start.Format("15:04"), rec.Code, rec.Body)
				}
			}
			if got := balance(t, pool, late.accountID); got != 10000 {
				t.Errorf("balance %d, want 10000 (no charge)", got)
			}
		})
	})

	t.Run("refusals", func(t *testing.T) {
		s := newStudent(t, pool, 10000, false)
		poor := newStudent(t, pool, 1000, false)
		soon := ceilTo(time.Now().Add(30*time.Minute), slotGrid) // inside a block, but less than 1 hour ahead
		addBlock(t, pool, teacher, soon, soon.Add(2*time.Hour))
		tests := []struct {
			name       string
			who        student
			start      time.Time
			minutes    int
			wantStatus int
			wantCode   string
		}{
			{"ends after the block (21:30-22:30)", s, at(21, 30), 60, http.StatusConflict, "slot_unavailable"},
			{"outside any block (10:00)", s, at(10, 0), 60, http.StatusConflict, "slot_unavailable"},
			{"less than 1 hour ahead", s, soon, 60, http.StatusConflict, "slot_unavailable"},
			{"a length he doesn't offer (20 min)", s, at(21, 0), 20, http.StatusConflict, "slot_unavailable"},
			{"offered but no private price (30 min)", s, at(21, 0), 30, http.StatusConflict, "no_price"},
			{"not enough credit", poor, at(21, 0), 60, http.StatusConflict, "insufficient_credit"},
			{"not on the quarter hour", s, at(21, 10), 60, http.StatusBadRequest, ""},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				rec := bookPrivate(pool, tt.who.cookie, newUUIDv7(), teacher, tt.start, tt.minutes, tt.who.learners...)
				if rec.Code != tt.wantStatus || errorCode(t, rec) != tt.wantCode {
					t.Errorf("status %d, body %s; want %d %q", rec.Code, rec.Body, tt.wantStatus, tt.wantCode)
				}
			})
		}
		if n := count(t, pool, `SELECT count(*) FROM booking WHERE payer_account_id = ANY($1::uuid[])`,
			[]string{s.accountID, poor.accountID}); n != 0 {
			t.Errorf("%d bookings written by refused requests, want 0", n)
		}

		t.Run("both session_id and private: 400", func(t *testing.T) {
			body := `{"session_id": "` + newUUIDv7() + `", "private": {"teacher_id": "` + teacher + `"}, "learner_ids": ["` + s.learners[0] + `"]}`
			req := httptest.NewRequest("POST", "/bookings", strings.NewReader(body))
			req.Header.Set("Idempotency-Key", "k")
			req.AddCookie(s.cookie)
			rec := httptest.NewRecorder()
			requireSignIn(pool, createBooking(pool))(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status %d, want 400", rec.Code)
			}
		})
	})

	t.Run("touching blocks act as one: a class may span 10:00", func(t *testing.T) {
		teacher2, day2 := privateTeacher(t, pool)
		addBlock(t, pool, teacher2, day2.Add(9*time.Hour), day2.Add(10*time.Hour))
		addBlock(t, pool, teacher2, day2.Add(10*time.Hour), day2.Add(11*time.Hour))
		s := newStudent(t, pool, 10000, false)
		rec := bookPrivate(pool, s.cookie, "k", teacher2, day2.Add(9*time.Hour+30*time.Minute), 60, s.learners...)
		if rec.Code != http.StatusCreated {
			t.Errorf("9:30-10:30 across two touching blocks: %d %s, want 201", rec.Code, rec.Body)
		} else {
			cancelWhenDone(t, pool, decodeBooking(t, rec).SessionID)
		}
	})

	t.Run("cancelling a private booking gives the time back", func(t *testing.T) {
		s := newStudent(t, pool, 10000, false)
		rec := bookPrivate(pool, s.cookie, "k", teacher, at(21, 0), 60, s.learners...)
		if rec.Code != http.StatusCreated {
			t.Fatalf("booking 21:00: %d %s", rec.Code, rec.Body)
		}
		b := decodeBooking(t, rec)
		if got := slotStarts(t, pool, teacher, day); strings.Contains(strings.Join(got, " "), "21:00") {
			t.Fatalf("21:00 still offered after booking it: %v", got)
		}
		if rec := cancel(pool, s.cookie, b.ID); rec.Code != http.StatusOK {
			t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
		}
		if got := slotStarts(t, pool, teacher, day); !strings.Contains(strings.Join(got, " "), "21:00") {
			t.Errorf("21:00 not offered again after cancelling: %v", got)
		}
	})
}

// Four students press "Book" on the same private slot at once: the exclusion
// constraint lets exactly one session in; the others get slot_taken and pay nothing.
func TestBookPrivateRace(t *testing.T) {
	pool := testPool(t)
	teacher, day := privateTeacher(t, pool)
	start := day.Add(20 * time.Hour)

	const racers = 4
	var students []student
	for range racers {
		students = append(students, newStudent(t, pool, 10000, false))
	}
	var wg sync.WaitGroup
	codes := make(chan int, racers)
	go_ := make(chan struct{})
	for _, s := range students {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-go_
			rec := bookPrivate(pool, s.cookie, "k", teacher, start, 60, s.learners...)
			if rec.Code == http.StatusCreated {
				cancelWhenDone(t, pool, decodeBooking(t, rec).SessionID)
			}
			codes <- rec.Code
		}()
	}
	close(go_)
	wg.Wait()
	close(codes)
	got := map[int]int{}
	for c := range codes {
		got[c]++
	}
	if got[http.StatusCreated] != 1 || got[http.StatusConflict] != racers-1 {
		t.Errorf("statuses %v, want one 201 and %d x 409", got, racers-1)
	}
	if n := count(t, pool, `SELECT count(*) FROM session WHERE teacher_id = $1 AND status <> 'cancelled' AND starts_at = $2`, teacher, start); n != 1 {
		t.Errorf("%d sessions at 20:00, want exactly 1", n)
	}
	charged := 0
	for _, s := range students {
		if balance(t, pool, s.accountID) < 10000 {
			charged++
		}
	}
	if charged != 1 {
		t.Errorf("%d students charged, want exactly 1", charged)
	}
}
