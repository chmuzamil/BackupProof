package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chmuzamil/backupproof/internal/transparency"
	"golang.org/x/mod/sumdb/tlog"
)

const (
	settingTlogCheckpoint = "tlog-checkpoint" // the latest signed checkpoint, with cosignatures
	settingTlogWitnesses  = "tlog-witnesses"  // JSON list of WitnessConfig
	settingTlogPublic     = "tlog-public"     // "1": /log/ needs no sign-in
)

// WitnessConfig is a witness the dashboard asks to cosign its checkpoints.
type WitnessConfig struct {
	URL string `json:"url"` // the witness's submission prefix
	Key string `json:"key"` // its verifier key: name+id+base64
	// Kept by the dashboard: the size it last cosigned, when, and why the
	// last request failed.
	Size      int64  `json:"size,omitempty"`
	Cosigned  string `json:"cosigned,omitempty"`
	LastError string `json:"lastError,omitempty"`
	// FailingSince is when its requests started failing (empty when the
	// last one worked).
	FailingSince string `json:"failingSince,omitempty"`
}

// witnessGrace is how long a witness may fail before it counts as down:
// longer than a short network blip or a restart.
const witnessGrace = 15 * time.Minute

// failingWitnesses are the witnesses that have failed for witnessGrace.
func failingWitnesses(ws []WitnessConfig, now time.Time) []WitnessConfig {
	var out []WitnessConfig
	for _, w := range ws {
		if since, err := time.Parse(time.RFC3339, w.FailingSince); err == nil && now.Sub(since) >= witnessGrace {
			out = append(out, w)
		}
	}
	return out
}

func witnessName(w WitnessConfig) string {
	if v, err := transparency.ParseVerifier(w.Key); err == nil {
		return v.Name
	}
	return w.URL
}

// alertWitnesses raises or resolves the alert for witnesses that stopped cosigning.
func (s *Server) alertWitnesses(ws []WitnessConfig) {
	failing := failingWitnesses(ws, time.Now())
	if len(failing) == 0 {
		s.resolve("witness-failed", nil, nil)
		return
	}
	var parts []string
	for _, w := range failing {
		parts = append(parts, fmt.Sprintf("%s (%s)", witnessName(w), w.LastError))
	}
	s.alert("witness-failed", nil, nil, "The transparency log isn't being cosigned by "+strings.Join(parts, "; ")+
		". Until it is, nothing outside the dashboard confirms that its proof history hasn't been rewritten.")
}

// handleWitnessHealth answers 200 while every witness cosigns and 503 once
// one has failed for witnessGrace, for an uptime monitor. It says nothing
// about the log itself, so it needs no sign-in even when the log is private.
func (s *Server) handleWitnessHealth(w http.ResponseWriter, r *http.Request) {
	ws := s.witnesses()
	failing := failingWitnesses(ws, time.Now())
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if len(failing) > 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "failing: %d of %d witnesses haven't cosigned for %s\n", len(failing), len(ws), witnessGrace)
		return
	}
	fmt.Fprintf(w, "ok: %d witnesses\n", len(ws))
}

var tlogMu sync.Mutex // one checkpoint round at a time

// logOrigin names the log: the dashboard's address without its scheme, then
// /log, as C2SP suggests; or the server key's ID when no address is set.
func (s *Server) logOrigin() string {
	if u, err := url.Parse(s.publicURL()); err == nil && u.Host != "" && !strings.Contains(u.Host, "localhost") {
		return strings.TrimSuffix(u.Host+strings.TrimSuffix(u.Path, "/"), "/") + "/log"
	}
	return "backupproof-" + strings.TrimPrefix(s.key.Public().KeyID, "bp:") + "/log"
}

func (s *Server) logSigner() transparency.Signer {
	return transparency.Signer{Name: s.logOrigin(), Key: s.key.Private}
}

// LogKey is the log's verifier key, for auditors and witnesses.
func (s *Server) LogKey() string {
	return transparency.LogVerifierKey(s.logOrigin(), s.key.Public().Public)
}

func (s *Server) witnesses() []WitnessConfig {
	var ws []WitnessConfig
	_ = json.Unmarshal([]byte(s.store.Setting(settingTlogWitnesses)), &ws)
	return ws
}

