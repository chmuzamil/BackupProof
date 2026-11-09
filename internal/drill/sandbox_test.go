package drill

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/source"
)

// The app test on real containers: a read-only site served through the shared
// loopback, probed from the holder, with a database beside it. It pulls images,
// so it only runs in CI.
func TestAppTestRealContainers(t *testing.T) {
	ctx := context.Background()
	if os.Getenv("CI") == "" || runtime.GOOS != "linux" || !DockerAvailable(ctx) {
		t.Skip("runs in CI on Linux with Docker")
	}
	dir := t.TempDir()
	site := filepath.Join(dir, "docker", "volumes", "site")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(site, "index.html"), []byte("restored page"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The hostile Compose file must make no difference at all.
	compose := filepath.Join(dir, "docker", "compose", "evil")
	if err := os.MkdirAll(compose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(compose, "docker-compose.yml"), []byte("services: {x: {privileged: true, volumes: ['/:/host']}}"), 0o644); err != nil {
		t.Fatal(err)
	}

	real := dockerCmd
	t.Cleanup(func() { dockerCmd = real })
	checkedIsolation := false
	dockerCmd = func(ctx context.Context, maxOut int, args ...string) (string, string, error) {
		// On the first probe, look at the running containers from outside.
		if args[0] == "exec" && !checkedIsolation {
			checkedIsolation = true
			holder := args[1]
			if _, _, err := real(ctx, 0, "exec", holder, "curl", "-q", "--silent", "--max-time", "5", "http://1.1.1.1/"); err == nil {
				t.Error("the sandbox reached the internet")
			}
			app := strings.TrimSuffix(holder, "-net") + "-app"
			out, _, err := real(ctx, 0, "inspect", "--format", "{{.HostConfig.NetworkMode}} {{.HostConfig.Privileged}} {{.HostConfig.CapDrop}} {{len .HostConfig.PortBindings}} {{.HostConfig.ReadonlyRootfs}}", app)
			if err != nil || !strings.Contains(out, "container:") || !strings.Contains(out, "false [ALL] 0") {
				t.Errorf("app container settings: %q %v", out, err)
			}
			if _, _, err := real(ctx, 0, "exec", app, "sh", "-c", "touch /usr/share/nginx/html/x"); err == nil {
				t.Error("restored data was writable")
			}
		}
		return real(ctx, maxOut, args...)
	}

	res := &Result{}
	at := &source.AppTest{
		App:      source.AppContainer{Image: "nginx:1.27-alpine", Mounts: []source.AppDataMount{{From: "docker/volumes/site", To: "/usr/share/nginx/html"}}},
		Database: &source.AppContainer{Image: "mariadb:11", Tmpfs: []string{"/var/lib/mysql"}, Env: []source.AppEnv{{Name: "MARIADB_ALLOW_EMPTY_ROOT_PASSWORD", Value: "1"}}},
		Port:     80, ExpectText: "restored page", MaxResponseMs: 10000, StartTimeoutSecs: 180,
	}
	appChecks(ctx, &runner{res: res, log: t.Logf}, dir, source.Spec{Name: "x", Kind: "docker", Drill: source.DrillSpec{AppTest: at}}, t.TempDir())
	for _, c := range res.Checks {
		if !c.Passed {
			t.Errorf("%s: %s", c.Name, c.Detail)
		}
	}
	if !checkedIsolation {
		t.Error("the isolation checks never ran")
	}
	out, _, err := real(ctx, 0, "ps", "-a", "--filter", "label="+appExpiryLabel, "--format", "{{.Names}}")
	if err != nil || strings.TrimSpace(out) != "" {
		t.Errorf("containers left behind: %q %v", out, err)
	}
}

// Timing out and a crash must both clean up on real containers too.
func TestAppTestRealContainersFailing(t *testing.T) {
	ctx := context.Background()
	if os.Getenv("CI") == "" || runtime.GOOS != "linux" || !DockerAvailable(ctx) {
		t.Skip("runs in CI on Linux with Docker")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "site"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := &Result{}
	at := &source.AppTest{ // nothing listens on this port
		App:  source.AppContainer{Image: "nginx:1.27-alpine", Mounts: []source.AppDataMount{{From: "site", To: "/usr/share/nginx/html"}}},
		Port: 8081, StartTimeoutSecs: 10,
	}
	appChecks(ctx, &runner{res: res, log: t.Logf}, dir, source.Spec{Name: "x", Kind: "files", Drill: source.DrillSpec{AppTest: at}}, t.TempDir())
	if allPassed(res.Checks) {
		t.Errorf("checks: %+v", res.Checks)
	}
	out, _, err := dockerCmd(ctx, 0, "ps", "-a", "--filter", "label="+appExpiryLabel, "--format", "{{.Names}}")
	if err != nil || strings.TrimSpace(out) != "" {
		t.Errorf("containers left behind: %q %v", out, err)
	}
}

// The sandbox drops capabilities the official images could otherwise rely on,
// so each one has to really start and take writes. It pulls the images, so it
// only runs in CI.
func TestSandboxRunsDatabaseImages(t *testing.T) {
	ctx := context.Background()
	if os.Getenv("CI") == "" || runtime.GOOS != "linux" || !DockerAvailable(ctx) {
		t.Skip("runs in CI on Linux with Docker")
	}
	sql := "create database drill; create table drill.t(a int); insert into drill.t values (1); select count(*) from drill.t"
	for _, c := range []struct {
		name  string
		opts  SandboxOptions
		ready []string
		write []string
	}{
		{"postgres", SandboxOptions{Image: "postgres:17", DataTmpfs: "/var/lib/postgresql", Env: map[string]string{"POSTGRES_HOST_AUTH_METHOD": "trust"}},
			[]string{"psql", "-h", "127.0.0.1", "-U", "postgres", "-Atc", "select 1"},
			[]string{"psql", "-h", "127.0.0.1", "-U", "postgres", "-At", "-c", "create table t(a int)", "-c", "insert into t values (1)", "-c", "select count(*) from t"}},
		{"mariadb", SandboxOptions{Image: "mariadb:11", DataTmpfs: "/var/lib/mysql", Env: map[string]string{"MARIADB_ALLOW_EMPTY_ROOT_PASSWORD": "1"}},
			[]string{"mariadb", "-h127.0.0.1", "-uroot", "-e", "SELECT 1"},
			[]string{"mariadb", "-h127.0.0.1", "-uroot", "-N", "-B", "-e", sql}},
		{"mysql", SandboxOptions{Image: "mysql:8.4", DataTmpfs: "/var/lib/mysql", Env: map[string]string{"MYSQL_ALLOW_EMPTY_PASSWORD": "yes"}},
			[]string{"mysql", "-h127.0.0.1", "-uroot", "-e", "SELECT 1"},
			[]string{"mysql", "-h127.0.0.1", "-uroot", "-N", "-B", "-e", sql}},
		{"mongo", SandboxOptions{Image: "mongo:7", DataTmpfs: "/data/db"},
			[]string{"mongosh", "--quiet", "--eval", "db.runCommand({ping:1}).ok"},
			[]string{"mongosh", "--quiet", "--eval", "db.t.insertOne({a:1}); db.t.countDocuments()"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			sb, err := StartSandbox(ctx, c.opts)
			if err != nil {
				t.Fatal(err)
			}
			defer sb.Remove()
			if err := sb.WaitReady(ctx, 3*time.Minute, c.ready...); err != nil {
				logs, _ := docker(ctx, "logs", "--tail", "30", sb.Name)
				t.Fatalf("not ready: %v\n%s", err, logs)
			}
			out, err := sb.Exec(ctx, c.write...)
			if err != nil {
				t.Fatalf("write: %v\n%s", err, out)
			}
			if !strings.HasSuffix(strings.TrimSpace(out), "1") {
				t.Errorf("write returned %q, want a count of 1", out)
			}
			caps, err := docker(ctx, "inspect", "--format", "{{.HostConfig.CapDrop}}", sb.Name)
			if err != nil || !strings.Contains(caps, "ALL") {
				t.Errorf("capabilities not dropped: %q %v", caps, err)
			}
		})
	}
}
