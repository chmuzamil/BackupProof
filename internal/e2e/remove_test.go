package e2e

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/chmuzamil/backupproof/internal/agent"
	"github.com/chmuzamil/backupproof/internal/server"
)

// TestRemoveStorage: storage can only be removed by an admin, and only once
// no item uses it.
func TestRemoveStorage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	srv, err := server.New(server.Config{DataDir: filepath.Join(dir, "server"), NoLocalAgent: true})
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
	admin.do("POST", "/api/users", map[string]string{"username": "op", "password": "operator-password-1", "role": "operator"}, &map[string]int64{})
	var tok struct{ Token string }
	admin.do("POST", "/api/agents/enroll-token", nil, &tok)
	if _, err := agent.Enroll(ctx, ts.URL, tok.Token, "web", filepath.Join(dir, "web")); err != nil {
		t.Fatal(err)
	}
	var agents []server.Agent
	admin.do("GET", "/api/agents", nil, &agents)
	var repo struct{ ID int64 }
	admin.do("POST", "/api/repositories", map[string]any{"name": "disk", "backend": map[string]any{"type": "local", "path": filepath.Join(dir, "repo")}, "password": "repository-password-123"}, &repo)
	var src struct{ ID int64 }
	admin.do("POST", "/api/sources", map[string]any{"name": "docs", "agentId": agents[0].ID, "repoId": repo.ID,
		"spec": map[string]any{"kind": "files", "paths": []string{dir}}, "backupCron": "@daily", "drillCron": "0 4 * * 0", "enabled": true}, &src)

	ojar, _ := cookiejar.New(nil)
	op := &api{t: t, base: ts.URL, c: &http.Client{Jar: ojar}}
	var login struct{ CSRF string }
	op.do("POST", "/api/login", map[string]string{"username": "op", "password": "operator-password-1"}, &login)
	op.csrf = login.CSRF
	repoPath := "/api/repositories/" + strconv.FormatInt(repo.ID, 10)
	if code := op.do("DELETE", repoPath, nil, nil); code != http.StatusForbidden {
		t.Errorf("operator removing storage: HTTP %d, want 403", code)
	}
	if code := admin.do("DELETE", repoPath, nil, nil); code != http.StatusConflict {
		t.Errorf("removing storage that an item uses: HTTP %d, want 409", code)
	}
	admin.do("DELETE", "/api/sources/"+strconv.FormatInt(src.ID, 10), nil, &map[string]bool{})
	admin.do("DELETE", repoPath, nil, &map[string]bool{})
	var repos []map[string]any
	admin.do("GET", "/api/repositories", nil, &repos)
	if len(repos) != 0 {
		t.Errorf("storage still listed after removing it: %v", repos)
	}
	if code := admin.do("DELETE", repoPath, nil, nil); code != http.StatusNotFound {
		t.Errorf("removing storage twice: HTTP %d, want 404", code)
	}
}

// TestRemoveServer: only a disconnected server that no item uses can be
// removed, and its key still verifies the proofs it signed.
func TestRemoveServer(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	srv, err := server.New(server.Config{DataDir: filepath.Join(dir, "server"), NoLocalAgent: true})
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
	var tok struct{ Token string }
	admin.do("POST", "/api/agents/enroll-token", nil, &tok)
	if _, err := agent.Enroll(ctx, ts.URL, tok.Token, "old-box", filepath.Join(dir, "old")); err != nil {
		t.Fatal(err)
	}
	var agents []server.Agent
	admin.do("GET", "/api/agents", nil, &agents)
	id := strconv.FormatInt(agents[0].ID, 10)
	var repo struct{ ID int64 }
	admin.do("POST", "/api/repositories", map[string]any{"name": "disk", "backend": map[string]any{"type": "local", "path": filepath.Join(dir, "repo")}, "password": "repository-password-123"}, &repo)
	var src struct{ ID int64 }
	admin.do("POST", "/api/sources", map[string]any{"name": "docs", "agentId": agents[0].ID, "repoId": repo.ID,
		"spec": map[string]any{"kind": "files", "paths": []string{dir}}, "backupCron": "@daily", "drillCron": "0 4 * * 0"}, &src)

	if code := admin.do("DELETE", "/api/agents/"+id, nil, nil); code != http.StatusConflict {
		t.Errorf("removing a connected server: HTTP %d, want 409", code)
	}
	admin.do("POST", "/api/agents/"+id+"/revoke", nil, &map[string]bool{})
	if code := admin.do("DELETE", "/api/agents/"+id, nil, nil); code != http.StatusConflict {
		t.Errorf("removing a server an item runs on: HTTP %d, want 409", code)
	}
	admin.do("DELETE", "/api/sources/"+strconv.FormatInt(src.ID, 10), nil, &map[string]bool{})
	if code := admin.do("DELETE", "/api/agents/"+id, nil, &map[string]bool{}); code != 200 {
		t.Fatalf("removing a disconnected, unused server: HTTP %d", code)
	}
	admin.do("GET", "/api/agents", nil, &agents)
	if len(agents) != 0 {
		t.Errorf("removed server still listed: %+v", agents)
	}
	var keys struct{ Agents []struct{ Name string } }
	admin.do("GET", "/api/public/keys", nil, &keys)
	if len(keys.Agents) != 1 || keys.Agents[0].Name != "agent:old-box" {
		t.Errorf("the removed server's key is no longer published for verifying its proofs: %+v", keys)
	}
}