// refreshCheckpoint signs a checkpoint for the log as it is now, if it grew
// or the origin changed, and asks every witness to cosign it.
func (s *Server) refreshCheckpoint(ctx context.Context) error {
	tlogMu.Lock()
	defer tlogMu.Unlock()
	n := s.store.TlogSize()
	origin := s.logOrigin()
	prev, prevErr := transparency.ParseNote(s.store.Setting(settingTlogCheckpoint))
	var prevCP transparency.Checkpoint
	if prevErr == nil {
		prevCP, prevErr = transparency.ParseCheckpoint(prev.Text)
	}
	ws := s.witnesses()
	needWitness := false
	for _, w := range ws {
		if w.Size != n {
			needWitness = true
		}
	}
	if prevErr == nil && prevCP.Size == n && prevCP.Origin == origin && !needWitness {
		return nil
	}
	root, err := s.store.TlogRoot(n)
	if err != nil {
		return err
	}
	c := transparency.Checkpoint{Origin: origin, Size: n, Root: root}
	note := transparency.Note{Text: c.Body(), Sigs: []string{s.logSigner().Sign(c.Body())}}
	if prevErr == nil && prevCP.Size == n && prevCP.Origin == origin {
		note = prev // keep the cosignatures already collected for this size
	}
	client := &http.Client{Timeout: 30 * time.Second}
	logNote := transparency.Note{Text: note.Text, Sigs: note.Sigs[:1]}.String()
	for i := range ws {
		w := &ws[i]
		v, err := transparency.ParseVerifier(w.Key)
		if err != nil {
			w.LastError = "the witness's key isn't valid: " + err.Error()
			continue
		}
		if w.Size == n && hasSig(note, v) {
			continue
		}
		sigs, err := s.askWitness(ctx, client, w, n, logNote)
		if err != nil {
			w.LastError = err.Error()
			if w.FailingSince == "" {
				w.FailingSince = time.Now().UTC().Format(time.RFC3339)
			}
			continue
		}
		added := false
		for _, sig := range sigs {
			if ok, _, verr := v.Verify(note.Text, sig); ok && verr == nil {
				note.Sigs = append(note.Sigs, sig)
				added = true
			}
		}
		if !added {
			w.LastError = "the witness answered without a valid cosignature from its key"
			if w.FailingSince == "" {
				w.FailingSince = time.Now().UTC().Format(time.RFC3339)
			}
			continue
		}
		w.Size, w.Cosigned, w.LastError, w.FailingSince = n, time.Now().UTC().Format(time.RFC3339), "", ""
	}
	raw, _ := json.Marshal(ws)
	if err := s.store.SetSetting(settingTlogWitnesses, string(raw)); err != nil {
		return err
	}
	s.alertWitnesses(ws)
	return s.store.SetSetting(settingTlogCheckpoint, note.String())
}

func hasSig(n transparency.Note, v transparency.Verifier) bool {
	for _, sig := range n.Sigs {
		if ok, _, err := v.Verify(n.Text, sig); ok && err == nil {
			return true
		}
	}
	return false
}

// askWitness sends the checkpoint with a consistency proof from the size the
// witness last saw, learning that size first if it isn't known.
func (s *Server) askWitness(ctx context.Context, client *http.Client, w *WitnessConfig, n int64, note string) ([]string, error) {
	old := w.Size
	for range 2 {
		var proof tlog.TreeProof
		if old > 0 && old < n {
			var err error
			if proof, err = s.store.TlogConsistency(n, old); err != nil {
				return nil, err
			}
		}
		sigs, err := transparency.AddCheckpoint(ctx, client, w.URL, old, proof, note)
		var conflict transparency.ErrConflict
		if errors.As(err, &conflict) && conflict.Size <= n {
			old = conflict.Size
			continue
		}
		return sigs, err
	}
	return nil, errors.New("the witness keeps reporting a different size")
}

// scheduleCheckpoint refreshes the checkpoint at most once a minute.
var lastCheckpoint time.Time

func (s *Server) scheduleCheckpoint(now time.Time) {
	if now.Sub(lastCheckpoint) < time.Minute {
		return
	}
	lastCheckpoint = now
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.refreshCheckpoint(ctx); err != nil {
			s.log.Printf("transparency log: %v", err)
		}
	}()
}

