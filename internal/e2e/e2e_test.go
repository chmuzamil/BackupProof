// Package e2e exercises the full fleet path: server, enrollment, outbound
// agent, backup, restore drill, attestation, ledger and evidence export.
package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chmuzamil/backupproof/internal/agent"
	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/server"
	_ "modernc.org/sqlite"
)

type api struct {
	t    *testing.T
	base string
	c    *http.Client
	csrf string
}

func (a *api) do(method, path string, in, out any) int {
	a.t.Helper()
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, a.base+path, body)
	req.Header.Set("Content-Type", "application/json")
	if a.csrf != "" {
		req.Header.Set("X-CSRF-Token", a.csrf)
	}
	resp, err := a.c.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			a.t.Fatalf("%s %s: %v: %s", method, path, err, raw)
		}
	}
	if resp.StatusCode >= 300 && out != nil {
		a.t.Fatalf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, raw)
	}
	return resp.StatusCode
}

func makeDB(t *testing.T, path string) {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Exec("CREATE TABLE invoices(id INTEGER PRIMARY KEY, amount REAL)")
	for i := 0; i < 250; i++ {
		db.Exec("INSERT INTO invoices(amount) VALUES(?)", i)
	}
}

func TestFleetBackupDrillEvidence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	srv, err := server.New(server.Config{DataDir: filepath.Join(dir, "server")})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	a := &api{t: t, base: ts.URL, c: &http.Client{Jar: jar}}

	// Unauthenticated access is refused; first-run setup creates the admin.
	if code := a.do("GET", "/api/dashboard", nil, nil); code != 401 {
		t.Fatalf("dashboard without session: %d", code)
	}
	var setup struct{ CSRF string }
	a.do("POST", "/api/setup", map[string]string{"username": "admin", "password": "a-long-admin-password"}, &setup)
	a.csrf = setup.CSRF
	if code := a.do("POST", "/api/setup", map[string]string{"username": "evil", "password": "another-long-password"}, nil); code != 409 {
		t.Fatalf("second setup must be refused, got %d", code)
	}
	// CSRF is enforced on mutations.
	saved := a.csrf
	a.csrf = "wrong"
	if code := a.do("POST", "/api/agents/enroll-token", nil, nil); code != 403 {
		t.Fatalf("missing CSRF must be refused, got %d", code)
	}
	a.csrf = saved
	// Keep the test offline: no RFC 3161 timestamp authorities.
	a.do("PUT", "/api/settings/tsa", map[string]any{"urls": []string{}}, &map[string]bool{})

	var tok struct{ Token, Command string }
	a.do("POST", "/api/agents/enroll-token", nil, &tok)
	agentDir := filepath.Join(dir, "agent")
	if _, err := agent.Enroll(ctx, ts.URL, tok.Token, "db-host", agentDir); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Enroll(ctx, ts.URL, tok.Token, "replay", filepath.Join(dir, "agent2")); err == nil {
		t.Fatal("enrollment token must be single-use")
	}
	ag, err := agent.New(agentDir)
	if err != nil {
		t.Fatal(err)
	}

	var repo struct{ ID int64 }
	a.do("POST", "/api/repositories", map[string]any{
		"name": "local", "backend": map[string]any{"type": "local", "path": filepath.Join(dir, "repo")},
		"password": "repository-password-123",
	}, &repo)

	dbPath := filepath.Join(dir, "billing.db")
	makeDB(t, dbPath)
	var agents []server.Agent
	a.do("GET", "/api/agents", nil, &agents)
	var src struct{ ID int64 }
	a.do("POST", "/api/sources", map[string]any{
		"name": "billing", "agentId": agents[0].ID, "repoId": repo.ID,
		"spec":       map[string]any{"kind": "sqlite", "paths": []string{dbPath}, "drill": map[string]any{"assertions": []map[string]string{{"name": "invoices present", "sql": "SELECT count(*) = 250 FROM invoices"}}}},
		"backupCron": "@daily", "drillCron": "0 4 * * 0", "retention": map[string]int{"keepLast": 5},
	}, &src)

	run := func(kind string) {
		var r struct{ JobID int64 }
		a.do("POST", fmt.Sprintf("/api/sources/%d/run", src.ID), map[string]string{"kind": kind}, &r)
		ran, err := ag.PollOnce(ctx)
		if err != nil || !ran {
			t.Fatalf("%s: agent did not run job: %v", kind, err)
		}
		var j server.Job
		a.do("GET", fmt.Sprintf("/api/jobs/%d", r.JobID), nil, &j)
		if j.State != "succeeded" {
			t.Fatalf("%s job %s: %s\n%s", kind, j.State, j.Error, j.Log)
		}
	}
	var dash struct {
		Sources []server.SourceStatus
	}
	a.do("GET", "/api/dashboard", nil, &dash)
	if dash.Sources[0].Status != "unproven" {
		t.Fatalf("new source must be unproven, got %s", dash.Sources[0].Status)
	}
	run("backup")
	// The first backup automatically queues a restore test.
	if ran, err := ag.PollOnce(ctx); err != nil || !ran {
		t.Fatalf("automatic first restore test did not run: %v", err)
	}
	var jobs []server.Job
	a.do("GET", "/api/jobs", nil, &jobs)
	if jobs[0].Kind != "drill" || jobs[0].Trigger != "first-backup" || jobs[0].State != "succeeded" {
		t.Fatalf("expected auto drill, got %+v", jobs[0])
	}
	run("check")

	a.do("GET", "/api/dashboard", nil, &dash)
	if st := dash.Sources[0]; st.Status != "proven" {
		t.Fatalf("after passing drill status=%s (%s)", st.Status, st.Reason)
	}

	// Offline verification of a single drill proof with independently pinned keys.
	var keys struct {
		Server proof.PublicKey
		Agents []proof.PublicKey
	}
	a.do("GET", "/api/public/keys", nil, &keys)
	trusted := append([]proof.PublicKey{keys.Server}, keys.Agents...)
	var proofs []server.ProofRow
	a.do("GET", "/api/proofs", nil, &proofs)
	if len(proofs) != 2 {
		t.Fatalf("expected 2 proofs, got %d", len(proofs))
	}
	var drillID int64
	for _, p := range proofs {
		if p.Kind == "drill" {
			drillID = p.ID
		}
	}
	var bundle proof.Bundle
	a.do("GET", fmt.Sprintf("/api/proofs/%d/bundle", drillID), nil, &bundle)
	rep := proof.VerifyBundle(&bundle, proof.VerifyOptions{TrustedKeys: trusted})
	if !rep.Valid || rep.Passed == nil || !*rep.Passed {
		t.Fatalf("bundle invalid: %+v", rep)
	}

	var pack server.EvidencePack
	a.do("GET", "/api/evidence", nil, &pack)
	if errs := server.VerifyPack(&pack, trusted); len(errs) > 0 {
		t.Fatalf("evidence pack: %v", errs)
	}
	if len(pack.Summary) != 1 || pack.Summary[0].DrillsPassed != 1 {
		t.Fatalf("summary: %+v", pack.Summary)
	}
	resp, _ := a.c.Get(ts.URL + "/api/evidence/report")
	html, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(html), "Recoverability Evidence Report") || !strings.Contains(string(html), "billing") {
		t.Fatal("report missing content")
	}

	var lv struct {
		OK      bool
		Entries int
	}
	a.do("GET", "/api/ledger/verify", nil, &lv)
	if !lv.OK || lv.Entries < 5 {
		t.Fatalf("ledger verify: %+v", lv)
	}

	// The ledger table is append-only even for someone with database access.
	db, _ := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "server", "backupproof.db")))
	if _, err := db.Exec("UPDATE ledger SET subject='x' WHERE seq=1"); err == nil {
		t.Fatal("ledger UPDATE must be rejected")
	}
	if _, err := db.Exec("DELETE FROM ledger WHERE seq=1"); err == nil {
		t.Fatal("ledger DELETE must be rejected")
	}
	db.Close()

	// Revoked agents are locked out.
	a.do("POST", fmt.Sprintf("/api/agents/%d/revoke", agents[0].ID), nil, &map[string]bool{})
	if _, err := ag.PollOnce(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("revoked agent must be refused, got %v", err)
	}
}

