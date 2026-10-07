package server

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"time"

	"github.com/chmuzamil/backupproof/internal/engine"
	"github.com/chmuzamil/backupproof/internal/proof"
)

func jsonMarshal(v any) ([]byte, error)   { return json.Marshal(v) }
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
func jsonDigest(v any) [32]byte           { b, _ := json.Marshal(v); return sha256.Sum256(b) }

// allKeys returns the server ledger key and every enrolled agent's key.
func (s *Server) allKeys() []proof.PublicKey {
	keys := []proof.PublicKey{s.key.Public()}
	agents, _ := s.store.Agents()
	for _, a := range agents {
		if pk, err := proof.ParsePublicKey(a.PublicKey); err == nil {
			pk.Name = "agent:" + a.Name
			keys = append(keys, pk)
		}
	}
	return keys
}

func (s *Server) handlePublicKeys(w http.ResponseWriter, r *http.Request) {
	keys := s.allKeys()
	text := ""
	for _, k := range keys {
		text += k.String() + "\n"
	}
	writeJSON(w, 200, map[string]any{"server": keys[0], "agents": keys[1:], "bpkeys": text})
}

func (s *Server) bundleFor(p *ProofRow) (*proof.Bundle, error) {
	entries, err := s.store.LedgerEntries(p.LedgerSeq)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 || entries[0].EnvelopeDigest != p.Envelope.Digest() {
		return nil, errors.New("ledger entry for this proof is missing or does not match")
	}
	cp := proof.SignCheckpoint(s.key, s.publicURL(), entries[len(entries)-1], time.Now())
	return &proof.Bundle{Format: proof.BundleFormat, Envelope: p.Envelope, Timestamp: p.Timestamp,
		Ledger: entries, Checkpoint: &cp, Keys: s.allKeys()}, nil
}

