package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 TOTP uses HMAC-SHA1; authenticator apps expect it.
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	bpcrypto "github.com/chmuzamil/backupproof/internal/crypto"
)

// Additive migrations for two-factor sign-in and API tokens.
var accountMigrations = []string{
	"ALTER TABLE users ADD COLUMN totp_secret BLOB",
	"ALTER TABLE users ADD COLUMN totp_enabled INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE users ADD COLUMN totp_last INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE users ADD COLUMN recovery_codes TEXT",
	`CREATE TABLE IF NOT EXISTS api_tokens (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role TEXT NOT NULL CHECK (role IN ('admin','operator','auditor')), token_hash TEXT UNIQUE NOT NULL,
  prefix TEXT NOT NULL, created TEXT NOT NULL, expires TEXT, last_used TEXT)`,
}

// --- TOTP (RFC 6238: HMAC-SHA1, 30-second steps, 6 digits) -----------------

const totpStep = 30

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func newTOTPSecret() []byte {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func totpCode(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1_000_000)
}

// checkTOTP accepts the code for the current step or one step either side
// (clock drift) and returns the step it matched. Steps at or before lastUsed
// are refused, so a code can't be replayed.
func checkTOTP(secret []byte, code string, now time.Time, lastUsed int64) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != 6 {
		return 0, false
	}
	cur := now.Unix() / totpStep
	for _, st := range []int64{cur - 1, cur, cur + 1} {
		if st <= lastUsed {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(totpCode(secret, st)), []byte(code)) == 1 {
			return st, true
		}
	}
	return 0, false
}

func totpURI(secret []byte, username string) string {
	label := "BackupProof:" + username
	return "otpauth://totp/" + urlPathEscape(label) + "?secret=" + b32.EncodeToString(secret) + "&issuer=BackupProof&algorithm=SHA1&digits=6&period=30"
}

func urlPathEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~:@", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// newRecoveryCodes returns ten one-time codes like "k7f3-9qzt-m2xa".
func newRecoveryCodes() []string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	out := make([]string, 10)
	buf := make([]byte, 12)
	for i := range out {
		if _, err := rand.Read(buf); err != nil {
			panic(err)
		}
		var b strings.Builder
		for j, x := range buf {
			if j > 0 && j%4 == 0 {
				b.WriteByte('-')
			}
			b.WriteByte(alphabet[int(x)%len(alphabet)])
		}
		out[i] = b.String()
	}
	return out
}

func normRecovery(code string) string {
	return strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(code), " ", ""), "-", ""))
}

// --- two-factor state ---------------------------------------------------------

type twoFactor struct {
	Enabled bool
	Secret  []byte
	Last    int64
}

func (s *Store) twoFactor(userID int64) (*twoFactor, error) {
	var ct []byte
	var tf twoFactor
	err := s.db.QueryRow("SELECT totp_secret,totp_enabled,totp_last FROM users WHERE id=?", userID).Scan(&ct, &tf.Enabled, &tf.Last)
	if err != nil {
		return nil, err
	}
	if len(ct) > 0 {
		if err := s.open(ct, fmt.Sprintf("totp:%d", userID), &tf.Secret); err != nil {
			return nil, err
		}
	}
	return &tf, nil
}

// BeginTwoFactor stores a new, not yet enabled secret and returns it.
func (s *Store) BeginTwoFactor(userID int64) ([]byte, error) {
	tf, err := s.twoFactor(userID)
	if err != nil {
		return nil, err
	}
	if tf.Enabled {
		return nil, errors.New("two-factor sign-in is already on; turn it off first to set up a new authenticator")
	}
	secret := newTOTPSecret()
	ct, err := s.seal(secret, fmt.Sprintf("totp:%d", userID))
	if err != nil {
		return nil, err
	}
	_, err = s.db.Exec("UPDATE users SET totp_secret=?, totp_enabled=0, totp_last=0, recovery_codes=NULL WHERE id=?", ct, userID)
	return secret, err
}

