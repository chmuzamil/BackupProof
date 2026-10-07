package ops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chmuzamil/backupproof/internal/engine"
	"github.com/chmuzamil/backupproof/internal/importer"
	"github.com/chmuzamil/backupproof/internal/snapshot"
	"github.com/chmuzamil/backupproof/internal/source"
)

// TestImportResticWithEnvFiles mirrors a typical production setup: a nightly
// script runs restic with `set -a; . /etc/restic/env` and the password in
// /etc/restic/password. BackupProof reuses those files in place.
func TestImportResticWithEnvFiles(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not installed")
	}
	ctx := context.Background()
	env, dir, _ := setup(t)

	repoDir := filepath.Join(dir, "restic-repo")
	pwFile := filepath.Join(dir, "password")
	envFile := filepath.Join(dir, "env")
	os.WriteFile(pwFile, []byte("restic-secret\n"), 0o600)
	os.WriteFile(envFile, []byte("# restic settings\nexport RESTIC_REPOSITORY=\""+filepath.ToSlash(repoDir)+"\"\nRESTIC_CACHE_DIR="+filepath.ToSlash(filepath.Join(dir, "cache"))+"\n"), 0o600)

	data := filepath.Join(dir, "var", "backups", "founder-os", "db")
	os.MkdirAll(data, 0o755)
	os.WriteFile(filepath.Join(data, "local-globals.sql"), []byte("CREATE ROLE app;"), 0o644)
	os.WriteFile(filepath.Join(data, "local-infra_panel.dump"), []byte("PGDMP\x01\x0e\x00fake custom-format dump"), 0o644)
	site := filepath.Join(dir, "var", "www", "panel")
	os.MkdirAll(site, 0o755)
	os.WriteFile(filepath.Join(site, "index.php"), []byte("<?php echo 'panel';"), 0o644)

	restic := func(args ...string) {
		cmd := exec.Command("restic", args...)
		cmd.Env = append(os.Environ(), "RESTIC_REPOSITORY="+repoDir, "RESTIC_PASSWORD_FILE="+pwFile, "RESTIC_CACHE_DIR="+filepath.Join(dir, "cache"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("restic %v: %v\n%s", args, err, out)
		}
	}
	restic("init")
	restic("backup", "--tag", "nightly", filepath.Join(dir, "var"))
	os.WriteFile(filepath.Join(site, "index.php"), []byte("<?php echo 'panel v2';"), 0o644)
	restic("backup", "--tag", "nightly", filepath.Join(dir, "var"))

	spec := source.Spec{Name: "luxvps-restic", Kind: "import", Import: &importer.Spec{
		Format: "restic", ResticEnvFile: envFile, ResticPasswordFile: pwFile,
	}}
	res, err := Import(ctx, env, spec)
	if err != nil {
		t.Fatalf("%v %+v", err, res)
	}
	if res.Found != 2 || res.Imported != 2 {
		t.Fatalf("expected both restic snapshots converted: %+v", res)
	}
	if res, _ := Import(ctx, env, spec); res.Imported != 0 || res.Skipped != 2 {
		t.Fatalf("re-run must only pick up new snapshots: %+v", res)
	}

	// The newest converted copy contains v2 of the site and the dumps.
	snaps, _ := snapshot.List(ctx, env.Repo)
	out := filepath.Join(dir, "out")
	if _, err := engine.Restore(ctx, env.Repo, snaps[0].Snapshot, out, engine.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	var found string
	filepath.WalkDir(out, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == "index.php" {
			b, _ := os.ReadFile(p)
			found = string(b)
		}
		return nil
	})
	if !strings.Contains(found, "panel v2") {
		t.Fatalf("newest converted copy has %q", found)
	}

	// The restore test passes; without Docker the dump is reported, not failed.
	spec.Drill = source.DrillSpec{MinFiles: 3}
	dres, _, err := Drill(ctx, env, spec, "", dir)
	if err != nil || !dres.Passed {
		t.Fatalf("drill: %v", err)
	}
	sawDumpNote := false
	for _, c := range dres.Checks {
		if c.Name == "database-dumps" || strings.HasPrefix(c.Name, "database-dump: ") {
			sawDumpNote = true
		}
	}
	if !sawDumpNote {
		t.Fatal("PostgreSQL dump inside the backup was not noticed")
	}
}
