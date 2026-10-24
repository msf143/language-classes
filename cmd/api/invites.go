package main

// Teacher invitations (step 7g). An admin invites an email address; the
// person gets a link (by email, and the admin sees it once, to send another
// way if the email goes astray). Accepting it (step 7g-2) signs them in and
// makes them a 'pending' teacher, until an admin activates them.
//
// The secret in the link is never stored, only its SHA-256 hash (like login
// links), so the email is sent right away from here, not later by the worker:
// the outbox would have to keep the secret to send it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const inviteTTL = 7 * 24 * time.Hour

type inviteDTO struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	InvitedBy string    `json:"invited_by"` // the admin's display name
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Expired   bool      `json:"expired"`
}

// createInvite handles POST /admin/invites {"email": "..."} (wrapped in
// requireAdmin). Answers 201 with the invitation, its link (shown once: it
// can't be shown again, only its hash is kept) and whether the email went out.
func createInvite(pool *pgxpool.Pool, send mailer, siteURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		var body struct {
			Email string `json:"email"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeError(w, r, http.StatusBadRequest, `body must be JSON like {"email": "teacher@example.com"}`)
			return
		}
		email, ok := normalizeEmail(body.Email)
		if !ok {
			writeErrorCode(w, r, http.StatusBadRequest, "invalid_email", "that doesn't look like an email address")
			return
		}

		token := newSecret()
		inv := inviteDTO{ID: newUUIDv7(), Email: email, InvitedBy: me.DisplayName}
		err := pgx.BeginFunc(r.Context(), pool, func(tx pgx.Tx) error {
			var already bool
			if err := tx.QueryRow(r.Context(), `
				SELECT EXISTS (SELECT 1 FROM teacher t JOIN account a ON a.id = t.account_id
				               WHERE lower(a.email) = $1 AND t.status = 'active')`, email).Scan(&already); err != nil {
				return err
			}
			if already {
				return &bookingError{http.StatusConflict, "already_teacher", "that person is already an active teacher"}
			}
			// Inviting again replaces the open invitation: its link stops working.
			if _, err := tx.Exec(r.Context(), `
				UPDATE teacher_invite SET status = 'cancelled' WHERE lower(email) = $1 AND status = 'open'`, email); err != nil {
				return err
			}
			return tx.QueryRow(r.Context(), `
				INSERT INTO teacher_invite (id, email, token_hash, invited_by, expires_at)
				VALUES ($1, $2, $3, $4, now() + $5::int * interval '1 second')
				RETURNING created_at, expires_at`,
				inv.ID, email, hashSecret(token), me.ID, int(inviteTTL.Seconds())).Scan(&inv.CreatedAt, &inv.ExpiresAt)
		})
		var be *bookingError
		switch {
		case errors.As(err, &be):
			writeErrorCode(w, r, be.status, be.code, be.msg)
			return
		case err != nil:
			logFrom(r).Error("create invite failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not create the invitation")
			return
		}

		link := siteURL + "/invite.html#token=" + token // after "#": never sent to any server in a request line, so never in a log
		sent := false
		if send != nil {
			if _, err := send(r.Context(), inviteEmail(email, me.DisplayName, link)); err != nil {
				logFrom(r).Error("invite email failed (the admin can still send the link)", "err", err, "invite_id", inv.ID)
			} else {
				sent = true
			}
		}
		logFrom(r).Info("admin: teacher invited", "admin_account_id", me.ID, "admin_email", me.Email,
			"invite_id", inv.ID, "invited_email", email, "email_sent", sent)
		writeJSON(w, http.StatusCreated, map[string]any{"invite": inv, "link": link, "email_sent": sent})
	}
}

func inviteEmail(to, adminName, link string) email {
	days := int(inviteTTL.Hours() / 24)
	return email{
		From: classesEmailFrom, To: to,
		Subject: "You're invited to teach on Hablemos · Te invitamos a enseñar en Hablemos",
		Text: fmt.Sprintf(`Hi!

%[1]s invites you to teach live language classes on Hablemos.

Open this link to join (it works for %[2]d days, once):
%[3]s

You'll set up your profile and prices; students see you once an admin
activates your profile.

—

¡Hola!

%[1]s te invita a dar clases de idiomas en vivo en Hablemos.

Abre este enlace para unirte (funciona durante %[2]d días, una sola vez):
%[3]s

Configurarás tu perfil y tus precios; los estudiantes te verán cuando un
administrador active tu perfil.
`, adminName, days, link),
	}
}

// listInvites handles GET /admin/invites: the open invitations (expired ones
// included, marked, so an admin sees who never answered).
func listInvites(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := pool.Query(r.Context(), `
			SELECT i.id::text, i.email, a.display_name, i.created_at, i.expires_at, i.expires_at <= now()
			FROM teacher_invite i JOIN account a ON a.id = i.invited_by
			WHERE i.status = 'open'
			ORDER BY i.created_at DESC`)
		if err != nil {
			logFrom(r).Error("list invites failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load the invitations")
			return
		}
		list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (inviteDTO, error) {
			var i inviteDTO
			err := row.Scan(&i.ID, &i.Email, &i.InvitedBy, &i.CreatedAt, &i.ExpiresAt, &i.Expired)
			return i, err
		})
		if err != nil {
			logFrom(r).Error("list invites failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load the invitations")
			return
		}
		if list == nil {
			list = []inviteDTO{}
		}
		writeJSON(w, http.StatusOK, list)
	}
}

// cancelInvite handles POST /admin/invites/{id}/cancel: the link stops
// working. Cancelling twice, or an accepted one, changes nothing.
func cancelInvite(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		id := r.PathValue("id")
		if !isUUID(id) {
			writeError(w, r, http.StatusBadRequest, "the invitation ID in the path is not valid")
			return
		}
		var status string
		err := pool.QueryRow(r.Context(), `
			WITH c AS (UPDATE teacher_invite SET status = 'cancelled' WHERE id = $1 AND status = 'open' RETURNING status)
			SELECT COALESCE((SELECT status FROM c), (SELECT status FROM teacher_invite WHERE id = $1), '')`, id).Scan(&status)
		switch {
		case err == nil && status == "":
			writeErrorCode(w, r, http.StatusNotFound, "invite_not_found", "no such invitation")
			return
		case err != nil:
			logFrom(r).Error("cancel invite failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not cancel the invitation")
			return
		}
		logFrom(r).Info("admin: invitation cancelled", "admin_account_id", me.ID, "invite_id", id)
		writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": status})
	}
}