// EnableTwoFactor turns two-factor sign-in on once the person has proven
// their authenticator works, and returns the one-time recovery codes.
func (s *Store) EnableTwoFactor(userID int64, code string) ([]string, error) {
	tf, err := s.twoFactor(userID)
	if err != nil {
		return nil, err
	}
	if tf.Enabled {
		return nil, errors.New("two-factor sign-in is already on")
	}
	if len(tf.Secret) == 0 {
		return nil, errors.New("start the setup first")
	}
	step, ok := checkTOTP(tf.Secret, code, time.Now(), tf.Last)
	if !ok {
		return nil, errors.New("that code didn't match. Check the time on your phone is correct and try the newest code")
	}
	codes := newRecoveryCodes()
	hashes := make([]string, len(codes))
	for i, c := range codes {
		hashes[i] = bpcrypto.TokenHash(normRecovery(c))
	}
	hj, _ := json.Marshal(hashes)
	_, err = s.db.Exec("UPDATE users SET totp_enabled=1, totp_last=?, recovery_codes=? WHERE id=?", step, string(hj), userID)
	return codes, err
}

func (s *Store) DisableTwoFactor(userID int64) error {
	_, err := s.db.Exec("UPDATE users SET totp_secret=NULL, totp_enabled=0, totp_last=0, recovery_codes=NULL WHERE id=?", userID)
	return err
}

// VerifySecondFactor checks an authenticator code or an unused recovery code.
// Each succeeds once: TOTP steps can't be reused and recovery codes are spent.
func (s *Store) VerifySecondFactor(userID int64, code string) (method string, err error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tf, err := s.twoFactor(userID)
	if err != nil {
		return "", err
	}
	if !tf.Enabled {
		return "", errors.New("two-factor sign-in is not on for this account")
	}
	if step, ok := checkTOTP(tf.Secret, code, time.Now(), tf.Last); ok {
		res, err := s.db.Exec("UPDATE users SET totp_last=? WHERE id=? AND totp_last<?", step, userID, step)
		if err != nil {
			return "", err
		}
		if n, _ := res.RowsAffected(); n == 0 { // another sign-in used this code first
			return "", errors.New("wrong code")
		}
		return "authenticator", nil
	}
	var raw sql.NullString
	if err := s.db.QueryRow("SELECT recovery_codes FROM users WHERE id=?", userID).Scan(&raw); err != nil {
		return "", err
	}
	var hashes []string
	_ = json.Unmarshal([]byte(raw.String), &hashes)
	want := bpcrypto.TokenHash(normRecovery(code))
	for i, h := range hashes {
		if subtle.ConstantTimeCompare([]byte(h), []byte(want)) == 1 {
			hashes = append(hashes[:i], hashes[i+1:]...)
			hj, _ := json.Marshal(hashes)
			_, err := s.db.Exec("UPDATE users SET recovery_codes=? WHERE id=?", string(hj), userID)
			return "recovery code", err
		}
	}
	return "", errors.New("wrong code")
}

// --- API tokens -----------------------------------------------------------------

type APIToken struct {
	ID       int64      `json:"id"`
	Name     string     `json:"name"`
	UserID   int64      `json:"userId"`
	Username string     `json:"username"`
	Role     string     `json:"role"`
	Prefix   string     `json:"prefix"`
	Created  string     `json:"created"`
	Expires  *time.Time `json:"expires,omitempty"`
	LastUsed *time.Time `json:"lastUsed,omitempty"`
}

