package drill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/source"
)

// fakeDocker stands in for the Docker CLI, so what the app test asks Docker to
// do can be checked on any machine. Real containers: TestAppTestRealContainers.
type fakeDocker struct {
	mu       sync.Mutex
	calls    [][]string
	rmCtxErr []error
	envFiles map[string]string

	running  map[string]string // container-name suffix -> inspect answer
	probe    func() (string, string, error)
	failRun  string // name suffix of the container whose docker run fails
	stderrOn string // text a failing docker run prints
	onState  func() // called on each state inspect
}

func (f *fakeDocker) cmd(ctx context.Context, _ int, args ...string) (string, string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, args)
	f.mu.Unlock()
	switch args[0] {
	case "version", "pull", "ps":
		return "", "", nil
	case "rm":
		f.mu.Lock()
		f.rmCtxErr = append(f.rmCtxErr, ctx.Err())
		f.mu.Unlock()
		return "", "", nil
	case "run":
		name := args[slices.Index(args, "--name")+1]
		if i := slices.Index(args, "--env-file"); i >= 0 {
			b, err := os.ReadFile(args[i+1])
			if err != nil {
				return "", "", err
			}
			f.mu.Lock()
			if f.envFiles == nil {
				f.envFiles = map[string]string{}
			}
			f.envFiles[args[i+1]] = string(b)
			f.mu.Unlock()
		}
		if f.failRun != "" && strings.HasSuffix(name, f.failRun) {
			return "", f.stderrOn, errors.New("exit status 125")
		}
		return "id", "", nil
	case "inspect":
		if strings.Contains(args[2], "State") {
			if f.onState != nil {
				f.onState()
			}
			for suffix, st := range f.running {
				if strings.HasSuffix(args[3], suffix) {
					return st, "", nil
				}
			}
			return "true 0 false", "", nil
		}
		return "sha256:imageid\n", "", nil
	case "exec":
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		if f.probe != nil {
			return f.probe()
		}
		return "<html>wp-submit</html>", "BPPROBE 200 0.012", nil
	}
	return "", "", nil
}

func (f *fakeDocker) find(sub string) [][]string {
	var out [][]string
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c[0] == sub {
			out = append(out, c)
		}
	}
	return out
}

func useFake(t *testing.T, f *fakeDocker) {
	t.Helper()
	old, oldPoll := dockerCmd, appPollEvery
	dockerCmd, appPollEvery = f.cmd, 5*time.Millisecond
	t.Cleanup(func() { dockerCmd, appPollEvery = old, oldPoll })
}

// restoredTree makes what a restored Docker backup looks like, including a
// hostile Compose file that the test must never touch.
func restoredTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for p, body := range map[string]string{
		"docker/volumes/wp/index.php":             "<?php",
		"docker/volumes/wp/sub/file.txt":          "data",
		"docker/volumes/db/ibdata1":               "db",
		"docker/compose/shop/docker-compose.yml":  "services:\n  evil:\n    privileged: true\n    volumes: ['/:/host']\n",
		"docker/compose/shop/.env":                "DB_PASSWORD=compose-secret\n",
		"docker/containers/shop_wordpress_1.json": `{"Config":{"Env":["X=container-secret"]}}`,
	} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func basicAppTest() *source.AppTest {
	return &source.AppTest{
		App:  source.AppContainer{Image: "wordpress:6-apache", Mounts: []source.AppDataMount{{From: "docker/volumes/wp", To: "/var/www/html"}}},
		Port: 80, Path: "/wp-login.php", ExpectText: "wp-submit", MaxResponseMs: 1000, StartTimeoutSecs: 5,
	}
}

func runApp(t *testing.T, ctx context.Context, dir string, at *source.AppTest) (*Result, string) {
	t.Helper()
	res := &Result{}
	parent := t.TempDir()
	appChecks(ctx, &runner{res: res, log: t.Logf}, dir, source.Spec{Name: "x", Kind: "docker", Drill: source.DrillSpec{AppTest: at}}, parent)
	return res, parent
}

