// Package ops ties the engine, sources, drills and proof layer together into
// the two operations everything else calls: Backup and Drill. Both always
// produce a signed attestation recorded in the ledger — including failures,
// so a failed drill can never silently disappear.
package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/chmuzamil/backupproof/internal/drill"
	"github.com/chmuzamil/backupproof/internal/engine"
	"github.com/chmuzamil/backupproof/internal/importer"
	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/retention"
	"github.com/chmuzamil/backupproof/internal/snapshot"
	"github.com/chmuzamil/backupproof/internal/source"
)

type Ledger interface {
	Append(kind, subject, envelopeDigest string) (proof.LedgerEntry, error)
}

// EnvelopeLedger is implemented by remote ledgers (the control plane), which
// need the full signed record to verify it before appending.
type EnvelopeLedger interface {
	AppendRecord(ctx context.Context, rec *Record) (proof.LedgerEntry, error)
}

type Env struct {
	Repo    *repo.Repo
	Storage proof.StorageInfo
	Signer  *proof.Key
	Ledger  Ledger
	// TSAs enables RFC 3161 timestamping when non-empty.
	TSAs []string
	Log  engine.Logger
	// Workers for parallel uploads.
	Workers int
	// DenyPaths are never backed up, imported or used as storage (the
	// built-in agent denies the server's own data directory).
	DenyPaths []string
}

// Record is one stored attestation (also written to proofs/ in the repository).
type Record struct {
	Kind       string             `json:"kind"` // backup | drill
	SnapshotID string             `json:"snapshotId"`
	Source     string             `json:"source"`
	Passed     bool               `json:"passed"`
	Envelope   *proof.Envelope    `json:"envelope"`
	Timestamp  *proof.Timestamp   `json:"timestamp,omitempty"`
	Ledger     *proof.LedgerEntry `json:"ledger,omitempty"`
	Created    time.Time          `json:"created"`
}

func (e *Env) log(format string, a ...any) {
	if e.Log != nil {
		e.Log(format, a...)
	}
}

func (e *Env) attest(ctx context.Context, kind string, subject proof.Subject, predType string, pred any, snapID, src string, passed bool) (*Record, error) {
	payload, err := proof.NewStatement(subject, predType, pred)
	if err != nil {
		return nil, err
	}
	env := e.Signer.Sign(payload)
	rec := &Record{Kind: kind, SnapshotID: snapID, Source: src, Passed: passed, Envelope: env, Created: time.Now().UTC()}
	if len(e.TSAs) > 0 {
		b, _ := json.Marshal(env)
		if ts, err := proof.RequestTimestamp(ctx, b, e.TSAs); err != nil {
			e.log("warning: RFC 3161 timestamp unavailable: %v", err)
		} else {
			rec.Timestamp = ts
			e.log("timestamped by %s at %s", ts.TSA, ts.Time.Format(time.RFC3339))
		}
	}
	if el, ok := e.Ledger.(EnvelopeLedger); ok {
		le, err := el.AppendRecord(ctx, rec)
		if err != nil {
			return rec, fmt.Errorf("ledger append: %w", err)
		}
		rec.Ledger = &le
	} else if e.Ledger != nil {
		le, err := e.Ledger.Append(kind, snapID, env.Digest())
		if err != nil {
			return rec, fmt.Errorf("ledger append: %w", err)
		}
		rec.Ledger = &le
	}
	raw, _ := json.MarshalIndent(rec, "", "  ")
	if err := e.Repo.PutPublic(ctx, "proofs/"+env.Digest()+".json", raw); err != nil {
		e.log("warning: could not store proof in repository: %v", err)
	}
	return rec, nil
}

// FindParent returns the newest snapshot of the same source from this host.
func FindParent(ctx context.Context, r *repo.Repo, sourceName string) *snapshot.WithID {
	all, err := snapshot.List(ctx, r)
	if err != nil {
		return nil
	}
	host, _ := os.Hostname()
	for _, s := range all {
		if s.Source.Name == sourceName && s.Host == host {
			s := s
			return &s
		}
	}
	return nil
}

