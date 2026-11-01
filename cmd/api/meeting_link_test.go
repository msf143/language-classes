package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCleanMeetingLink(t *testing.T) {
	const meet = "https://meet.google.com/abc-defg-hij"
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"", "", true},
		{"   ", "", true},
		{meet, meet, true},
		{"  " + meet + "  ", meet, true},
		{"meet.google.com/abc-defg-hij", meet, true},
		{"http://meet.google.com/abc", "", false},
		{"javascript:alert(1)//x.y", "", false},
		{"javascript://x.y/%0Aalert(1)", "", false},
		{"data:text/html,hello.world", "", false},
		{"https://localhost/room", "", false},
		{"https://user:pw@meet.google.com/abc", "", false},
		{"https://meet.google.com/a b", "", false},
		{`https://meet.google.com/"onclick=x`, "", false},
		{"not a link", "", false},
		{"https://meet.google.com/" + strings.Repeat("a", maxMeetingLinkChars), "", false},
	}
	for _, tt := range tests {
		got, problem := cleanMeetingLink(tt.in)
		if got != tt.want || (problem == "") != tt.ok {
			t.Errorf("cleanMeetingLink(%q) = %q, %q; want %q, ok %v", tt.in, got, problem, tt.want, tt.ok)
		}
	}
}

func saveLink(pool *pgxpool.Pool, s student, link string) (int, string) {
	body, _ := json.Marshal(teacherProfileDTO{DisplayName: "Gabriel", BreakMinutes: 5, OfferedLengths: []int{60}, MeetingLink: link})
	rec := me(s, requireTeacher(pool, putTeacherProfile(pool)), "PUT", string(body), "")
	return rec.Code, rec.Body.String()
}

func classLink(t *testing.T, pool *pgxpool.Pool, sessionID string) string {
	t.Helper()
	var link *string
	if err := pool.QueryRow(context.Background(), `SELECT meeting_url FROM session WHERE id = $1`, sessionID).Scan(&link); err != nil {
		t.Fatal(err)
	}
	if link == nil {
		return ""
	}
	return *link
}

// From saving the link to seeing it: new classes get it, saving a new one
// reaches upcoming classes only, and it shows only to the people in the class.
func TestMeetingLink(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	teacherID, teacher := teacherLogin(t, pool)
	before := newSession(t, pool, teacherID, sessionOpts{slot: 10})
	past := newSession(t, pool, teacherID, sessionOpts{slot: -2})
	cancelled := newSession(t, pool, teacherID, sessionOpts{slot: 11})
	pool.Exec(ctx, `UPDATE session SET status = 'cancelled', cancel_reason = 'teacher_cancelled' WHERE id = $1`, cancelled)

	const first, second = "https://meet.google.com/aaa-bbbb-ccc", "https://meet.google.com/ddd-eeee-fff"
	if code, body := saveLink(pool, teacher, "meet.google.com/aaa-bbbb-ccc"); code != 200 || !strings.Contains(body, first) {
		t.Fatalf("save: %d %s", code, body)
	}
	if code, body := saveLink(pool, teacher, "javascript:alert(1)"); code != 400 || !strings.Contains(body, "invalid_profile") {
		t.Errorf("a bad link: %d %s", code, body)
	}
	rec := me(teacher, requireTeacher(pool, getTeacherProfile(pool)), "GET", "", "")
	if !strings.Contains(rec.Body.String(), first) {
		t.Errorf("reading it back: %s", rec.Body)
	}

	t.Run("reaches upcoming classes only", func(t *testing.T) {
		if got := classLink(t, pool, before); got != first {
			t.Errorf("an upcoming class: %q", got)
		}
		if got := classLink(t, pool, past); got != "" {
			t.Errorf("a past class: %q", got)
		}
		if got := classLink(t, pool, cancelled); got != "" {
			t.Errorf("a cancelled class: %q", got)
		}
	})

	t.Run("a new group class gets it", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{"title": "New", "target_language": "es", "audience": "adults",
			"date": time.Now().UTC().AddDate(0, 0, 20).Format("2006-01-02"), "from": "10:00", "duration_minutes": 60, "min_seats": 2})
		rec := me(teacher, requireTeacher(pool, createGroupClass(pool)), "POST", string(body), "")
		if rec.Code != http.StatusCreated {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		var c struct{ ID string }
		json.Unmarshal(rec.Body.Bytes(), &c)
		cancelWhenDone(t, pool, c.ID)
		if got := classLink(t, pool, c.ID); got != first {
			t.Errorf("%q (%s)", got, rec.Body)
		}
	})

	student1 := newStudent(t, pool, 5000, false)
	stranger := newStudent(t, pool, 0, false)
	booking := bookOK(t, pool, student1, before)

	seesLink := func(rawBody, link string) bool { return strings.Contains(rawBody, link) }
	t.Run("who sees it", func(t *testing.T) {
		mine := me(student1, requireSignIn(pool, listMyBookings(pool)), "GET", "", "").Body.String()
		if !seesLink(mine, first) {
			t.Errorf("the student in the class: %s", mine)
		}
		if got := me(stranger, requireSignIn(pool, listMyBookings(pool)), "GET", "", "").Body.String(); seesLink(got, first) {
			t.Error("a stranger's bookings show it")
		}
		tch := me(teacher, requireTeacher(pool, listTeacherSessions(pool)), "GET", "", "").Body.String()
		if !seesLink(tch, first) {
			t.Error("the teacher's list doesn't show it")
		}
		if got := call(signedIn(pool, listSessions(pool)), "GET", "/sessions?kind=group", nil).Body.String(); seesLink(got, first) || strings.Contains(got, "meet.google.com") {
			t.Error("the public catalog shows it")
		}
		if got := call(signedIn(pool, listSessions(pool)), "GET", "/sessions?kind=group", stranger.cookie).Body.String(); seesLink(got, first) {
			t.Error("the catalog shows it to a stranger")
		}
		if got := call(listPublicTeachers(pool), "GET", "/teachers", nil).Body.String(); seesLink(got, first) {
			t.Error("Our Teachers shows it")
		}
	})

	t.Run("a new link reaches the class and the emails", func(t *testing.T) {
		if code, body := saveLink(pool, teacher, second); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		f, err := loadBookingFacts(ctx, pool, booking.ID)
		if err != nil {
			t.Fatal(err)
		}
		toPayer, _ := bookedEmails(f, "https://site.test")
		reminder := reminderEmail(f, f.payer, f.attendees, "https://site.test")
		for name, m := range map[string]email{"booked": toPayer, "reminder": reminder} {
			if !strings.Contains(m.Text, "Join the class by video: "+second) || !strings.Contains(m.Text, "Entra a la clase por video: "+second) {
				t.Errorf("%s email:\n%s", name, m.Text)
			}
			if strings.Contains(m.Text, "will send you") || strings.Contains(m.Text, "%!") {
				t.Errorf("%s email has the old sentence or a format error:\n%s", name, m.Text)
			}
		}
	})

	t.Run("no link: back to saying it's coming", func(t *testing.T) {
		if code, body := saveLink(pool, teacher, ""); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		if got := classLink(t, pool, before); got != "" {
			t.Errorf("the class still has %q", got)
		}
		f, _ := loadBookingFacts(ctx, pool, booking.ID)
		toPayer, _ := bookedEmails(f, "https://site.test")
		if !strings.Contains(toPayer.Text, "The teacher will send you the video link") ||
			!strings.Contains(toPayer.Text, "El profesor te enviará el enlace") || strings.Contains(toPayer.Text, "%!") {
			t.Errorf("email:\n%s", toPayer.Text)
		}
	})
}