func checkMap(res *Result) map[string]bool {
	m := map[string]bool{}
	for _, c := range res.Checks {
		m[c.Name] = c.Passed
	}
	return m
}

func assertCleanedUp(t *testing.T, f *fakeDocker, parent string, want int) {
	t.Helper()
	removed := f.find("rm")
	if len(removed) != want {
		t.Fatalf("removed %d containers, want %d: %v", len(removed), want, removed)
	}
	for _, c := range removed {
		if !slices.Contains(c, "-f") || !slices.Contains(c, "-v") {
			t.Errorf("not a forced removal with volumes: %v", c)
		}
	}
	for _, err := range f.rmCtxErr {
		if err != nil {
			t.Errorf("cleanup used a dead context: %v", err)
		}
	}
	left, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("throwaway folder left behind: %v", left)
	}
}

func TestAppTestPasses(t *testing.T) {
	f := &fakeDocker{}
	useFake(t, f)
	at := basicAppTest()
	at.Template, at.TemplateVersion = "wordpress", 1
	dir := restoredTree(t)
	res, parent := runApp(t, context.Background(), dir, at)
	want := []string{"app-sandbox-start", "app-running", "app-http-status", "app-body-text", "app-response-time"}
	var got []string
	for _, c := range res.Checks {
		got = append(got, c.Name)
		if !c.Passed {
			t.Errorf("%s failed: %s", c.Name, c.Detail)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("checks = %v, want %v", got, want)
	}
	if !strings.Contains(res.Checks[0].Detail, "template wordpress v1") {
		t.Errorf("template not recorded: %q", res.Checks[0].Detail)
	}
	assertCleanedUp(t, f, parent, 2)
}

// The whole isolation promise, as Docker is asked for it.
func TestAppTestSandboxOptions(t *testing.T) {
	f := &fakeDocker{}
	useFake(t, f)
	at := basicAppTest()
	at.App.Tmpfs = []string{"/run/app"}
	at.App.Mounts = append(at.App.Mounts, source.AppDataMount{From: "docker/volumes/wp/sub", To: "/cache", Writable: true})
	at.Database = &source.AppContainer{Image: "mariadb:11", Mounts: []source.AppDataMount{{From: "docker/volumes/db", To: "/var/lib/mysql", Writable: true}}}
	dir := restoredTree(t)
	res, _ := runApp(t, context.Background(), dir, at)
	if !allPassed(res.Checks) {
		t.Fatalf("checks: %+v", res.Checks)
	}
	if !checkMap(res)["app-database-running"] {
		t.Error("database not reported")
	}
	runs := f.find("run")
	if len(runs) != 3 {
		t.Fatalf("%d containers started, want holder, database and app", len(runs))
	}
	holder := runs[0][slices.Index(runs[0], "--name")+1]
	if !strings.HasSuffix(holder, "-net") {
		t.Fatalf("the holder must start first, got %v", runs[0])
	}
	if n := runs[0][slices.Index(runs[0], "--network")+1]; n != "none" {
		t.Errorf("holder network = %s", n)
	}
	for i, r := range runs {
		for _, need := range []string{"--cap-drop", "--pull", "--memory", "--cpus", "--pids-limit", "--security-opt", "--log-driver"} {
			if !slices.Contains(r, need) {
				t.Errorf("run %d is missing %s: %v", i, need, r)
			}
		}
		if !slices.Contains(r, "no-new-privileges") || r[slices.Index(r, "--cap-drop")+1] != "ALL" {
			t.Errorf("run %d is not locked down: %v", i, r)
		}
		for _, banned := range []string{"-p", "--publish", "--publish-all", "-P", "--privileged", "--pid", "--ipc", "--userns", "--volumes-from", "--device", "-v", "--entrypoint=sh"} {
			if slices.Contains(r, banned) {
				t.Errorf("run %d has %s: %v", i, banned, r)
			}
		}
		if i > 0 {
			if n := r[slices.Index(r, "--network")+1]; n != "container:"+holder {
				t.Errorf("run %d network = %s, want the holder's", i, n)
			}
			for j, a := range r {
				if a == "--cap-add" && !slices.Contains(sandboxCaps, r[j+1]) {
					t.Errorf("unexpected capability %s", r[j+1])
				}
			}
		}
		joined := strings.Join(r, " ")
		if strings.Contains(joined, "compose") || strings.Contains(joined, "containers/") {
			t.Errorf("run %d touches restored settings: %v", i, r)
		}
	}
	// Read-only exactly where it should be, and writable paths are copies.
	mounts := map[string]string{}
	for _, r := range runs {
		for j, a := range r {
			if a == "--mount" {
				parts := strings.Split(r[j+1], ",")
				mounts[strings.TrimPrefix(parts[2], "dst=")] = r[j+1]
			}
		}
	}
	if m := mounts["/var/www/html"]; !strings.HasSuffix(m, ",readonly") || !strings.Contains(m, filepath.Join(dir, "docker", "volumes", "wp")) {
		t.Errorf("restored data not read-only: %q", m)
	}
	for _, to := range []string{"/cache", "/var/lib/mysql"} {
		m := mounts[to]
		if strings.HasSuffix(m, ",readonly") || strings.Contains(m, dir) {
			t.Errorf("writable path %s must be a throwaway copy: %q", to, m)
		}
	}
	// Probes run from inside the holder only.
	for _, e := range f.find("exec") {
		if e[1] != holder || e[2] != "curl" || !strings.HasPrefix(e[len(e)-1], "http://127.0.0.1:80") {
			t.Errorf("probe: %v", e)
		}
	}
}

func TestAppTestFailures(t *testing.T) {
	for _, c := range []struct {
		name   string
		f      *fakeDocker
		tweak  func(*source.AppTest)
		failed string
		absent []string
	}{
		{name: "wrong status", f: &fakeDocker{probe: func() (string, string, error) { return "oops", "BPPROBE 500 0.01", nil }},
			failed: "app-http-status", absent: []string{"app-body-text"}},
		{name: "text missing", f: &fakeDocker{probe: func() (string, string, error) { return "welcome", "BPPROBE 200 0.01", nil }},
			failed: "app-body-text"},
		{name: "too slow", f: &fakeDocker{probe: func() (string, string, error) { return "wp-submit", "BPPROBE 200 9.5", nil }},
			failed: "app-response-time"},
		{name: "app exits", f: &fakeDocker{running: map[string]string{"-app": "false 1 false"}},
			failed: "app-running", absent: []string{"app-http-status"}},
		{name: "out of memory", f: &fakeDocker{running: map[string]string{"-app": "false 137 true"}},
			failed: "app-running"},
		{name: "cannot start", f: &fakeDocker{failRun: "-app", stderrOn: "no such image"},
			failed: "app-sandbox-start", absent: []string{"app-running"}},
		{name: "database stops", f: &fakeDocker{running: map[string]string{"-db": "false 1 false"}},
			tweak: func(a *source.AppTest) {
				a.Database = &source.AppContainer{Image: "mariadb:11", Mounts: []source.AppDataMount{{From: "docker/volumes/db", To: "/var/lib/mysql", Writable: true}}}
			}, failed: "app-database-running"},
		{name: "missing data", failed: "app-sandbox-start", tweak: func(a *source.AppTest) { a.App.Mounts[0].From = "docker/volumes/nothing" }},
		{name: "invalid config at run time", failed: "app-sandbox-start", tweak: func(a *source.AppTest) { a.Port = 0 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := c.f
			if f == nil {
				f = &fakeDocker{}
			}
			useFake(t, f)
			at := basicAppTest()
			if c.tweak != nil {
				c.tweak(at)
			}
			res, parent := runApp(t, context.Background(), restoredTree(t), at)
			m := checkMap(res)
			if passed, ok := m[c.failed]; !ok || passed {
				t.Errorf("%s: want a failed check, got %+v", c.failed, res.Checks)
			}
			for _, a := range c.absent {
				if _, ok := m[a]; ok {
					t.Errorf("%s should not have run", a)
				}
			}
			if allPassed(res.Checks) {
				t.Error("must not pass")
			}
			n := len(f.find("rm"))
			if n < 2 {
				t.Errorf("only %d containers removed", n)
			}
			if left, _ := os.ReadDir(parent); len(left) != 0 {
				t.Errorf("left behind: %v", left)
			}
		})
	}
}

func TestAppTestTimesOut(t *testing.T) {
	f := &fakeDocker{probe: func() (string, string, error) {
		return "", "curl: (7) refused\nBPPROBE 000 0", errors.New("exit status 7")
	}}
	useFake(t, f)
	at := basicAppTest()
	at.StartTimeoutSecs = 1
	began := time.Now()
	res, parent := runApp(t, context.Background(), restoredTree(t), at)
	if d := time.Since(began); d < time.Second || d > 20*time.Second {
		t.Errorf("took %s, want about the time limit", d)
	}
	m := checkMap(res)
	if !m["app-running"] || m["app-http-status"] {
		t.Errorf("checks: %+v", res.Checks)
	}
	assertCleanedUp(t, f, parent, 2)
}

func TestAppTestCancelledCleansUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeDocker{
		onState: cancel, // cancelled while waiting for the app
		probe:   func() (string, string, error) { return "", "", errors.New("killed") },
	}
	useFake(t, f)
	res, parent := runApp(t, ctx, restoredTree(t), basicAppTest())
	if allPassed(res.Checks) {
		t.Error("a cancelled test must not pass")
	}
	assertCleanedUp(t, f, parent, 2)
}