// Backup captures a source, commits a snapshot and attests it.
func Backup(ctx context.Context, e *Env, spec source.Spec, tags []string) (snapshot.WithID, *Record, error) {
	if err := spec.Validate(); err != nil {
		return snapshot.WithID{}, nil, err
	}
	if err := e.checkDenied(spec); err != nil {
		return snapshot.WithID{}, nil, err
	}
	parent := FindParent(ctx, e.Repo, spec.Name)
	b, err := engine.NewBuilder(ctx, e.Repo, engine.Options{Parent: parent, Excludes: spec.Excludes, Log: e.Log, Workers: e.Workers, DenyPaths: e.DenyPaths})
	if err != nil {
		return snapshot.WithID{}, nil, err
	}
	meta, err := source.Backup(ctx, spec, b, e.Log)
	if err != nil {
		return snapshot.WithID{}, nil, err
	}
	snap, err := b.Commit(ctx, &snapshot.Snapshot{
		Source: snapshot.Source{Name: spec.Name, Kind: spec.Kind}, Tags: tags, Paths: spec.Paths, SourceMeta: meta,
	})
	if err != nil {
		return snapshot.WithID{}, nil, err
	}
	st := snap.Stats
	e.log("snapshot %s saved: %d entries, %d bytes scanned, %d new chunks (%d bytes uploaded) in %dms",
		snap.ID.Short(), snap.Entries, st.Bytes, st.NewChunks, st.Uploaded, st.DurationMs)

	pred := proof.BackupPredicate{
		RepoID: e.Repo.Config().ID, SnapshotID: snap.ID.String(), Source: spec.Name, Kind: spec.Kind,
		Host: snap.Host, Time: snap.Time, Entries: snap.Entries, Bytes: st.Bytes, DurationMs: st.DurationMs,
		Storage: e.Storage, SourceMeta: publicMeta(meta), Engine: engine.Version,
	}
	rec, err := e.attest(ctx, "backup", proof.SnapshotSubject(pred.RepoID, pred.SnapshotID, snap.Root), proof.PredicateBackup, pred, pred.SnapshotID, spec.Name, true)
	return snap, rec, err
}

// publicMeta drops fields that may contain paths of secrets; table names and
// counts are kept because reconciliation evidence depends on them.
func publicMeta(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		switch k {
		case "importedFrom", "originalId", "originalTime", "originalLabel", "importedAt", "tables", "countsExact", "serverVersion", "serverVersionNum", "serverMajor", "dumpBytes", "dumpFormat", "dumpTool", "integrity", "oplog", "database":
			out[k] = v
		}
	}
	return out
}

// Drill restores a snapshot, runs all checks and attests the outcome
// (pass or fail).
func Drill(ctx context.Context, e *Env, spec source.Spec, ref string, workDir string) (*drill.Result, *Record, error) {
	return DrillAttested(ctx, e, spec, ref, "", workDir)
}

