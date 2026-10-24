package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
