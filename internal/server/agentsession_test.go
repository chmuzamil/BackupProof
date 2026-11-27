package server

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/protocol"
)

func TestAgentSessions(t *testing.T) {
	srv, err := New(Config{DataDir: t.TempDir(), NoLocalAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	web := httptest.NewServer(srv.Handler())
	defer web.Close()

	enroll := func(name string) (int64, string, *proof.Key) {
		key, _ := proof.GenerateKey("agent:" + name)
		tok, err := srv.store.CreateEnrollToken("test", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		id, agentTok, err := srv.store.Enroll(tok, Agent{Name: name, Hostname: name, PublicKey: key.Public().String()})
		if err != nil {
			t.Fatal(err)
		}
		return id, agentTok, key
	}
	call := func(path, token string, body any) (int, []byte) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", web.URL+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out bytes.Buffer
		_, _ = out.ReadFrom(resp.Body)
		return resp.StatusCode, out.Bytes()
	}
	// An agent endpoint that answers at once: 401 when the agent isn't
	// recognised, otherwise an answer about the (missing) job.
	poll := func(token string) int {
		code, _ := call("/api/agent/jobs/999999/log", token, map[string]string{"line": "x"})
		if code != 401 {
			return 200
		}
		return code
	}
	startSession := func(id int64, tok string, key ed25519.PrivateKey, at time.Time) (int, string) {
		req := protocol.SessionRequest{Time: at.Unix(), Signature: ed25519.Sign(key, AgentSessionMessage(id, at.Unix()))}
		code, body := call("/api/agent/session", tok, req)
		var resp protocol.SessionResponse
		_ = json.Unmarshal(body, &resp)
		return code, resp.Token
	}

	// An agent older than 0.5 never starts a session and keeps working.
	_, oldTok, _ := enroll("old")
	if code := poll(oldTok); code != 200 && code != 204 {
		t.Fatalf("old agent: %d", code)
	}

	id, tok, key := enroll("new")
	other, _ := proof.GenerateKey("someone else")
	if code, _ := startSession(id, tok, other.Private, time.Now()); code != 401 {
		t.Errorf("a session signed with another key: %d", code)
	}
	if code, _ := startSession(id, tok, key.Private, time.Now().Add(-time.Hour)); code != 400 {
		t.Errorf("a session request from an hour ago: %d", code)
	}
	if code, _ := startSession(id, "bpa_wrong", key.Private, time.Now()); code != 401 {
		t.Errorf("a session with a wrong long-lived token: %d", code)
	}
	code, session := startSession(id, tok, key.Private, time.Now())
	if code != 200 || session == "" {
		t.Fatalf("session: %d", code)
	}
	if code := poll(session); code != 200 && code != 204 {
		t.Errorf("polling with the session: %d", code)
	}
	// From now on the long-lived token alone isn't enough.
	if code := poll(tok); code != 401 {
		t.Errorf("the long-lived token still works after the first session: %d", code)
	}
	// A session can't start another one, or be used after it ends.
	if code, _ := startSession(id, session, key.Private, time.Now()); code != 401 {
		t.Errorf("a session token started a session: %d", code)
	}
	if _, err := srv.store.db.Exec("UPDATE agent_sessions SET expires=?", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if code := poll(session); code != 401 {
		t.Errorf("an expired session: %d", code)
	}
	// Revoking the agent ends its sessions.
	_, session = startSession(id, tok, key.Private, time.Now())
	if err := srv.store.RevokeAgent(id); err != nil {
		t.Fatal(err)
	}
	if code := poll(session); code != 401 {
		t.Errorf("a revoked agent's session: %d", code)
	}
	if code, _ := startSession(id, tok, key.Private, time.Now()); code != 401 {
		t.Errorf("a revoked agent started a session: %d", code)
	}
}
