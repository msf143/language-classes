package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func decodeProfile(t *testing.T, rec *httptest.ResponseRecorder) teacherProfileAnswer {
	t.Helper()
	var a teacherProfileAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatalf("%d %s: %v", rec.Code, rec.Body, err)
	}
	return a
}

// GET and PUT /teacher/profile: a new teacher's defaults, a save (tidied), and
// what's refused. Uses the dev database's teacher helpers (retired afterwards).
func TestTeacherProfile(t *testing.T) {
	pool := testPool(t)
	cookie, teacherID := newSignedInTeacher(t, pool)
	get := func() *httptest.ResponseRecorder {
		return send(pool, getTeacherProfile(pool), "GET", "/teacher/profile", "", "", cookie)
	}
	put := func(body string) *httptest.ResponseRecorder {
		return send(pool, putTeacherProfile(pool), "PUT", "/teacher/profile", body, "", cookie)
	}

	start := decodeProfile(t, get())
	if start.DisplayName != "Test" || start.BioEN != "" || start.BreakMinutes != 5 ||
		!slices.Equal(start.AllowedLengths, []int{30, 45, 60}) || start.MaxBreakMinutes != 15 {
		t.Errorf("defaults: %+v", start)
	}

	t.Run("a save: trimmed, lengths sorted and de-duplicated, then read back", func(t *testing.T) {
		rec := put(`{"display_name": "  Carlos  ", "bio_en": " Hi! I'm Carlos. ", "bio_es": "¡Hola! Soy Carlos.",
			"break_minutes": 10, "offered_lengths": [60, 30, 60]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		want := teacherProfileDTO{"Carlos", "Hi! I'm Carlos.", "¡Hola! Soy Carlos.", 10, []int{30, 60}}
		for _, got := range []teacherProfileDTO{decodeProfile(t, rec).teacherProfileDTO, decodeProfile(t, get()).teacherProfileDTO} {
			if got.DisplayName != want.DisplayName || got.BioEN != want.BioEN || got.BioES != want.BioES ||
				got.BreakMinutes != want.BreakMinutes || !slices.Equal(got.OfferedLengths, want.OfferedLengths) {
				t.Errorf("got %+v, want %+v", got, want)
			}
		}
		// An empty bio is stored as NULL ("none"), and no lengths as an empty list.
		if rec := put(`{"display_name": "Carlos", "bio_en": "", "bio_es": "", "break_minutes": 0, "offered_lengths": []}`); rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if n := count(t, pool, `SELECT count(*) FROM teacher WHERE id = $1 AND bio_en IS NULL AND offered_durations = '{}'`, teacherID); n != 1 {
			t.Error("empty bio not NULL, or lengths not empty")
		}
	})

	t.Run("refused", func(t *testing.T) {
		long := strings.Repeat("á", maxBioChars+1)
		tests := []struct{ name, body, wantCode string }{
			{"no name", `{"display_name": "  ", "break_minutes": 5, "offered_lengths": [60]}`, "invalid_profile"},
			{"name too long", `{"display_name": "` + strings.Repeat("x", 61) + `", "break_minutes": 5}`, "invalid_profile"},
			{"bio too long", `{"display_name": "C", "bio_es": "` + long + `", "break_minutes": 5}`, "invalid_profile"},
			{"break too long", `{"display_name": "C", "break_minutes": 20}`, "invalid_profile"},
			{"negative break", `{"display_name": "C", "break_minutes": -1}`, "invalid_profile"},
			{"a 90-minute length", `{"display_name": "C", "break_minutes": 5, "offered_lengths": [60, 90]}`, "invalid_profile"},
			{"an unknown field", `{"display_name": "C", "brake_minutes": 5}`, ""},
			{"not JSON", `display_name=C`, ""},
		}
		for _, tt := range tests {
			rec := put(tt.body)
			if rec.Code != http.StatusBadRequest || (tt.wantCode != "" && errorCode(t, rec) != tt.wantCode) {
				t.Errorf("%s: %d %s", tt.name, rec.Code, rec.Body)
			}
		}
		// Nothing above changed the saved profile.
		if p := decodeProfile(t, get()); p.DisplayName != "Carlos" || p.BreakMinutes != 0 {
			t.Errorf("a refused save changed the profile: %+v", p)
		}
	})

	t.Run("students can't", func(t *testing.T) {
		student := newStudent(t, pool, 0, false)
		if rec := send(pool, putTeacherProfile(pool), "PUT", "/teacher/profile", `{"display_name": "X"}`, "", student.cookie); rec.Code != http.StatusForbidden {
			t.Errorf("student PUT: %d, want 403", rec.Code)
		}
	})
}