// DrillAttested restore-tests a specific snapshot and refuses to start unless
// its content root equals expectedRoot (when given), the root from the
// verified backup attestation.
func DrillAttested(ctx context.Context, e *Env, spec source.Spec, ref, expectedRoot, workDir string) (*drill.Result, *Record, error) {
	if ref == "" {
		ref = "latest:" + spec.Name
	}
	snap, err := snapshot.Resolve(ctx, e.Repo, ref)
	if err != nil {
		return nil, nil, err
	}
	if expectedRoot != "" && snap.Root != expectedRoot {
		return nil, nil, fmt.Errorf("snapshot %s does not match the verified backup proof (content root differs); refusing to restore it", snap.ID.Short())
	}
	if snap.Source.Name != spec.Name {
		return nil, nil, fmt.Errorf("snapshot %s belongs to source %q, not %q", snap.ID.Short(), snap.Source.Name, spec.Name)
	}
	if spec.Kind == "" {
		spec.Kind = snap.Source.Kind
	}
	e.log("restore drill for %s, snapshot %s from %s", spec.Name, snap.ID.Short(), snap.Time.Format(time.RFC3339))
	res, err := drill.Run(ctx, e.Repo, snap, spec, drill.Options{WorkDir: workDir, Log: e.Log})
	if err != nil {
		return nil, nil, err
	}
	host, _ := os.Hostname()
	pred := proof.DrillPredicate{
		RepoID: e.Repo.Config().ID, SnapshotID: snap.ID.String(), Source: spec.Name, Kind: spec.Kind,
		SnapshotTime: snap.Time, ExpectedRoot: snap.Root, RestoredRoot: res.RestoredRoot,
		Verifier: host, Sandbox: res.Sandbox, StartedAt: res.StartedAt, FinishedAt: res.FinishedAt,
		RTOMs: res.FinishedAt.Sub(res.StartedAt).Milliseconds(), RestoredBytes: res.RestoredBytes,
		Checks: res.Checks, Passed: res.Passed, Engine: engine.Version,
	}
	rec, err := e.attest(ctx, "drill", proof.SnapshotSubject(pred.RepoID, pred.SnapshotID, snap.Root), proof.PredicateDrill, pred, pred.SnapshotID, spec.Name, res.Passed)
	if err != nil {
		return res, rec, err
	}
	if !res.Passed {
		return res, rec, errors.New("restore drill FAILED — see checks")
	}
	return res, rec, nil
}

