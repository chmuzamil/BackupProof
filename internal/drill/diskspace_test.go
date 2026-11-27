package drill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/chunker"
	"github.com/chmuzamil/backupproof/internal/engine"
	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/snapshot"
	"github.com/chmuzamil/backupproof/internal/source"
)

func fileSnapshot(t *testing.T) (*repo.Repo, snapshot.WithID) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	be, _ := backend.NewLocal(filepath.Join(dir, "repo"))
	r, err := repo.Init(ctx, be, []byte("correct horse battery"), chunker.Params{Min: 4 << 10, Avg: 16 << 10, Max: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src")
	os.MkdirAll(src, 0o755)
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte(strings.Repeat("restore me ", 10000)), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := engine.NewBuilder(ctx, r, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.AddPath(ctx, src); err != nil {
		t.Fatal(err)
	}
	s, err := b.Commit(ctx, &snapshot.Snapshot{Source: snapshot.Source{Name: "files", Kind: "files"}})
	if err != nil {
		t.Fatal(err)
	}
	return r, s
}

func TestRestoreTestChecksDiskSpaceFirst(t *testing.T) {
	r, s := fileSnapshot(t)
	spec := source.Spec{Name: "files", Kind: "files", Paths: []string{"/x"}}
	old := drillFreeBytes
	t.Cleanup(func() { drillFreeBytes = old })

	drillFreeBytes = func(string) (uint64, bool) { return 100 << 20, true } // 100 MB free
	res, err := Run(context.Background(), r, s, spec, Options{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed || len(res.Checks) != 1 || res.Checks[0].Name != "disk-space" {
		t.Fatalf("with too little space: passed=%v checks=%+v", res.Passed, res.Checks)
	}
	if d := res.Checks[0].Detail; !strings.Contains(d, "not a fault in the backup") || !strings.Contains(d, "100.0 MB is free") {
		t.Errorf("detail: %s", d)
	}

	drillFreeBytes = func(string) (uint64, bool) { return 50 << 30, true }
	res, err = Run(context.Background(), r, s, spec, Options{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed || res.Checks[0].Name != "disk-space" || !res.Checks[0].Passed || len(res.Checks) < 3 {
		t.Errorf("with enough space: passed=%v checks=%+v", res.Passed, res.Checks)
	}

	// Where free space can't be read, the test runs as before, without the check.
	drillFreeBytes = func(string) (uint64, bool) { return 0, false }
	res, _ = Run(context.Background(), r, s, spec, Options{WorkDir: t.TempDir()})
	if !res.Passed || res.Checks[0].Name == "disk-space" {
		t.Errorf("without a reading: passed=%v checks=%+v", res.Passed, res.Checks)
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[uint64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KB", 100 << 20: "100.0 MB", 3 << 30: "3.0 GB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// Only a test that failed for lack of memory or disk on its server gets the
// "nothing was found wrong" treatment.
func TestLimitOnlyFailure(t *testing.T) {
	oom := proof.Check{Name: "pg_restore", Detail: "docker exec: exit status 1. The restore-test container ran out of memory (limit 4g): a limit of the restore test, " + NotTheBackup + ". Raise the item's restore-test memory"}
	disk := proof.Check{Name: "disk-space", Detail: "the restore needs about 3.0 GB free on this server and 1.0 GB is free: a limit of this server, " + NotTheBackup + "."}
	for _, c := range []struct {
		name   string
		checks []proof.Check
		want   bool
	}{
		{"out of memory", []proof.Check{{Name: "restore-from-storage", Passed: true}, oom}, true},
		{"out of disk", []proof.Check{disk}, true},
		{"bad dump", []proof.Check{{Name: "pg_restore", Detail: "pg_restore: error: invalid input"}}, false},
		{"out of memory and a real failure", []proof.Check{oom, {Name: "content-root"}}, false},
		{"all passed", []proof.Check{{Name: "disk-space", Passed: true}}, false},
		{"none", nil, false},
	} {
		if got := LimitOnlyFailure(c.checks); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
