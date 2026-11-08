package drill_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/chunker"
	"github.com/chmuzamil/backupproof/internal/drill"
	"github.com/chmuzamil/backupproof/internal/ops"
	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/snapshot"
	"github.com/chmuzamil/backupproof/internal/source"
)

// fakeCLI answers like a healthy Docker and remembers what it was asked.
func fakeCLI(t *testing.T, calls *[]string) {
	t.Helper()
	t.Cleanup(drill.UseFakeDocker(func(_ context.Context, _ int, args ...string) (string, string, error) {
		*calls = append(*calls, strings.Join(args, " "))
		switch args[0] {
		case "inspect":
			if strings.Contains(args[2], "State") {
				return "true 0 false", "", nil
			}
			return "sha256:imageid", "", nil
		case "exec":
			return "hello", "BPPROBE 200 0.01", nil
		}
		return "", "", nil
	}))
}

// backupAndDrill goes through the same operations an agent does.
func backupAndDrill(t *testing.T, at *source.AppTest, expect string) (*drill.Result, string) {
	t.Helper()
	dir := t.TempDir()
	be, err := backend.NewLocal(filepath.Join(dir, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(context.Background(), be, []byte("test password 123"), chunker.Params{Min: 4 << 10, Avg: 16 << 10, Max: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	key, err := proof.GenerateKey("test-host")
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := proof.OpenFileLedger(filepath.Join(dir, "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	env := &ops.Env{Repo: r, Signer: key, Ledger: ledger, Storage: proof.StorageInfo{Location: "local"}, Log: t.Logf}
	src := filepath.Join(dir, "site")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "index.html"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if at != nil {
		for i := range at.App.Mounts {
			at.App.Mounts[i].From = snapshot.CleanPath(src)
		}
	}
	spec := source.Spec{Name: "site", Kind: "files", Paths: []string{src},
		Drill: source.DrillSpec{ExpectPaths: []string{filepath.Join(src, expect)}, AppTest: at}}
	if _, _, err := ops.Backup(context.Background(), env, spec, nil); err != nil {
		t.Fatal(err)
	}
	// A failing test also returns an error, alongside its signed result.
	res, rec, err := ops.Drill(context.Background(), env, spec, "", dir)
	if res == nil || rec == nil {
		t.Fatal(err)
	}
	stmt, err := proof.ParseStatement(rec.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	return res, string(stmt.Predicate)
}

func appTest() *source.AppTest {
	return &source.AppTest{App: source.AppContainer{Image: "nginx:1.27", Mounts: []source.AppDataMount{{From: "x", To: "/usr/share/nginx/html"}}}, Port: 80}
}

func hasAppCheck(res *drill.Result) bool {
	for _, c := range res.Checks {
		if strings.HasPrefix(c.Name, "app-") {
			return true
		}
	}
	return false
}

func TestAppCheckIsInTheSignedProof(t *testing.T) {
	var calls []string
	fakeCLI(t, &calls)
	res, pred := backupAndDrill(t, appTest(), "index.html")
	if !res.Passed {
		t.Fatalf("checks: %+v", res.Checks)
	}
	if !hasAppCheck(res) {
		t.Fatalf("no app checks: %+v", res.Checks)
	}
	var p struct {
		Checks []proof.Check `json:"checks"`
	}
	if err := json.Unmarshal([]byte(pred), &p); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range p.Checks {
		found = found || c.Name == "app-http-status"
	}
	if !found {
		t.Errorf("app-http-status missing from the proof: %s", pred)
	}
}

// Off by default: an item without the setting asks nothing of Docker.
func TestNoAppTestMeansNoDocker(t *testing.T) {
	var calls []string
	fakeCLI(t, &calls)
	res, _ := backupAndDrill(t, nil, "index.html")
	if !res.Passed || hasAppCheck(res) {
		t.Fatalf("checks: %+v", res.Checks)
	}
	for _, c := range calls {
		t.Errorf("unexpected docker call: %s", c)
	}
}

// The app only starts once the restore itself has passed.
func TestAppTestSkippedWhenRestoreFails(t *testing.T) {
	var calls []string
	fakeCLI(t, &calls)
	res, _ := backupAndDrill(t, appTest(), "not-in-the-backup.txt")
	if res.Passed || hasAppCheck(res) {
		t.Fatalf("checks: %+v", res.Checks)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "run") {
			t.Errorf("started a container after a failed restore: %s", c)
		}
	}
}
