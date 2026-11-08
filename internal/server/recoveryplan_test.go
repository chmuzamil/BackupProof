package server

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/source"
)

type planFixture struct {
	*recoveryFixture
	repo int64
}

func newPlanFixture(t *testing.T) *planFixture {
	t.Helper()
	f := newRecoveryFixture(t, 0, 0)
	repo, err := f.srv.store.CreateRepository("offsite", backend.Config{Type: "s3", Bucket: "bucket", Prefix: "site", Endpoint: "https://s3.example.com", Region: "eu"},
		RepoSecret{Password: "storage-password-do-not-leak", Credentials: backend.Credentials{AccessKey: "AKIA-do-not-leak", SecretKey: "secret-key-do-not-leak"}})
	if err != nil {
		t.Fatal(err)
	}
	return &planFixture{f, repo}
}

func (f *planFixture) item(t *testing.T, name string, spec source.Spec, sec *SourceSecret) Source {
	t.Helper()
	id, err := f.srv.store.SaveSource(&Source{Name: name, AgentID: f.agent, RepoID: f.repo, Spec: spec, BackupCron: "0 2 * * *", DrillCron: "0 4 * * 0", Enabled: true}, sec)
	if err != nil {
		t.Fatal(err)
	}
	src, err := f.srv.store.Source(id)
	if err != nil {
		t.Fatal(err)
	}
	return *src
}

func (f *planFixture) addProof(t *testing.T, src Source, kind, snap string, passed bool, taken time.Time, checks []proof.Check) {
	t.Helper()
	pred, _ := json.Marshal(proof.DrillPredicate{SnapshotTime: taken, Checks: checks})
	stmt, _ := json.Marshal(proof.Statement{Type: proof.StatementType, Subject: []proof.Subject{{Name: "snap", Digest: map[string]string{"blake3": "root-" + snap}}}, PredicateType: proof.PredicateDrill, Predicate: pred})
	env, _ := json.Marshal(proof.Envelope{PayloadType: proof.PayloadType, Payload: base64.StdEncoding.EncodeToString(stmt)})
	var seq int
	_ = f.srv.store.db.QueryRow("SELECT COUNT(*)+1 FROM proofs").Scan(&seq)
	rto := int64(90000)
	if _, err := f.srv.store.db.Exec(`INSERT INTO proofs(kind,source_id,source_name,snapshot_id,passed,rto_ms,envelope,ledger_seq,signer,created) VALUES(?,?,?,?,?,?,?,?,'t',?)`,
		kind, src.ID, src.Name, snap, passed, rto, string(env), seq, now()); err != nil {
		t.Fatal(err)
	}
}

