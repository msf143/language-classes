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

// `api admin`: grant (creating the account if needed), grant again, revoke;
// /me reports the flag; requireAdmin lets admins through and nobody else.
func TestAdmins(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	isAdmin := func(id string) bool {
		var b bool
		pool.QueryRow(ctx, `SELECT is_admin FROM account WHERE id = $1`, id).Scan(&b)
		return b
	}

	t.Run("a new person: account created, admin", func(t *testing.T) {
		email := testEmail()
		id, created, err := setAdmin(ctx, pool, " "+email+" ", true)
		if err != nil || !created || !isAdmin(id) {
			t.Errorf("id %s created %v admin %v err %v", id, created, isAdmin(id), err)
		}
	})

	accountID, email := newAccount(t, pool)
	cookie := signIn(t, pool, email)
	behindAdmin := func() int {
		return call(requireAdmin(pool, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), "GET", "/admin/x", cookie).Code
	}
	if got := behindAdmin(); got != http.StatusForbidden {
		t.Errorf("before: %d, want 403", got)
	}

	for range 2 { // granting twice is the same as once
		if id, created, err := setAdmin(ctx, pool, email, true); err != nil || id != accountID || created {
			t.Fatalf("grant: %s %v %v", id, created, err)
		}
	}
	if got := behindAdmin(); got != http.StatusNoContent {
		t.Errorf("admin: %d, want through (204)", got)
	}
	if rec := call(requireSignIn(pool, getMe(pool)), "GET", "/me", cookie); !strings.Contains(rec.Body.String(), `"is_admin":true`) {
		t.Errorf("/me doesn't say is_admin: %s", rec.Body)
	}

	if _, _, err := setAdmin(ctx, pool, email, false); err != nil || isAdmin(accountID) {
		t.Errorf("revoke: err %v, still admin %v", err, isAdmin(accountID))
	}
	if got := behindAdmin(); got != http.StatusForbidden {
		t.Errorf("after revoke: %d, want 403 at once", got)
	}

	t.Run("refused", func(t *testing.T) {
		if _, _, err := setAdmin(ctx, pool, "not-an-email", true); err == nil {
			t.Error("a non-email was accepted")
		}
		if _, _, err := setAdmin(ctx, pool, testEmail(), false); err == nil {
			t.Error("revoking from someone without an account should fail")
		}
		if got := call(requireAdmin(pool, func(w http.ResponseWriter, r *http.Request) {}), "GET", "/admin/x", nil).Code; got != 401 {
			t.Errorf("signed out: %d, want 401", got)
		}
	})
}

// adminCookie: a signed-in admin in this pool.
func adminCookie(t *testing.T, pool *pgxpool.Pool) *http.Cookie {
	t.Helper()
	_, email := newAccount(t, pool)
	if _, _, err := setAdmin(context.Background(), pool, email, true); err != nil {
		t.Fatal(err)
	}
	return signIn(t, pool, email)
}