// logRoute serves /log/: to anyone when the log is public, otherwise to
// signed-in auditors and API tokens.
func (s *Server) logRoute(h http.HandlerFunc) http.HandlerFunc {
	authed := s.auth("auditor", h)
	return func(w http.ResponseWriter, r *http.Request) {
		if s.store.Setting(settingTlogPublic) == "1" {
			h(w, r)
			return
		}
		authed(w, r)
	}
}

func (s *Server) handleLogCheckpoint(w http.ResponseWriter, r *http.Request) {
	note := s.store.Setting(settingTlogCheckpoint)
	if note == "" {
		if err := s.refreshCheckpoint(r.Context()); err != nil {
			writeErr(w, 500, err)
			return
		}
		note = s.store.Setting(settingTlogCheckpoint)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(note))
}

func (s *Server) handleLogTile(w http.ResponseWriter, r *http.Request) {
	n, err := transparency.ParseNote(s.store.Setting(settingTlogCheckpoint))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	c, err := transparency.ParseCheckpoint(n.Text)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	b, err := s.store.TlogTile(strings.TrimPrefix(r.URL.Path, "/log/"), c.Size)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if !strings.Contains(r.URL.Path, ".p/") {
		w.Header().Set("Cache-Control", "max-age=31536000, immutable")
	}
	_, _ = w.Write(b)
}

// TransparencySettings is the Settings page's view of the log.
type TransparencySettings struct {
	Origin     string          `json:"origin"`
	LogKey     string          `json:"logKey"`
	Public     bool            `json:"public"`
	Size       int64           `json:"size"`
	Witnesses  []WitnessConfig `json:"witnesses"`
	Checkpoint string          `json:"checkpoint"`
}

func (s *Server) transparencySettings() TransparencySettings {
	ws := s.witnesses()
	if ws == nil {
		ws = []WitnessConfig{}
	}
	return TransparencySettings{Origin: s.logOrigin(), LogKey: s.LogKey(), Public: s.store.Setting(settingTlogPublic) == "1",
		Size: s.store.TlogSize(), Witnesses: ws, Checkpoint: s.store.Setting(settingTlogCheckpoint)}
}

func (s *Server) handleGetTransparency(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.transparencySettings())
}

// handlePutTransparency sets the witnesses and whether /log/ is public.
func (s *Server) handlePutTransparency(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Public    bool `json:"public"`
		Witnesses []struct {
			URL string `json:"url"`
			Key string `json:"key"`
		} `json:"witnesses"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if len(req.Witnesses) > 8 {
		writeErr(w, 400, errors.New("at most 8 witnesses"))
		return
	}
	old := map[string]WitnessConfig{}
	for _, wc := range s.witnesses() {
		old[wc.Key] = wc
	}
	var ws []WitnessConfig
	seen := map[string]bool{}
	for _, in := range req.Witnesses {
		u, err := url.Parse(strings.TrimSpace(in.URL))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			writeErr(w, 400, fmt.Errorf("the witness address %q must start with https://", in.URL))
			return
		}
		v, err := transparency.ParseVerifier(in.Key)
		if err != nil || v.Name == "" {
			writeErr(w, 400, fmt.Errorf("the witness key %q: %v", in.Key, err))
			return
		}
		if seen[v.Name] {
			writeErr(w, 400, fmt.Errorf("two witnesses are called %s", v.Name))
			return
		}
		seen[v.Name] = true
		wc := WitnessConfig{URL: strings.TrimSuffix(u.String(), "/"), Key: v.String()}
		if prev, ok := old[wc.Key]; ok && prev.URL == wc.URL {
			wc = prev // keep what it last cosigned
		}
		ws = append(ws, wc)
	}
	raw, _ := json.Marshal(ws)
	public := ""
	if req.Public {
		public = "1"
	}
	if err := s.store.SetSetting(settingTlogWitnesses, string(raw)); err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := s.store.SetSetting(settingTlogPublic, public); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.audit(s.actor(r), "transparency-settings", fmt.Sprintf("%d witnesses, public log: %v", len(ws), req.Public))
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if err := s.refreshCheckpoint(ctx); err != nil {
		s.log.Printf("transparency log: %v", err)
	}
	writeJSON(w, 200, s.transparencySettings())
}
