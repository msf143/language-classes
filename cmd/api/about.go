package main

// The About page's text (see migration 000016).
//
//	GET /about-page    the newest version; public
//	PUT /about-page    a new version; admins only
//
// The server stores plain text and doesn't interpret it. Turning a "#" line into
// a heading is the page's job, and it builds elements with text, never HTML.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
)

const maxAboutChars = 10000 // per language: far more than an About page needs

type aboutPageDTO struct {
	BodyEN  string    `json:"body_en"`
	BodyES  string    `json:"body_es"`
	SavedAt time.Time `json:"saved_at"`
}

const newestAboutSQL = `
	SELECT body_en, body_es, created_at FROM about_page_version
	ORDER BY created_at DESC, id DESC LIMIT 1`

// getAboutPage handles GET /about-page.
func getAboutPage(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var p aboutPageDTO
		if err := pool.QueryRow(r.Context(), newestAboutSQL).Scan(&p.BodyEN, &p.BodyES, &p.SavedAt); err != nil {
			logFrom(r).Error("about page: load failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load the page")
			return
		}
		p.SavedAt = p.SavedAt.UTC()
		writeJSON(w, http.StatusOK, p)
	}
}

// putAboutPage handles PUT /about-page {"body_en", "body_es"} (wrapped in
// requireAdmin): adds a version, which becomes the page. Both languages are
// required, so a slip can't leave visitors with an empty page.
// Code: invalid_about (400).
func putAboutPage(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin, _ := accountFrom(r)
		var body struct {
			BodyEN string `json:"body_en"`
			BodyES string `json:"body_es"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeError(w, r, http.StatusBadRequest, `body must be JSON like {"body_en": "...", "body_es": "..."}`)
			return
		}
		en, es := strings.TrimSpace(body.BodyEN), strings.TrimSpace(body.BodyES)
		if en == "" || es == "" {
			writeErrorCode(w, r, http.StatusBadRequest, "invalid_about", "the page needs text in both languages")
			return
		}
		// Characters, not bytes: "á" is one character but two bytes in UTF-8.
		if utf8.RuneCountInString(en) > maxAboutChars || utf8.RuneCountInString(es) > maxAboutChars {
			writeErrorCode(w, r, http.StatusBadRequest, "invalid_about", fmt.Sprintf("each language can be at most %d characters", maxAboutChars))
			return
		}
		var p aboutPageDTO
		if err := pool.QueryRow(r.Context(), `
			INSERT INTO about_page_version (id, body_en, body_es, saved_by) VALUES ($1, $2, $3, $4)
			RETURNING body_en, body_es, created_at`, newUUIDv7(), en, es, admin.ID).
			Scan(&p.BodyEN, &p.BodyES, &p.SavedAt); err != nil {
			logFrom(r).Error("about page: save failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not save the page")
			return
		}
		p.SavedAt = p.SavedAt.UTC()
		logFrom(r).Info("admin: about page saved", "admin_id", admin.ID)
		writeJSON(w, http.StatusOK, p)
	}
}
