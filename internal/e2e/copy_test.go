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
		"spec": map[string]any{"kind": "files", "paths": []string{docs}}, "backupCron": "@daily", "drillCron": "0 4 * * 0"}, &src)
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
	// The second storage is in use now, so it can't be removed.
	if code := a.do("DELETE", fmt.Sprintf("/api/repositories/%d", second), nil, nil); code != http.StatusConflict {
		t.Errorf("removing a storage used for copies: HTTP %d, want 409", code)
	}
}
