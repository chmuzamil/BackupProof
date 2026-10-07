package ops

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/chunker"
	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/source"
	_ "modernc.org/sqlite"
)

func setup(t *testing.T) (*Env, string, *proof.FileLedger) {
	t.Helper()
	dir := t.TempDir()
	be, _ := backend.NewLocal(filepath.Join(dir, "repo"))
	r, err := repo.Init(context.Background(), be, []byte("test password 123"), chunker.Params{Min: 4 << 10, Avg: 16 << 10, Max: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	key, _ := proof.GenerateKey("test-host")
	ledger, err := proof.OpenFileLedger(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return &Env{Repo: r, Signer: key, Ledger: ledger, Storage: proof.StorageInfo{Location: "local"}, Log: t.Logf}, dir, ledger
}

func makeSQLite(t *testing.T, path string, rows int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		"PRAGMA journal_mode=WAL",
		"CREATE TABLE users(id INTEGER PRIMARY KEY, email TEXT UNIQUE)",
		"CREATE TABLE orders(id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users(id), total REAL)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < rows; i++ {
		db.Exec("INSERT INTO users(email) VALUES(?)", fmt.Sprintf("u%d@example.com", i))
		db.Exec("INSERT INTO orders(user_id,total) VALUES(?,?)", i+1, float64(i)*1.5)
	}
}

func TestSQLiteBackupDrillAndOfflineProof(t *testing.T) {
	ctx := context.Background()
	env, dir, ledger := setup(t)
	dbPath := filepath.Join(dir, "app.db")
	makeSQLite(t, dbPath, 500)

	spec := source.Spec{Name: "app-db", Kind: "sqlite", Paths: []string{dbPath},
		Drill: source.DrillSpec{Assertions: []source.Assertion{{Name: "has users", SQL: "SELECT count(*) >= 500 FROM users"}}}}
	snap, brec, err := Backup(ctx, env, spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if brec.Ledger == nil || brec.Ledger.Seq != 1 {
		t.Fatalf("backup not recorded in ledger: %+v", brec.Ledger)
	}

	res, drec, err := Drill(ctx, env, spec, "", dir)
	if err != nil {
		for _, c := range res.Checks {
			t.Logf("%v %s: %s", c.Passed, c.Name, c.Detail)
		}
		t.Fatal(err)
	}
	if !res.Passed || res.RestoredRoot != snap.Root {
		t.Fatalf("drill did not pass: %+v", res)
	}
	names := map[string]bool{}
	for _, c := range res.Checks {
		names[c.Name] = true
	}
	for _, want := range []string{"restore-from-storage", "content-root", "sqlite-integrity-check", "row-count-reconciliation", "assert: has users"} {
		if !names[want] {
			t.Errorf("missing check %q", want)
		}
	}

	// Offline verification by a third party that only knows the public key.
	all, _ := ledger.All()
	bundle, err := BuildBundle(drec, all, env.Signer, "test", []proof.PublicKey{env.Signer.Public()})
	if err != nil {
		t.Fatal(err)
	}
	rep := proof.VerifyBundle(bundle, proof.VerifyOptions{TrustedKeys: []proof.PublicKey{env.Signer.Public()}})
	if !rep.Valid || rep.Passed == nil || !*rep.Passed {
		t.Fatalf("bundle did not verify: %+v", rep)
	}

	// Proof records are also stored in the repository itself.
	recs, err := LoadRecords(ctx, env.Repo)
	if err != nil || len(recs) != 2 {
		t.Fatalf("expected 2 proof records in repo, got %d (%v)", len(recs), err)
	}
}

func TestDrillFailsOnDataLoss(t *testing.T) {
	ctx := context.Background()
	env, dir, _ := setup(t)
	dbPath := filepath.Join(dir, "app.db")
	makeSQLite(t, dbPath, 50)
	spec := source.Spec{Name: "app-db", Kind: "sqlite", Paths: []string{dbPath},
		Drill: source.DrillSpec{Assertions: []source.Assertion{{Name: "impossible", SQL: "SELECT count(*) > 1000 FROM users"}}}}
	if _, _, err := Backup(ctx, env, spec, nil); err != nil {
		t.Fatal(err)
	}
	res, rec, err := Drill(ctx, env, spec, "", dir)
	if err == nil || res.Passed {
		t.Fatal("drill with failing assertion must fail")
	}
	if rec == nil || rec.Passed || rec.Ledger == nil {
		t.Fatal("failed drills must still be attested and recorded in the ledger")
	}
}

func TestFilesDrillDetectsMissingExpectedPath(t *testing.T) {
	ctx := context.Background()
	env, dir, _ := setup(t)
	src := filepath.Join(dir, "site")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "index.html"), []byte("<h1>hi</h1>"), 0o644)
	spec := source.Spec{Name: "site", Kind: "files", Paths: []string{src},
		Drill: source.DrillSpec{ExpectPaths: []string{filepath.Join(src, "index.html")}, MinFiles: 1}}
	if _, _, err := Backup(ctx, env, spec, nil); err != nil {
		t.Fatal(err)
	}
	if res, _, err := Drill(ctx, env, spec, "", dir); err != nil || !res.Passed {
		t.Fatalf("expected pass: %v", err)
	}
	spec.Drill.ExpectPaths = append(spec.Drill.ExpectPaths, filepath.Join(src, "wp-config.php"))
	if res, _, _ := Drill(ctx, env, spec, "", dir); res.Passed {
		t.Fatal("missing expected path must fail the drill")
	}
}