// A private class booked after the link is saved gets it; so does a friend's email.
func TestMeetingLinkOnPrivateClass(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	teacherID, day := privateTeacher(t, pool)
	const link = "https://meet.google.com/ggg-hhhh-iii"
	pool.Exec(ctx, `UPDATE teacher SET meeting_link = $2 WHERE id = $1`, teacherID, link)
	s := newStudent(t, pool, 5000, false)
	rec := bookPrivate(pool, s.cookie, newUUIDv7(), teacherID, day.Add(19*time.Hour), 60, s.learners[0])
	if rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	b := decodeBooking(t, rec)
	cancelWhenDone(t, pool, b.SessionID)
	if got := classLink(t, pool, b.SessionID); got != link {
		t.Errorf("%q", got)
	}
}

// Saving a link while a group booking is under way must not deadlock: the
// booking locks the class, then reads the teacher row (FOR SHARE). Here a gate
// does the same two steps around a save. If the save locked the teacher row
// before the classes, the gate's second step would wait for the save, the save
// for the gate, and Postgres would kill one of them.
func TestSaveLinkDoesNotDeadlockWithBooking(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	teacherID, teacher := teacherLogin(t, pool)
	session := newSession(t, pool, teacherID, sessionOpts{slot: 12})

	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `SELECT 1 FROM session WHERE id = $1 FOR UPDATE`, session); err != nil {
		t.Fatal(err)
	}
	type result struct {
		code int
		body string
	}
	done := make(chan result, 1)
	go func() {
		code, body := saveLink(pool, teacher, "https://meet.google.com/jjj-kkkk-lll")
		done <- result{code, body}
	}()
	waitForLockWaiters(t, pool, 1)
	if _, err := gate.Exec(ctx, `SELECT 1 FROM teacher WHERE id = $1 FOR SHARE`, teacherID); err != nil {
		t.Fatalf("the booking's second lock: %v", err)
	}
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.code != 200 {
		t.Errorf("the save: %d %s", r.code, r.body)
	}
	if got := classLink(t, pool, session); !strings.Contains(got, "jjj-kkkk-lll") {
		t.Errorf("%q", got)
	}
}
