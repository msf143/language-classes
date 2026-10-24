package main

// The teacher's own profile (step 7b): the name students see, a short bio in
// both languages, the break taken inside each class, and the private-class
// lengths offered. GET reads it; PUT replaces it (the whole profile at once).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var allowedLengths = []int{30, 45, 60} // private-class lengths a teacher may offer (and price, step 7c)

const (
	maxNameChars    = 60
	maxBioChars     = 2000
	maxBreakMinutes = 15 // a class may end this much early at most (the database allows up to 60)
)

type teacherProfileDTO struct {
	DisplayName    string `json:"display_name"` // shown to students ("with Carlos")
	BioEN          string `json:"bio_en"`       // "" = none
	BioES          string `json:"bio_es"`
	BreakMinutes   int    `json:"break_minutes"`   // teacher.buffer_minutes
	OfferedLengths []int  `json:"offered_lengths"` // teacher.offered_durations, ascending
}

// The answer also says what the form may offer, so the page has no limits of its own.
type teacherProfileAnswer struct {
	teacherProfileDTO
	AllowedLengths  []int `json:"allowed_lengths"`
	MaxBreakMinutes int   `json:"max_break_minutes"`
}

// getTeacherProfile handles GET /teacher/profile (wrapped in requireTeacher).
func getTeacherProfile(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := loadTeacherProfile(r, pool)
		if err != nil {
			logFrom(r).Error("teacher profile: load failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load your profile")
			return
		}
		writeJSON(w, http.StatusOK, teacherProfileAnswer{p, allowedLengths, maxBreakMinutes})
	}
}

func loadTeacherProfile(r *http.Request, pool *pgxpool.Pool) (teacherProfileDTO, error) {
	var p teacherProfileDTO
	err := pool.QueryRow(r.Context(), `
		SELECT a.display_name, COALESCE(t.bio_en, ''), COALESCE(t.bio_es, ''), t.buffer_minutes, t.offered_durations
		FROM teacher t JOIN account a ON a.id = t.account_id WHERE t.id = $1`, teacherFrom(r)).
		Scan(&p.DisplayName, &p.BioEN, &p.BioES, &p.BreakMinutes, &p.OfferedLengths)
	slices.Sort(p.OfferedLengths)
	return p, err
}

// putTeacherProfile handles PUT /teacher/profile (wrapped in requireTeacher):
// checks the whole profile, then writes the name (on the account) and the rest
// (on the teacher row) in one transaction. Answers with the saved profile.
func putTeacherProfile(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var p teacherProfileDTO
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
		dec.DisallowUnknownFields() // a misspelt field is an error, not silently ignored
		if err := dec.Decode(&p); err != nil {
			writeError(w, r, http.StatusBadRequest, "body must be JSON like {display_name, bio_en, bio_es, break_minutes, offered_lengths}")
			return
		}
		if msg := checkTeacherProfile(&p); msg != "" {
			writeErrorCode(w, r, http.StatusBadRequest, "invalid_profile", msg)
			return
		}
		err := pgx.BeginFunc(r.Context(), pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(r.Context(), `
				UPDATE account SET display_name = $2
				WHERE id = (SELECT account_id FROM teacher WHERE id = $1)`, teacherFrom(r), p.DisplayName); err != nil {
				return err
			}
			// NULLIF stores an empty bio as NULL ("no bio"), like a teacher who never wrote one.
			_, err := tx.Exec(r.Context(), `
				UPDATE teacher SET bio_en = NULLIF($2, ''), bio_es = NULLIF($3, ''),
				       buffer_minutes = $4, offered_durations = $5
				WHERE id = $1`, teacherFrom(r), p.BioEN, p.BioES, p.BreakMinutes, p.OfferedLengths)
			return err
		})
		if err != nil {
			logFrom(r).Error("teacher profile: save failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not save your profile")
			return
		}
		logFrom(r).Info("teacher profile saved", "teacher_id", teacherFrom(r))
		saved, err := loadTeacherProfile(r, pool)
		if err != nil {
			logFrom(r).Error("teacher profile: reload failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "saved, but could not load your profile")
			return
		}
		writeJSON(w, http.StatusOK, teacherProfileAnswer{saved, allowedLengths, maxBreakMinutes})
	}
}

// checkTeacherProfile tidies p (trims text, sorts and de-duplicates lengths)
// and returns what's wrong with it, or "" if it's fine.
func checkTeacherProfile(p *teacherProfileDTO) string {
	p.DisplayName = strings.TrimSpace(p.DisplayName)
	p.BioEN, p.BioES = strings.TrimSpace(p.BioEN), strings.TrimSpace(p.BioES)
	slices.Sort(p.OfferedLengths)
	p.OfferedLengths = slices.Compact(p.OfferedLengths)
	if p.OfferedLengths == nil {
		p.OfferedLengths = []int{} // none: group classes only (an empty array, not NULL)
	}
	// Characters, not bytes: "á" is one character but two bytes in UTF-8.
	switch n := utf8.RuneCountInString(p.DisplayName); {
	case n == 0:
		return "display_name is required"
	case n > maxNameChars:
		return fmt.Sprintf("display_name can be at most %d characters", maxNameChars)
	}
	if utf8.RuneCountInString(p.BioEN) > maxBioChars || utf8.RuneCountInString(p.BioES) > maxBioChars {
		return fmt.Sprintf("each bio can be at most %d characters", maxBioChars)
	}
	if p.BreakMinutes < 0 || p.BreakMinutes > maxBreakMinutes {
		return fmt.Sprintf("break_minutes must be 0 to %d", maxBreakMinutes)
	}
	for _, l := range p.OfferedLengths {
		if !slices.Contains(allowedLengths, l) {
			return fmt.Sprintf("offered_lengths may only contain %v", allowedLengths)
		}
	}
	return ""
}
