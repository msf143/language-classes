package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// In a private schema, so the list holds only this test's teachers.
func TestPublicTeachers(t *testing.T) {
	pool := migratedSchemaPool(t, 6)
	ctx := context.Background()
	active, activeLogin := teacherLogin(t, pool)
	photo := mustPhoto(t, pool, activeLogin)
	pool.Exec(ctx, `UPDATE account SET display_name = 'Zoe', email = 'zoe-private@example.com' WHERE id = $1`, activeLogin.accountID)
	pool.Exec(ctx, `UPDATE teacher SET bio_en = 'From Bogotá.', bio_es = 'De Bogotá.' WHERE id = $1`, active)
	second := newTeacher(t, pool) // no photo, no bio
	pool.Exec(ctx, `UPDATE account SET display_name = 'Ana' WHERE id = (SELECT account_id FROM teacher WHERE id = $1)`, second)
	pending, inactive := newTeacher(t, pool), newTeacher(t, pool)
	pool.Exec(ctx, `UPDATE teacher SET status = 'pending' WHERE id = $1`, pending)
	pool.Exec(ctx, `UPDATE teacher SET status = 'inactive' WHERE id = $1`, inactive)

	rec := httptest.NewRecorder()
	listPublicTeachers(pool)(rec, httptest.NewRequest("GET", "/teachers", nil)) // nobody signed in
	var list []publicTeacherDTO
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(list) != 2 || list[0].ID != second || list[1].ID != active {
		t.Fatalf("want Ana then Zoe, active only: %+v", list)
	}
	z := list[1]
	if z.Name != "Zoe" || z.PhotoID == nil || *z.PhotoID != photo || z.BioEN != "From Bogotá." || z.BioES != "De Bogotá." {
		t.Errorf("with everything: %+v", z)
	}
	if a := list[0]; a.PhotoID != nil || a.BioEN != "" || a.BioES != "" {
		t.Errorf("with nothing: %+v", a)
	}
	if strings.Contains(rec.Body.String(), "@") {
		t.Errorf("an email address in a public answer: %s", rec.Body)
	}
	if code := getPhoto(pool, photo, nil).Code; code != 200 {
		t.Errorf("the photo it names, signed out: %d", code)
	}
}