func TestAppTestDockerMissing(t *testing.T) {
	f := &fakeDocker{}
	old, oldPoll := dockerCmd, appPollEvery
	dockerCmd = func(ctx context.Context, maxOut int, args ...string) (string, string, error) {
		if args[0] == "version" {
			return "", "cannot connect", errors.New("exit status 1")
		}
		return f.cmd(ctx, maxOut, args...)
	}
	t.Cleanup(func() { dockerCmd, appPollEvery = old, oldPoll })
	res, _ := runApp(t, context.Background(), restoredTree(t), basicAppTest())
	if m := checkMap(res); m["app-sandbox-start"] || !strings.Contains(res.Checks[0].Detail, "needs Docker") {
		t.Errorf("checks: %+v", res.Checks)
	}
}

func TestAppTestSecretsStayOut(t *testing.T) {
	const secret = "s3cret-value-xyz"
	t.Setenv("BP_APPTEST_DB", secret)
	f := &fakeDocker{failRun: "-app", stderrOn: "boom " + secret}
	useFake(t, f)
	at := basicAppTest()
	at.App.Env = []source.AppEnv{{Name: "WORDPRESS_DB_PASSWORD", SecretEnv: "BP_APPTEST_DB"}, {Name: "WORDPRESS_DB_HOST", Value: "127.0.0.1"}}
	res, parent := runApp(t, context.Background(), restoredTree(t), at)
	for _, c := range f.find("run") {
		if strings.Contains(strings.Join(c, " "), secret) {
			t.Errorf("secret on a command line: %v", c)
		}
	}
	found := false
	for path, body := range f.envFiles {
		found = found || strings.Contains(body, "WORDPRESS_DB_PASSWORD="+secret)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("settings file not removed: %v", err)
		}
	}
	if !found {
		t.Errorf("the secret didn't reach the container's settings file: %v", f.envFiles)
	}
	out, _ := json.Marshal(res.Checks)
	if strings.Contains(string(out), secret) {
		t.Errorf("secret in the proof: %s", out)
	}
	assertCleanedUp(t, f, parent, 2)
}

