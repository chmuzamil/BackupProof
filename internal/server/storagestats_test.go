package server

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/engine"
	"github.com/chmuzamil/backupproof/internal/source"
)

// Each storage gets a health check a week after it was added (and weekly
// after that); results are kept for the size chart.
func TestStorageChecksAreScheduledAndRecorded(t *testing.T) {
	dir := t.TempDir()
	srv, err := New(Config{DataDir: filepath.Join(dir, "s"), NoLocalAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	st := srv.store
	res, err := st.db.Exec("INSERT INTO agents(name,public_key,token_hash,created) VALUES('web','key','hash',?)", now())
	if err != nil {
		t.Fatal(err)
	}
	agentID, _ := res.LastInsertId()
	repoID, err := st.CreateRepository("disk", backend.Config{Type: "local", Path: dir}, RepoSecret{Password: "a-long-repository-password"})
	if err != nil {
		t.Fatal(err)
	}
	srcID, err := st.SaveSource(&Source{Name: "docs", AgentID: agentID, RepoID: repoID, Spec: source.Spec{Kind: "files", Paths: []string{dir}}, BackupCron: "@daily", DrillCron: "@weekly", Enabled: true}, &SourceSecret{})
	if err != nil {
		t.Fatal(err)
	}
	sources, _ := st.Sources()
	queued := func() int {
		var n int
		_ = st.db.QueryRow("SELECT count(*) FROM jobs WHERE kind='check' AND state='queued'").Scan(&n)
		return n
	}
	srv.scheduleChecks(time.Now(), sources)
	if queued() != 0 {
		t.Fatal("a brand-new storage was checked straight away")
	}
	srv.scheduleChecks(time.Now().Add(8*24*time.Hour), sources)
	if queued() != 1 {
		t.Fatalf("no check queued after a week: %d", queued())
	}
	srv.scheduleChecks(time.Now().Add(9*24*time.Hour), sources)
	if queued() != 1 {
		t.Fatal("a second check was queued while one was waiting")
	}
	srv.recordCheck(srcID, true, engine.CheckResult{StoredBytes: 1 << 20, StoredBlobs: 3, Snapshots: 2, ReadBlobs: 1})
	srv.recordCheck(srcID, false, map[string]any{"storedBytes": 2 << 20, "missingBlobs": 1})
	stats, err := st.RepoStats(26)
	if err != nil {
		t.Fatal(err)
	}
	got := stats[repoID]
	if len(got) != 2 || got[0].Bytes != 1<<20 || !got[0].OK || got[1].OK || got[1].Missing != 1 || got[1].Bytes != 2<<20 {
		t.Fatalf("stats: %+v", got)
	}
}
