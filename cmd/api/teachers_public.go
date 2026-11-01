package main

// GET /teachers: the public list for the Our Teachers page. Active teachers
// only, and only what a visitor should see: never an email address, and nothing
// about how busy a teacher is.

import (
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type publicTeacherDTO struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	PhotoID *string `json:"photo_id"` // shown at /photos/{id}; null = none
	BioEN   string  `json:"bio_en"`   // "" = none
	BioES   string  `json:"bio_es"`
}

func listPublicTeachers(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, _ := pool.Query(r.Context(), `
			SELECT t.id::text, a.display_name,
			       (SELECT ph.id::text FROM profile_photo ph WHERE ph.account_id = a.id),
			       COALESCE(t.bio_en, ''), COALESCE(t.bio_es, '')
			FROM teacher t JOIN account a ON a.id = t.account_id
			WHERE t.status = 'active'
			ORDER BY lower(a.display_name), t.id`)
		list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[publicTeacherDTO])
		if err != nil {
			logFrom(r).Error("public teachers: query failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load the teachers")
			return
		}
		if list == nil {
			list = []publicTeacherDTO{}
		}
		writeJSON(w, http.StatusOK, list)
	}
}