func TestOnboardingEndpoints(t *testing.T) {
	dir := t.TempDir()
	srv, err := server.New(server.Config{DataDir: filepath.Join(dir, "server"), PublicURL: "https://backup.example.com"})
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

	// Storage test: a new local folder works and is reported in plain words.
	var st struct {
		OK       bool
		Message  string
		Existing bool
	}
	a.do("POST", "/api/repositories/test", map[string]any{"backend": map[string]any{"type": "local", "path": filepath.Join(dir, "store")}}, &st)
	if !st.OK || st.Existing || !strings.Contains(st.Message, "Connected") {
		t.Fatalf("storage test: %+v", st)
	}

	// Enrollment returns one-line install commands for every OS.
	var tok struct {
		Token    string
		Commands map[string]string
	}
	a.do("POST", "/api/agents/enroll-token", nil, &tok)
	if !strings.Contains(tok.Commands["linux"], "https://backup.example.com/install.sh") || !strings.Contains(tok.Commands["windows"], tok.Token) {
		t.Fatalf("commands: %+v", tok.Commands)
	}
	resp, _ := http.Get(ts.URL + "/install.sh")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `SERVER="https://backup.example.com"`) {
		t.Fatal("install.sh does not point at the public URL")
	}
	resp, _ = http.Get(ts.URL + "/download/../../secret.key")
	if resp.StatusCode == 200 {
		t.Fatal("download must not serve arbitrary files")
	}
	resp.Body.Close()

	// Folder browser lists roots.
	var br struct{ Entries []struct{ Path string } }
	a.do("GET", "/api/browse", nil, &br)
	if len(br.Entries) == 0 {
		t.Fatal("browse returned no roots")
	}
}
