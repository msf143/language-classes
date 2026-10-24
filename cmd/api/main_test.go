package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Page files carry Cache-Control: no-cache, and an unchanged file is answered
// with 304 Not Modified, so the check browsers make before reusing a copy is cheap.
func TestWebFilesRevalidate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "styles.css"), []byte("body {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	get := func(ifModifiedSince string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/styles.css", nil)
		if ifModifiedSince != "" {
			req.Header.Set("If-Modified-Since", ifModifiedSince)
		}
		rec := httptest.NewRecorder()
		webFiles(dir).ServeHTTP(rec, req)
		return rec
	}

	first := get("")
	lastModified := first.Header().Get("Last-Modified")
	if first.Code != http.StatusOK || first.Body.String() != "body {}" || lastModified == "" {
		t.Fatalf("first fetch: %d %q, Last-Modified %q", first.Code, first.Body, lastModified)
	}
	again := get(lastModified)
	if again.Code != http.StatusNotModified || again.Body.Len() != 0 {
		t.Errorf("unchanged file: %d with %d bytes, want 304 and no body", again.Code, again.Body.Len())
	}
	for _, rec := range []*httptest.ResponseRecorder{first, again} {
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("Cache-Control %q, want no-cache", got)
		}
	}
}