func (f *planFixture) plan(t *testing.T) *recoveryPlan {
	t.Helper()
	a, err := f.srv.store.Agent(f.agent)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.srv.buildRecoveryPlan(a, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func findItem(t *testing.T, p *recoveryPlan, name string) planItem {
	t.Helper()
	for _, it := range p.Items {
		if it.Name == name {
			return it
		}
	}
	t.Fatalf("%s isn't in the plan", name)
	return planItem{}
}

func TestPlanOrder(t *testing.T) {
	f := newPlanFixture(t)
	f.item(t, "a-files", source.Spec{Kind: "files", Paths: []string{"/a"}}, nil)
	f.item(t, "b-app", source.Spec{Kind: "docker", Docker: &source.DockerSpec{Project: "app"}}, nil)
	f.item(t, "c-db", source.Spec{Kind: "postgres", Database: "c"}, nil)
	p := f.plan(t)
	want := []string{"c-db", "b-app", "a-files", "shop"}
	var got []string
	for i, it := range p.Items {
		got = append(got, it.Name)
		if it.Order != i+1 {
			t.Errorf("%s has order %d", it.Name, it.Order)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestPlanUsesNewestPassedBackup(t *testing.T) {
	f := newPlanFixture(t)
	src := f.item(t, "site", source.Spec{Kind: "files", Paths: []string{"/a"}}, nil)
	base := time.Now().Add(-48 * time.Hour)
	f.addProof(t, src, "drill", "older-passed", true, base, []proof.Check{{Name: "content-root", Passed: true}})
	f.addProof(t, src, "drill", "newer-passed", true, base.Add(24*time.Hour), []proof.Check{
		{Name: "restore-from-storage", Passed: true}, {Name: "app-http-status", Passed: true}, {Name: "content-root", Passed: true}})
	// A newer backup whose test failed must not be offered.
	f.addProof(t, src, "drill", "newest-failed", false, base.Add(40*time.Hour), []proof.Check{{Name: "content-root"}})
	it := findItem(t, f.plan(t), "site")
	if !it.Tested || it.SnapshotID != "newer-passed" {
		t.Errorf("wrong backup: %+v", it)
	}
	if it.RestoreMs == nil || *it.RestoreMs != 90000 {
		t.Errorf("restore time: %v", it.RestoreMs)
	}
	if len(it.AutoChecks) != 3 || !strings.Contains(strings.Join(it.AutoChecks, "|"), "The app answered") {
		t.Errorf("automated checks: %v", it.AutoChecks)
	}
	if len(it.Confirm) == 0 {
		t.Error("nothing left for a person to confirm")
	}
	for _, c := range it.AutoChecks {
		for _, co := range it.Confirm {
			if c == co {
				t.Errorf("%q is listed as both automatic and manual", c)
			}
		}
	}
	for _, step := range it.CLISteps {
		if strings.Contains(step, "restore --repo") && !strings.Contains(step, "newer-passed") {
			t.Errorf("CLI step doesn't name the tested backup: %s", step)
		}
	}
}

func TestPlanSaysWhatIsUntestedAndUnsupported(t *testing.T) {
	f := newPlanFixture(t)
	f.item(t, "db", source.Spec{Kind: "postgres", Database: "orders"}, nil)
	f.item(t, "app", source.Spec{Kind: "docker", Docker: &source.DockerSpec{Project: "app"}}, nil)
	p := f.plan(t)
	if it := findItem(t, p, "db"); it.Tested || !strings.Contains(strings.Join(it.NotCovered, " "), "original server") || !strings.Contains(strings.Join(it.CLISteps, " "), "postgres/orders.dump") {
		t.Errorf("database: %+v", it)
	}
	if it := findItem(t, p, "app"); !strings.Contains(strings.Join(it.NotCovered, " "), "doesn't recreate Compose files") {
		t.Errorf("docker: %+v", it)
	}
	text := p.markdown()
	for _, want := range []string{"No backup has passed a restore test", "Database restores from the dashboard go to the original server only", "not Compose files",
		"undocumented", "If the dashboard is lost too", "backupproof proof list", "DNS and firewall", "You need to confirm"} {
		if !strings.Contains(text, want) {
			t.Errorf("plan is missing %q", want)
		}
	}
	if !strings.Contains(strings.Join(p.Warnings, " "), "no backup that passed") {
		t.Errorf("warnings: %v", p.Warnings)
	}
}

func TestPlanWarnsAboutLocalStorage(t *testing.T) {
	f := newRecoveryFixture(t, 0, 0) // its storage is a folder on a disk
	a, err := f.srv.store.Agent(f.agent)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.srv.buildRecoveryPlan(a, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.Warnings, " "), "folder on a disk") {
		t.Errorf("warnings: %v", p.Warnings)
	}
}

func TestPlanOnlyHasThisServersItems(t *testing.T) {
	f := newPlanFixture(t)
	res, err := f.srv.store.db.Exec("INSERT INTO agents(name,public_key,token_hash,created,last_seen) VALUES('other','key2','hash2',?,?)", now(), now())
	if err != nil {
		t.Fatal(err)
	}
	other, _ := res.LastInsertId()
	if _, err := f.srv.store.SaveSource(&Source{Name: "elsewhere", AgentID: other, RepoID: f.repo, Spec: source.Spec{Kind: "files", Paths: []string{"/a"}}, BackupCron: "0 2 * * *", DrillCron: "0 4 * * 0"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, it := range f.plan(t).Items {
		if it.Name == "elsewhere" {
			t.Error("another server's item is in the plan")
		}
	}
}

// Secrets in any field the dashboard stores must never reach a plan.
func TestPlanHasNoSecrets(t *testing.T) {
	f := newPlanFixture(t)
	const secret = "plan-secret-do-not-leak"
	f.item(t, "db", source.Spec{Kind: "mongodb", Host: "h", User: "admin-do-not-leak", Password: secret, URI: "mongodb://u:" + secret + "@h"}, &SourceSecret{Password: secret, URI: "mongodb://u:" + secret + "@h"})
	spec := source.Spec{Kind: "docker", Docker: &source.DockerSpec{Project: "shop"}}
	spec.Drill.AppTest = &source.AppTest{Port: 80, App: source.AppContainer{Image: "wordpress:6", Mounts: []source.AppDataMount{{From: "docker/volumes/w", To: "/w"}},
		Env: []source.AppEnv{{Name: "HOSTNAME_LITERAL", Value: "literal-value-do-not-print"}, {Name: "WORDPRESS_DB_PASSWORD", SecretEnv: "BP_APPTEST_WP"}}}}
	app := f.item(t, "store", spec, nil)
	f.addProof(t, app, "drill", "snap1", true, time.Now(), []proof.Check{{Name: "app-http-status", Passed: true, Detail: "detail-do-not-print"}})
	p := f.plan(t)
	raw, _ := json.Marshal(p)
	for _, out := range []string{string(raw), p.markdown()} {
		for _, leak := range []string{secret, "storage-password-do-not-leak", "AKIA-do-not-leak", "secret-key-do-not-leak", "literal-value-do-not-print", "detail-do-not-print", "admin-do-not-leak"} {
			if strings.Contains(out, leak) {
				t.Errorf("the plan contains %q", leak)
			}
		}
	}
	// The name of a variable to set is fine, and useful.
	if !strings.Contains(p.markdown(), "BP_APPTEST_WP") {
		t.Error("the plan should say which variable to set again")
	}
}

func TestPlanRepoURL(t *testing.T) {
	for _, c := range []struct {
		cfg  backend.Config
		want string
	}{
		{backend.Config{Type: "local", Path: "/mnt/b"}, "/mnt/b"},
		{backend.Config{Type: "s3", Bucket: "b", Prefix: "p", Endpoint: "https://e", Region: "r"}, "s3://b/p?endpoint=https%3A%2F%2Fe&region=r"},
		{backend.Config{Type: "sftp", User: "u", Host: "h", Path: "/x"}, "sftp://u@h:22/x"},
		{backend.Config{Type: "rclone", Remote: "g:b"}, ""},
	} {
		if got := planRepoURL(c.cfg); got != c.want {
			t.Errorf("%+v: %q", c.cfg, got)
		}
		if c.want != "" && c.cfg.Type != "local" {
			if _, err := backend.ParseURL(c.want); err != nil {
				t.Errorf("the command line can't read %q: %v", c.want, err)
			}
		}
	}
}

// What the plan says about a dashboard that has lost its database: backups
// made before can't be restored from the new one, because it has no proofs.
func TestNewDashboardHasNoProofsForOldBackups(t *testing.T) {
	f := newPlanFixture(t)
	src := f.item(t, "site", source.Spec{Kind: "files", Paths: []string{"/a"}}, nil)
	if _, err := f.srv.attestedRoot(src.ID, "made-before-the-loss"); err == nil {
		t.Error("restored without a signed proof")
	}
}

// Moving an item under Edit: the proofs stay with the item, so restores on the
// new server remain possible. A real restore there is not exercised here.
func TestMovingItemToAnotherServerKeepsProofs(t *testing.T) {
	f := newPlanFixture(t)
	src := f.item(t, "site", source.Spec{Kind: "files", Paths: []string{"/a"}}, nil)
	f.addProof(t, src, "backup", "snap1", true, time.Now(), nil)
	res, err := f.srv.store.db.Exec("INSERT INTO agents(name,public_key,token_hash,created,last_seen) VALUES('new','key3','hash3',?,?)", now(), now())
	if err != nil {
		t.Fatal(err)
	}
	moved, _ := res.LastInsertId()
	src.AgentID = moved
	if _, err := f.srv.store.SaveSource(&src, &SourceSecret{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.attestedRoot(src.ID, "snap1"); err != nil {
		t.Errorf("the moved item lost its proof: %v", err)
	}
	got, err := f.srv.store.Source(src.ID)
	if err != nil || got.AgentID != moved {
		t.Errorf("server not changed: %+v %v", got, err)
	}
}
