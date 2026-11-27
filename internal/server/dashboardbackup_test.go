package server

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/chunker"
	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/source"
)

// A dashboard backs itself up, and a new dashboard rebuilt from that backup
// has the same accounts, items, signing key and proof ledger.
func TestDashboardBackupAndRebuild(t *testing.T) {
	dashboardLocalOK = true
	t.Cleanup(func() { dashboardLocalOK = false })
	ctx := context.Background()
	dir := t.TempDir()
	srv, err := New(Config{DataDir: filepath.Join(dir, "old"), NoLocalAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if err := srv.store.SetSetting("tsa", "[]"); err != nil { // no timestamp requests from tests
		t.Fatal(err)
	}
	if _, err := srv.store.CreateUser("owner", "a-long-enough-password", "admin"); err != nil {
		t.Fatal(err)
	}
	storage := filepath.Join(dir, "storage")
	be, _ := backend.NewLocal(storage)
	const pw = "a-long-repository-password"
	if _, err := repo.Init(ctx, be, []byte(pw), chunker.Params{Min: 4 << 10, Avg: 16 << 10, Max: 64 << 10}); err != nil {
		t.Fatal(err)
	}
	repoID, err := srv.store.CreateRepository("offsite", backend.Config{Type: "local", Path: storage}, RepoSecret{Password: pw})
	if err != nil {
		t.Fatal(err)
	}
	res, err := srv.store.db.Exec("INSERT INTO agents(name,public_key,token_hash,created,last_seen) VALUES('web','key','hash',?,?)", now(), now())
	if err != nil {
		t.Fatal(err)
	}
	agentID, _ := res.LastInsertId()
	if _, err := srv.store.SaveSource(&Source{Name: "shop", AgentID: agentID, RepoID: repoID, Spec: source.Spec{Kind: "files", Paths: []string{dir}},
		BackupCron: "0 2 * * *", DrillCron: "0 4 * * 0", Enabled: true}, &SourceSecret{Password: pw}); err != nil {
		t.Fatal(err)
	}

	if err := srv.store.SetSetting(settingDashRepo, strconv.FormatInt(repoID, 10)); err != nil {
		t.Fatal(err)
	}
	// Twice: a backup's own proof is recorded after its copy of the database
	// was taken, so it's in the next backup.
	for range 2 {
		if err := srv.backupDashboard(ctx, "test"); err != nil {
			t.Fatalf("dashboard backup: %v", err)
		}
	}
	st := srv.dashboardBackupStatus()
	if st.Last == "" || st.LastError != "" {
		t.Errorf("status after a backup: %+v", st)
	}
	proofs, err := srv.store.Proofs(0, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range proofs {
		if p.SourceName == DashboardSourceName && p.Kind == "backup" && p.Passed {
			found = true
			full, err := srv.store.Proof(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := full.Envelope.Verify([]proof.PublicKey{srv.key.Public()}); err != nil {
				t.Errorf("the dashboard backup's proof doesn't verify with the dashboard's key: %v", err)
			}
		}
	}
	if !found {
		t.Fatal("no signed proof of the dashboard backup")
	}

	// Rebuild into an empty folder from the storage alone.
	r, err := repo.Open(ctx, be, []byte(pw))
	if err != nil {
		t.Fatal(err)
	}
	newDir := filepath.Join(dir, "new")
	rebuilt, err := RestoreDashboard(ctx, r, newDir, "")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if rebuilt.Users != 1 || rebuilt.Items != 1 || rebuilt.LedgerEntries == 0 {
		t.Errorf("rebuilt: %+v", rebuilt)
	}
	if _, err := RestoreDashboard(ctx, r, newDir, ""); err == nil {
		t.Error("restored over an existing dashboard")
	}
	again, err := New(Config{DataDir: newDir, NoLocalAgent: true})
	if err != nil {
		t.Fatalf("starting the rebuilt dashboard: %v", err)
	}
	defer again.Close()
	if again.key.Public().KeyID != srv.key.Public().KeyID {
		t.Error("the rebuilt dashboard has a different signing key, so its old proofs wouldn't match")
	}
	src, err := again.store.Sources()
	if err != nil || len(src) != 1 || src[0].Name != "shop" {
		t.Fatalf("items after rebuild: %v %v", src, err)
	}
	// Stored secrets still decrypt: the storage opens from the rebuilt dashboard.
	if _, sec, err := again.store.Repository(repoID); err != nil || sec.Password != pw {
		t.Errorf("the storage's password didn't survive: %v", err)
	}
}