// LoadRecords reads all proof records stored in the repository.
func LoadRecords(ctx context.Context, r *repo.Repo) ([]Record, error) {
	var out []Record
	var keys []string
	err := r.Backend().List(ctx, "proofs/", func(o backendInfo) error {
		keys = append(keys, o.Key)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		raw, err := r.Backend().Get(ctx, k)
		if err != nil {
			continue
		}
		var rec Record
		if json.Unmarshal(raw, &rec) == nil {
			out = append(out, rec)
		}
	}
	return out, nil
}

// Maintain applies a source's retention policy (never forgetting the newest
// verified snapshots) and reclaims unreferenced data.
func Maintain(ctx context.Context, e *Env, sourceName string, policy retention.Policy, verified []string) (forgotten int, pruned engine.PruneResult, err error) {
	if policy.Empty() {
		return 0, pruned, nil
	}
	all, err := snapshot.List(ctx, e.Repo)
	if err != nil {
		return 0, pruned, err
	}
	isVerified := map[string]bool{}
	for _, id := range verified {
		isVerified[id] = true
	}
	var items []retention.Item
	for _, s := range all {
		if s.Source.Name == sourceName {
			items = append(items, retention.Item{ID: s.ID.String(), Time: s.Time, Verified: isVerified[s.ID.String()]})
		}
	}
	var forget []string
	for _, d := range retention.Apply(items, policy, time.Local) {
		if !d.Keep {
			forget = append(forget, d.ID)
		}
	}
	if len(forget) > 0 {
		e.log("retention: forgetting %d of %d snapshots of %s", len(forget), len(items), sourceName)
		if err := engine.Forget(ctx, e.Repo, forget); err != nil {
			return 0, pruned, err
		}
	}
	pruned, err = engine.Prune(ctx, e.Repo, engine.PruneOptions{Log: e.Log})
	return len(forget), pruned, err
}

// ImportResult summarises an import run.
type ImportResult struct {
	Found    int      `json:"found"`
	Imported int      `json:"imported"`
	Skipped  int      `json:"skipped"`
	Failed   []string `json:"failed,omitempty"`
	Bytes    int64    `json:"bytes"`
}

// Import converts restore points from another backup tool into snapshots of
// spec.Name. Each becomes a normal snapshot (restore-testable, attested) that
// keeps its original time and records where it came from. Points already
// imported are skipped, so it is safe to run repeatedly.
func Import(ctx context.Context, e *Env, spec source.Spec) (*ImportResult, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := e.checkDenied(spec); err != nil {
		return nil, err
	}
	src, err := importer.Open(ctx, *spec.Import)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	points, err := src.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading old backups: %w", err)
	}
	res := &ImportResult{Found: len(points)}
	e.log("found %d old backup(s) to convert (%s format)", len(points), src.Format())
	done := map[string]bool{}
	if all, err := snapshot.List(ctx, e.Repo); err == nil {
		for _, s := range all {
			if id, ok := s.SourceMeta["originalId"].(string); ok && s.Source.Name == spec.Name {
				done[id] = true
			}
		}
	}
	for _, p := range points {
		if done[p.ID] {
			res.Skipped++
			continue
		}
		e.log("converting %s from %s (%d files)", p.Label, p.Time.Local().Format("2006-01-02 15:04"), p.Files)
		b, err := engine.NewBuilder(ctx, e.Repo, engine.Options{Log: e.Log, Workers: e.Workers})
		if err != nil {
			return res, err
		}
		if err := src.Fill(ctx, p, b, e.Log); err != nil {
			e.log("could not convert %s: %v", p.ID, err)
			res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", p.ID, err))
			continue
		}
		meta := map[string]any{"importedFrom": src.Format(), "originalId": p.ID, "originalTime": p.Time, "originalLabel": p.Label, "importedAt": time.Now().UTC()}
		snap, err := b.Commit(ctx, &snapshot.Snapshot{
			Time: p.Time, Host: "imported", Source: snapshot.Source{Name: spec.Name, Kind: "import"},
			Tags: []string{"imported", "from:" + src.Format()}, SourceMeta: meta,
		})
		if err != nil {
			res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", p.ID, err))
			continue
		}
		res.Imported++
		res.Bytes += snap.Stats.Bytes
		pred := proof.BackupPredicate{
			RepoID: e.Repo.Config().ID, SnapshotID: snap.ID.String(), Source: spec.Name, Kind: "import",
			Host: "imported", Time: snap.Time, Entries: snap.Entries, Bytes: snap.Stats.Bytes, DurationMs: snap.Stats.DurationMs,
			Storage: e.Storage, SourceMeta: publicMeta(meta), Engine: engine.Version,
		}
		if _, err := e.attest(ctx, "backup", proof.SnapshotSubject(pred.RepoID, pred.SnapshotID, snap.Root), proof.PredicateBackup, pred, pred.SnapshotID, spec.Name, true); err != nil {
			return res, err
		}
		e.log("converted %s → backup copy %s", p.Label, snap.ID.Short())
	}
	if len(res.Failed) > 0 && res.Imported == 0 {
		return res, fmt.Errorf("none of the old backups could be converted: %s", res.Failed[0])
	}
	return res, nil
}

var errProtected = errors.New("this location belongs to the BackupProof server itself and can't be backed up, imported or used as storage from here")

// checkDenied refuses specs that would read the denied paths directly. File
// trees that merely contain a denied path are handled by the engine, which
// skips it while walking.
func (e *Env) checkDenied(spec source.Spec) error {
	if len(e.DenyPaths) == 0 {
		return nil
	}
	if spec.Kind == "sqlite" {
		for _, p := range spec.Paths {
			if engine.IsDenied(p, e.DenyPaths) {
				return errProtected
			}
		}
	}
	if im := spec.Import; im != nil {
		if im.Storage.Type == "local" && engine.IsDenied(im.Storage.Path, e.DenyPaths) {
			return errProtected
		}
		if im.Storage.Type == "rclone" && strings.HasPrefix(im.Storage.Remote, ":local") {
			if _, p, ok := strings.Cut(strings.TrimPrefix(im.Storage.Remote, ":local"), ":"); ok && engine.IsDenied(p, e.DenyPaths) {
				return errProtected
			}
		}
	}
	return nil
}

// StorageDenied reports whether a local storage location is protected.
func StorageDenied(path string, deny []string) error {
	if engine.IsDenied(path, deny) {
		return errProtected
	}
	return nil
}
