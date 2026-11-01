package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testJPEG is a real JPEG of that size.
func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCleanPhoto(t *testing.T) {
	var pngBytes bytes.Buffer
	png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 100, 100)))
	tests := []struct {
		name string
		raw  []byte
		code string
	}{
		{"what the page sends", testJPEG(t, 512, 512), ""},
		{"not square is fine", testJPEG(t, 300, 200), ""},
		{"the smallest", testJPEG(t, minPhotoSide, minPhotoSide), ""},
		{"the largest", testJPEG(t, maxPhotoSide, maxPhotoSide), ""},
		{"text", []byte("hello"), "photo_invalid"},
		{"empty", nil, "photo_invalid"},
		{"a PNG", pngBytes.Bytes(), "photo_invalid"},
		{"too small", testJPEG(t, minPhotoSide-1, 100), "photo_wrong_size"},
		{"too wide", testJPEG(t, maxPhotoSide+1, 100), "photo_wrong_size"},
		{"a JPEG cut short", testJPEG(t, 512, 512)[:200], "photo_invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clean, code := cleanPhoto(tt.raw)
			if code != tt.code {
				t.Fatalf("code %q, want %q", code, tt.code)
			}
			if code == "" {
				if _, err := jpeg.Decode(bytes.NewReader(clean)); err != nil {
					t.Errorf("the result isn't a JPEG: %v", err)
				}
			}
		})
	}
}

// Metadata (a phone's GPS location lives in a block like this) and anything
// hidden after the image don't survive the re-encoding.
func TestCleanPhotoDropsExtras(t *testing.T) {
	raw := testJPEG(t, 100, 100)
	secret := "GPS-48.8584,2.2945"
	app1 := append([]byte{0xFF, 0xE1, 0x00, byte(len(secret) + 2)}, secret...) // an APP1 block, where EXIF goes
	withBlock := append(append(append([]byte{}, raw[:2]...), app1...), raw[2:]...)
	withBlock = append(withBlock, []byte("TRAILER-after-the-image")...)
	if !bytes.Contains(withBlock, []byte(secret)) {
		t.Fatal("test setup: the secret isn't in the upload")
	}
	clean, code := cleanPhoto(withBlock)
	if code != "" {
		t.Fatalf("code %q", code)
	}
	if bytes.Contains(clean, []byte(secret)) || bytes.Contains(clean, []byte("TRAILER")) {
		t.Error("extras survived the cleaning")
	}
}

func putPhoto(pool *pgxpool.Pool, s student, raw []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest("PUT", "/me/photo", bytes.NewReader(raw))
	req.AddCookie(s.cookie)
	rec := httptest.NewRecorder()
	requireSignIn(pool, uploadPhoto(pool))(rec, req)
	return rec
}

