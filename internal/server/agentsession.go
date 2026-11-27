package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	bpcrypto "github.com/chmuzamil/backupproof/internal/crypto"
	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/protocol"
)

// Short-lived agent sessions. An agent from 0.5 on proves it holds its own
// signing key (the one its proofs are signed with) to get a token that lasts
// 15 minutes, and uses that. From its first session on, its long-lived token is
// accepted only to start a session, so a copy of that token is useless
// without the key. Agents older than 0.5 keep using their long-lived token.

const (
	agentSessionTTL  = 15 * time.Minute
	agentSessionSkew = 5 * time.Minute
)

var agentSessionMigrations = []string{
	`CREATE TABLE IF NOT EXISTS agent_sessions (token_hash TEXT PRIMARY KEY, agent_id INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE, expires TEXT NOT NULL)`,
	`ALTER TABLE agents ADD COLUMN sessions_only INTEGER NOT NULL DEFAULT 0`,
}

// AgentSessionMessage is what an agent signs to start a session.
func AgentSessionMessage(agentID int64, at int64) []byte {
	return []byte(fmt.Sprintf("backupproof-agent-session/v1\n%d\n%d\n", agentID, at))
}

func (s *Server) handleAgentSession(w http.ResponseWriter, r *http.Request) {
	tok, ok := bearer(r)
	a, err := s.store.AgentByToken(tok)
	if !ok || err != nil {
		time.Sleep(200 * time.Millisecond)
		writeErr(w, http.StatusUnauthorized, errors.New("unknown or revoked agent"))
		return
	}
	var req protocol.SessionRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if d := time.Since(time.Unix(req.Time, 0)); d > agentSessionSkew || d < -agentSessionSkew {
		writeErr(w, 400, errors.New("the server's clock and this one differ by more than 5 minutes"))
		return
	}
	pk, err := proof.ParsePublicKey(a.PublicKey)
	if err != nil || len(req.Signature) != ed25519.SignatureSize || !ed25519.Verify(pk.Public, AgentSessionMessage(a.ID, req.Time), req.Signature) {
		s.audit("agent:"+a.Name, "agent-session-refused", "a session request wasn't signed with the agent's key")
		writeErr(w, http.StatusUnauthorized, errors.New("not signed with this agent's key"))
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		writeErr(w, 500, err)
		return
	}
	session := "bps_" + hex.EncodeToString(raw)
	expires := time.Now().Add(agentSessionTTL).UTC()
	if err := s.store.CreateAgentSession(a.ID, session, expires); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, protocol.SessionResponse{Token: session, Expires: expires, ExpiresIn: int64(agentSessionTTL / time.Second)})
}

func bearer(r *http.Request) (string, bool) {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(p) || h[:len(p)] != p {
		return "", false
	}
	return h[len(p):], true
}

// CreateAgentSession stores a session, clears expired ones, and from now on
// refuses the agent's long-lived token outside starting a session.
func (s *Store) CreateAgentSession(agentID int64, token string, expires time.Time) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM agent_sessions WHERE expires < ?", now()); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO agent_sessions(token_hash,agent_id,expires) VALUES(?,?,?)", bpcrypto.TokenHash(token), agentID, expires.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE agents SET sessions_only=1 WHERE id=?", agentID); err != nil {
		return err
	}
	return tx.Commit()
}

// AgentBySession finds the agent a live session token belongs to.
func (s *Store) AgentBySession(token string) (*Agent, error) {
	var id int64
	var expires string
	err := s.db.QueryRow("SELECT agent_id, expires FROM agent_sessions WHERE token_hash=?", bpcrypto.TokenHash(token)).Scan(&id, &expires)
	if err != nil {
		return nil, err
	}
	if t, err := time.Parse(time.RFC3339Nano, expires); err != nil || time.Now().After(t) {
		return nil, errors.New("session expired")
	}
	a, err := s.Agent(id)
	if err != nil || a.Revoked || a.Removed {
		return nil, errors.New("unknown or revoked agent")
	}
	return a, nil
}

// SessionsOnly reports whether an agent has moved to sessions, so its
// long-lived token alone is no longer enough.
func (s *Store) SessionsOnly(agentID int64) bool {
	var v int
	err := s.db.QueryRow("SELECT sessions_only FROM agents WHERE id=?", agentID).Scan(&v)
	return err == nil && v == 1
}

// EndAgentSessions removes an agent's sessions (when it's revoked or removed).
func (s *Store) EndAgentSessions(agentID int64) {
	_, _ = s.db.Exec("DELETE FROM agent_sessions WHERE agent_id=?", agentID)
}

var errSessionsOnly = errors.New("this server now signs in with its key: its long-lived token alone isn't accepted")

// agentFromRequest is the agent a request comes from: by a session token, or
// by the long-lived token of an agent that hasn't moved to sessions.
func (s *Server) agentFromRequest(r *http.Request) (*Agent, error) {
	tok, ok := bearer(r)
	if !ok {
		return nil, errors.New("unknown or revoked agent")
	}
	if a, err := s.store.AgentBySession(tok); err == nil {
		return a, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	a, err := s.store.AgentByToken(tok)
	if err != nil {
		return nil, err
	}
	if s.store.SessionsOnly(a.ID) {
		return nil, errSessionsOnly
	}
	return a, nil
}
