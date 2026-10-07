package e2e

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chmuzamil/backupproof/internal/agent"
	"github.com/chmuzamil/backupproof/internal/server"
	"github.com/chmuzamil/backupproof/internal/snapshot"
)

// TestRestoreFromDashboard: browse a backup, download files as a zip, put
// files back where they were, restore to a new folder, and restore a
// database, all from the dashboard API.
func TestRestoreFromDashboard(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	data := filepath.Join(dir, "server")
	srv, err := server.New(server.Config{DataDir: data, NoLocalAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	a := &api{t: t, base: ts.URL, c: &http.Client{Jar: jar}}
	var setup struct{ CSRF string }
	a.do("POST", "/api/setup", map[string]string{"username": "admin", "password": "a-long-admin-password"}, &setup)
	a.csrf = setup.CSRF
	a.do("PUT", "/api/settings/tsa", map[string]any{"urls": []string{}}, &map[string]bool{})
	a.do("POST", "/api/users", map[string]string{"username": "op", "password": "operator-password-1", "role": "operator"}, &map[string]int64{})

	// This agent plays the dashboard's own built-in server, so the dashboard
	// may read its folder storage (as on a real install).
	var tok struct{ Token string }
	a.do("POST", "/api/agents/enroll-token", nil, &tok)
	agentDir := filepath.Join(data, "builtin-agent")
	if _, err := agent.Enroll(ctx, ts.URL, tok.Token, "This server", agentDir); err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	var agents []server.Agent
	a.do("GET", "/api/agents", nil, &agents)
	var repo struct{ ID int64 }
	a.do("POST", "/api/repositories", map[string]any{"name": "disk", "backend": map[string]any{"type": "local", "path": filepath.Join(dir, "repo")}, "password": "repository-password-123"}, &repo)

	site := filepath.Join(dir, "site")
	write := func(p, s string) {
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(site, "index.html"), "<h1>hello</h1>")
	write(filepath.Join(site, "img", "logo.svg"), "<svg/>")
	dbPath := filepath.Join(dir, "billing.db")
	makeDB(t, dbPath)

	newItem := func(name string, spec map[string]any) int64 {
		var src struct{ ID int64 }
		a.do("POST", "/api/sources", map[string]any{"name": name, "agentId": agents[0].ID, "repoId": repo.ID, "spec": spec,
			"backupCron": "@daily", "drillCron": "0 4 * * 0"}, &src)
		return src.ID
	}
	files := newItem("site", map[string]any{"kind": "files", "paths": []string{site}})
	billing := newItem("billing", map[string]any{"kind": "sqlite", "paths": []string{dbPath}})
	runJob := func(path string, body any) server.Job {
		t.Helper()
		var r struct{ JobID int64 }
		a.do("POST", path, body, &r)
		for {
			ran, err := ag.PollOnce(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var j server.Job
			a.do("GET", fmt.Sprintf("/api/jobs/%d", r.JobID), nil, &j)
			if j.State == "succeeded" || j.State == "failed" {
				return j
			}
			if !ran {
				t.Fatalf("job #%d never ran", r.JobID)
			}
		}
	}
	for _, id := range []int64{files, billing} {
		if j := runJob(fmt.Sprintf("/api/sources/%d/run", id), map[string]string{"kind": "backup"}); j.State != "succeeded" {
			t.Fatalf("backup: %s\n%s", j.Error, j.Log)
		}
		if ran, err := ag.PollOnce(ctx); err != nil || !ran { // the first restore test follows
			t.Fatalf("first restore test did not run: %v", err)
		}
	}

	// Restore points: the backup, already restore-tested.
	var pts struct {
		Points []struct {
			SnapshotID string
			Tested     bool
		}
	}
	a.do("GET", fmt.Sprintf("/api/sources/%d/restore-points", files), nil, &pts)
	if len(pts.Points) != 1 || !pts.Points[0].Tested {
		t.Fatalf("restore points: %+v", pts)
	}
	snap := pts.Points[0].SnapshotID
	sitePath := snapshot.CleanPath(site)

	// Browse down to the site folder.
	var ls struct {
		Entries []struct {
			Name, Path string
			Dir        bool
		}
	}
	a.do("GET", fmt.Sprintf("/api/sources/%d/files?snapshot=%s&path=%s", files, snap, url.QueryEscape(sitePath)), nil, &ls)
	if len(ls.Entries) != 2 || !ls.Entries[0].Dir || ls.Entries[0].Name != "img" || ls.Entries[1].Name != "index.html" {
		t.Fatalf("listing %s: %+v", sitePath, ls.Entries)
	}

	// Download one folder as a zip.
	resp, err := a.c.Get(fmt.Sprintf("%s/api/sources/%d/download?snapshot=%s&path=%s", ts.URL, files, snap, url.QueryEscape(sitePath+"/img")))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("zip: %v (%s)", err, body)
	}
	if len(zr.File) != 1 || zr.File[0].Name != sitePath+"/img/logo.svg" {
		t.Fatalf("zip contents: %v", zr.File)
	}

	// Damage the original, then put it back where it was.
	write(filepath.Join(site, "index.html"), "defaced")
	_ = os.Remove(filepath.Join(site, "img", "logo.svg"))
	if j := runJob(fmt.Sprintf("/api/sources/%d/restore", files), map[string]any{"snapshotId": snap, "paths": []string{sitePath}}); j.State != "succeeded" {
		t.Fatalf("restore in place: %s\n%s", j.Error, j.Log)
	}
	if b, _ := os.ReadFile(filepath.Join(site, "index.html")); string(b) != "<h1>hello</h1>" {
		t.Errorf("index.html after restore: %q", b)
	}
	if _, err := os.Stat(filepath.Join(site, "img", "logo.svg")); err != nil {
		t.Errorf("logo.svg not restored: %v", err)
	}

	// Restore everything into a new folder instead.
	out := filepath.Join(dir, "restored")
	if j := runJob(fmt.Sprintf("/api/sources/%d/restore", files), map[string]any{"snapshotId": snap, "folder": out}); j.State != "succeeded" {
		t.Fatalf("restore to folder: %s\n%s", j.Error, j.Log)
	}
	if b, _ := os.ReadFile(filepath.Join(out, filepath.FromSlash(sitePath), "index.html")); string(b) != "<h1>hello</h1>" {
		t.Errorf("restored copy: %q", b)
	}

	// A database: into a new file, then over the original.
	var dbPts struct {
		Points   []struct{ SnapshotID string }
		Database bool
	}
	a.do("GET", fmt.Sprintf("/api/sources/%d/restore-points", billing), nil, &dbPts)
	if !dbPts.Database || len(dbPts.Points) == 0 {
		t.Fatalf("db restore points: %+v", dbPts)
	}
	copyPath := filepath.Join(dir, "billing-restored.db")
	if j := runJob(fmt.Sprintf("/api/sources/%d/restore-db", billing), map[string]any{"snapshotId": dbPts.Points[0].SnapshotID, "dbTarget": copyPath}); j.State != "succeeded" {
		t.Fatalf("restore-db: %s\n%s", j.Error, j.Log)
	}
	if n := countRows(t, copyPath); n != 250 {
		t.Errorf("restored database has %d rows, want 250", n)
	}
	db, _ := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	db.Exec("DELETE FROM invoices")
	db.Close()
	if j := runJob(fmt.Sprintf("/api/sources/%d/restore-db", billing), map[string]any{"snapshotId": dbPts.Points[0].SnapshotID, "dbReplace": true}); j.State != "succeeded" {
		t.Fatalf("restore-db replace: %s\n%s", j.Error, j.Log)
	}
	if n := countRows(t, dbPath); n != 250 {
		t.Errorf("original database has %d rows after replace, want 250", n)
	}

	// Refusals.
	fake := strings.Repeat("ab", 32)
	if code := a.do("POST", fmt.Sprintf("/api/sources/%d/restore", files), map[string]any{"snapshotId": fake}, nil); code != http.StatusBadRequest {
		t.Errorf("restore of an unsigned backup: HTTP %d, want 400", code)
	}
	if code := a.do("GET", fmt.Sprintf("/api/sources/%d/files?snapshot=%s", files, dbPts.Points[0].SnapshotID), nil, nil); code != http.StatusBadRequest {
		t.Errorf("browsing another item's backup: HTTP %d, want 400", code)
	}
	ojar, _ := cookiejar.New(nil)
	op := &api{t: t, base: ts.URL, c: &http.Client{Jar: ojar}}
	var login struct{ CSRF string }
	op.do("POST", "/api/login", map[string]string{"username": "op", "password": "operator-password-1"}, &login)
	op.csrf = login.CSRF
	if code := op.do("POST", fmt.Sprintf("/api/sources/%d/restore", files), map[string]any{"snapshotId": snap}, nil); code != http.StatusForbidden {
		t.Errorf("operator restore: HTTP %d, want 403", code)
	}
	if code := op.do("GET", fmt.Sprintf("/api/sources/%d/download?snapshot=%s", files, snap), nil, nil); code != http.StatusForbidden {
		t.Errorf("operator download: HTTP %d, want 403", code)
	}
}

func countRows(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM invoices").Scan(&n); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return n
}
