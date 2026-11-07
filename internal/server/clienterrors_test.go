package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// With error reporting off (no BP_SENTRY_DSN) the dashboard is told so and
// anything it posts is accepted and dropped.
func TestClientErrorsOff(t *testing.T) {
	srv, err := New(Config{DataDir: filepath.Join(t.TempDir(), "s"), NoLocalAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	res, err := http.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	json.NewDecoder(res.Body).Decode(&st)
	res.Body.Close()
	if on, ok := st["errorReporting"].(bool); !ok || on {
		t.Errorf("status errorReporting = %v, want false", st["errorReporting"])
	}

	res, err = http.Post(ts.URL+"/api/client-errors", "application/json", strings.NewReader(`{"message":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Errorf("POST /api/client-errors: HTTP %d, want 204", res.StatusCode)
	}
}