// mustPhoto uploads a photo and returns its id.
func mustPhoto(t *testing.T, pool *pgxpool.Pool, s student) string {
	t.Helper()
	rec := putPhoto(pool, s, testJPEG(t, 512, 512))
	var body struct {
		PhotoID string `json:"photo_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || body.PhotoID == "" {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	return body.PhotoID
}

// getPhoto asks for a photo as cookie's owner (nil = nobody signed in).
func getPhoto(pool *pgxpool.Pool, id string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/photos/"+id, nil)
	req.SetPathValue("id", id)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	signedIn(pool, servePhoto(pool))(rec, req)
	return rec
}

// Upload, /me says so; replace, the old address dies; remove, it's really gone.
func TestPhotoUploadReplaceRemove(t *testing.T) {
	pool := testPool(t)
	s := newStudent(t, pool, 0, false)
	meID := func() *string {
		var m struct {
			Account account `json:"account"`
		}
		json.Unmarshal(me(s, requireSignIn(pool, getMe(pool)), "GET", "", "").Body.Bytes(), &m)
		return m.Account.PhotoID
	}
	if meID() != nil {
		t.Fatal("a new account has a photo")
	}
	first := mustPhoto(t, pool, s)
	if got := meID(); got == nil || *got != first {
		t.Errorf("/me photo_id = %v, want %s", got, first)
	}
	rec := getPhoto(pool, first, s.cookie)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("own photo: %d %v", rec.Code, rec.Header())
	}
	if _, err := jpeg.Decode(bytes.NewReader(rec.Body.Bytes())); err != nil {
		t.Errorf("what came back isn't a JPEG: %v", err)
	}

	second := mustPhoto(t, pool, s)
	if second == first {
		t.Error("replacing kept the same id, so browsers would keep showing the old photo")
	}
	if got := getPhoto(pool, first, s.cookie).Code; got != 404 {
		t.Errorf("the old address: %d, want 404", got)
	}
	if n := count(t, pool, `SELECT count(*) FROM profile_photo WHERE account_id = $1`, s.accountID); n != 1 {
		t.Errorf("%d rows for one account", n)
	}

	removed := func() string { return me(s, requireSignIn(pool, removeMyPhoto(pool)), "POST", "", "").Body.String() }
	if got := removed(); !strings.Contains(got, "true") {
		t.Errorf("remove: %s", got)
	}
	if got := removed(); !strings.Contains(got, "false") {
		t.Errorf("remove again: %s", got)
	}
	if got := getPhoto(pool, second, s.cookie).Code; got != 404 {
		t.Errorf("after removing: %d", got)
	}
	if n := count(t, pool, `SELECT count(*) FROM profile_photo WHERE account_id = $1`, s.accountID); n != 0 {
		t.Error("the row is still there: removing must really delete it")
	}
	if meID() != nil {
		t.Error("/me still has a photo")
	}
}

func TestPhotoUploadRefused(t *testing.T) {
	pool := testPool(t)
	s := newStudent(t, pool, 0, false)
	tests := []struct {
		name   string
		raw    []byte
		status int
		code   string
	}{
		{"over the size limit", make([]byte, maxPhotoBytes+1), 413, "photo_too_large"},
		{"not a photo", []byte("hello"), 400, "photo_invalid"},
		{"too small", testJPEG(t, 10, 10), 400, "photo_wrong_size"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := putPhoto(pool, s, tt.raw)
			if rec.Code != tt.status || errorCode(t, rec) != tt.code {
				t.Errorf("%d %s", rec.Code, rec.Body)
			}
		})
	}
	if n := count(t, pool, `SELECT count(*) FROM profile_photo WHERE account_id = $1`, s.accountID); n != 0 {
		t.Error("a refused upload was stored")
	}
	req := httptest.NewRequest("PUT", "/me/photo", bytes.NewReader(testJPEG(t, 100, 100)))
	rec := httptest.NewRecorder()
	requireSignIn(pool, uploadPhoto(pool))(rec, req)
	if rec.Code != 401 {
		t.Errorf("signed out: %d", rec.Code)
	}
}

// teacherLogin makes a teacher and signs its account in.
func teacherLogin(t *testing.T, pool *pgxpool.Pool) (teacherID string, s student) {
	t.Helper()
	teacherID = newTeacher(t, pool)
	var email string
	if err := pool.QueryRow(context.Background(), `
		SELECT a.id::text, a.email FROM teacher t JOIN account a ON a.id = t.account_id WHERE t.id = $1`, teacherID).Scan(&s.accountID, &email); err != nil {
		t.Fatal(err)
	}
	s.cookie = signIn(t, pool, email)
	return teacherID, s
}

// Who may see a student's photo: the decision agreed on 2026-11-01.
func TestStudentPhotoVisibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	owner := newStudent(t, pool, 5000, false)
	photo := mustPhoto(t, pool, owner)

	teacherID, teacher := teacherLogin(t, pool)
	_, otherTeacher := teacherLogin(t, pool)
	classmate := newStudent(t, pool, 5000, false)
	stranger := newStudent(t, pool, 0, false)
	admin := newStudent(t, pool, 0, false)
	pool.Exec(ctx, `UPDATE account SET is_admin = true WHERE id = $1`, admin.accountID)

	session := newSession(t, pool, teacherID, sessionOpts{})
	before := getPhoto(pool, photo, teacher.cookie).Code
	if before != 404 {
		t.Errorf("a teacher before any booking: %d", before)
	}
	b := bookOK(t, pool, owner, session)
	bookOK(t, pool, classmate, session)

	tests := []struct {
		name   string
		cookie *http.Cookie
		want   int
	}{
		{"nobody signed in", nil, 404},
		{"a stranger", stranger.cookie, 404},
		{"another student in the same class", classmate.cookie, 404},
		{"another teacher", otherTeacher.cookie, 404},
		{"themself", owner.cookie, 200},
		{"an admin", admin.cookie, 200},
		{"their teacher", teacher.cookie, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := getPhoto(pool, photo, tt.cookie)
			if rec.Code != tt.want {
				t.Errorf("%d, want %d", rec.Code, tt.want)
			}
			// A student's photo must never be stored by anything shared.
			if rec.Code == 200 && !strings.HasPrefix(rec.Header().Get("Cache-Control"), "private") {
				t.Errorf("Cache-Control %q", rec.Header().Get("Cache-Control"))
			}
		})
	}

	if _, err := cancelBookingTx(ctx, pool, owner.accountID, b.ID); err != nil {
		t.Fatal(err)
	}
	if got := getPhoto(pool, photo, teacher.cookie).Code; got != 404 {
		t.Errorf("their teacher after the booking was cancelled: %d, want 404", got)
	}
}

// A child's parent pays and a friend's account attends: each is the teacher's student.
func TestPhotoOfSomeoneAttendingForFree(t *testing.T) {
	pool := testPool(t)
	teacherID, teacher := teacherLogin(t, pool)
	payer := newStudent(t, pool, 5000, false)
	friend := newStudent(t, pool, 0, false)
	friendPhoto := mustPhoto(t, pool, friend)
	payerPhoto := mustPhoto(t, pool, payer)
	session := newSession(t, pool, teacherID, sessionOpts{})
	b := bookOK(t, pool, payer, session)
	// The friend's learner in the payer's booking, as joining a seat leaves it.
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO booking_attendee (id, booking_id, learner_id, status) VALUES ($1, $2, $3, 'joined')`,
		newUUIDv7(), b.ID, friend.learners[0]); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]string{"the friend attending": friendPhoto, "the payer": payerPhoto} {
		if got := getPhoto(pool, id, teacher.cookie).Code; got != 200 {
			t.Errorf("%s: %d", name, got)
		}
	}
}