func TestAppTestMissingSecret(t *testing.T) {
	f := &fakeDocker{}
	useFake(t, f)
	at := basicAppTest()
	at.App.Env = []source.AppEnv{{Name: "WORDPRESS_DB_PASSWORD", SecretEnv: "BP_APPTEST_NOT_SET"}}
	res, _ := runApp(t, context.Background(), restoredTree(t), at)
	if checkMap(res)["app-sandbox-start"] || !strings.Contains(res.Checks[0].Detail, "BP_APPTEST_NOT_SET") {
		t.Errorf("checks: %+v", res.Checks)
	}
	if len(f.find("run")) != 0 {
		t.Error("nothing should start without its secret")
	}
}

// A response body or anything else the restored app says never reaches a proof.
func TestAppTestBodyNotRecorded(t *testing.T) {
	f := &fakeDocker{probe: func() (string, string, error) { return "customer-record-123", "BPPROBE 200 0.01", nil }}
	useFake(t, f)
	res, _ := runApp(t, context.Background(), restoredTree(t), basicAppTest())
	out, _ := json.Marshal(res.Checks)
	if strings.Contains(string(out), "customer-record-123") {
		t.Errorf("response body in the proof: %s", out)
	}
}

// Writable copies must never let the restored tree change.
func TestAppTestRestoredTreeUntouched(t *testing.T) {
	f := &fakeDocker{}
	useFake(t, f)
	dir := restoredTree(t)
	before := treeListing(t, dir)
	at := basicAppTest()
	at.App.Mounts[0].Writable = true
	runApp(t, context.Background(), dir, at)
	if after := treeListing(t, dir); !reflect.DeepEqual(before, after) {
		t.Errorf("restored tree changed:\n%v\n%v", before, after)
	}
}

