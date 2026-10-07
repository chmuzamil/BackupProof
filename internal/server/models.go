package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/retention"
	"github.com/chmuzamil/backupproof/internal/source"
)

// --- repositories --------------------------------------------------------

type Repository struct {
	ID      int64          `json:"id"`
	Name    string         `json:"name"`
	Backend backend.Config `json:"backend"`
	RepoID  string         `json:"repoId,omitempty"`
	Created string         `json:"created"`
}

// RepoSecret is everything needed to open a repository; it is stored
// encrypted and only ever sent to agents inside a job lease.
type RepoSecret struct {
	Password    string              `json:"password"`
	Credentials backend.Credentials `json:"credentials"`
}

func (s *Store) CreateRepository(name string, cfg backend.Config, sec RepoSecret) (int64, error) {
	if name == "" {
		return 0, errors.New("name is required")
	}
	if len(sec.Password) < 12 {
		return 0, errors.New("repository password must be at least 12 characters")
	}
	b, _ := json.Marshal(cfg)
	ct, err := s.seal(sec, "repo:"+name)
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec("INSERT INTO repositories(name,backend,secret,created) VALUES(?,?,?,?)", name, string(b), ct, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ErrRepositoryInUse is returned when storage that items still use is removed.
type ErrRepositoryInUse []string

func (e ErrRepositoryInUse) Error() string {
	return "this storage is still used by " + strings.Join(e, ", ") + ". Remove those items, or move them to other storage, first"
}

// DeleteRepository forgets a storage location. The backups already in it are
// not touched; with its password (the recovery kit) it can be added again.
func (s *Store) DeleteRepository(id int64) error {
	rows, err := s.db.Query("SELECT name FROM sources WHERE repo_id=? ORDER BY name", id)
	if err != nil {
		return err
	}
	var users ErrRepositoryInUse
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		users = append(users, "“"+n+"”")
	}
	rows.Close()
	if len(users) > 0 {
		return users
	}
	res, err := s.db.Exec("DELETE FROM repositories WHERE id=?", id)
	if err != nil {
		return err
	}
	_, _ = s.db.Exec("DELETE FROM repo_stats WHERE repo_id=?", id)
	_, _ = s.db.Exec("DELETE FROM settings WHERE key=?", fmt.Sprintf("repo_check_at:%d", id))
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("repository %d not found", id)
	}
	return nil
}

