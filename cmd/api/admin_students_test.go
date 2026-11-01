package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type studentsAnswer struct {
	Students []adminStudentDTO `json:"students"`
	Total    int               `json:"total"`
}

func searchStudents(t *testing.T, h http.HandlerFunc, cookie *http.Cookie, body string) (int, studentsAnswer) {
	t.Helper()
	req := httptest.NewRequest("POST", "/admin/students/search", strings.NewReader(body))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	var got studentsAnswer
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code, got
}

func TestAdminStudents(t *testing.T) {
	pool := migratedSchemaPool(t, 6) // a private schema: only this test's accounts
	ctx := context.Background()
	h := requireAdmin(pool, searchAdminStudents(pool))
	admin := adminCookie(t, pool)

	teacherID, _ := teacherLogin(t, pool)
	parent := newStudent(t, pool, 5000, false, true, true) // themself and two children
	photo := mustPhoto(t, pool, parent)
	pool.Exec(ctx, `UPDATE account SET display_name = 'Ana Gómez', email = 'ANA.gomez@example.com' WHERE id = $1`, parent.accountID)
	pool.Exec(ctx, `UPDATE learner SET status = 'archived' WHERE id = $1`, parent.learners[2]) // a removed child isn't counted
	pool.Exec(ctx, `UPDATE learner SET display_name = 'Hidden Child' WHERE id = $1`, parent.learners[1])
	session := newSession(t, pool, teacherID, sessionOpts{audience: "all"})
	book(pool, parent.cookie, newUUIDv7(), session, parent.learners[0])
	cancelled := newSession(t, pool, teacherID, sessionOpts{audience: "all", slot: 60})
	b := decodeBooking(t, book(pool, parent.cookie, newUUIDv7(), cancelled, parent.learners[0]))
	if _, err := cancelBookingTx(ctx, pool, parent.accountID, b.ID); err != nil {
		t.Fatal(err)
	}
	// A friend attending someone else's booking counts for the friend.
	friend := newStudent(t, pool, 0, false)
	pool.Exec(ctx, `UPDATE account SET display_name = 'Luis 100%' WHERE id = $1`, friend.accountID)
	pb := decodeBooking(t, book(pool, parent.cookie, newUUIDv7(), newSession(t, pool, teacherID, sessionOpts{audience: "all", slot: 70}), parent.learners[0]))
	pool.Exec(ctx, `INSERT INTO booking_attendee (id, booking_id, learner_id, status) VALUES ($1, $2, $3, 'joined')`, newUUIDv7(), pb.ID, friend.learners[0])

	by := func(list []adminStudentDTO, id string) *adminStudentDTO {
		for i := range list {
			if list[i].ID == id {
				return &list[i]
			}
		}
		return nil
	}

	t.Run("who may ask", func(t *testing.T) {
		if code, _ := searchStudents(t, h, nil, `{}`); code != 401 {
			t.Errorf("signed out: %d", code)
		}
		if code, _ := searchStudents(t, h, friend.cookie, `{}`); code != 403 {
			t.Errorf("a student: %d", code)
		}
	})

	t.Run("a row", func(t *testing.T) {
		code, got := searchStudents(t, h, admin, `{"q": ""}`)
		if code != 200 {
			t.Fatalf("%d", code)
		}
		s := by(got.Students, parent.accountID)
		if s == nil {
			t.Fatalf("not listed: %+v", got)
		}
		if s.PhotoID == nil || *s.PhotoID != photo || s.Children != 1 || s.Bookings != 2 || s.Status != "active" {
			t.Errorf("%+v (photo %v), want 1 child (one archived), 2 bookings (one cancelled)", s, s.PhotoID)
		}
		if f := by(got.Students, friend.accountID); f == nil || f.Bookings != 1 || f.PhotoID != nil {
			t.Errorf("the friend: %+v", f)
		}
		for _, x := range got.Students {
			if x.DisplayName == "Hidden Child" {
				t.Error("a child is listed as a person")
			}
		}
		raw := httptest.NewRecorder()
		h(raw, func() *http.Request {
			req := httptest.NewRequest("POST", "/admin/students/search", strings.NewReader(`{}`))
			req.AddCookie(admin)
			return req
		}())
		if strings.Contains(raw.Body.String(), "Hidden Child") {
			t.Error("a child's name is in the answer")
		}
	})

	t.Run("teachers aren't students", func(t *testing.T) {
		var teacherAccount string
		pool.QueryRow(ctx, `SELECT account_id::text FROM teacher WHERE id = $1`, teacherID).Scan(&teacherAccount)
		if _, got := searchStudents(t, h, admin, `{}`); by(got.Students, teacherAccount) != nil {
			t.Error("a teacher is listed")
		}
	})

	tests := []struct {
		name, q string
		want    string // an account expected in the results, "" = none
		total   int
	}{
		{"part of a name, any case", "gÓMEZ", parent.accountID, 1},
		{"part of an email, any case", "ana.GOMEZ@", parent.accountID, 1},
		{"spaces around it", "  gómez ", parent.accountID, 1},
		{"% is just a character", "100%", friend.accountID, 1},
		{"a lone % matches only names containing it", "%", friend.accountID, 1},
		{"no match", "nobody-like-this", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := json.Marshal(map[string]string{"q": tt.q})
			code, got := searchStudents(t, h, admin, string(b))
			if code != 200 || got.Total != tt.total || len(got.Students) != tt.total {
				t.Fatalf("%d %+v, want total %d", code, got, tt.total)
			}
			if tt.want != "" && by(got.Students, tt.want) == nil {
				t.Errorf("%s not found in %+v", tt.want, got.Students)
			}
		})
	}

	t.Run("too long", func(t *testing.T) {
		if code, _ := searchStudents(t, h, admin, `{"q": "`+strings.Repeat("x", 101)+`"}`); code != 400 {
			t.Errorf("%d", code)
		}
	})

	t.Run("the newest first, the first 100, and the full count", func(t *testing.T) {
		for range adminStudentsLimit + 5 {
			if _, err := pool.Exec(ctx, `INSERT INTO account (id, email, display_name) VALUES ($1, $2, 'Bulk')`, newUUIDv7(), testEmail()); err != nil {
				t.Fatal(err)
			}
		}
		_, got := searchStudents(t, h, admin, `{"q": "bulk"}`)
		if got.Total != adminStudentsLimit+5 || len(got.Students) != adminStudentsLimit {
			t.Errorf("total %d, listed %d", got.Total, len(got.Students))
		}
		for i := 1; i < len(got.Students); i++ {
			if got.Students[i].JoinedAt.After(got.Students[i-1].JoinedAt) {
				t.Fatal("not newest first")
			}
		}
	})
}