func (s *Server) handleProofBundle(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	p, err := s.store.Proof(id)
	if err != nil {
		writeErr(w, 404, errors.New("proof not found"))
		return
	}
	b, err := s.bundleFor(p)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="backupproof-%s-%s-%s.bundle.json"`, p.Kind, p.SourceName, short(p.SnapshotID)))
	writeJSON(w, 200, b)
}

// --- evidence pack ---------------------------------------------------------------

type Control struct {
	Framework string `json:"framework"`
	ID        string `json:"id"`
	Summary   string `json:"summary"`
	Evidence  string `json:"evidence"`
}

// Controls maps BackupProof evidence to the controls auditors test.
var Controls = []Control{
	{"SOC 2", "A1.2", "Backup processes and recovery infrastructure", "Signed backup attestations per source, storage location and object-lock settings, watchdog alerts for missed backups."},
	{"SOC 2", "A1.3", "Recovery plan procedures are tested", "Signed restore-drill attestations with per-check results and measured RTO; failed drills are recorded, not omitted."},
	{"ISO/IEC 27001:2022", "A.8.13", "Backup copies maintained and regularly tested", "Backup and drill attestations over the period with ledger proof of completeness."},
	{"NIST CSF 2.0", "PR.DS-11 / RC.RP-03", "Backups created, protected, maintained and tested; integrity of backups verified before use", "Content-root verification of every restored file and engine-native integrity checks."},
	{"DORA", "Art. 12", "Backup policies, periodic restoration testing, integrity checks and reconciliations", "Row-count reconciliation against backup-time counts, integrity checks, separate verifier agents."},
	{"NIS2", "Art. 21(2)(c)", "Business continuity: backup management and disaster recovery", "Evidence pack covering schedules, outcomes and recovery times."},
	{"HIPAA", "164.308(a)(7)(ii)(A),(D)", "Data backup plan; testing and revision procedures", "Retrievable exact copies proven by Merkle content-root match after restore."},
}

type SourceSummary struct {
	Source           string     `json:"source"`
	Kind             string     `json:"kind"`
	Backups          int        `json:"backups"`
	Drills           int        `json:"drills"`
	DrillsPassed     int        `json:"drillsPassed"`
	DrillsFailed     int        `json:"drillsFailed"`
	Timestamped      int        `json:"timestamped"`
	LastPassingDrill *time.Time `json:"lastPassingDrill,omitempty"`
	MedianRTOMs      int64      `json:"medianRtoMs,omitempty"`
	MaxRTOMs         int64      `json:"maxRtoMs,omitempty"`
}

type EvidencePack struct {
	Format      string              `json:"format"`
	GeneratedAt time.Time           `json:"generatedAt"`
	From        time.Time           `json:"from"`
	To          time.Time           `json:"to"`
	Server      string              `json:"server"`
	Engine      string              `json:"engine"`
	Keys        []proof.PublicKey   `json:"keys"`
	Controls    []Control           `json:"controls"`
	Summary     []SourceSummary     `json:"summary"`
	Proofs      []PackProof         `json:"proofs"`
	Ledger      []proof.LedgerEntry `json:"ledger"`
	Checkpoint  *proof.Checkpoint   `json:"checkpoint,omitempty"`
	HowToVerify string              `json:"howToVerify"`
}

type PackProof struct {
	Kind       string           `json:"kind"`
	Source     string           `json:"source"`
	SnapshotID string           `json:"snapshotId"`
	Passed     bool             `json:"passed"`
	RTOMs      *int64           `json:"rtoMs,omitempty"`
	LedgerSeq  int64            `json:"ledgerSeq"`
	Signer     string           `json:"signer"`
	Envelope   *proof.Envelope  `json:"envelope"`
	Timestamp  *proof.Timestamp `json:"timestamp,omitempty"`
}

func parseRange(r *http.Request) (time.Time, time.Time, error) {
	to := time.Now().UTC()
	from := to.AddDate(0, 0, -90)
	var err error
	if v := r.URL.Query().Get("from"); v != "" {
		if from, err = time.Parse("2006-01-02", v); err != nil {
			return from, to, errors.New("from must be YYYY-MM-DD")
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if to, err = time.Parse("2006-01-02", v); err != nil {
			return from, to, errors.New("to must be YYYY-MM-DD")
		}
		to = to.Add(24*time.Hour - time.Nanosecond)
	}
	if to.Before(from) {
		return from, to, errors.New("to is before from")
	}
	return from, to, nil
}

func (s *Server) buildEvidence(from, to time.Time) (*EvidencePack, error) {
	rows, err := s.store.ProofsBetween(from, to)
	if err != nil {
		return nil, err
	}
	pack := &EvidencePack{Format: "backupproof-evidence/v1", GeneratedAt: time.Now().UTC(), From: from, To: to,
		Server: s.publicURL(), Engine: engine.Version, Keys: s.allKeys(), Controls: Controls,
		Summary: []SourceSummary{}, Proofs: []PackProof{}, Ledger: []proof.LedgerEntry{},
		HowToVerify: "backupproof proof verify-pack <file> --key <server bpkey1:...>  (keys from GET /api/public/keys, obtained independently)"}
	bySource := map[string]*SourceSummary{}
	rtos := map[string][]int64{}
	var minSeq int64
	for _, p := range rows {
		pack.Proofs = append(pack.Proofs, PackProof{Kind: p.Kind, Source: p.SourceName, SnapshotID: p.SnapshotID, Passed: p.Passed,
			RTOMs: p.RTOMs, LedgerSeq: p.LedgerSeq, Signer: p.Signer, Envelope: p.Envelope, Timestamp: p.Timestamp})
		if minSeq == 0 || p.LedgerSeq < minSeq {
			minSeq = p.LedgerSeq
		}
		sum := bySource[p.SourceName]
		if sum == nil {
			kind := ""
			var pred struct{ Kind string }
			json.Unmarshal(p.Predicate, &pred)
			kind = pred.Kind
			sum = &SourceSummary{Source: p.SourceName, Kind: kind}
			bySource[p.SourceName] = sum
		}
		if p.Timestamp != nil {
			sum.Timestamped++
		}
		switch p.Kind {
		case "backup":
			sum.Backups++
		case "drill":
			sum.Drills++
			if p.Passed {
				sum.DrillsPassed++
				t, _ := time.Parse(time.RFC3339Nano, p.Created)
				if sum.LastPassingDrill == nil || t.After(*sum.LastPassingDrill) {
					sum.LastPassingDrill = &t
				}
				if p.RTOMs != nil {
					rtos[p.SourceName] = append(rtos[p.SourceName], *p.RTOMs)
				}
			} else {
				sum.DrillsFailed++
			}
		}
	}
	for name, sum := range bySource {
		if v := rtos[name]; len(v) > 0 {
			sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
			sum.MedianRTOMs, sum.MaxRTOMs = v[len(v)/2], v[len(v)-1]
		}
		pack.Summary = append(pack.Summary, *sum)
	}
	sort.Slice(pack.Summary, func(i, j int) bool { return pack.Summary[i].Source < pack.Summary[j].Source })
	if minSeq > 0 {
		// The full, unbroken ledger segment from the first proof to the head
		// shows nothing in the period was removed or reordered.
		if pack.Ledger, err = s.store.LedgerEntries(minSeq); err != nil {
			return nil, err
		}
		cp := proof.SignCheckpoint(s.key, s.publicURL(), pack.Ledger[len(pack.Ledger)-1], time.Now())
		pack.Checkpoint = &cp
	}
	return pack, nil
}

func (s *Server) handleEvidence(w http.ResponseWriter, r *http.Request) {
	from, to, err := parseRange(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	pack, err := s.buildEvidence(from, to)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	s.store.Audit(s.actor(r), "export-evidence", fmt.Sprintf("%s to %s, %d proofs", from.Format("2006-01-02"), to.Format("2006-01-02"), len(pack.Proofs)))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="backupproof-evidence-%s-%s.json"`, from.Format("20060102"), to.Format("20060102")))
	writeJSON(w, 200, pack)
}

