package ops

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/engine"
	"github.com/chmuzamil/backupproof/internal/importer"
	"github.com/chmuzamil/backupproof/internal/snapshot"
	"github.com/chmuzamil/backupproof/internal/source"
)

func restoredContains(t *testing.T, env *Env, dir, file, want string) {
	t.Helper()
	snaps, _ := snapshot.List(context.Background(), env.Repo)
	out := filepath.Join(dir, "check-"+file)
	if _, err := engine.Restore(context.Background(), env.Repo, snaps[0].Snapshot, out, engine.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	var got string
	filepath.WalkDir(out, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == file {
			b, _ := os.ReadFile(p)
			got = string(b)
		}
		return nil
	})
	if !strings.Contains(got, want) {
		t.Fatalf("%s: got %q, want %q", file, got, want)
	}
}

// TestImportKopia uses the real kopia program.
func TestImportKopia(t *testing.T) {
	if _, err := exec.LookPath("kopia"); err != nil {
		t.Skip("kopia not installed")
	}
	ctx := context.Background()
	env, dir, _ := setup(t)
	repo := filepath.Join(dir, "kopia-repo")
	cfg := filepath.Join(dir, "kopia.config")
	data := filepath.Join(dir, "srv", "app")
	os.MkdirAll(data, 0o755)
	os.WriteFile(filepath.Join(data, "settings.yml"), []byte("version: 1"), 0o644)
	kopia := func(args ...string) {
		cmd := exec.Command("kopia", append([]string{"--config-file=" + cfg}, args...)...)
		cmd.Env = append(os.Environ(), "KOPIA_PASSWORD=kopia-secret-pw", "KOPIA_CHECK_FOR_UPDATES=false")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("kopia %v: %v\n%s", args, err, out)
		}
	}
	kopia("repository", "create", "filesystem", "--path="+repo)
	kopia("snapshot", "create", data)
	time.Sleep(1100 * time.Millisecond)
	os.WriteFile(filepath.Join(data, "settings.yml"), []byte("version: 2"), 0o644)
	kopia("snapshot", "create", data)
	pwFile := filepath.Join(dir, "kopia-pw")
	os.WriteFile(pwFile, []byte("kopia-secret-pw\n"), 0o600)

	// Mode 1: reuse the existing connection on this computer.
	spec := source.Spec{Name: "kopia-existing", Kind: "import", Import: &importer.Spec{Format: "kopia", KopiaConfigFile: cfg, PasswordFile: pwFile}}
	res, err := Import(ctx, env, spec)
	if err != nil || res.Imported != 2 {
		t.Fatalf("existing connection: %+v %v", res, err)
	}
	restoredContains(t, env, dir, "settings.yml", "version: 2")

	// Mode 2: connect to the storage directly (read-only, temporary config).
	spec = source.Spec{Name: "kopia-direct", Kind: "import", Import: &importer.Spec{Format: "kopia",
		Storage: backend.Config{Type: "local", Path: repo}, Password: "kopia-secret-pw"}}
	if res, err := Import(ctx, env, spec); err != nil || res.Imported != 2 {
		t.Fatalf("direct: %+v %v", res, err)
	}
	spec.Name, spec.Import.Password = "kopia-wrong", "nope"
	if _, err := Import(ctx, env, spec); err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("wrong password must be reported: %v", err)
	}
}

// TestImportRclone uses the real rclone program with an on-the-fly local
// remote, the same code path as Google Drive / Dropbox / OneDrive remotes.
func TestImportRclone(t *testing.T) {
	if _, err := exec.LookPath("rclone"); err != nil {
		t.Skip("rclone not installed")
	}
	ctx := context.Background()
	env, dir, _ := setup(t)
	drive := filepath.Join(dir, "drive", "Backups")
	os.MkdirAll(filepath.Join(drive, "2026-10-01"), 0o755)
	os.MkdirAll(filepath.Join(drive, "2026-10-02"), 0o755)
	os.WriteFile(filepath.Join(drive, "2026-10-01", "shop.sql"), []byte("day one"), 0o644)
	os.WriteFile(filepath.Join(drive, "2026-10-02", "shop.sql"), []byte("day two"), 0o644)

	spec := source.Spec{Name: "gdrive", Kind: "import", Import: &importer.Spec{Format: "files",
		Storage: backend.Config{Type: "rclone", Remote: ":local:" + filepath.ToSlash(drive)}}}
	res, err := Import(ctx, env, spec)
	if err != nil || res.Imported != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	restoredContains(t, env, dir, "shop.sql", "day two")
}

func TestBorgParsingAndStreaming(t *testing.T) {
	pts, err := importer.ParseBorgList([]byte(`{"archives":[{"archive":"x","barchive":"x","id":"abc123","name":"server-2026-10-01","start":"2026-10-01T01:30:05.000000","time":"2026-10-01T01:30:05.000000"}]}`))
	if err != nil || len(pts) != 1 || pts[0].Label != "server-2026-10-01" || pts[0].Time.Day() != 1 {
		t.Fatalf("%+v %v", pts, err)
	}
	envv := strings.Join(importer.BorgEnv(importer.Spec{BorgRepository: "ssh://u@h:23/./b", PasswordFile: "/etc/borg/pass", BorgSSHKeyFile: "/root/.ssh/borg"}, nil), "\n")
	for _, want := range []string{"BORG_REPO=ssh://u@h:23/./b", "BORG_PASSCOMMAND=cat '/etc/borg/pass'", "ssh -i '/root/.ssh/borg' -o BatchMode=yes", "BORG_RELOCATED_REPO_ACCESS_IS_OK=yes"} {
		if !strings.Contains(envv, want) {
			t.Errorf("missing %q", want)
		}
	}

	// export-tar output is a plain tar stream.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := []byte("borg file")
	tw.WriteHeader(&tar.Header{Name: "etc/nginx/nginx.conf", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg, ModTime: time.Now()})
	tw.Write(body)
	tw.Close()
	env, _, _ := setup(t)
	b, _ := engine.NewBuilder(context.Background(), env.Repo, engine.Options{})
	if err := importer.AddTarStream(context.Background(), b, &buf, ""); err != nil {
		t.Fatal(err)
	}
	s, err := b.Commit(context.Background(), &snapshot.Snapshot{Source: snapshot.Source{Name: "borg", Kind: "import"}})
	if err != nil || s.Stats.Files != 1 {
		t.Fatalf("%+v %v", s.Stats, err)
	}
}
