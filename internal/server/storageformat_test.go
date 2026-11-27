package server

import (
	"testing"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/source"
)

func TestAgentAtLeast(t *testing.T) {
	for v, want := range map[string]bool{
		"backupproof/0.4.0": true, "backupproof/0.4.2": true, "backupproof/1.0.0": true, "backupproof/0.10.1": true,
		"backupproof/0.3.0": false, "backupproof/0.2.23": false, "backupproof/0.2.21+d7fec35": false,
		"backupproof/0.4.0-demo": true, "": false, "dev": false,
	} {
		if got := agentAtLeast(v, 0, 4); got != want {
			t.Errorf("agentAtLeast(%q) = %v, want %v", v, got, want)
		}
	}
}

// New storage is v2 only when every server that uses it can read v2.
func TestNewStorageFormat(t *testing.T) {
	srv, err := New(Config{DataDir: t.TempDir(), NoLocalAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	addAgent := func(name, version string) int64 {
		res, err := srv.store.db.Exec("INSERT INTO agents(name,public_key,token_hash,created,version) VALUES(?,?,?,?,?)", name, "key-"+name, "hash-"+name, now(), version)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	newer, older := addAgent("new", "backupproof/0.4.0"), addAgent("old", "backupproof/0.3.0")
	repoID, err := srv.store.CreateRepository("disk", backend.Config{Type: "s3", Bucket: "b"}, RepoSecret{Password: "a-long-repository-password"})
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.newStorageFormat(repoID); got != 2 {
		t.Errorf("unused storage: v%d, want v2", got)
	}
	add := func(name string, agent int64, verifier *int64) {
		if _, err := srv.store.SaveSource(&Source{Name: name, AgentID: agent, VerifierID: verifier, RepoID: repoID, Spec: source.Spec{Kind: "files", Paths: []string{"/x"}},
			BackupCron: "0 2 * * *", DrillCron: "0 4 * * 0", Enabled: true}, &SourceSecret{}); err != nil {
			t.Fatal(err)
		}
	}
	add("a", newer, nil)
	if got := srv.newStorageFormat(repoID); got != 2 {
		t.Errorf("used by a 0.4 server: v%d, want v2", got)
	}
	add("b", newer, &older) // the old server only runs its restore tests
	if got := srv.newStorageFormat(repoID); got != 1 {
		t.Errorf("tested by a 0.3 server: v%d, want v1", got)
	}
	if old, _ := srv.tooOldForV2(repoID); len(old) != 1 || old[0] != "old" {
		t.Errorf("too old: %v", old)
	}
}