// adminSend calls an admin handler wrapped in requireAdmin, as main.go does.
func adminSend(pool *pgxpool.Pool, h http.HandlerFunc, method, path, id string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if id != "" {
		req.SetPathValue("id", id)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	requireAdmin(pool, h)(rec, req)
	return rec
}

// The admin's teacher list and profile, and switching a teacher off and on:
// refused while he has booked classes; once off, his unbooked group class is
// hidden from the catalog and can't be booked; back on, it's back.
func TestAdminTeachers(t *testing.T) {
	pool := migratedSchemaPool(t, 6)
	ctx := context.Background()
	admin := adminCookie(t, pool)

	carlos := newTeacher(t, pool) // active, group prices for 30/45/60 × 1–4
	bookedClass := newSession(t, pool, carlos, sessionOpts{slot: 30})
	emptyClass := newSession(t, pool, carlos, sessionOpts{slot: 31})
	ana := newStudent(t, pool, 5000, false)
	b := bookOK(t, pool, ana, bookedClass)
	pending, retired := newTeacher(t, pool), newTeacher(t, pool)
	pool.Exec(ctx, `UPDATE teacher SET status = 'pending' WHERE id = $1`, pending)
	pool.Exec(ctx, `UPDATE teacher SET status = 'inactive' WHERE id = $1`, retired)

	t.Run("the list: pending first, with counts", func(t *testing.T) {
		rec := adminSend(pool, listAdminTeachers(pool), "GET", "/admin/teachers", "", admin)
		var list []adminTeacherDTO
		json.Unmarshal(rec.Body.Bytes(), &list)
		if len(list) != 3 || list[0].ID != pending || list[1].ID != carlos || list[2].ID != retired {
			t.Fatalf("order: %+v", list)
		}
		if c := list[1]; c.UpcomingClasses != 2 || c.BookedClasses != 1 || c.Prices != 12 || c.Status != "active" {
			t.Errorf("counts: %+v", c)
		}
	})

	t.Run("one teacher in full", func(t *testing.T) {
		rec := adminSend(pool, getAdminTeacher(pool), "GET", "/admin/teachers/"+carlos, carlos, admin)
		var got struct {
			Status    string            `json:"status"`
			PriceList []priceCellDTO    `json:"price_list"`
			Classes   []classSummaryDTO `json:"classes"`
		}
		json.Unmarshal(rec.Body.Bytes(), &got)
		if rec.Code != 200 || len(got.PriceList) != 12 || len(got.Classes) != 2 || got.Classes[0].SeatsTaken != 1 {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
		if rec := adminSend(pool, getAdminTeacher(pool), "GET", "/", newUUIDv7(), admin); rec.Code != 404 {
			t.Errorf("unknown teacher: %d", rec.Code)
		}
	})

	deactivate := func(id string) *httptest.ResponseRecorder {
		return adminSend(pool, setTeacherStatus(pool, false), "POST", "/", id, admin)
	}
	catalogHas := func(sessionID string) bool {
		ids, _ := listedIDs(t, pool, "?kind=group", nil)
		return ids[sessionID]
	}

	t.Run("deactivate refused while a class is booked, with the list", func(t *testing.T) {
		rec := deactivate(carlos)
		if rec.Code != 409 || errorCode(t, rec) != "has_booked_classes" || !strings.Contains(rec.Body.String(), bookedClass) ||
			strings.Contains(rec.Body.String(), emptyClass) {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
		if n := count(t, pool, `SELECT count(*) FROM teacher WHERE id = $1 AND status = 'active'`, carlos); n != 1 {
			t.Error("status changed anyway")
		}
	})

	t.Run("after the booking is cancelled: off, hidden, not bookable; on again: back", func(t *testing.T) {
		if rec := cancel(pool, ana.cookie, b.ID); rec.Code != 200 {
			t.Fatalf("student cancel: %d %s", rec.Code, rec.Body)
		}
		if !catalogHas(emptyClass) {
			t.Fatal("the class isn't in the catalog to begin with")
		}
		if rec := deactivate(carlos); rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if catalogHas(emptyClass) {
			t.Error("an inactive teacher's class is still in the catalog")
		}
		if rec := book(pool, ana.cookie, newUUIDv7(), emptyClass, ana.learners...); errorCode(t, rec) != "session_not_open" {
			t.Errorf("booking an inactive teacher's class: %d %s", rec.Code, rec.Body)
		}
		if rec := deactivate(carlos); rec.Code != 200 {
			t.Errorf("deactivating twice: %d", rec.Code)
		}
		if rec := adminSend(pool, setTeacherStatus(pool, true), "POST", "/", carlos, admin); rec.Code != 200 || !catalogHas(emptyClass) {
			t.Errorf("activate: %d %s; back in the catalog: %v", rec.Code, rec.Body, catalogHas(emptyClass))
		}
	})

	t.Run("not for students", func(t *testing.T) {
		if rec := adminSend(pool, setTeacherStatus(pool, false), "POST", "/", carlos, ana.cookie); rec.Code != 403 {
			t.Errorf("%d", rec.Code)
		}
	})
}

// An admin deactivates a teacher while a student books his class. The test
// plays the admin: it locks the teacher row and sets him inactive, holding the
// transaction open while the booking starts. With the booking's shared lock,
// the booking waits, then sees "inactive" and is refused. Without it, the
// booking reads the still-committed "active" and goes through: a paid booking
// with an inactive teacher.
func TestDeactivateBookingRace(t *testing.T) {
	pool := migratedSchemaPool(t, 6)
	ctx := context.Background()
	teacher := newTeacher(t, pool)
	class := newSession(t, pool, teacher, sessionOpts{slot: 30})
	s := newStudent(t, pool, 5000, false)

	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `SELECT 1 FROM teacher WHERE id = $1 FOR UPDATE`, teacher); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Exec(ctx, `UPDATE teacher SET status = 'inactive' WHERE id = $1`, teacher); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- book(pool, s.cookie, newUUIDv7(), class, s.learners...) }()
	deadline := time.Now().Add(3 * time.Second)
	for len(done) == 0 && count(t, pool, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`) == 0 &&
		time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rec := <-done
	if rec.Code == http.StatusCreated {
		t.Errorf("booked a class of a teacher being deactivated: %s", rec.Body)
	} else if errorCode(t, rec) != "session_not_open" {
		t.Errorf("%d %s, want session_not_open", rec.Code, rec.Body)
	}
	if b := balance(t, pool, s.accountID); b != 5000 {
		t.Errorf("balance %d, want 5000", b)
	}
}
