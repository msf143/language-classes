package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// newSignedInTeacher creates an account in Bogotá with an active teacher
// profile, signs it in, and returns the cookie and the teacher's ID.
func newSignedInTeacher(t *testing.T, pool *pgxpool.Pool) (*http.Cookie, string) {
	t.Helper()
	ctx := context.Background()
	accountID, email := newAccount(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE account SET timezone = 'America/Bogota' WHERE id = $1`, accountID); err != nil {
		t.Fatal(err)
	}
	teacherID := newUUIDv7()
	if _, err := pool.Exec(ctx, `INSERT INTO teacher (id, account_id) VALUES ($1, $2)`, teacherID, accountID); err != nil {
		t.Fatal(err)
	}
	return signIn(t, pool, email), teacherID
}

// send calls a teacher handler (wrapped in requireTeacher, as main.go does).
func send(pool *pgxpool.Pool, h http.HandlerFunc, method, path, body, pathID string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if pathID != "" {
		req.SetPathValue("id", pathID)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	requireTeacher(pool, h)(rec, req)
	return rec
}

func publishBody(start, end time.Time, weeks int) string {
	b, _ := json.Marshal(map[string]any{"starts_at": start, "ends_at": end, "repeat_weeks": weeks})
	return string(b)
}

func decodeBlocks(t *testing.T, rec *httptest.ResponseRecorder) []blockDTO {
	t.Helper()
	var blocks []blockDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &blocks); err != nil {
		t.Fatalf("body is not a list of blocks: %s", rec.Body)
	}
	return blocks
}

func TestTeacherAvailability(t *testing.T) {
	pool := testPool(t)
	cookie, teacherID := newSignedInTeacher(t, pool)
	publish := publishAvailability(pool)
	bogota, _ := time.LoadLocation("America/Bogota")

	// A Tuesday three or more weeks ahead, 9:00-12:00 in Bogotá (far enough that the
	// overlap case below, which starts two weeks earlier, is still in the future).
	day := time.Now().In(bogota).AddDate(0, 0, 21)
	for day.Weekday() != time.Tuesday {
		day = day.AddDate(0, 0, 1)
	}
	nineAM := time.Date(day.Year(), day.Month(), day.Day(), 9, 0, 0, 0, bogota)
	noon := nineAM.Add(3 * time.Hour)

	t.Run("publish one block, then it's listed", func(t *testing.T) {
		rec := send(pool, publish, "POST", "/teacher/availability", publishBody(nineAM, noon, 0), "", cookie)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body)
		}
		if b := decodeBlocks(t, rec); len(b) != 1 || !b[0].StartsAt.Equal(nineAM) || !b[0].EndsAt.Equal(noon) {
			t.Errorf("published %+v, want one block 9:00-12:00 Bogotá", b)
		}
		listed := decodeBlocks(t, send(pool, listAvailability(pool), "GET", "/teacher/availability", "", "", cookie))
		if len(listed) != 1 || !listed[0].StartsAt.Equal(nineAM) {
			t.Errorf("listed %+v, want the one block", listed)
		}
	})

	t.Run("repeat 3 weeks: three blocks, same local time each week", func(t *testing.T) {
		evening := time.Date(day.Year(), day.Month(), day.Day(), 19, 0, 0, 0, bogota)
		rec := send(pool, publish, "POST", "/teacher/availability", publishBody(evening, evening.Add(2*time.Hour), 3), "", cookie)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body)
		}
		blocks := decodeBlocks(t, rec)
		if len(blocks) != 3 {
			t.Fatalf("%d blocks, want 3", len(blocks))
		}
		for i, b := range blocks {
			local := b.StartsAt.In(bogota)
			if local.Weekday() != time.Tuesday || local.Hour() != 19 || !b.StartsAt.Equal(evening.AddDate(0, 0, 7*i)) {
				t.Errorf("block %d starts %v (Bogotá), want Tuesday 19:00, week %d", i, local, i)
			}
		}
	})

	t.Run("overlapping an open block: refused, and nothing is created", func(t *testing.T) {
		before := count(t, pool, `SELECT count(*) FROM availability_block WHERE teacher_id = $1`, teacherID)
		// Starts two weeks earlier and repeats 3 weeks: only the third overlaps 9:00-12:00.
		overlap := nineAM.AddDate(0, 0, -14).Add(2 * time.Hour) // 11:00-13:00, ending in the week of the first block
		rec := send(pool, publish, "POST", "/teacher/availability", publishBody(overlap, overlap.Add(2*time.Hour), 3), "", cookie)
		if rec.Code != http.StatusConflict || errorCode(t, rec) != "block_overlap" {
			t.Errorf("status %d, body %s; want 409 block_overlap", rec.Code, rec.Body)
		}
		if after := count(t, pool, `SELECT count(*) FROM availability_block WHERE teacher_id = $1`, teacherID); after != before {
			t.Errorf("%d blocks before, %d after: the first two weeks should have rolled back", before, after)
		}
	})

	t.Run("withdraw: twice is fine, then the time can be published again", func(t *testing.T) {
		listed := decodeBlocks(t, send(pool, listAvailability(pool), "GET", "/teacher/availability", "", "", cookie))
		var morning blockDTO
		for _, b := range listed {
			if b.StartsAt.Equal(nineAM) {
				morning = b
			}
		}
		for i := range 2 {
			rec := send(pool, withdrawAvailability(pool), "POST", "/teacher/availability/x/withdraw", "", morning.ID, cookie)
			var got blockDTO
			_ = json.Unmarshal(rec.Body.Bytes(), &got)
			if rec.Code != http.StatusOK || got.Status != "withdrawn" {
				t.Errorf("withdraw #%d: status %d, body %s", i+1, rec.Code, rec.Body)
			}
		}
		if rec := send(pool, publish, "POST", "/teacher/availability", publishBody(nineAM, noon, 1), "", cookie); rec.Code != http.StatusCreated {
			t.Errorf("republishing a withdrawn time: status %d, body %s", rec.Code, rec.Body)
		}
	})

	t.Run("bad requests", func(t *testing.T) {
		tests := []struct{ name, body string }{
			{"not JSON", "hello"},
			{"ends before it starts", publishBody(noon, nineAM, 1)},
			{"in the past", publishBody(time.Now().Add(-2*time.Hour).Truncate(time.Hour), time.Now().Add(-time.Hour).Truncate(time.Hour), 1)},
			{"longer than 12 hours", publishBody(nineAM, nineAM.Add(13*time.Hour), 1)},
			{"not on a quarter hour", publishBody(nineAM.Add(10*time.Minute), noon, 1)},
			{"repeat too many weeks", publishBody(nineAM.AddDate(0, 1, 0), noon.AddDate(0, 1, 0), 13)},
		}
		for _, tt := range tests {
			if rec := send(pool, publish, "POST", "/teacher/availability", tt.body, "", cookie); rec.Code != http.StatusBadRequest {
				t.Errorf("%s: status %d, body %s; want 400", tt.name, rec.Code, rec.Body)
			}
		}
	})
}

func TestRequireTeacher(t *testing.T) {
	pool := testPool(t)
	teacherCookie, _ := newSignedInTeacher(t, pool)
	otherTeacherCookie, _ := newSignedInTeacher(t, pool)
	student := newStudent(t, pool, 0, false)

	if rec := send(pool, listAvailability(pool), "GET", "/teacher/availability", "", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d, want 401", rec.Code)
	}
	if rec := send(pool, listAvailability(pool), "GET", "/teacher/availability", "", "", student.cookie); rec.Code != http.StatusForbidden || errorCode(t, rec) != "not_teacher" {
		t.Errorf("student: %d %s, want 403 not_teacher", rec.Code, rec.Body)
	}

	// A teacher can't see or withdraw another teacher's blocks.
	start := time.Now().Add(72 * time.Hour).Truncate(time.Hour)
	rec := send(pool, publishAvailability(pool), "POST", "/teacher/availability", publishBody(start, start.Add(time.Hour), 1), "", teacherCookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body)
	}
	id := decodeBlocks(t, rec)[0].ID
	if listed := decodeBlocks(t, send(pool, listAvailability(pool), "GET", "/teacher/availability", "", "", otherTeacherCookie)); len(listed) != 0 {
		t.Errorf("the other teacher sees %d blocks, want 0", len(listed))
	}
	if rec := send(pool, withdrawAvailability(pool), "POST", "/teacher/availability/x/withdraw", "", id, otherTeacherCookie); rec.Code != http.StatusNotFound {
		t.Errorf("withdrawing another teacher's block: %d, want 404", rec.Code)
	}
}
