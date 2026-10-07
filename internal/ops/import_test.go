package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/engine"
	"github.com/chmuzamil/backupproof/internal/importer"
	"github.com/chmuzamil/backupproof/internal/snapshot"
	"github.com/chmuzamil/backupproof/internal/source"
)

func TestImportPlainFiles(t *testing.T) {
	ctx := context.Background()
	env, dir, _ := setup(t)
	dumps := filepath.Join(dir, "dumps")
	os.MkdirAll(filepath.Join(dumps, "2026-09"), 0o755)
	os.WriteFile(filepath.Join(dumps, "2026-09", "shop.sql.gz"), []byte(strings.Repeat("x", 5000)), 0o644)
	os.WriteFile(filepath.Join(dumps, "site.tar.gz"), []byte("tar"), 0o644)
	spec := source.Spec{Name: "old-dumps", Kind: "import", Import: &importer.Spec{Format: "files", Storage: backend.Config{Type: "local", Path: dumps}}}
	res, err := Import(ctx, env, spec)
	if err != nil || res.Imported != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	spec.Drill = source.DrillSpec{ExpectPaths: []string{"2026-09/shop.sql.gz"}, MinFiles: 2}
	if dres, _, err := Drill(ctx, env, spec, "", dir); err != nil || !dres.Passed {
		t.Fatalf("drill: %v", err)
	}
}

// TestImportEncryptedBucket converts files encrypted by the real gpg,
// openssl and age command-line tools (fixtures in importer/testdata).
func TestImportEncryptedBucket(t *testing.T) {
	ctx := context.Background()
	env, dir, _ := setup(t)
	td := filepath.Join("..", "importer", "testdata")
	ageKey, _ := os.ReadFile(filepath.Join(td, "age-key.txt"))
	spec := source.Spec{Name: "old-bucket", Kind: "import", Import: &importer.Spec{
		Format: "files", Storage: backend.Config{Type: "local", Path: filepath.Join(td, "bucket")},
		Password: "pw-import-test", PrivateKey: string(ageKey), Unpack: true,
	}}
	res, err := Import(ctx, env, spec)
	if err != nil {
		t.Fatalf("%v (%+v)", err, res)
	}
	if res.Found != 2 || res.Imported != 2 {
		t.Fatalf("expected one backup copy per day, got %+v", res)
	}
	snaps, _ := snapshot.List(ctx, env.Repo)
	if got := snaps[0].Time.Format("2006-01-02"); got != "2026-09-02" {
		t.Fatalf("newest copy dated %s, want the date from the file names", got)
	}
	// Restore the newest day and compare with the original plaintext.
	out := filepath.Join(dir, "out")
	if _, err := engine.Restore(ctx, env.Repo, snaps[0].Snapshot, out, engine.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"2026-09-02/shop.sql":                         "INSERT INTO orders VALUES (1),(2),(3),(4);",
		"2026-09-02/site-20260902/site/index.html":    "<h1>Shop</h1>",
		"2026-09-02/site-20260902/site/wp-config.php": `define("DB_NAME","shop")`,
		"2026-09-02/notes-2026-09-02.txt":             "ops notes",
	}
	for p, sub := range want {
		b, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(p)))
		if err != nil || !strings.Contains(string(b), sub) {
			t.Errorf("%s: %v %q", p, err, b)
		}
	}
	older := filepath.Join(dir, "older")
	engine.Restore(ctx, env.Repo, snaps[1].Snapshot, older, engine.RestoreOptions{})
	for _, p := range []string{"2026-09-01/shop.sql", "2026-09-01/legacy-shop.sql"} {
		b, err := os.ReadFile(filepath.Join(older, filepath.FromSlash(p)))
		if err != nil || !strings.Contains(string(b), "VALUES (1),(2),(3);") {
			t.Errorf("%s: %v %q", p, err, b)
		}
	}
	// Wrong password fails clearly instead of importing garbage.
	spec.Name, spec.Import.Password = "wrong", "not-the-password"
	if _, err := Import(ctx, env, spec); err == nil {
		t.Fatal("wrong password must fail")
	}
}
