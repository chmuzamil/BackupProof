package drill

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

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