var reportTmpl = template.Must(template.New("report").Funcs(template.FuncMap{
	"dur": func(ms *int64) string {
		if ms == nil {
			return "—"
		}
		return (time.Duration(*ms) * time.Millisecond).Round(time.Second).String()
	},
	"durv": func(ms int64) string {
		if ms == 0 {
			return "—"
		}
		return (time.Duration(ms) * time.Millisecond).Round(time.Second).String()
	},
	"date": func(t time.Time) string { return t.Format("2006-01-02") },
	"tptr": func(t *time.Time) string {
		if t == nil {
			return "never"
		}
		return t.Format("2006-01-02 15:04 MST")
	},
	"shortid": short,
}).Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Recoverability Evidence Report</title>
<style>
body{font:14px/1.5 system-ui,sans-serif;color:#1b1f24;background:#fff;max-width:960px;margin:24px auto;padding:0 16px}
h1{font-size:22px;margin:0 0 4px}h2{font-size:16px;margin:28px 0 8px;border-bottom:1px solid #ddd;padding-bottom:4px}
table{border-collapse:collapse;width:100%;font-size:13px}th,td{text-align:left;padding:6px 8px;border-bottom:1px solid #eee;vertical-align:top}
th{background:#f6f7f9}.ok{color:#0a7d33;font-weight:600}.bad{color:#b42318;font-weight:600}.muted{color:#5b6470}
code{font:12px ui-monospace,monospace;word-break:break-all}.wrap{overflow-x:auto}
@media print{body{margin:0}}
</style></head><body>
<h1>Recoverability Evidence Report</h1>
<div class="muted">Period {{date .From}} – {{date .To}} · generated {{.GeneratedAt.Format "2006-01-02 15:04 MST"}} · {{.Server}} · {{.Engine}}</div>

<h2>Summary by source</h2><div class="wrap"><table>
<tr><th>Source</th><th>Kind</th><th>Backups</th><th>Drills passed</th><th>Drills failed</th><th>Last proven restore</th><th>Median RTO</th><th>Max RTO</th><th>RFC 3161 timestamped</th></tr>
{{range .Summary}}<tr><td>{{.Source}}</td><td>{{.Kind}}</td><td>{{.Backups}}</td><td class="ok">{{.DrillsPassed}}</td><td class="{{if .DrillsFailed}}bad{{end}}">{{.DrillsFailed}}</td><td>{{tptr .LastPassingDrill}}</td><td>{{durv .MedianRTOMs}}</td><td>{{durv .MaxRTOMs}}</td><td>{{.Timestamped}}</td></tr>
{{else}}<tr><td colspan="9" class="muted">No proofs recorded in this period.</td></tr>{{end}}
</table></div>

<h2>Control mapping</h2><div class="wrap"><table><tr><th>Framework</th><th>Control</th><th>Requirement</th><th>Evidence provided</th></tr>
{{range .Controls}}<tr><td>{{.Framework}}</td><td>{{.ID}}</td><td>{{.Summary}}</td><td>{{.Evidence}}</td></tr>{{end}}
</table></div>

<h2>Attestations</h2><div class="wrap"><table><tr><th>Ledger #</th><th>Type</th><th>Source</th><th>Snapshot</th><th>Result</th><th>RTO</th><th>Signer</th><th>Trusted time</th></tr>
{{range .Proofs}}<tr><td>{{.LedgerSeq}}</td><td>{{.Kind}}</td><td>{{.Source}}</td><td><code>{{shortid .SnapshotID}}</code></td>
<td>{{if .Passed}}<span class="ok">pass</span>{{else}}<span class="bad">FAIL</span>{{end}}</td><td>{{dur .RTOMs}}</td><td>{{.Signer}}</td>
<td>{{if .Timestamp}}{{.Timestamp.Time.Format "2006-01-02 15:04 MST"}}{{else}}<span class="muted">—</span>{{end}}</td></tr>{{end}}
</table></div>

<h2>Integrity</h2>
{{if .Checkpoint}}<p>Ledger checkpoint #{{.Checkpoint.Seq}}, head <code>{{.Checkpoint.Head}}</code>, signed by <code>{{.Checkpoint.KeyID}}</code> at {{.Checkpoint.Time.Format "2006-01-02 15:04 MST"}}.
The ledger segment in the accompanying JSON evidence pack is an unbroken hash chain; removing, editing or reordering any attestation breaks verification.</p>{{end}}
<p>Signing keys:</p><ul>{{range .Keys}}<li><code>{{.KeyID}}</code> {{.Name}}</li>{{end}}</ul>
<p class="muted">Verify independently: <code>{{.HowToVerify}}</code></p>
</body></html>`))

func (s *Server) handleEvidenceReport(w http.ResponseWriter, r *http.Request) {
	from, to, err := parseRange(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	pack, err := s.buildEvidence(from, to)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	reportTmpl.Execute(w, pack)
}

// VerifyPack checks an evidence pack offline: every envelope signature, the
// ledger chain, that each proof is the ledger entry it claims to be, and the
// signed checkpoint.
func VerifyPack(pack *EvidencePack, trusted []proof.PublicKey) []string {
	var errs []string
	if len(trusted) == 0 {
		trusted = pack.Keys
	}
	if err := proof.VerifyChain(pack.Ledger); err != nil {
		errs = append(errs, err.Error())
	}
	bySeq := map[int64]proof.LedgerEntry{}
	for _, e := range pack.Ledger {
		bySeq[e.Seq] = e
	}
	for _, p := range pack.Proofs {
		if _, err := p.Envelope.Verify(trusted); err != nil {
			errs = append(errs, fmt.Sprintf("proof at ledger #%d: %v", p.LedgerSeq, err))
		}
		e, ok := bySeq[p.LedgerSeq]
		if !ok || e.EnvelopeDigest != p.Envelope.Digest() {
			errs = append(errs, fmt.Sprintf("proof at ledger #%d is not the recorded ledger entry", p.LedgerSeq))
		}
	}
	if pack.Checkpoint != nil && len(pack.Ledger) > 0 {
		last := pack.Ledger[len(pack.Ledger)-1]
		if err := pack.Checkpoint.Verify(trusted); err != nil {
			errs = append(errs, "checkpoint: "+err.Error())
		} else if pack.Checkpoint.Head != last.Hash || pack.Checkpoint.Seq != last.Seq {
			errs = append(errs, "checkpoint does not match ledger head")
		}
	}
	return errs
}