func (s *Store) Repositories() ([]Repository, error) {
	rows, err := s.db.Query("SELECT id,name,backend,IFNULL(repo_id,''),created FROM repositories ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Repository
	for rows.Next() {
		var r Repository
		var b string
		if err := rows.Scan(&r.ID, &r.Name, &b, &r.RepoID, &r.Created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(b), &r.Backend); err != nil {
			return nil, fmt.Errorf("repository %q: storage settings: %w", r.Name, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Repository(id int64) (*Repository, *RepoSecret, error) {
	var r Repository
	var b string
	var ct []byte
	if err := s.db.QueryRow("SELECT id,name,backend,IFNULL(repo_id,''),created,secret FROM repositories WHERE id=?", id).Scan(&r.ID, &r.Name, &b, &r.RepoID, &r.Created, &ct); err != nil {
		return nil, nil, fmt.Errorf("repository %d not found", id)
	}
	if err := json.Unmarshal([]byte(b), &r.Backend); err != nil {
		return nil, nil, fmt.Errorf("repository %q: storage settings: %w", r.Name, err)
	}
	var sec RepoSecret
	if err := s.open(ct, "repo:"+r.Name, &sec); err != nil {
		return nil, nil, err
	}
	return &r, &sec, nil
}

func (s *Store) SetRepoID(id int64, repoID string) error {
	_, err := s.db.Exec("UPDATE repositories SET repo_id=? WHERE id=? AND (repo_id IS NULL OR repo_id='')", repoID, id)
	return err
}

// --- sources -------------------------------------------------------------

type Source struct {
	ID               int64            `json:"id"`
	Name             string           `json:"name"`
	AgentID          int64            `json:"agentId"`
	VerifierID       *int64           `json:"verifierId,omitempty"`
	RepoID           int64            `json:"repoId"`
	Spec             source.Spec      `json:"spec"`
	BackupCron       string           `json:"backupCron"`
	DrillCron        string           `json:"drillCron"`
	Retention        retention.Policy `json:"retention"`
	ProofMaxAgeHours int              `json:"proofMaxAgeHours"`
	Enabled          bool             `json:"enabled"`
	NextBackup       *time.Time       `json:"nextBackup,omitempty"`
	NextDrill        *time.Time       `json:"nextDrill,omitempty"`
	Created          string           `json:"created"`
}

type SourceSecret struct {
	Password string `json:"password,omitempty"`
	URI      string `json:"uri,omitempty"`
	// For imports: the old backup storage keys and vault/restic password.
	ImportPassword    string              `json:"importPassword,omitempty"`
	ImportCredentials backend.Credentials `json:"importCredentials,omitempty"`
	ImportPrivateKey  string              `json:"importPrivateKey,omitempty"`
}

func (s *SourceSecret) empty() bool {
	return s == nil || (s.Password == "" && s.URI == "" && s.ImportPassword == "" && s.ImportPrivateKey == "" && s.ImportCredentials == (backend.Credentials{}))
}

// merge keeps old values for fields the user left blank when editing.
func (s *SourceSecret) merge(old SourceSecret) {
	if s.Password == "" {
		s.Password = old.Password
	}
	if s.URI == "" {
		s.URI = old.URI
	}
	if s.ImportPassword == "" {
		s.ImportPassword = old.ImportPassword
	}
	if s.ImportCredentials == (backend.Credentials{}) {
		s.ImportCredentials = old.ImportCredentials
	}
	if s.ImportPrivateKey == "" {
		s.ImportPrivateKey = old.ImportPrivateKey
	}
}

// DrillAgent is the agent that runs restore drills: a separate verifier when
// configured (separation of duties), otherwise the source's own agent.
func (s *Source) DrillAgent() int64 {
	if s.VerifierID != nil && *s.VerifierID != 0 {
		return *s.VerifierID
	}
	return s.AgentID
}

func (s *Store) SaveSource(src *Source, sec *SourceSecret) (int64, error) {
	src.Spec.Name = src.Name
	if err := src.Spec.Validate(); err != nil {
		return 0, err
	}
	if _, err := ParseSchedule(src.BackupCron); err != nil {
		return 0, fmt.Errorf("backup schedule: %w", err)
	}
	if _, err := ParseSchedule(src.DrillCron); err != nil {
		return 0, fmt.Errorf("drill schedule: %w", err)
	}
	if src.ProofMaxAgeHours <= 0 {
		src.ProofMaxAgeHours = 192
	}
	spec, _ := json.Marshal(src.Spec.Redacted())
	ret, _ := json.Marshal(src.Retention)
	var ct []byte
	if !sec.empty() {
		if src.ID != 0 {
			var oldCT []byte
			if err := s.db.QueryRow("SELECT secret FROM sources WHERE id=?", src.ID).Scan(&oldCT); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, err
			}
			var old SourceSecret
			if s.open(oldCT, "source:"+src.Name, &old) == nil {
				sec.merge(old)
			}
		}
		var err error
		if ct, err = s.seal(sec, "source:"+src.Name); err != nil {
			return 0, err
		}
	}
	if src.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO sources(name,agent_id,verifier_id,repo_id,spec,secret,backup_cron,drill_cron,retention,proof_max_age_hours,enabled,created)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, src.Name, src.AgentID, src.VerifierID, src.RepoID, string(spec), ct, src.BackupCron, src.DrillCron, string(ret), src.ProofMaxAgeHours, src.Enabled, now())
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	q := `UPDATE sources SET agent_id=?,verifier_id=?,repo_id=?,spec=?,backup_cron=?,drill_cron=?,retention=?,proof_max_age_hours=?,enabled=?,next_backup=NULL,next_drill=NULL`
	args := []any{src.AgentID, src.VerifierID, src.RepoID, string(spec), src.BackupCron, src.DrillCron, string(ret), src.ProofMaxAgeHours, src.Enabled}
	if ct != nil {
		q += ",secret=?"
		args = append(args, ct)
	}
	_, err := s.db.Exec(q+" WHERE id=?", append(args, src.ID)...)
	return src.ID, err
}

const sourceCols = "id,name,agent_id,verifier_id,repo_id,spec,backup_cron,drill_cron,retention,proof_max_age_hours,enabled,next_backup,next_drill,created"

func scanSource(row interface{ Scan(...any) error }) (*Source, error) {
	var src Source
	var spec, ret string
	var verifier sql.NullInt64
	var nb, nd sql.NullString
	if err := row.Scan(&src.ID, &src.Name, &src.AgentID, &verifier, &src.RepoID, &spec, &src.BackupCron, &src.DrillCron, &ret, &src.ProofMaxAgeHours, &src.Enabled, &nb, &nd, &src.Created); err != nil {
		return nil, err
	}
	if verifier.Valid {
		src.VerifierID = &verifier.Int64
	}
	if err := json.Unmarshal([]byte(spec), &src.Spec); err != nil {
		return nil, fmt.Errorf("source %q: spec: %w", src.Name, err)
	}
	if err := json.Unmarshal([]byte(ret), &src.Retention); err != nil {
		return nil, fmt.Errorf("source %q: retention: %w", src.Name, err)
	}
	src.NextBackup, src.NextDrill = parseTime(nb), parseTime(nd)
	return &src, nil
}

func (s *Store) Sources() ([]Source, error) {
	rows, err := s.db.Query("SELECT " + sourceCols + " FROM sources ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *src)
	}
	return out, nil
}

func (s *Store) Source(id int64) (*Source, error) {
	src, err := scanSource(s.db.QueryRow("SELECT "+sourceCols+" FROM sources WHERE id=?", id))
	if err != nil {
		return nil, fmt.Errorf("source %d not found", id)
	}
	return src, nil
}

// SourceWithSecrets returns the full spec including credentials, for job leases.
func (s *Store) SourceWithSecrets(id int64) (*Source, error) {
	src, err := s.Source(id)
	if err != nil {
		return nil, err
	}
	var ct []byte
	if err := s.db.QueryRow("SELECT secret FROM sources WHERE id=?", id).Scan(&ct); err != nil {
		return nil, err
	}
	var sec SourceSecret
	if err := s.open(ct, "source:"+src.Name, &sec); err != nil {
		return nil, err
	}
	src.Spec.Password, src.Spec.URI = sec.Password, sec.URI
	if src.Spec.Import != nil {
		src.Spec.Import.Password, src.Spec.Import.Credentials, src.Spec.Import.PrivateKey = sec.ImportPassword, sec.ImportCredentials, sec.ImportPrivateKey
	}
	if src.Spec.URI == "(redacted)" {
		src.Spec.URI = ""
	}
	return src, nil
}

// DeleteSource removes an item. Its open alerts are resolved (they can't be
// fixed any more) and, like its proofs, unlinked, because SQLite may give the
// next new item the same ID. Backups, proofs and alert history are kept.
func (s *Store) DeleteSource(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("DELETE FROM sources WHERE id=?", id); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE alerts SET resolved=COALESCE(resolved, ?), source_id=NULL WHERE source_id=?", now(), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetNextRuns(id int64, nextBackup, nextDrill time.Time) error {
	_, err := s.db.Exec("UPDATE sources SET next_backup=?, next_drill=? WHERE id=?",
		nextBackup.UTC().Format(time.RFC3339Nano), nextDrill.UTC().Format(time.RFC3339Nano), id)
	return err
}

// --- jobs ----------------------------------------------------------------

type Job struct {
	ID       int64           `json:"id"`
	Kind     string          `json:"kind"` // backup | drill | maintain
	SourceID int64           `json:"sourceId"`
	AgentID  int64           `json:"agentId"`
	State    string          `json:"state"` // queued | running | succeeded | failed
	Trigger  string          `json:"trigger"`
	Created  string          `json:"created"`
	Started  *time.Time      `json:"started,omitempty"`
	Finished *time.Time      `json:"finished,omitempty"`
	Error    string          `json:"error,omitempty"`
	Log      string          `json:"log,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
}

func (s *Store) EnqueueJob(kind string, sourceID, agentID int64, trigger string) (int64, error) {
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM jobs WHERE source_id=? AND kind=? AND state IN ('queued','running')", sourceID, kind).Scan(&n); err != nil {
		return 0, err
	}
	if n > 0 {
		return 0, errors.New("a " + kind + " job for this source is already queued or running")
	}
	res, err := s.db.Exec("INSERT INTO jobs(kind,source_id,agent_id,state,trigger,created) VALUES(?,?,?,'queued',?,?)", kind, sourceID, agentID, trigger, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// LeaseJob hands the oldest queued job for agentID to that agent.
func (s *Store) LeaseJob(agentID int64, lease time.Duration) (*Job, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	var id int64
	err := s.db.QueryRow("SELECT id FROM jobs WHERE agent_id=? AND state='queued' ORDER BY id LIMIT 1", agentID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec("UPDATE jobs SET state='running', started=?, lease_expires=? WHERE id=?",
		now(), time.Now().Add(lease).UTC().Format(time.RFC3339Nano), id); err != nil {
		return nil, err
	}
	return s.Job(id)
}

func (s *Store) ExtendLease(jobID, agentID int64, lease time.Duration, logLines string) error {
	res, err := s.db.Exec("UPDATE jobs SET lease_expires=?, log=substr(log || ?, -200000) WHERE id=? AND agent_id=? AND state='running'",
		time.Now().Add(lease).UTC().Format(time.RFC3339Nano), logLines, jobID, agentID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("job is not running on this agent")
	}
	return nil
}

func (s *Store) FinishJob(jobID, agentID int64, ok bool, errMsg string, result any) error {
	state := "succeeded"
	if !ok {
		state = "failed"
	}
	var rb []byte
	if result != nil {
		rb, _ = json.Marshal(result)
	}
	res, err := s.db.Exec("UPDATE jobs SET state=?, finished=?, error=?, result=?, lease_expires=NULL WHERE id=? AND agent_id=? AND state='running'",
		state, now(), errMsg, string(rb), jobID, agentID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("job is not running on this agent")
	}
	return nil
}

// ExpireLeases fails jobs whose agent stopped heartbeating.
func (s *Store) ExpireLeases() ([]Job, error) {
	rows, err := s.db.Query("SELECT id FROM jobs WHERE state='running' AND lease_expires < ?", now())
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	var out []Job
	var errs []error
	for _, id := range ids {
		if _, err := s.db.Exec("UPDATE jobs SET state='failed', finished=?, error='the server running this stopped responding' WHERE id=? AND state='running'", now(), id); err != nil {
			errs = append(errs, fmt.Errorf("job #%d: %w", id, err))
			continue
		}
		if j, err := s.Job(id); err == nil {
			out = append(out, *j)
		}
	}
	return out, errors.Join(errs...)
}

const jobCols = "id,kind,source_id,agent_id,state,trigger,created,started,finished,IFNULL(error,''),log,IFNULL(result,'')"

func scanJob(row interface{ Scan(...any) error }, withLog bool) (*Job, error) {
	var j Job
	var st, fi sql.NullString
	var result string
	if err := row.Scan(&j.ID, &j.Kind, &j.SourceID, &j.AgentID, &j.State, &j.Trigger, &j.Created, &st, &fi, &j.Error, &j.Log, &result); err != nil {
		return nil, err
	}
	j.Started, j.Finished = parseTime(st), parseTime(fi)
	if result != "" {
		j.Result = json.RawMessage(result)
	}
	if !withLog {
		j.Log = ""
	}
	return &j, nil
}

func (s *Store) Job(id int64) (*Job, error) {
	return scanJob(s.db.QueryRow("SELECT "+jobCols+" FROM jobs WHERE id=?", id), true)
}

func (s *Store) Jobs(sourceID int64, limit int) ([]Job, error) {
	q := "SELECT " + jobCols + " FROM jobs"
	var args []any
	if sourceID > 0 {
		q += " WHERE source_id=?"
		args = append(args, sourceID)
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, nil
}

func (s *Store) LastJob(sourceID int64, kind, state string) *Job {
	j, err := scanJob(s.db.QueryRow("SELECT "+jobCols+" FROM jobs WHERE source_id=? AND kind=? AND state=? ORDER BY id DESC LIMIT 1", sourceID, kind, state), false)
	if err != nil {
		return nil
	}
	return j
}

// --- proofs --------------------------------------------------------------

type ProofRow struct {
	ID         int64            `json:"id"`
	Kind       string           `json:"kind"`
	SourceID   *int64           `json:"sourceId,omitempty"`
	SourceName string           `json:"sourceName"`
	SnapshotID string           `json:"snapshotId"`
	Passed     bool             `json:"passed"`
	RTOMs      *int64           `json:"rtoMs,omitempty"`
	Envelope   *proof.Envelope  `json:"envelope,omitempty"`
	Timestamp  *proof.Timestamp `json:"timestamp,omitempty"`
	LedgerSeq  int64            `json:"ledgerSeq"`
	Signer     string           `json:"signer"`
	Created    string           `json:"created"`
	Predicate  json.RawMessage  `json:"predicate,omitempty"`
}

func insertProof(tx *sql.Tx, p *ProofRow) error {
	env, _ := json.Marshal(p.Envelope)
	var ts []byte
	if p.Timestamp != nil {
		ts, _ = json.Marshal(p.Timestamp)
	}
	_, err := tx.Exec(`INSERT INTO proofs(kind,source_id,source_name,snapshot_id,passed,rto_ms,envelope,timestamp,ledger_seq,signer,created)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, p.Kind, p.SourceID, p.SourceName, p.SnapshotID, p.Passed, p.RTOMs, string(env), nullStr(ts), p.LedgerSeq, p.Signer, now())
	return err
}

func nullStr(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

const proofCols = "id,kind,source_id,source_name,snapshot_id,passed,rto_ms,envelope,timestamp,ledger_seq,signer,created"

func scanProof(row interface{ Scan(...any) error }, full bool) (*ProofRow, error) {
	var p ProofRow
	var sid, rto sql.NullInt64
	var env string
	var ts sql.NullString
	if err := row.Scan(&p.ID, &p.Kind, &sid, &p.SourceName, &p.SnapshotID, &p.Passed, &rto, &env, &ts, &p.LedgerSeq, &p.Signer, &p.Created); err != nil {
		return nil, err
	}
	if sid.Valid {
		p.SourceID = &sid.Int64
	}
	if rto.Valid {
		p.RTOMs = &rto.Int64
	}
	var e proof.Envelope
	if err := json.Unmarshal([]byte(env), &e); err != nil {
		return nil, fmt.Errorf("proof #%d: envelope: %w", p.ID, err)
	}
	if st, err := proof.ParseStatement(&e); err == nil {
		p.Predicate = st.Predicate
	}
	if ts.Valid {
		p.Timestamp = &proof.Timestamp{}
		if err := json.Unmarshal([]byte(ts.String), p.Timestamp); err != nil {
			return nil, fmt.Errorf("proof #%d: timestamp: %w", p.ID, err)
		}
	}
	if full {
		p.Envelope = &e
	}
	return &p, nil
}

func (s *Store) Proofs(sourceID int64, limit int) ([]ProofRow, error) {
	q := "SELECT " + proofCols + " FROM proofs"
	var args []any
	if sourceID > 0 {
		q += " WHERE source_id=?"
		args = append(args, sourceID)
	}
	q += " ORDER BY id DESC LIMIT ?"
	rows, err := s.db.Query(q, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProofRow
	for rows.Next() {
		p, err := scanProof(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, nil
}

func (s *Store) Proof(id int64) (*ProofRow, error) {
	return scanProof(s.db.QueryRow("SELECT "+proofCols+" FROM proofs WHERE id=?", id), true)
}

func (s *Store) ProofsBetween(from, to time.Time) ([]ProofRow, error) {
	rows, err := s.db.Query("SELECT "+proofCols+" FROM proofs WHERE created >= ? AND created <= ? ORDER BY id",
		from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProofRow
	for rows.Next() {
		p, err := scanProof(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, nil
}

func (s *Store) LastProof(sourceID int64, kind string, passedOnly bool) *ProofRow {
	q := "SELECT " + proofCols + " FROM proofs WHERE source_id=? AND kind=?"
	if passedOnly {
		q += " AND passed=1"
	}
	p, err := scanProof(s.db.QueryRow(q+" ORDER BY id DESC LIMIT 1", sourceID, kind), false)
	if err != nil {
		return nil
	}
	return p
}

// VerifiedSnapshots lists snapshot IDs with a passing drill (for retention).
func (s *Store) VerifiedSnapshots(sourceID int64) ([]string, error) {
	rows, err := s.db.Query("SELECT DISTINCT snapshot_id FROM proofs WHERE source_id=? AND kind='drill' AND passed=1", sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// --- alerts --------------------------------------------------------------

type Alert struct {
	ID       int64      `json:"id"`
	SourceID *int64     `json:"sourceId,omitempty"`
	AgentID  *int64     `json:"agentId,omitempty"`
	Kind     string     `json:"kind"`
	Message  string     `json:"message"`
	Created  string     `json:"created"`
	Resolved *time.Time `json:"resolved,omitempty"`
}

// RaiseAlert opens an alert unless an identical one is already open.
// It reports whether a new alert was created (so notifications fire once).
func (s *Store) RaiseAlert(kind string, sourceID, agentID *int64, msg string) bool {
	res, err := s.db.Exec("INSERT INTO alerts(source_id,agent_id,kind,message,created) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING", sourceID, agentID, kind, msg, now())
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

func (s *Store) ResolveAlerts(kind string, sourceID, agentID *int64) int64 {
	res, err := s.db.Exec("UPDATE alerts SET resolved=? WHERE resolved IS NULL AND kind=? AND IFNULL(source_id,0)=IFNULL(?,0) AND IFNULL(agent_id,0)=IFNULL(?,0)", now(), kind, sourceID, agentID)
	if err != nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return n
}

func (s *Store) Alerts(openOnly bool, limit int) ([]Alert, error) {
	q := "SELECT id,source_id,agent_id,kind,message,created,resolved FROM alerts"
	if openOnly {
		q += " WHERE resolved IS NULL"
	}
	rows, err := s.db.Query(q+" ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		var a Alert
		var sid, aid sql.NullInt64
		var res sql.NullString
		if err := rows.Scan(&a.ID, &sid, &aid, &a.Kind, &a.Message, &a.Created, &res); err != nil {
			return nil, err
		}
		if sid.Valid {
			a.SourceID = &sid.Int64
		}
		if aid.Valid {
			a.AgentID = &aid.Int64
		}
		a.Resolved = parseTime(res)
		out = append(out, a)
	}
	return out, nil
}
