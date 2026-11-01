package main

// Profile photos: one per account (see migration 000015).
//
//	PUT  /me/photo          the raw JPEG as the request body; replaces any old one
//	POST /me/photo/remove   deletes mine
//	GET  /photos/{id}       the image, if this viewer may see it
//	POST /admin/photos/remove {"email"}   an admin deletes someone's photo
//
// Who may see a photo: anyone, if it belongs to an active teacher; otherwise
// its owner, an admin, or a teacher of a class the owner has a booking in. To
// anyone else the answer is 404, the same as for a photo that doesn't exist, so
// the address can't be used to learn that a student has one.

import (
	"bytes"
	"encoding/json"
	"errors"
	"image/jpeg"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	maxPhotoBytes = 512 << 10 // 512 KiB, in and out; the page sends about 50 KB
	minPhotoSide  = 64        // pixels
	maxPhotoSide  = 1024      // the page sends 512 × 512
)

// cleanPhoto checks an upload and returns our own re-encoded copy, or an error
// code. Decoding to pixels and encoding again keeps only the picture: no GPS
// location or camera details, and nothing hidden after the image data, even
// from someone calling the API without our page. DecodeConfig reads just the
// header, so a small file claiming enormous dimensions is refused before any
// memory is spent on its pixels.
func cleanPhoto(raw []byte) (clean []byte, code string) {
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, "photo_invalid"
	}
	if cfg.Width < minPhotoSide || cfg.Height < minPhotoSide || cfg.Width > maxPhotoSide || cfg.Height > maxPhotoSide {
		return nil, "photo_wrong_size"
	}
	img, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "photo_invalid"
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 85}); err != nil {
		return nil, "photo_invalid"
	}
	if out.Len() > maxPhotoBytes {
		return nil, "photo_too_large"
	}
	return out.Bytes(), ""
}

// uploadPhoto handles PUT /me/photo (wrapped in requireSignIn). The upsert
// gives a replaced photo a new id, so the old address stops working.
func uploadPhoto(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPhotoBytes))
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErrorCode(w, r, http.StatusRequestEntityTooLarge, "photo_too_large", "the photo is too large")
			return
		}
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "could not read the photo")
			return
		}
		clean, code := cleanPhoto(raw)
		if code == "photo_too_large" {
			writeErrorCode(w, r, http.StatusRequestEntityTooLarge, code, "the photo is too large")
			return
		}
		if code != "" {
			writeErrorCode(w, r, http.StatusBadRequest, code, "the photo must be a JPEG between 64 and 1024 pixels on each side")
			return
		}
		var id string
		if err := pool.QueryRow(r.Context(), `
			INSERT INTO profile_photo (id, account_id, image) VALUES ($1, $2, $3)
			ON CONFLICT (account_id) DO UPDATE SET id = EXCLUDED.id, image = EXCLUDED.image
			RETURNING id::text`, newUUIDv7(), me.ID, clean).Scan(&id); err != nil {
			logFrom(r).Error("save photo failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not save the photo")
			return
		}
		logFrom(r).Info("photo saved", "account_id", me.ID, "photo_id", id, "bytes", len(clean))
		writeJSON(w, http.StatusOK, map[string]string{"photo_id": id})
	}
}

// removeMyPhoto handles POST /me/photo/remove (wrapped in requireSignIn).
// Doing it again is fine: removed says whether there was one.
func removeMyPhoto(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		tag, err := pool.Exec(r.Context(), `DELETE FROM profile_photo WHERE account_id = $1`, me.ID)
		if err != nil {
			logFrom(r).Error("remove photo failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not remove the photo")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"removed": tag.RowsAffected() > 0})
	}
}

// adminRemovePhoto handles POST /admin/photos/remove {"email"} (wrapped in
// requireAdmin). The email goes in the body, so it stays out of the request
// log's query string. No such account and no photo answer alike: removed false.
func adminRemovePhoto(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin, _ := accountFrom(r)
		var body struct {
			Email string `json:"email"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
			writeError(w, r, http.StatusBadRequest, `body must be JSON like {"email": "someone@example.com"}`)
			return
		}
		email, ok := normalizeEmail(body.Email)
		if !ok {
			writeError(w, r, http.StatusBadRequest, "please enter a valid email address")
			return
		}
		var accountID string
		err := pool.QueryRow(r.Context(), `
			DELETE FROM profile_photo p USING account a
			WHERE a.id = p.account_id AND lower(a.email) = $1
			RETURNING p.account_id::text`, email).Scan(&accountID)
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, map[string]bool{"removed": false})
			return
		}
		if err != nil {
			logFrom(r).Error("admin: remove photo failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not remove the photo")
			return
		}
		logFrom(r).Info("admin: photo removed", "admin_id", admin.ID, "account_id", accountID)
		writeJSON(w, http.StatusOK, map[string]bool{"removed": true})
	}
}

// servePhoto handles GET /photos/{id} (wrapped in signedIn: anonymous visitors
// get through, without an account). A public photo may be cached for good,
// since a new photo means a new address. A private one is kept only in the
// viewer's own browser, for an hour, so a removed one soon stops showing.
func servePhoto(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		notFound := func() { writeErrorCode(w, r, http.StatusNotFound, "photo_not_found", "no such photo") }
		id := r.PathValue("id")
		if !isUUID(id) {
			notFound()
			return
		}
		var ownerID string
		var image []byte
		var public bool
		err := pool.QueryRow(r.Context(), `
			SELECT p.account_id::text, p.image,
			       EXISTS (SELECT 1 FROM teacher t WHERE t.account_id = p.account_id AND t.status = 'active')
			FROM profile_photo p WHERE p.id = $1`, id).Scan(&ownerID, &image, &public)
		if errors.Is(err, pgx.ErrNoRows) {
			notFound()
			return
		}
		if err != nil {
			logFrom(r).Error("load photo failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load the photo")
			return
		}
		if !public {
			viewer, _ := accountFrom(r)
			allowed := viewer != nil && (viewer.ID == ownerID || viewer.IsAdmin)
			if viewer != nil && !allowed {
				if allowed, err = teachesStudent(r, pool, viewer.ID, ownerID); err != nil {
					logFrom(r).Error("check photo access failed", "err", err)
					writeError(w, r, http.StatusInternalServerError, "could not load the photo")
					return
				}
			}
			if !allowed {
				notFound()
				return
			}
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("X-Content-Type-Options", "nosniff") // always an image, whatever the bytes look like
		if public {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "private, max-age=3600")
		}
		w.Write(image)
	}
}

// teachesStudent: is teacherAccount the teacher of a class studentAccount has a
// booking in that isn't cancelled, as the payer or through someone on their
// account attending? Past classes count: he may look back at who he taught.
func teachesStudent(r *http.Request, pool *pgxpool.Pool, teacherAccount, studentAccount string) (bool, error) {
	var yes bool
	err := pool.QueryRow(r.Context(), `
		SELECT EXISTS (
			SELECT 1
			FROM teacher t
			JOIN session s ON s.teacher_id = t.id
			JOIN booking b ON b.session_id = s.id
			LEFT JOIN booking_attendee ba ON ba.booking_id = b.id AND ba.status = 'joined'
			LEFT JOIN learner l ON l.id = ba.learner_id
			WHERE t.account_id = $1 AND t.status IN ('active', 'pending')
			  AND b.status <> 'cancelled'
			  AND (b.payer_account_id = $2 OR l.account_id = $2))`,
		teacherAccount, studentAccount).Scan(&yes)
	return yes, err
}
