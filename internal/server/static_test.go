package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// The dashboard's files answer 400 for a path that isn't valid UTF-8, not a
// server error, while normal and missing files behave as before.
func TestStaticPaths(t *testing.T) {
	srv, err := New(Config{DataDir: filepath.Join(t.TempDir(), "s"), NoLocalAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for path, want := range map[string]int{
		"/":            http.StatusOK,
		"/app.js":      http.StatusOK,
		"/%c0":         http.StatusBadRequest,
		"/x%ff%fe.js":  http.StatusBadRequest,
		"/missing.css": http.StatusNotFound,
	} {
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("GET %s: HTTP %d, want %d", path, res.StatusCode, want)
		}
	}
}
