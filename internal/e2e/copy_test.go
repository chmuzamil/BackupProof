package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/chmuzamil/backupproof/internal/agent"
	"github.com/chmuzamil/backupproof/internal/server"
)

// TestSecondCopy: with a second storage set, each backup is copied there and
// gets a signed copy proof with the same content root.
func TestSecondCopy(t *testing.T) {
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
	mkRepo := func(name string) int64 {
		var r struct{ ID int64 }
		a.do("POST", "/api/repositories", map[string]any{"name": name, "backend": map[string]any{"type": "local", "path": filepath.Join(dir, name)}, "password": "repository-password-123"}, &r)
		return r.ID
	}
	main, second := mkRepo("main"), mkRepo("offsite")
	docs := filepath.Join(dir, "docs")
	_ = os.MkdirAll(docs, 0o755)
	_ = os.WriteFile(filepath.Join(docs, "a.txt"), []byte("hello"), 0o644)
	var src struct{ ID int64 }
	a.do("POST", "/api/sources", map[string]any{"name": "docs", "agentId": agents[0].ID, "repoId": main,
		"spec": map[string]any{"kind": "files", "paths": []string{docs}}, "backupCron": "@daily", "drillCron": "0 4 * * 0", "enabled": true}, &src)
	if code := a.do("PUT", fmt.Sprintf("/api/sources/%d/copy", src.ID), map[string]any{"repoId": main}, nil); code != http.StatusBadRequest {
		t.Errorf("second copy in the same storage: HTTP %d, want 400", code)
	}
	a.do("PUT", fmt.Sprintf("/api/sources/%d/copy", src.ID), map[string]any{"repoId": second}, &map[string]bool{})

	var r struct{ JobID int64 }
	a.do("POST", fmt.Sprintf("/api/sources/%d/run", src.ID), map[string]string{"kind": "backup"}, &r)
	for i := 0; i < 3; i++ { // backup, then the copy and the first restore test
		if ran, err := ag.PollOnce(ctx); err != nil || !ran {
			t.Fatalf("job %d did not run: %v", i, err)
		}
	}
	var proofs []struct {
		Kind   string
		Passed bool
	}
	a.do("GET", fmt.Sprintf("/api/proofs?source=%d", src.ID), nil, &proofs)
	kinds := map[string]bool{}
	for _, p := range proofs {
		if p.Passed {
			kinds[p.Kind] = true
		}
	}
	if !kinds["backup"] || !kinds["copy"] || !kinds["drill"] {
		t.Fatalf("want backup, copy and restore-test proofs, got %+v", proofs)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "offsite", "snapshots")); len(entries) != 1 {
		t.Errorf("second storage has %d backups, want 1", len(entries))
	}
	// A storage used only for second copies still gets health checks.
	var chk struct{ JobID int64 }
	if code := a.do("POST", fmt.Sprintf("/api/repositories/%d/check", second), nil, &chk); code != 200 {
		t.Fatalf("checking the second storage: HTTP %d", code)
	}
	if ran, err := ag.PollOnce(ctx); err != nil || !ran {
		t.Fatalf("check did not run: %v", err)
	}
	var stats map[string][]struct {
		OK        bool
		Bytes     int64
		Snapshots int
	}
	a.do("GET", "/api/repositories/stats", nil, &stats)
	if st := stats[fmt.Sprint(second)]; len(st) != 1 || !st[0].OK || st[0].Bytes == 0 || st[0].Snapshots != 1 {
		t.Errorf("second storage check: %+v", stats)
	}
	// The second storage is in use now, so it can't be removed.
	if code := a.do("DELETE", fmt.Sprintf("/api/repositories/%d", second), nil, nil); code != http.StatusConflict {
		t.Errorf("removing a storage used for copies: HTTP %d, want 409", code)
	}

	// The copy gets a restore test of its own, separate from the main ones.
	type status struct {
		Status       string
		LastDrill    *struct{ ID int64 }
		LastCopyTest *struct{ Passed bool }
	}
	runCopyTest := func() {
		t.Helper()
		if code := a.do("POST", fmt.Sprintf("/api/sources/%d/run", src.ID), map[string]string{"kind": "copy-drill"}, &r); code != 200 {
			t.Fatalf("starting the copy test: HTTP %d", code)
		}
		if ran, _ := ag.PollOnce(ctx); !ran {
			t.Fatal("the copy test did not run")
		}
	}
	var before, after status
	a.do("GET", fmt.Sprintf("/api/sources/%d", src.ID), nil, &before)
	if before.LastCopyTest != nil {
		t.Fatalf("copy tested before any test ran: %+v", before.LastCopyTest)
	}
	runCopyTest()
	a.do("GET", fmt.Sprintf("/api/sources/%d", src.ID), nil, &after)
	if after.LastCopyTest == nil || !after.LastCopyTest.Passed {
		t.Fatalf("copy test: %+v", after.LastCopyTest)
	}
	if after.LastDrill == nil || after.LastDrill.ID != before.LastDrill.ID {
		t.Errorf("the copy test counted as a main restore test: %+v then %+v", before.LastDrill, after.LastDrill)
	}

	// Damage in the second storage fails only the copy's test.
	err = filepath.WalkDir(filepath.Join(dir, "offsite", "data"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		return os.WriteFile(path, []byte("damaged"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	runCopyTest()
	a.do("GET", fmt.Sprintf("/api/sources/%d", src.ID), nil, &after)
	if after.LastCopyTest == nil || after.LastCopyTest.Passed {
		t.Errorf("a damaged copy passed its test: %+v", after.LastCopyTest)
	}
	if after.Status != before.Status {
		t.Errorf("a damaged copy changed the item's status from %s to %s", before.Status, after.Status)
	}
	var alerts []struct{ Kind string }
	a.do("GET", "/api/alerts?open=1", nil, &alerts)
	open := map[string]bool{}
	for _, al := range alerts {
		open[al.Kind] = true
	}
	if !open["copy-drill-failed"] || open["drill-failed"] {
		t.Errorf("open alerts after a damaged copy: %+v", alerts)
	}
}