// An active teacher's photo is public and cacheable; a pending one's isn't yet.
func TestTeacherPhotoVisibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	teacherID, teacher := teacherLogin(t, pool)
	photo := mustPhoto(t, pool, teacher)

	rec := getPhoto(pool, photo, nil)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("active teacher, signed out: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	pool.Exec(ctx, `UPDATE teacher SET status = 'pending' WHERE id = $1`, teacherID)
	if got := getPhoto(pool, photo, nil).Code; got != 404 {
		t.Errorf("pending teacher, signed out: %d", got)
	}
	if got := getPhoto(pool, photo, teacher.cookie).Code; got != 200 {
		t.Errorf("pending teacher, themself: %d", got)
	}
}

func TestAdminRemovePhoto(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	owner := newStudent(t, pool, 0, false)
	photo := mustPhoto(t, pool, owner)
	var email string
	pool.QueryRow(ctx, `SELECT email FROM account WHERE id = $1`, owner.accountID).Scan(&email)
	admin := newStudent(t, pool, 0, false)
	pool.Exec(ctx, `UPDATE account SET is_admin = true WHERE id = $1`, admin.accountID)
	notAdmin := newStudent(t, pool, 0, false)

	post := func(who student, address string) *httptest.ResponseRecorder {
		return me(who, requireAdmin(pool, adminRemovePhoto(pool)), "POST", `{"email": "`+address+`"}`, "")
	}
	if rec := post(notAdmin, email); rec.Code != 403 {
		t.Errorf("not an admin: %d", rec.Code)
	}
	if got := getPhoto(pool, photo, owner.cookie).Code; got != 200 {
		t.Fatalf("a non-admin's attempt removed it: %d", got)
	}
	if rec := post(admin, "nobody-"+newUUIDv7()+"@example.com"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "false") {
		t.Errorf("no such account: %d %s", rec.Code, rec.Body)
	}
	if rec := post(admin, "not an email"); rec.Code != 400 {
		t.Errorf("bad email: %d", rec.Code)
	}
	if rec := post(admin, strings.ToUpper(email)); rec.Code != 200 || !strings.Contains(rec.Body.String(), "true") {
		t.Errorf("remove: %d %s", rec.Code, rec.Body)
	}
	if got := getPhoto(pool, photo, owner.cookie).Code; got != 404 {
		t.Errorf("after the admin removed it: %d", got)
	}
}