func (s *Store) CreateAPIToken(userID int64, name, role string, ttl time.Duration) (string, int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", 0, errors.New("give the token a name, such as “CI deploys”")
	}
	if _, ok := roleRank[role]; !ok {
		return "", 0, errors.New("role must be admin, operator or auditor")
	}
	tok := bpcrypto.RandomToken("bpt_")
	var exp any
	if ttl > 0 {
		exp = time.Now().Add(ttl).UTC().Format(time.RFC3339Nano)
	}
	res, err := s.db.Exec("INSERT INTO api_tokens(name,user_id,role,token_hash,prefix,created,expires) VALUES(?,?,?,?,?,?,?)",
		name, userID, role, bpcrypto.TokenHash(tok), tok[:12], now(), exp)
	if err != nil {
		return "", 0, err
	}
	id, _ := res.LastInsertId()
	return tok, id, nil
}

func (s *Store) APITokens() ([]APIToken, error) {
	rows, err := s.db.Query(`SELECT t.id,t.name,t.user_id,u.username,t.role,t.prefix,t.created,t.expires,t.last_used
FROM api_tokens t JOIN users u ON u.id=t.user_id ORDER BY t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var t APIToken
		var exp, used sql.NullString
		if err := rows.Scan(&t.ID, &t.Name, &t.UserID, &t.Username, &t.Role, &t.Prefix, &t.Created, &exp, &used); err != nil {
			return nil, err
		}
		t.Expires, t.LastUsed = parseTime(exp), parseTime(used)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAPIToken(id int64) (string, error) {
	var name string
	if err := s.db.QueryRow("SELECT name FROM api_tokens WHERE id=?", id).Scan(&name); err != nil {
		return "", fmt.Errorf("token %d not found", id)
	}
	_, err := s.db.Exec("DELETE FROM api_tokens WHERE id=?", id)
	return name, err
}

// TokenUser resolves a bearer token. The token acts with its own role, but
// never more than its owner currently has.
func (s *Store) TokenUser(tok string) (*User, string, error) {
	var u User
	var id int64
	var name, role string
	var exp, used sql.NullString
	err := s.db.QueryRow(`SELECT t.id,t.name,t.role,t.expires,t.last_used,u.id,u.username,u.role,u.created
FROM api_tokens t JOIN users u ON u.id=t.user_id WHERE t.token_hash=?`, bpcrypto.TokenHash(tok)).
		Scan(&id, &name, &role, &exp, &used, &u.ID, &u.Username, &u.Role, &u.Created)
	if err != nil {
		return nil, "", errors.New("invalid API token")
	}
	if e := parseTime(exp); e != nil && time.Now().After(*e) {
		return nil, "", errors.New("this API token has expired")
	}
	if roleRank[role] < roleRank[u.Role] {
		u.Role = role
	}
	if lu := parseTime(used); lu == nil || time.Since(*lu) > time.Minute {
		_, _ = s.db.Exec("UPDATE api_tokens SET last_used=? WHERE id=?", now(), id)
	}
	return &u, name, nil
}

// --- activity ---------------------------------------------------------------------

type Activity struct {
	Seq    int64  `json:"seq"`
	Time   string `json:"time"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Detail string `json:"detail,omitempty"`
}

// Activity lists recorded actions (ledger "event" entries), newest first.
func (s *Store) Activity(before int64, limit int, q string) ([]Activity, error) {
	query := "SELECT seq,time,subject,IFNULL(detail,'') FROM ledger WHERE kind='event'"
	var args []any
	if before > 0 {
		query += " AND seq < ?"
		args = append(args, before)
	}
	if q = strings.TrimSpace(q); q != "" {
		query += " AND (subject LIKE ? ESCAPE '\\' OR detail LIKE ? ESCAPE '\\')"
		like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q) + "%"
		args = append(args, like, like)
	}
	query += " ORDER BY seq DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Activity
	for rows.Next() {
		var a Activity
		var subject string
		if err := rows.Scan(&a.Seq, &a.Time, &subject, &a.Detail); err != nil {
			return nil, err
		}
		a.Actor, a.Action, _ = strings.Cut(subject, ": ")
		out = append(out, a)
	}
	return out, rows.Err()
}
