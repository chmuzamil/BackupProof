package server

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/proof"
)

type copyFixture struct {
	*recoveryFixture
	second int64
}

func newCopyFixture(t *testing.T) *copyFixture {
	t.Helper()
	f := newRecoveryFixture(t, 0, 0)
	second, err := f.srv.store.CreateRepository("offsite", backend.Config{Type: "local", Path: t.TempDir()}, RepoSecret{Password: "another-long-repository-password"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.SetRepoID(second, "repo-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.db.Exec("UPDATE sources SET copy_repo_id=? WHERE id=?", second, f.source.ID); err != nil {
		t.Fatal(err)
	}
	src, err := f.srv.store.Source(f.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.source = *src
	return &copyFixture{f, second}
}

func (f *copyFixture) addCopy(t *testing.T, repoID, snapshot, root string) {
	t.Helper()
	pred, _ := json.Marshal(proof.CopyPredicate{RepoID: repoID, SnapshotID: snapshot})
	stmt, _ := json.Marshal(proof.Statement{Type: proof.StatementType, Subject: []proof.Subject{{Name: "snap", Digest: map[string]string{"blake3": root}}}, PredicateType: proof.PredicateCopy, Predicate: pred})
	env, _ := json.Marshal(proof.Envelope{PayloadType: proof.PayloadType, Payload: base64.StdEncoding.EncodeToString(stmt)})
	var seq int
	_ = f.srv.store.db.QueryRow("SELECT COUNT(*)+1 FROM proofs").Scan(&seq)
	if _, err := f.srv.store.db.Exec(`INSERT INTO proofs(kind,source_id,source_name,snapshot_id,passed,envelope,ledger_seq,signer,created) VALUES('copy',?,?,?,1,?,?,'t',?)`,
		f.source.ID, f.source.Name, snapshot, string(env), seq, now()); err != nil {
		t.Fatal(err)
	}
}

func (f *copyFixture) queued(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.srv.store.db.QueryRow("SELECT count(*) FROM jobs WHERE kind=? AND state='queued'", copyDrillKind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *copyFixture) finishQueued(t *testing.T) {
	t.Helper()
	if _, err := f.srv.store.db.Exec("UPDATE jobs SET state='succeeded' WHERE kind=?", copyDrillKind); err != nil {
		t.Fatal(err)
	}
}

func TestCopyTestsRunEveryThirtyDays(t *testing.T) {
	f := newCopyFixture(t)
	start := time.Now()
	sources := []Source{f.source}

	f.srv.scheduleCopyTests(start, sources)
	if f.queued(t) != 0 {
		t.Fatal("queued a test with nothing copied")
	}

	// A copy in a storage the item no longer uses doesn't count.
	f.addCopy(t, "old-storage", "old", "root-old")
	f.srv.scheduleCopyTests(start, sources)
	if f.queued(t) != 0 {
		t.Fatal("queued a test of a copy in another storage")
	}

	f.addCopy(t, "repo-2", "copy-1", "root-1")
	f.srv.scheduleCopyTests(start, sources)
	if f.queued(t) != 1 {
		t.Fatal("the first copy wasn't queued for a test")
	}
	f.finishQueued(t)
	f.srv.scheduleCopyTests(start.Add(29*24*time.Hour), sources)
	if f.queued(t) != 0 {
		t.Fatal("tested again within 30 days")
	}
	f.srv.scheduleCopyTests(start.Add(31*24*time.Hour), sources)
	if f.queued(t) != 1 {
		t.Fatal("not tested again after 30 days")
	}
	f.finishQueued(t)

	paused := f.source
	paused.Enabled = false
	f.srv.scheduleCopyTests(start.Add(90*24*time.Hour), []Source{paused})
	if f.queued(t) != 0 {
		t.Fatal("tested the copy of a paused item")
	}
}

func TestCopyTestLeasePointsAtTheSecondStorage(t *testing.T) {
	f := newCopyFixture(t)
	f.addCopy(t, "repo-2", "copy-1", "root-1")
	id, err := f.srv.queueCopyDrill(&f.source, "manual:test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	job, err := f.srv.store.Job(id)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := f.srv.buildLease(job)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Kind != "drill" || lease.RepoID != f.second || lease.SnapshotID != "copy-1" || lease.ExpectedRoot != "root-1" || lease.Password != "another-long-repository-password" {
		t.Fatalf("lease: kind=%s repo=%d snapshot=%s root=%s", lease.Kind, lease.RepoID, lease.SnapshotID, lease.ExpectedRoot)
	}

	// A proof has to describe the copy that was leased.
	statement := func(repoID, root string) *proof.Statement {
		pred, _ := json.Marshal(proof.DrillPredicate{RepoID: repoID, ExpectedRoot: root})
		return &proof.Statement{Predicate: pred}
	}
	if _, err := f.srv.checkCopyDrill(job, statement("repo-2", "root-1"), "copy-1"); err != nil {
		t.Errorf("the right copy was refused: %v", err)
	}
	for _, bad := range []struct{ repo, root, snap string }{{"repo-1", "root-1", "copy-1"}, {"repo-2", "other", "copy-1"}, {"repo-2", "root-1", "copy-2"}} {
		if _, err := f.srv.checkCopyDrill(job, statement(bad.repo, bad.root), bad.snap); err == nil {
			t.Errorf("accepted a proof of the wrong copy: %+v", bad)
		}
	}

	// Changing the second storage after queueing stops the test.
	if _, err := f.srv.store.db.Exec("UPDATE sources SET copy_repo_id=NULL WHERE id=?", f.source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.buildLease(job); err == nil {
		t.Error("leased a test of a storage the item no longer uses")
	}
}

func TestCopyTestAlertsAreSeparate(t *testing.T) {
	f := newCopyFixture(t)
	f.srv.copyDrillOutcome(&f.source, false, []proof.Check{{Name: "content-root"}})
	if !f.open(t, "copy-drill-failed") {
		t.Fatal("a failed copy test raised no alert")
	}
	if f.open(t, "drill-failed") {
		t.Fatal("a failed copy test raised the main restore-test alert")
	}
	f.srv.jobOutcome("drill", f.source.ID, true, "")
	if !f.open(t, "copy-drill-failed") {
		t.Error("a main restore test closed the copy's alert")
	}
	f.srv.copyDrillOutcome(&f.source, true, nil)
	if f.open(t, "copy-drill-failed") {
		t.Error("a passed copy test left its alert open")
	}
}

// A copy's cleanup can remove the copy a test is reading, so the two never
// overlap, and a test that waited checks the newest copy when it starts.
func TestCopyAndCopyTestDontOverlap(t *testing.T) {
	f := newCopyFixture(t)
	st := f.srv.store
	f.addCopy(t, "repo-2", "copy-1", "root-1")
	test, err := f.srv.queueCopyDrill(&f.source, "schedule", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	copyJob, err := st.EnqueueJobParams("copy", f.source.ID, f.source.AgentID, "after-backup", map[string]string{"snapshotId": "snap-2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec("UPDATE jobs SET state='running' WHERE id=?", copyJob); err != nil {
		t.Fatal(err)
	}
	drillAgent := f.source.DrillAgent()
	if j, _ := st.LeaseJob(drillAgent, time.Minute, true); j != nil {
		t.Fatalf("job %d (%s) started while a copy was running", j.ID, j.Kind)
	}

	// The copy finishes with a new copy; the waiting test starts on it.
	f.addCopy(t, "repo-2", "copy-2", "root-2")
	if _, err := st.db.Exec("UPDATE jobs SET state='succeeded' WHERE id=?", copyJob); err != nil {
		t.Fatal(err)
	}
	job, err := st.LeaseJob(drillAgent, time.Minute, true)
	if err != nil || job == nil || job.ID != test {
		t.Fatalf("the test didn't start after the copy: %+v, %v", job, err)
	}
	lease, err := f.srv.buildLease(job)
	if err != nil {
		t.Fatal(err)
	}
	if lease.SnapshotID != "copy-2" || lease.ExpectedRoot != "root-2" {
		t.Fatalf("tested %s (%s), want the newest copy", lease.SnapshotID, lease.ExpectedRoot)
	}
	pred, _ := json.Marshal(proof.DrillPredicate{RepoID: "repo-2", ExpectedRoot: "root-2"})
	if _, err := f.srv.checkCopyDrill(job, &proof.Statement{Predicate: pred}, "copy-2"); err != nil {
		t.Errorf("the proof of the copy that was tested was refused: %v", err)
	}

	// And a new copy waits while the test runs.
	next, err := st.EnqueueJobParams("copy", f.source.ID, f.source.AgentID, "after-backup", map[string]string{"snapshotId": "snap-3"})
	if err != nil {
		t.Fatal(err)
	}
	if j, _ := st.LeaseJob(f.source.AgentID, time.Minute, true); j != nil {
		t.Fatalf("job %d (%s) started while the copy test was running", j.ID, j.Kind)
	}
	if _, err := st.db.Exec("UPDATE jobs SET state='succeeded' WHERE id=?", test); err != nil {
		t.Fatal(err)
	}
	if j, _ := st.LeaseJob(f.source.AgentID, time.Minute, true); j == nil || j.ID != next {
		t.Fatalf("the copy didn't start after the test: %+v", j)
	}
}
