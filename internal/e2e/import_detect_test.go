package e2e

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chmuzamil/backupproof/internal/server"
)

// TestImportScanRecognisesRepository: looking for "backup files" in a
// location that holds a restic repository tells the dashboard which import
// to use and where the repository is, instead of listing its chunks.
func TestImportScanRecognisesRepository(t *testing.T) {
	dir := t.TempDir()
	h := strings.Repeat("e5", 32)
	for _, k := range []string{"founder-os-vps/config", "founder-os-vps/keys/" + h, "founder-os-vps/data/e5/" + h} {
		p := filepath.Join(dir, filepath.FromSlash(k))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("encrypted"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	srv, err := server.New(server.Config{DataDir: filepath.Join(t.TempDir(), "server"), NoLocalAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	admin := &api{t: t, base: ts.URL, c: &http.Client{Jar: jar}}
	var setup struct{ CSRF string }
	admin.do("POST", "/api/setup", map[string]string{"username": "admin", "password": "a-long-admin-password"}, &setup)
	admin.csrf = setup.CSRF

	var scan struct {
		Found      int
		Repository struct{ Format, Tool, Prefix string }
		Storage    struct{ Type, Path string }
	}
	admin.do("POST", "/api/import/scan", map[string]any{"format": "files", "storage": map[string]any{"type": "local", "path": dir}}, &scan)
	if scan.Repository.Format != "restic" || scan.Repository.Prefix != "founder-os-vps" || scan.Storage.Path != filepath.Join(dir, "founder-os-vps") || scan.Found != 0 {
		t.Errorf("scan should point at the restic repository, got %+v", scan)
	}
}
