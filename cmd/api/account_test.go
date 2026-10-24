package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// me calls a My-account handler as the signed-in student (wrapped in
// requireSignIn, as main.go does).
func me(s student, h http.HandlerFunc, method, body, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/me", strings.NewReader(body))
	if id != "" {
		req.SetPathValue("id", id)
	}
	req.AddCookie(s.cookie)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestMyAccount(t *testing.T) {
	pool := testPool(t)
	s := newStudent(t, pool, 5000, false) // one learner: themself
	wrap := func(h http.HandlerFunc) http.HandlerFunc { return requireSignIn(pool, h) }
	learners := func() []learnerDTO {
		var m struct {
			Learners []learnerDTO `json:"learners"`
		}
		json.Unmarshal(me(s, wrap(getMe(pool)), "GET", "", "").Body.Bytes(), &m)
		return m.Learners
	}

	t.Run("my name", func(t *testing.T) {
		if rec := me(s, wrap(putMe(pool)), "PUT", `{"display_name": "  Mike  "}`, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"Mike"`) {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
		for _, body := range []string{`{"display_name": " "}`, `{"display_name": "` + strings.Repeat("x", 61) + `"}`, `{"name": "Mike"}`} {
			if rec := me(s, wrap(putMe(pool)), "PUT", body, ""); rec.Code != 400 {
				t.Errorf("%s: %d", body, rec.Code)
			}
		}
	})

	var sofia learnerDTO
	t.Run("add a family member", func(t *testing.T) {
		rec := me(s, wrap(addLearner(pool)), "POST", `{"display_name": "Sofía", "is_minor": true, "native_language": "en"}`, "")
		json.Unmarshal(rec.Body.Bytes(), &sofia)
		if rec.Code != 201 || sofia.ID == "" || !sofia.IsMinor {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if l := learners(); len(l) != 2 || l[1].DisplayName != "Sofía" || l[1].NativeLanguage != "en" || !l[1].IsMinor {
			t.Errorf("/me learners: %+v", l)
		}
		if rec := me(s, wrap(addLearner(pool)), "POST", `{"display_name": "X", "native_language": "fr"}`, ""); rec.Code != 400 || errorCode(t, rec) != "invalid_learner" {
			t.Errorf("French: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("edit: mine yes, someone else's no", func(t *testing.T) {
		if rec := me(s, wrap(updateLearner(pool)), "PUT", `{"display_name": "Sofía M.", "is_minor": true, "native_language": "es"}`, sofia.ID); rec.Code != 200 {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
		other := newStudent(t, pool, 0, false)
		if rec := me(other, wrap(updateLearner(pool)), "PUT", `{"display_name": "Hacked", "native_language": "en"}`, sofia.ID); rec.Code != 404 {
			t.Errorf("someone else's learner: %d", rec.Code)
		}
	})

	t.Run("archive: refused while booked, then gone", func(t *testing.T) {
		class := newSession(t, pool, newTeacher(t, pool), sessionOpts{slot: 30, audience: "all"})
		rec := book(pool, s.cookie, newUUIDv7(), class, sofia.ID)
		if rec.Code != http.StatusCreated {
			t.Fatalf("booking Sofía: %d %s", rec.Code, rec.Body)
		}
		b := decodeBooking(t, rec)
		if rec := me(s, wrap(archiveLearner(pool)), "POST", "", sofia.ID); rec.Code != 409 || errorCode(t, rec) != "learner_booked" {
			t.Errorf("while booked: %d %s", rec.Code, rec.Body)
		}
		if rec := cancel(pool, s.cookie, b.ID); rec.Code != 200 {
			t.Fatalf("cancel: %d", rec.Code)
		}
		for range 2 { // archiving twice is the same as once
			if rec := me(s, wrap(archiveLearner(pool)), "POST", "", sofia.ID); rec.Code != 200 {
				t.Errorf("archive: %d %s", rec.Code, rec.Body)
			}
		}
		if l := learners(); len(l) != 1 {
			t.Errorf("still listed: %+v", l)
		}
		if rec := book(pool, s.cookie, newUUIDv7(), class, sofia.ID); errorCode(t, rec) != "unknown_learner" {
			t.Errorf("booking an archived learner: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("at most 10 people", func(t *testing.T) {
		for i := len(learners()); i < maxActiveLearners; i++ {
			if rec := me(s, wrap(addLearner(pool)), "POST", `{"display_name": "Kid", "is_minor": true, "native_language": "en"}`, ""); rec.Code != 201 {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
		}
		if rec := me(s, wrap(addLearner(pool)), "POST", `{"display_name": "One too many", "native_language": "en"}`, ""); rec.Code != 409 || errorCode(t, rec) != "too_many_learners" {
			t.Errorf("11th: %d %s", rec.Code, rec.Body)
		}
	})
}

// A booking for Sofía is in progress (the test plays it: account locked,
// booking + attendee written, not yet committed) when her parent archives her.
// With the archive's account lock, the archive waits, then sees the booking
// and refuses. Without it, it doesn't see the uncommitted booking, archives
// her, and the booking then commits: an archived learner booked into a class.
func TestArchiveLearnerRace(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := newStudent(t, pool, 5000, false, true) // themself + a child
	child := s.learners[1]
	class := newSession(t, pool, newTeacher(t, pool), sessionOpts{slot: 31, audience: "all"})

	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	bookingID := newUUIDv7()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`SELECT 1 FROM account WHERE id = $1 FOR UPDATE`, []any{s.accountID}},
		{`INSERT INTO booking (id, session_id, payer_account_id, party_size, price_per_person_cents, total_cents, idempotency_key)
		  VALUES ($1, $2, $3, 1, 1500, 1500, $4)`, []any{bookingID, class, s.accountID, "race-" + bookingID}},
		{`INSERT INTO booking_attendee (id, booking_id, learner_id) VALUES ($1, $2, $3)`, []any{newUUIDv7(), bookingID, child}},
	} {
		if _, err := gate.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- me(s, requireSignIn(pool, archiveLearner(pool)), "POST", "", child) }()
	deadline := time.Now().Add(3 * time.Second)
	for len(done) == 0 && count(t, pool, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`) == 0 &&
		time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if rec := <-done; rec.Code != http.StatusConflict {
		t.Errorf("archive: %d %s, want 409 learner_booked", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT count(*) FROM learner WHERE id = $1 AND status = 'active'`, child); n != 1 {
		t.Error("the child was archived while booked")
	}
}
