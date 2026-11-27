package server

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/source"
)

func TestTimeWindow(t *testing.T) {
	at := func(hm string) time.Time { tt, _ := time.ParseInLocation("15:04", hm, time.Local); return tt }
	night := Limits{WindowStart: "22:00", WindowEnd: "06:00"}
	for hm, want := range map[string]bool{"23:30": true, "02:00": true, "05:59": true, "06:00": false, "12:00": false, "22:00": true} {
		if got := night.open(at(hm)); got != want {
			t.Errorf("22:00–06:00 at %s: %v, want %v", hm, got, want)
		}
	}
	day := Limits{WindowStart: "09:00", WindowEnd: "17:00"}
	if !day.open(at("12:00")) || day.open(at("18:00")) {
		t.Error("09:00–17:00 window wrong")
	}
	if !(Limits{}).open(at("03:00")) {
		t.Error("no window should always be open")
	}
	for _, bad := range []Limits{{WindowStart: "22:00"}, {WindowStart: "25:00", WindowEnd: "06:00"}, {WindowStart: "10:00", WindowEnd: "10:00"}, {UploadKBps: -1}} {
		if bad.validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// Outside the window, scheduled jobs wait while ones started by a person run.
func TestWindowHoldsScheduledJobs(t *testing.T) {
	dir := t.TempDir()
	srv, err := New(Config{DataDir: filepath.Join(dir, "s"), NoLocalAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	st := srv.store
	res, _ := st.db.Exec("INSERT INTO agents(name,public_key,token_hash,created) VALUES('web','key','hash',?)", now())
	agentID, _ := res.LastInsertId()
	repoID, _ := st.CreateRepository("disk", backend.Config{Type: "local", Path: dir}, RepoSecret{Password: "a-long-repository-password"})
	a, _ := st.SaveSource(&Source{Name: "a", AgentID: agentID, RepoID: repoID, Spec: source.Spec{Kind: "files", Paths: []string{dir}}, BackupCron: "@daily", DrillCron: "@weekly", Enabled: true}, &SourceSecret{})
	b, _ := st.SaveSource(&Source{Name: "b", AgentID: agentID, RepoID: repoID, Spec: source.Spec{Kind: "files", Paths: []string{dir}}, BackupCron: "@daily", DrillCron: "@weekly", Enabled: true}, &SourceSecret{})
	sched, _ := st.EnqueueJob("backup", a, agentID, "schedule")
	manual, _ := st.EnqueueJob("backup", b, agentID, "manual")
	j, err := st.LeaseJob(agentID, time.Minute, false)
	if err != nil || j == nil || j.ID != manual {
		t.Fatalf("outside the window: leased %+v, want the manual job %d", j, manual)
	}
	if j, _ := st.LeaseJob(agentID, time.Minute, false); j != nil {
		t.Fatalf("scheduled job %d ran outside the window", j.ID)
	}
	if j, _ := st.LeaseJob(agentID, time.Minute, true); j == nil || j.ID != sched {
		t.Fatalf("inside the window the scheduled job should run, got %+v", j)
	}
	if err := st.SetAgentLimits(agentID, Limits{UploadKBps: 512, WindowStart: "22:00", WindowEnd: "06:00"}); err != nil {
		t.Fatal(err)
	}
	ag, _ := st.Agent(agentID)
	if ag.Limits.UploadKBps != 512 || ag.Limits.WindowStart != "22:00" {
		t.Errorf("limits not saved: %+v", ag.Limits)
	}
}

// While a storage is upgraded no job that uses it, as first or second copy,
// is handed out, and the upgrade can't start while one is running.
func TestPausedStorageHoldsJobs(t *testing.T) {
	dir := t.TempDir()
	srv, err := New(Config{DataDir: filepath.Join(dir, "s"), NoLocalAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	st := srv.store
	res, _ := st.db.Exec("INSERT INTO agents(name,public_key,token_hash,created) VALUES('web','key','hash',?)", now())
	agentID, _ := res.LastInsertId()
	one, _ := st.CreateRepository("one", backend.Config{Type: "local", Path: filepath.Join(dir, "1")}, RepoSecret{Password: "a-long-repository-password"})
	two, _ := st.CreateRepository("two", backend.Config{Type: "local", Path: filepath.Join(dir, "2")}, RepoSecret{Password: "a-long-repository-password"})
	spec := source.Spec{Kind: "files", Paths: []string{dir}}
	onOne, _ := st.SaveSource(&Source{Name: "on-one", AgentID: agentID, RepoID: one, Spec: spec, BackupCron: "@daily", DrillCron: "@weekly", Enabled: true}, &SourceSecret{})
	copyToOne, _ := st.SaveSource(&Source{Name: "copy-to-one", AgentID: agentID, RepoID: two, Spec: spec, BackupCron: "@daily", DrillCron: "@weekly", Enabled: true}, &SourceSecret{})
	onTwo, _ := st.SaveSource(&Source{Name: "on-two", AgentID: agentID, RepoID: two, Spec: spec, BackupCron: "@daily", DrillCron: "@weekly", Enabled: true}, &SourceSecret{})
	if _, err := st.db.Exec("UPDATE sources SET copy_repo_id=? WHERE id=?", one, copyToOne); err != nil {
		t.Fatal(err)
	}
	j1, _ := st.EnqueueJob("backup", onOne, agentID, "manual")
	j2, _ := st.EnqueueJob("copy", copyToOne, agentID, "manual")
	j3, _ := st.EnqueueJob("backup", onTwo, agentID, "manual")

	if running, err := st.PauseRepository(one); err != nil || running != 0 {
		t.Fatalf("pause: %d running, %v", running, err)
	}
	if j, _ := st.LeaseJob(agentID, time.Minute, true); j == nil || j.ID != j3 {
		t.Fatalf("with storage one paused, leased %+v, want job %d on storage two", j, j3)
	}
	if j, _ := st.LeaseJob(agentID, time.Minute, true); j != nil {
		t.Fatalf("job %d using a paused storage was handed out", j.ID)
	}
	st.ResumeRepository(one)
	if j, _ := st.LeaseJob(agentID, time.Minute, true); j == nil || j.ID != j1 {
		t.Fatalf("after resuming, leased %+v, want %d", j, j1)
	}
	// Job 1 is running on storage one now, so pausing it reports it.
	if running, _ := st.PauseRepository(one); running != 1 {
		t.Errorf("%d jobs reported running on storage one, want 1", running)
	}
	st.ResumeRepository(one)
	if j, _ := st.LeaseJob(agentID, time.Minute, true); j == nil || j.ID != j2 {
		t.Fatalf("leased %+v, want the copy job %d", j, j2)
	}
}
