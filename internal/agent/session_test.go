package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/protocol"
)

// fakeSessions is a dashboard that hands out sessions lasting ttl, by its
// own clock (offset from the agent's by skew), and refuses expired ones.
type fakeSessions struct {
	mu       sync.Mutex
	ttl      time.Duration
	skew     time.Duration
	now      func() time.Time
	sessions map[string]time.Time
	started  int
	refused  int
	pub      ed25519.PublicKey
}

func (f *fakeSessions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	serverNow := f.now().Add(f.skew)
	if r.URL.Path == "/api/agent/session" {
		var req protocol.SessionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if tok != "long-lived" || !ed25519.Verify(f.pub, sessionMessage(7, req.Time), req.Signature) {
			w.WriteHeader(401)
			return
		}
		f.started++
		s := fmt.Sprintf("bps_%d", f.started)
		f.sessions[s] = serverNow.Add(f.ttl)
		_ = json.NewEncoder(w).Encode(protocol.SessionResponse{Token: s, Expires: serverNow.Add(f.ttl), ExpiresIn: int64(f.ttl / time.Second)})
		return
	}
	if exp, ok := f.sessions[tok]; !ok || !serverNow.Before(exp) {
		f.refused++
		w.WriteHeader(401)
		return
	}
	w.WriteHeader(204)
}

// With 15-minute sessions the agent renews before each ends, also when its
// clock is minutes off the dashboard's, so no request is refused; and a
// session that ends early is replaced on the spot.
func TestSessionsRenewSeamlessly(t *testing.T) {
	key, _ := proof.GenerateKey("agent:test")
	for _, skew := range []time.Duration{0, 4 * time.Minute, -4 * time.Minute} {
		t.Run(fmt.Sprintf("skew %v", skew), func(t *testing.T) {
			clock := time.Now()
			var clockMu sync.Mutex
			now := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
			f := &fakeSessions{ttl: 15 * time.Minute, skew: skew, now: now, sessions: map[string]time.Time{}, pub: key.Private.Public().(ed25519.PublicKey)}
			srv := httptest.NewServer(f)
			defer srv.Close()
			c := &client{base: srv.URL, token: "long-lived", http: srv.Client(), agentID: 7, key: key, now: now}
			ctx := context.Background()
			// Two hours of requests every 30 seconds.
			for range 240 {
				if code, err := c.post(ctx, "/api/agent/poll", struct{}{}, nil); err != nil || code != 204 {
					t.Fatalf("request at %s: %d %v", now().Format(time.TimeOnly), code, err)
				}
				clockMu.Lock()
				clock = clock.Add(30 * time.Second)
				clockMu.Unlock()
			}
			if f.refused != 0 {
				t.Errorf("%d requests were refused on an expired session", f.refused)
			}
			// About one session per 10 minutes: renewed 5 minutes before the end.
			if f.started < 11 || f.started > 14 {
				t.Errorf("%d sessions in two hours, want about 12", f.started)
			}
			// The dashboard forgets its sessions (restored, say): one 401,
			// then a new session and the request goes through.
			f.mu.Lock()
			f.sessions = map[string]time.Time{}
			f.mu.Unlock()
			if code, err := c.post(ctx, "/api/agent/poll", struct{}{}, nil); err != nil || code != 204 {
				t.Fatalf("after the dashboard dropped its sessions: %d %v", code, err)
			}
		})
	}
}
