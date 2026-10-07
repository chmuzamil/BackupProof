package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chmuzamil/backupproof/internal/backend"
)

func TestDetectKeys(t *testing.T) {
	h := strings.Repeat("ab", 32) // a 64-character hex name
	cases := []struct {
		name string
		keys []string
		want *Repository
	}{
		{"restic at the top", []string{"config", "data/ab/" + h, "index/" + h, "keys/" + h, "snapshots/" + h}, &Repository{"restic", "restic", ""}},
		{"restic in a folder", []string{"founder-os-vps/config", "founder-os-vps/data/e5/" + h, "founder-os-vps/keys/" + h}, &Repository{"restic", "restic", "founder-os-vps"}},
		{"Kopia", []string{"servers/web/kopia.repository.f", "servers/web/p0123abc"}, &Repository{"kopia", "Kopia", "servers/web"}},
		{"Borg", []string{"borg/README", "borg/config", "borg/data/0/1", "borg/data/0/3"}, &Repository{"borg", "BorgBackup", "borg"}},
		{"dated dumps", []string{"2026-09-01/shop.sql.gz.gpg", "2026-09-02/shop.sql.gz.gpg", "config"}, nil},
		{"hash-named files without a config", []string{"data/ab/" + h}, nil},
	}
	for _, c := range cases {
		got := DetectKeys(c.keys)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

// Importing a restic repository as "backup files" stops with a clear message
// instead of failing to decrypt every chunk.
func TestFilesImportRefusesRepository(t *testing.T) {
	dir := t.TempDir()
	h := strings.Repeat("e5", 32)
	for _, k := range []string{"server1/config", "server1/keys/" + h, "server1/data/e5/" + h} {
		p := filepath.Join(dir, filepath.FromSlash(k))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("encrypted"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	src, err := Open(ctx, Spec{Format: "files", Storage: backend.Config{Type: "local", Path: dir}})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.List(ctx); err == nil || !strings.Contains(err.Error(), "restic repository") || !strings.Contains(err.Error(), "server1") {
		t.Fatalf("want a restic repository error naming the folder, got %v", err)
	}

	be, err := backend.Open(ctx, backend.Config{Type: "local", Path: dir}, backend.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	repo, err := DetectRepository(ctx, be)
	if err != nil || repo == nil {
		t.Fatalf("DetectRepository: %+v, %v", repo, err)
	}
	if got := repo.WithPrefix(backend.Config{Type: "local", Path: dir}).Path; got != filepath.Join(dir, "server1") {
		t.Errorf("WithPrefix: %s", got)
	}
	if got := repo.WithPrefix(backend.Config{Type: "s3", Bucket: "b", Prefix: "backups/"}).Prefix; got != "backups/server1" {
		t.Errorf("WithPrefix s3: %s", got)
	}
}
