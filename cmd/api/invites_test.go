package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type inviteAnswer struct {
	Invite    inviteDTO `json:"invite"`
	Link      string    `json:"link"`
	EmailSent bool      `json:"email_sent"`
}

func postInvite(h http.HandlerFunc, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/admin/invites", strings.NewReader(body))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// Creating, replacing, listing and cancelling invitations; what's stored
// (the hash, never the secret); the email; the refusals.
func TestTeacherInvites(t *testing.T) {
	pool := migratedSchemaPool(t, 4)
	ctx := context.Background()
	admin := adminCookie(t, pool)
	m := &recordingMailer{}
	create := requireAdmin(pool, createInvite(pool, m.send, "https://hablemos.live"))

	rec := postInvite(create, admin, `{"email": "  Carlos@Example.com "}`)
	var first inviteAnswer
	json.Unmarshal(rec.Body.Bytes(), &first)
	if rec.Code != http.StatusCreated || first.Invite.Email != "carlos@example.com" || !first.EmailSent ||
		!strings.HasPrefix(first.Link, "https://hablemos.live/invite.html#token=") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	token := strings.TrimPrefix(first.Link, "https://hablemos.live/invite.html#token=")

	t.Run("stored: the hash, not the secret; open; expires in 7 days", func(t *testing.T) {
		if n := count(t, pool, `SELECT count(*) FROM teacher_invite WHERE id = $1 AND token_hash = $2 AND status = 'open'
			AND expires_at BETWEEN now() + interval '7 days' - interval '1 minute' AND now() + interval '7 days'`,
			first.Invite.ID, hashSecret(token)); n != 1 {
			t.Error("not stored as expected")
		}
		// What's stored is 32 bytes (a SHA-256), not the 43-character secret.
		if n := count(t, pool, `SELECT count(*) FROM teacher_invite WHERE id = $1 AND octet_length(token_hash) = 32`, first.Invite.ID); n != 1 || len(token) != 43 {
			t.Errorf("stored value isn't a 32-byte hash (token length %d)", len(token))
		}
	})

	t.Run("the email: to them, with the link, both languages", func(t *testing.T) {
		e := m.sent[0]
		if e.To != "carlos@example.com" || !strings.Contains(e.Text, first.Link) || !strings.Contains(e.Text, "te invita a dar clases") {
			t.Errorf("%+v", e)
		}
	})

	t.Run("inviting again replaces the open one", func(t *testing.T) {
		var second inviteAnswer
		json.Unmarshal(postInvite(create, admin, `{"email": "carlos@example.com"}`).Body.Bytes(), &second)
		if n := count(t, pool, `SELECT count(*) FROM teacher_invite WHERE id = $1 AND status = 'cancelled'`, first.Invite.ID); n != 1 {
			t.Error("the first invitation is still open")
		}
		list := requireAdmin(pool, listInvites(pool))
		var open []inviteDTO
		json.Unmarshal(call(list, "GET", "/admin/invites", admin).Body.Bytes(), &open)
		if len(open) != 1 || open[0].ID != second.Invite.ID || open[0].Expired {
			t.Errorf("open: %+v", open)
		}
		// Cancel it (twice: the same as once); then nothing is open.
		cancel := requireAdmin(pool, cancelInvite(pool))
		for range 2 {
			req := httptest.NewRequest("POST", "/", nil)
			req.SetPathValue("id", second.Invite.ID)
			req.AddCookie(admin)
			rec := httptest.NewRecorder()
			cancel(rec, req)
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"cancelled"`) {
				t.Errorf("cancel: %d %s", rec.Code, rec.Body)
			}
		}
		json.Unmarshal(call(list, "GET", "/admin/invites", admin).Body.Bytes(), &open)
		if len(open) != 0 {
			t.Errorf("still open: %+v", open)
		}
	})

	t.Run("refused", func(t *testing.T) {
		teacher := newTeacher(t, pool) // an active teacher
		var email string
		pool.QueryRow(ctx, `SELECT a.email FROM teacher t JOIN account a ON a.id = t.account_id WHERE t.id = $1`, teacher).Scan(&email)
		if rec := postInvite(create, admin, `{"email": "`+email+`"}`); rec.Code != 409 || errorCode(t, rec) != "already_teacher" {
			t.Errorf("an active teacher: %d %s", rec.Code, rec.Body)
		}
		if rec := postInvite(create, admin, `{"email": "not an email"}`); rec.Code != 400 {
			t.Errorf("not an email: %d", rec.Code)
		}
		student := newStudent(t, pool, 0, false)
		if rec := postInvite(create, student.cookie, `{"email": "x@example.com"}`); rec.Code != 403 {
			t.Errorf("a student: %d", rec.Code)
		}
	})

	t.Run("no mailer configured: still created, link for the admin to send", func(t *testing.T) {
		noMail := requireAdmin(pool, createInvite(pool, nil, "https://hablemos.live"))
		var a inviteAnswer
		rec := postInvite(noMail, admin, `{"email": "ana@example.com"}`)
		json.Unmarshal(rec.Body.Bytes(), &a)
		if rec.Code != 201 || a.EmailSent || a.Link == "" {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})
}

// newInviteToken: an open invitation for email, created by an admin; returns
// the secret from its link.
func newInviteToken(t *testing.T, pool *pgxpool.Pool, email string) string {
	t.Helper()
	admin := adminCookie(t, pool)
	rec := postInvite(requireAdmin(pool, createInvite(pool, nil, "https://hablemos.live")), admin, `{"email": "`+email+`"}`)
	var a inviteAnswer
	json.Unmarshal(rec.Body.Bytes(), &a)
	if rec.Code != http.StatusCreated {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body)
	}
	return strings.TrimPrefix(a.Link, "https://hablemos.live/invite.html#token=")
}

func accept(pool *pgxpool.Pool, token string) *httptest.ResponseRecorder {
	return call(func(w http.ResponseWriter, r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader(`{"token": "` + token + `", "timezone": "America/Bogota"}`))
		acceptInvite(pool)(w, r)
	}, "POST", "/invites/accept", nil)
}

// Accepting: signed in at once as a pending teacher, the teacher page works,
// students see nothing of him; and every way a link can be no good.
func TestAcceptInvite(t *testing.T) {
	pool := migratedSchemaPool(t, 4)
	ctx := context.Background()
	token := newInviteToken(t, pool, "Carlos@Example.com")

	rec := accept(pool, token)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"pending"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || !cookie.Secure {
		t.Fatalf("no proper session cookie: %+v", rec.Result().Cookies())
	}

	t.Run("account, pending teacher, invitation accepted", func(t *testing.T) {
		if n := count(t, pool, `
			SELECT count(*) FROM account a JOIN teacher t ON t.account_id = a.id
			JOIN teacher_invite i ON i.accepted_account_id = a.id AND i.status = 'accepted'
			WHERE lower(a.email) = 'carlos@example.com' AND a.timezone = 'America/Bogota' AND t.status = 'pending'`); n != 1 {
			t.Error("not all in place")
		}
	})

	t.Run("signed in: the teacher page works and says pending", func(t *testing.T) {
		rec := send(pool, getTeacherProfile(pool), "GET", "/teacher/profile", "", "", cookie)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"pending"`) {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
		if rec := call(requireSignIn(pool, getMe(pool)), "GET", "/me", cookie); !strings.Contains(rec.Body.String(), `"is_teacher":true`) {
			t.Errorf("/me: %s", rec.Body)
		}
	})

	t.Run("students see nothing of a pending teacher", func(t *testing.T) {
		var teacherID string
		pool.QueryRow(ctx, `SELECT t.id FROM teacher t JOIN account a ON a.id = t.account_id WHERE lower(a.email) = 'carlos@example.com'`).Scan(&teacherID)
		pool.Exec(ctx, `INSERT INTO price_tier (id, teacher_id, kind, duration_minutes, party_size, price_per_person_cents)
			VALUES ($1, $2, 'group', 60, 1, 1500)`, newUUIDv7(), teacherID)
		rec := send(pool, createGroupClass(pool), "POST", "/teacher/sessions", classBody(96*time.Hour, nil), "", cookie)
		var c struct{ ID string }
		json.Unmarshal(rec.Body.Bytes(), &c)
		if rec.Code != http.StatusCreated {
			t.Fatalf("a pending teacher preparing a class: %d %s", rec.Code, rec.Body)
		}
		if ids, _ := listedIDs(t, pool, "?kind=group", nil); ids[c.ID] {
			t.Error("a pending teacher's class is in the public catalog")
		}
	})

	t.Run("links that are no good", func(t *testing.T) {
		expired := newInviteToken(t, pool, "late@example.com")
		pool.Exec(ctx, `UPDATE teacher_invite SET created_at = now() - interval '8 days', expires_at = now() - interval '1 day'
			WHERE token_hash = $1`, hashSecret(expired))
		cancelled := newInviteToken(t, pool, "nope@example.com")
		pool.Exec(ctx, `UPDATE teacher_invite SET status = 'cancelled' WHERE token_hash = $1`, hashSecret(cancelled))
		for _, tc := range []struct {
			name, token string
			status      int
			code        string
		}{
			{"used already", token, 409, "invite_used"},
			{"expired", expired, 410, "invite_expired"},
			{"cancelled", cancelled, 409, "invite_invalid"},
			{"made up", newSecret(), 404, "invite_invalid"},
		} {
			if rec := accept(pool, tc.token); rec.Code != tc.status || errorCode(t, rec) != tc.code {
				t.Errorf("%s: %d %s", tc.name, rec.Code, rec.Body)
			}
		}
	})

	t.Run("a former (inactive) teacher comes back as pending", func(t *testing.T) {
		old := newTeacher(t, pool)
		pool.Exec(ctx, `UPDATE teacher SET status = 'inactive' WHERE id = $1`, old)
		var email string
		pool.QueryRow(ctx, `SELECT a.email FROM teacher t JOIN account a ON a.id = t.account_id WHERE t.id = $1`, old).Scan(&email)
		if rec := accept(pool, newInviteToken(t, pool, email)); rec.Code != 200 ||
			count(t, pool, `SELECT count(*) FROM teacher WHERE id = $1 AND status = 'pending'`, old) != 1 {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})
}

// Two clicks on the same link at once. The test plays the first: it locks the
// invitation and marks it accepted, holding its transaction open while the
// second accept starts. With FOR UPDATE, the second waits, then sees
// "accepted" → invite_used. Without it, it reads "open" and uses the link a
// second time.
func TestAcceptInviteRace(t *testing.T) {
	pool := migratedSchemaPool(t, 4)
	ctx := context.Background()
	token := newInviteToken(t, pool, "twice@example.com")
	accountID, _ := newAccount(t, pool)

	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `
		UPDATE teacher_invite SET status = 'accepted', accepted_at = now(), accepted_account_id = $2
		WHERE token_hash = $1`, hashSecret(token), accountID); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- accept(pool, token) }()
	waitForLockWaiters(t, pool, 1)
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if rec := <-done; rec.Code != 409 || errorCode(t, rec) != "invite_used" {
		t.Errorf("second use: %d %s, want 409 invite_used", rec.Code, rec.Body)
	}
}
