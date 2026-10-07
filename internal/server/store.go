package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bpcrypto "github.com/chmuzamil/backupproof/internal/crypto"
	"github.com/chmuzamil/backupproof/internal/proof"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY, username TEXT UNIQUE NOT NULL, password_hash TEXT NOT NULL,
  role TEXT NOT NULL CHECK (role IN ('admin','operator','auditor')), created TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (
  token_hash TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  csrf TEXT NOT NULL, expires TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS agents (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL, hostname TEXT, os TEXT, version TEXT,
  public_key TEXT NOT NULL, token_hash TEXT UNIQUE NOT NULL, last_seen TEXT, created TEXT NOT NULL,
  revoked INTEGER NOT NULL DEFAULT 0, docker INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS enroll_tokens (
  hash TEXT PRIMARY KEY, expires TEXT NOT NULL, used_at TEXT, created_by TEXT);
CREATE TABLE IF NOT EXISTS repositories (
  id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, backend TEXT NOT NULL, secret BLOB NOT NULL,
  repo_id TEXT, created TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sources (
  id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, agent_id INTEGER NOT NULL REFERENCES agents(id),
  verifier_id INTEGER REFERENCES agents(id), repo_id INTEGER NOT NULL REFERENCES repositories(id),
  spec TEXT NOT NULL, secret BLOB, backup_cron TEXT NOT NULL, drill_cron TEXT NOT NULL,
  retention TEXT NOT NULL DEFAULT '{}', proof_max_age_hours INTEGER NOT NULL DEFAULT 192,
  enabled INTEGER NOT NULL DEFAULT 1, next_backup TEXT, next_drill TEXT, created TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS jobs (
  id INTEGER PRIMARY KEY, kind TEXT NOT NULL, source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  agent_id INTEGER NOT NULL, state TEXT NOT NULL, trigger TEXT NOT NULL, lease_expires TEXT,
  created TEXT NOT NULL, started TEXT, finished TEXT, error TEXT, log TEXT NOT NULL DEFAULT '', result TEXT);
CREATE INDEX IF NOT EXISTS jobs_state ON jobs(state, agent_id);
CREATE INDEX IF NOT EXISTS jobs_source ON jobs(source_id, id);
CREATE TABLE IF NOT EXISTS proofs (
  id INTEGER PRIMARY KEY, kind TEXT NOT NULL, source_id INTEGER REFERENCES sources(id) ON DELETE SET NULL,
  source_name TEXT NOT NULL, snapshot_id TEXT NOT NULL, passed INTEGER NOT NULL, rto_ms INTEGER,
  envelope TEXT NOT NULL, timestamp TEXT, ledger_seq INTEGER NOT NULL, signer TEXT NOT NULL, created TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS proofs_source ON proofs(source_id, id);
CREATE TABLE IF NOT EXISTS ledger (
  seq INTEGER PRIMARY KEY, time TEXT NOT NULL, kind TEXT NOT NULL, subject TEXT NOT NULL,
  envelope_digest TEXT NOT NULL, prev TEXT NOT NULL, hash TEXT NOT NULL, detail TEXT);
CREATE TRIGGER IF NOT EXISTS ledger_no_update BEFORE UPDATE ON ledger
  BEGIN SELECT RAISE(ABORT, 'ledger is append-only'); END;
CREATE TRIGGER IF NOT EXISTS ledger_no_delete BEFORE DELETE ON ledger
  BEGIN SELECT RAISE(ABORT, 'ledger is append-only'); END;
CREATE TABLE IF NOT EXISTS alerts (
  id INTEGER PRIMARY KEY, source_id INTEGER, agent_id INTEGER, kind TEXT NOT NULL, message TEXT NOT NULL,
  created TEXT NOT NULL, resolved TEXT);
CREATE UNIQUE INDEX IF NOT EXISTS alerts_open ON alerts(kind, IFNULL(source_id,0), IFNULL(agent_id,0)) WHERE resolved IS NULL;
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

type Store struct {
	db *sql.DB
	// wmu serializes writes that must be atomic with respect to the ledger head.
	wmu    sync.Mutex
	secret []byte
}

func OpenStore(dataDir string, secret []byte) (*Store, error) {
	dsn := "file:" + filepath.ToSlash(filepath.Join(dataDir, "backupproof.db")) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// Additive migrations for databases created by older versions.
	for _, m := range append(append([]string{"ALTER TABLE agents ADD COLUMN inventory TEXT"}, accountMigrations...), append(storageMigrations, limitMigrations...)...) {
		// "duplicate column" on up-to-date databases is expected.
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	// Older versions left the alerts of removed items open, linked to an item
	// that no longer exists (and whose ID a new item could reuse).
	if _, err := db.Exec("UPDATE alerts SET resolved=COALESCE(resolved, ?), source_id=NULL WHERE source_id IS NOT NULL AND source_id NOT IN (SELECT id FROM sources)", now()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db, secret: secret}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func parseTime(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s.String)
	if err != nil {
		return nil
	}
	return &t
}

// --- secrets -------------------------------------------------------------

func (s *Store) seal(v any, aad string) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return bpcrypto.SealWithKey(s.secret, b, []byte(aad))
}

func (s *Store) open(ct []byte, aad string, v any) error {
	if len(ct) == 0 {
		return nil
	}
	b, err := bpcrypto.OpenWithKey(s.secret, ct, []byte(aad))
	if err != nil {
		return errors.New("cannot decrypt stored secret (wrong BP_SECRET_KEY?)")
	}
	return json.Unmarshal(b, v)
}

// --- settings ------------------------------------------------------------

func (s *Store) Setting(key string) string {
	var v string
	if err := s.db.QueryRow("SELECT value FROM settings WHERE key=?", key).Scan(&v); err != nil {
		// Unset (sql.ErrNoRows) or unreadable: callers treat both as "not configured".
		return ""
	}
	return v
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec("INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return err
}

// --- ledger --------------------------------------------------------------

func scanEntry(row interface{ Scan(...any) error }) (proof.LedgerEntry, error) {
	var e proof.LedgerEntry
	var t string
	err := row.Scan(&e.Seq, &t, &e.Kind, &e.Subject, &e.EnvelopeDigest, &e.Prev, &e.Hash)
	if err == nil {
		e.Time, _ = time.Parse(time.RFC3339Nano, t)
	}
	return e, err
}

const ledgerCols = "seq,time,kind,subject,envelope_digest,prev,hash"

func (s *Store) LedgerHead() (*proof.LedgerEntry, error) {
	e, err := scanEntry(s.db.QueryRow("SELECT " + ledgerCols + " FROM ledger ORDER BY seq DESC LIMIT 1"))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// AppendLedger appends an entry atomically with optional extra writes in
// the same transaction (e.g. inserting the proof row it refers to).
func (s *Store) AppendLedger(kind, subject, digest, detail string, extra func(tx *sql.Tx, e proof.LedgerEntry) error) (proof.LedgerEntry, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return proof.LedgerEntry{}, err
	}
	defer tx.Rollback()
	var head *proof.LedgerEntry
	if h, err := scanEntry(tx.QueryRow("SELECT " + ledgerCols + " FROM ledger ORDER BY seq DESC LIMIT 1")); err == nil {
		head = &h
	} else if !errors.Is(err, sql.ErrNoRows) {
		return proof.LedgerEntry{}, err
	}
	e := proof.NextEntry(head, kind, subject, digest, time.Now())
	if _, err := tx.Exec("INSERT INTO ledger(seq,time,kind,subject,envelope_digest,prev,hash,detail) VALUES(?,?,?,?,?,?,?,?)",
		e.Seq, e.Time.Format(time.RFC3339Nano), e.Kind, e.Subject, e.EnvelopeDigest, e.Prev, e.Hash, detail); err != nil {
		return e, err
	}
	if extra != nil {
		if err := extra(tx, e); err != nil {
			return e, err
		}
	}
	return e, tx.Commit()
}

// Audit records an operator action in the same tamper-evident chain.
func (s *Store) Audit(actor, action, detail string) error {
	digest := bpcrypto.Hash([]byte(actor), []byte{0}, []byte(action), []byte{0}, []byte(detail)).String()
	_, err := s.AppendLedger("event", actor+": "+action, digest, detail, nil)
	return err
}

type LedgerRow struct {
	proof.LedgerEntry
	Detail string `json:"detail,omitempty"`
}

func (s *Store) Ledger(fromSeq int64, limit int) ([]LedgerRow, error) {
	q := "SELECT " + ledgerCols + ",IFNULL(detail,'') FROM ledger WHERE seq >= ? ORDER BY seq"
	args := []any{fromSeq}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LedgerRow
	for rows.Next() {
		var e proof.LedgerEntry
		var t, d string
		if err := rows.Scan(&e.Seq, &t, &e.Kind, &e.Subject, &e.EnvelopeDigest, &e.Prev, &e.Hash, &d); err != nil {
			return nil, err
		}
		e.Time, _ = time.Parse(time.RFC3339Nano, t)
		out = append(out, LedgerRow{e, d})
	}
	return out, rows.Err()
}

func (s *Store) LedgerEntries(fromSeq int64) ([]proof.LedgerEntry, error) {
	rows, err := s.Ledger(fromSeq, 0)
	if err != nil {
		return nil, err
	}
	out := make([]proof.LedgerEntry, len(rows))
	for i, r := range rows {
		out[i] = r.LedgerEntry
	}
	return out, nil
}

// --- users & sessions ----------------------------------------------------

type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	Created   string `json:"created"`
	TwoFactor bool   `json:"twoFactor"`
	// ViaToken names the API token a request was made with ("" for a browser session).
	ViaToken string `json:"viaToken,omitempty"`
}

func (s *Store) UserCount() (int, error) {
	var n int
	err := s.db.QueryRow("SELECT count(*) FROM users").Scan(&n)
	return n, err
}

func (s *Store) CreateUser(username, password, role string) (int64, error) {
	if len(password) < 10 {
		return 0, errors.New("password must be at least 10 characters")
	}
	h, err := bpcrypto.HashPassword(password)
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec("INSERT INTO users(username,password_hash,role,created) VALUES(?,?,?,?)", username, h, role, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Authenticate(username, password string) (*User, error) {
	var u User
	var h string
	err := s.db.QueryRow("SELECT id,username,role,created,totp_enabled,password_hash FROM users WHERE username=?", username).Scan(&u.ID, &u.Username, &u.Role, &u.Created, &u.TwoFactor, &h)
	if err != nil {
		bpcrypto.CheckPassword("$argon2id$v=19$m=65536,t=3,p=2$00$00", password) // equalize timing
		return nil, errors.New("invalid username or password")
	}
	if !bpcrypto.CheckPassword(h, password) {
		return nil, errors.New("invalid username or password")
	}
	return &u, nil
}

func (s *Store) UserByID(id int64) (*User, error) {
	var u User
	err := s.db.QueryRow("SELECT id,username,role,created,totp_enabled FROM users WHERE id=?", id).Scan(&u.ID, &u.Username, &u.Role, &u.Created, &u.TwoFactor)
	if err != nil {
		return nil, fmt.Errorf("user %d not found", id)
	}
	return &u, nil
}

func (s *Store) Users() ([]User, error) {
	rows, err := s.db.Query("SELECT id,username,role,created,totp_enabled FROM users ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.Created, &u.TwoFactor); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) DeleteUser(id int64) error {
	var admins int
	if err := s.db.QueryRow("SELECT count(*) FROM users WHERE role='admin' AND id<>?", id).Scan(&admins); err != nil {
		return err
	}
	if admins == 0 {
		return errors.New("cannot delete the last admin")
	}
	_, err := s.db.Exec("DELETE FROM users WHERE id=?", id)
	return err
}

func (s *Store) SetPassword(id int64, password string) error {
	if len(password) < 10 {
		return errors.New("password must be at least 10 characters")
	}
	h, err := bpcrypto.HashPassword(password)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("UPDATE users SET password_hash=? WHERE id=?", h, id)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec("DELETE FROM sessions WHERE user_id=?", id); err != nil {
		return fmt.Errorf("password changed, but signing out existing sessions failed: %w", err)
	}
	return nil
}

func (s *Store) CreateSession(userID int64, ttl time.Duration) (token, csrf string, err error) {
	token = bpcrypto.RandomToken("bps_")
	csrf = bpcrypto.RandomToken("")
	_, err = s.db.Exec("INSERT INTO sessions(token_hash,user_id,csrf,expires) VALUES(?,?,?,?)",
		bpcrypto.TokenHash(token), userID, csrf, time.Now().Add(ttl).UTC().Format(time.RFC3339Nano))
	return
}

func (s *Store) Session(token string) (*User, string, error) {
	var u User
	var csrf, exp string
	err := s.db.QueryRow(`SELECT u.id,u.username,u.role,u.created,u.totp_enabled,s.csrf,s.expires FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=?`,
		bpcrypto.TokenHash(token)).Scan(&u.ID, &u.Username, &u.Role, &u.Created, &u.TwoFactor, &csrf, &exp)
	if err != nil {
		return nil, "", errors.New("not signed in")
	}
	if t, _ := time.Parse(time.RFC3339Nano, exp); time.Now().After(t) {
		// Expired sessions are rejected on every lookup, so removing the row is
		// only housekeeping; the caller just needs to know it expired.
		_, _ = s.db.Exec("DELETE FROM sessions WHERE token_hash=?", bpcrypto.TokenHash(token))
		return nil, "", errors.New("session expired")
	}
	return &u, csrf, nil
}

func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE token_hash=?", bpcrypto.TokenHash(token))
	return err
}

// --- agents --------------------------------------------------------------

type Agent struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	Hostname  string     `json:"hostname"`
	OS        string     `json:"os"`
	Version   string     `json:"version"`
	PublicKey string     `json:"publicKey"`
	KeyID     string     `json:"keyId"`
	LastSeen  *time.Time `json:"lastSeen,omitempty"`
	Created   string     `json:"created"`
	Revoked   bool       `json:"revoked"`
	Docker    bool       `json:"docker"`
	Online    bool       `json:"online"`
	// Builtin is the agent embedded in the server ("This server").
	Builtin   bool            `json:"builtin"`
	Inventory json.RawMessage `json:"inventory,omitempty"`
	Limits    Limits          `json:"limits"`
}

func (s *Store) CreateEnrollToken(by string, ttl time.Duration) (string, error) {
	tok := bpcrypto.RandomToken("bpe_")
	_, err := s.db.Exec("INSERT INTO enroll_tokens(hash,expires,created_by) VALUES(?,?,?)", bpcrypto.TokenHash(tok), time.Now().Add(ttl).UTC().Format(time.RFC3339Nano), by)
	return tok, err
}

// Enroll consumes a single-use token and registers an agent.
func (s *Store) Enroll(token string, a Agent) (int64, string, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback()
	var exp string
	var used sql.NullString
	if err := tx.QueryRow("SELECT expires,used_at FROM enroll_tokens WHERE hash=?", bpcrypto.TokenHash(token)).Scan(&exp, &used); err != nil {
		return 0, "", errors.New("invalid enrollment token")
	}
	if used.Valid {
		return 0, "", errors.New("enrollment token already used")
	}
	if t, _ := time.Parse(time.RFC3339Nano, exp); time.Now().After(t) {
		return 0, "", errors.New("enrollment token expired")
	}
	if _, err := proof.ParsePublicKey(a.PublicKey); err != nil {
		return 0, "", fmt.Errorf("agent public key: %w", err)
	}
	agentToken := bpcrypto.RandomToken("bpa_")
	res, err := tx.Exec("INSERT INTO agents(name,hostname,os,version,public_key,token_hash,last_seen,created,docker) VALUES(?,?,?,?,?,?,?,?,?)",
		a.Name, a.Hostname, a.OS, a.Version, a.PublicKey, bpcrypto.TokenHash(agentToken), now(), now(), a.Docker)
	if err != nil {
		return 0, "", err
	}
	id, _ := res.LastInsertId()
	if _, err := tx.Exec("UPDATE enroll_tokens SET used_at=? WHERE hash=?", now(), bpcrypto.TokenHash(token)); err != nil {
		return 0, "", err
	}
	return id, agentToken, tx.Commit()
}

const agentCols = "id,name,IFNULL(hostname,''),IFNULL(os,''),IFNULL(version,''),public_key,last_seen,created,revoked,docker,IFNULL(inventory,''),upload_kbps,download_kbps,window_start,window_end"

func scanAgent(row interface{ Scan(...any) error }) (*Agent, error) {
	var a Agent
	var seen sql.NullString
	var inv string
	if err := row.Scan(&a.ID, &a.Name, &a.Hostname, &a.OS, &a.Version, &a.PublicKey, &seen, &a.Created, &a.Revoked, &a.Docker, &inv,
		&a.Limits.UploadKBps, &a.Limits.DownloadKBps, &a.Limits.WindowStart, &a.Limits.WindowEnd); err != nil {
		return nil, err
	}
	a.LastSeen = parseTime(seen)
	if inv != "" {
		a.Inventory = json.RawMessage(inv)
	}
	if pk, err := proof.ParsePublicKey(a.PublicKey); err == nil {
		a.KeyID = pk.KeyID
	}
	a.Online = a.LastSeen != nil && time.Since(*a.LastSeen) < 2*time.Minute && !a.Revoked
	return &a, nil
}

func (s *Store) AgentByToken(token string) (*Agent, error) {
	a, err := scanAgent(s.db.QueryRow("SELECT "+agentCols+" FROM agents WHERE token_hash=?", bpcrypto.TokenHash(token)))
	if err != nil || a.Revoked {
		return nil, errors.New("unknown or revoked agent")
	}
	return a, nil
}

func (s *Store) Agent(id int64) (*Agent, error) {
	return scanAgent(s.db.QueryRow("SELECT "+agentCols+" FROM agents WHERE id=?", id))
}

func (s *Store) Agents() ([]Agent, error) {
	rows, err := s.db.Query("SELECT " + agentCols + " FROM agents ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, nil
}

func (s *Store) TouchAgent(id int64, version string, docker bool) error {
	_, err := s.db.Exec("UPDATE agents SET last_seen=?, version=?, docker=? WHERE id=?", now(), version, docker, id)
	return err
}

func (s *Store) RevokeAgent(id int64) error {
	_, err := s.db.Exec("UPDATE agents SET revoked=1 WHERE id=?", id)
	return err
}

func (s *Store) SetInventory(id int64, inv []byte) error {
	_, err := s.db.Exec("UPDATE agents SET inventory=?, last_seen=? WHERE id=?", string(inv), now(), id)
	return err
}