func treeListing(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[p] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

// A backup holding "data -> /" must not put the server's disk in the container.
func TestAppTestRefusesSymlinkMounts(t *testing.T) {
	f := &fakeDocker{}
	useFake(t, f)
	dir := restoredTree(t)
	symlinkOrSkip(t, string(filepath.Separator), filepath.Join(dir, "docker", "volumes", "evil"))
	at := basicAppTest()
	at.App.Mounts[0].From = "docker/volumes/evil"
	res, _ := runApp(t, context.Background(), dir, at)
	if m := checkMap(res); m["app-sandbox-start"] || !strings.Contains(res.Checks[0].Detail, "symbolic link") {
		t.Errorf("checks: %+v", res.Checks)
	}
	if len(f.find("run")) != 0 {
		t.Error("nothing should start")
	}
}

func TestCopyRestoredKeepsLinksWithoutFollowing(t *testing.T) {
	src := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "host-secret"), []byte("host"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "real"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, outside, filepath.Join(src, "link"))
	dst := filepath.Join(t.TempDir(), "copy")
	if err := copyRestored(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "real")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(dst, "link"))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link not kept as a link: %v %v", fi, err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "link", "host-secret")); err == nil && runtime.GOOS != "windows" {
		if target, _ := os.Readlink(filepath.Join(dst, "link")); target != outside {
			t.Error("link changed")
		}
	}
	entries, _ := os.ReadDir(dst)
	if len(entries) != 2 {
		t.Errorf("copy has %d entries, want 2", len(entries))
	}
}

func TestParseProbe(t *testing.T) {
	cases := []struct {
		in     string
		status int
		ms     int64
		ok     bool
	}{
		{"BPPROBE 200 0.250", 200, 250, true},
		{"curl: (52) Empty reply\nBPPROBE 000 0.1", 0, 100, true},
		{"", 0, 0, false},
		{"BPPROBE nonsense", 0, 0, false},
	}
	for _, c := range cases {
		status, ms, ok := parseProbe(c.in)
		if status != c.status || ms != c.ms || ok != c.ok {
			t.Errorf("parseProbe(%q) = %d %d %v", c.in, status, ms, ok)
		}
	}
}

func TestSweepRemovesOnlyExpiredContainers(t *testing.T) {
	var removed []string
	old := dockerCmd
	t.Cleanup(func() { dockerCmd = old })
	dockerCmd = func(_ context.Context, _ int, args ...string) (string, string, error) {
		switch args[0] {
		case "ps":
			return fmt.Sprintf("expired %d\nfresh %d\ngarbage\n", time.Now().Add(-time.Hour).Unix(), time.Now().Add(time.Hour).Unix()), "", nil
		case "rm":
			removed = append(removed, args[len(args)-1])
		}
		return "", "", nil
	}
	(&appRun{}).sweepLeftovers(context.Background())
	if !slices.Equal(removed, []string{"expired"}) {
		t.Errorf("removed %v", removed)
	}
}
