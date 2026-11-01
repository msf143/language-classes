package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// In a private schema, so saving here never changes the dev site's About page.
func TestAboutPage(t *testing.T) {
	pool := migratedSchemaPool(t, 4)
	ctx := context.Background()
	get := func() aboutPageDTO {
		t.Helper()
		rec := httptest.NewRecorder()
		getAboutPage(pool)(rec, httptest.NewRequest("GET", "/about-page", nil))
		var p aboutPageDTO
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &p) != nil {
			t.Fatalf("GET: %d %s", rec.Code, rec.Body)
		}
		return p
	}
	first := get()
	if !strings.HasPrefix(first.BodyEN, "# Real People.") || !strings.HasSuffix(first.BodyEN, "Let's talk.") ||
		!strings.HasPrefix(first.BodyES, "# Personas reales.") {
		t.Errorf("the first text: %q / %q", first.BodyEN, first.BodyES)
	}

	admin := newStudent(t, pool, 0, false)
	pool.Exec(ctx, `UPDATE account SET is_admin = true WHERE id = $1`, admin.accountID)
	visitor := newStudent(t, pool, 0, false)
	put := func(who *student, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/about-page", strings.NewReader(body))
		if who != nil {
			req.AddCookie(who.cookie)
		}
		rec := httptest.NewRecorder()
		requireAdmin(pool, putAboutPage(pool))(rec, req)
		return rec
	}
	good := `{"body_en": "  # Hello\n\nWorld  ", "body_es": "# Hola\n\nMundo"}`
	versions := func() int { return count(t, pool, `SELECT count(*) FROM about_page_version`) }

	tests := []struct {
		name   string
		who    *student
		body   string
		status int
	}{
		{"signed out", nil, good, 401},
		{"not an admin", &visitor, good, 403},
		{"no English", &admin, `{"body_en": "   ", "body_es": "Hola"}`, 400},
		{"no Spanish", &admin, `{"body_en": "Hello", "body_es": ""}`, 400},
		{"too long", &admin, `{"body_en": "` + strings.Repeat("á", maxAboutChars+1) + `", "body_es": "Hola"}`, 400},
		{"a field that doesn't exist", &admin, `{"body_en": "Hello", "body_es": "Hola", "html": "<b>"}`, 400},
		{"not JSON", &admin, `hello`, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if rec := put(tt.who, tt.body); rec.Code != tt.status {
				t.Errorf("%d %s, want %d", rec.Code, rec.Body, tt.status)
			}
		})
	}
	if n := versions(); n != 1 {
		t.Fatalf("%d versions after refused saves, want 1", n)
	}
	if got := get(); got.BodyEN != first.BodyEN {
		t.Error("a refused save changed the page")
	}

	// A save becomes the page, tidied; the old version stays, and who saved is kept.
	if rec := put(&admin, good); rec.Code != 200 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	if got := get(); got.BodyEN != "# Hello\n\nWorld" || got.BodyES != "# Hola\n\nMundo" {
		t.Errorf("after saving: %+v", got)
	}
	if n := versions(); n != 2 {
		t.Errorf("%d versions, want 2 (the old one kept)", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM about_page_version WHERE saved_by = $1`, admin.accountID); n != 1 {
		t.Error("who saved it wasn't recorded")
	}
	// Whatever is typed is stored as it is: making it safe is the page's job.
	if rec := put(&admin, `{"body_en": "<script>x</script>", "body_es": "Hola"}`); rec.Code != 200 {
		t.Fatalf("save: %d", rec.Code)
	}
	if got := get(); got.BodyEN != "<script>x</script>" {
		t.Errorf("stored %q", got.BodyEN)
	}
}
