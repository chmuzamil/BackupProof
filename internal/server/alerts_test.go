package server

import (
	"bytes"
	"testing"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/source"
)

// Removing an item resolves and unlinks its alerts, so none stays open
// forever and a new item that gets the same ID doesn't inherit them.
func TestDeleteSourceResolvesAlerts(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	res, err := st.db.Exec("INSERT INTO agents(name,public_key,token_hash,created) VALUES('web','key','hash',?)", now())
	if err != nil {
		t.Fatal(err)
	}
	agentID, _ := res.LastInsertId()
	repoID, err := st.CreateRepository("disk", backend.Config{Type: "local", Path: dir}, RepoSecret{Password: "a-long-repository-password"})
	if err != nil {
		t.Fatal(err)
	}
	newSource := func() int64 {
		t.Helper()
		id, err := st.SaveSource(&Source{Name: "luxvps", AgentID: agentID, RepoID: repoID, Spec: source.Spec{Kind: "files", Paths: []string{dir}}, BackupCron: "@daily", DrillCron: "@weekly", Enabled: true}, &SourceSecret{})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := newSource()
	st.RaiseAlert("backup-failed", &id, nil, "luxvps: the backup failed")
	if err := st.DeleteSource(id); err != nil {
		t.Fatal(err)
	}
	if open, _ := st.Alerts(true, 10); len(open) != 0 {
		t.Fatalf("alerts still open after removing the item: %+v", open)
	}
	all, _ := st.Alerts(false, 10)
	if len(all) != 1 || all[0].SourceID != nil || all[0].Resolved == nil {
		t.Fatalf("the alert should stay in history, resolved and unlinked: %+v", all)
	}
	if again := newSource(); again == id {
		t.Logf("SQLite reused item ID %d; the old alert must not point at it", again)
	}

	// Alerts that an older version left behind are cleaned up on start.
	gone := int64(999)
	st.RaiseAlert("backup-failed", &gone, nil, "removed by an old version")
	st.Close()
	st, err = OpenStore(dir, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if open, _ := st.Alerts(true, 10); len(open) != 0 {
		t.Fatalf("orphaned alert still open after restart: %+v", open)
	}
}
