package e2e

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chmuzamil/backupproof/internal/agent"
	"github.com/chmuzamil/backupproof/internal/server"
	"github.com/chmuzamil/backupproof/internal/source"
)

func dockerOut(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestDockerBackupAndRestore backs up a Docker app's volume while its
// container runs, then puts the volume back exactly (contents and owners).
// It needs Docker and is skipped without it.
func TestDockerBackupAndRestore(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not running")
	}
	ctx := context.Background()
	id := fmt.Sprintf("bptest%06d", rand.IntN(1e6))
	vol, ctr := id+"-data", id+"-app"
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", ctr).Run()
		exec.Command("docker", "volume", "rm", "-f", vol).Run()
	})
	dockerOut(t, "volume", "create", vol)
	dockerOut(t, "run", "--rm", "-v", vol+":/v", source.HelperImage, "sh", "-c",
		"echo 'hello' > /v/a.txt && mkdir /v/db && echo 'rows' > /v/db/table && chown -R 999:999 /v/db && chmod 700 /v/db")
	dockerOut(t, "run", "-d", "--name", ctr, "--label", "com.docker.compose.project="+id, "-v", vol+":/data", source.HelperImage, "sleep", "600")

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
	var repo struct{ ID int64 }
	a.do("POST", "/api/repositories", map[string]any{"name": "disk", "backend": map[string]any{"type": "local", "path": filepath.Join(dir, "repo")}, "password": "repository-password-123"}, &repo)
	var src struct{ ID int64 }
	a.do("POST", "/api/sources", map[string]any{"name": "app", "agentId": agents[0].ID, "repoId": repo.ID,
		"spec":       map[string]any{"kind": "docker", "docker": map[string]any{"project": id, "stop": true}},
		"backupCron": "@daily", "drillCron": "0 4 * * 0"}, &src)

	runJob := func(path string, body any) server.Job {
		t.Helper()
		var r struct{ JobID int64 }
		a.do("POST", path, body, &r)
		if ran, err := ag.PollOnce(ctx); err != nil || !ran {
			t.Fatalf("job did not run: %v", err)
		}
		var j server.Job
		a.do("GET", fmt.Sprintf("/api/jobs/%d", r.JobID), nil, &j)
		return j
	}
	if j := runJob(fmt.Sprintf("/api/sources/%d/run", src.ID), map[string]string{"kind": "backup"}); j.State != "succeeded" {
		t.Fatalf("backup: %s\n%s", j.Error, j.Log)
	}
	if running := dockerOut(t, "inspect", "-f", "{{.State.Running}}", ctr); running != "true" {
		t.Fatal("the container was not started again after the backup")
	}
	if ran, err := ag.PollOnce(ctx); err != nil || !ran { // first restore test
		t.Fatalf("restore test did not run: %v", err)
	}
	var st struct{ Status string }
	a.do("GET", fmt.Sprintf("/api/sources/%d", src.ID), nil, &st)
	if st.Status != "proven" {
		t.Fatalf("after the restore test the app is %q, want proven", st.Status)
	}

	// Damage the volume, then put it back.
	dockerOut(t, "exec", ctr, "sh", "-c", "echo broken > /data/a.txt && rm -rf /data/db && echo junk > /data/new.txt")
	var pts struct{ Points []struct{ SnapshotID string } }
	a.do("GET", fmt.Sprintf("/api/sources/%d/restore-points", src.ID), nil, &pts)
	if j := runJob(fmt.Sprintf("/api/sources/%d/restore", src.ID), map[string]any{"snapshotId": pts.Points[0].SnapshotID, "paths": []string{"docker/volumes/" + vol}}); j.State != "succeeded" {
		t.Fatalf("restore: %s\n%s", j.Error, j.Log)
	}
	got := dockerOut(t, "run", "--rm", "-v", vol+":/v", source.HelperImage, "sh", "-c",
		"cat /v/a.txt; cat /v/db/table; stat -c '%u:%g %a' /v/db /v/db/table; ls /v")
	for _, want := range []string{"hello", "rows", "999:999 700", "999:999"} {
		if !strings.Contains(got, want) {
			t.Errorf("restored volume is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "new.txt") || strings.Contains(got, "broken") {
		t.Errorf("restored volume still has later changes:\n%s", got)
	}
	if running := dockerOut(t, "inspect", "-f", "{{.State.Running}}", ctr); running != "true" {
		t.Error("the container was not started again after the restore")
	}
	// Part of a volume can't be put back in place.
	if j := runJob(fmt.Sprintf("/api/sources/%d/restore", src.ID), map[string]any{"snapshotId": pts.Points[0].SnapshotID, "paths": []string{"docker/volumes/" + vol + "/db"}}); j.State != "failed" {
		t.Error("restoring part of a volume in place should be refused")
	}
}
