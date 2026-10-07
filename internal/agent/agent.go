// Package agent runs on every protected server. It only makes outbound
// HTTPS requests: it long-polls the control plane for job leases, executes
// backups, restore drills and checks against the repository directly (bulk
// data never flows through the control plane), signs attestations with its
// own Ed25519 key, and submits them for ledgering.
package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/chunker"
	"github.com/chmuzamil/backupproof/internal/discover"
	"github.com/chmuzamil/backupproof/internal/drill"
	"github.com/chmuzamil/backupproof/internal/engine"
	"github.com/chmuzamil/backupproof/internal/ops"
	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/protocol"
	"github.com/chmuzamil/backupproof/internal/repo"
)

type State struct {
	Server    string `json:"server"`
	AgentID   int64  `json:"agentId"`
	Token     string `json:"token"`
	ServerKey string `json:"serverKey"`
	Name      string `json:"name"`
}

func statePath(dir string) string { return filepath.Join(dir, "agent.json") }
func keyPath(dir string) string   { return filepath.Join(dir, "agent.key") }

func LoadState(dir string) (*State, error) {
	b, err := os.ReadFile(statePath(dir))
	if err != nil {
		return nil, fmt.Errorf("agent is not enrolled (%v); run `backupproof agent enroll` first", err)
	}
	var s State
	return &s, json.Unmarshal(b, &s)
}

type client struct {
	base  string
	token string
	http  *http.Client
}

func (c *client) post(ctx context.Context, path string, in, out any) (int, error) {
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, nil
	}
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		// Non-JSON error bodies (proxies, panics) fall back to the raw text.
		if err := json.Unmarshal(body, &e); err != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(body))
		}
		return resp.StatusCode, fmt.Errorf("%s: HTTP %d: %s", path, resp.StatusCode, e.Error)
	}
	if out != nil {
		return resp.StatusCode, json.Unmarshal(body, out)
	}
	return resp.StatusCode, nil
}

// Enroll registers this machine with the control plane using a single-use token.
func Enroll(ctx context.Context, server, token, name, dir string) (*State, error) {
	return enroll(ctx, &http.Client{Timeout: 30 * time.Second}, server, token, name, dir)
}

// loopbackClient trusts any certificate; only ever used to reach our own
// server process over 127.0.0.1.
func loopbackClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
}

// StartBuiltin enrolls (once) and runs the agent embedded in the server, so
// the machine running the dashboard can protect itself with no install step.
func StartBuiltin(ctx context.Context, serverURL, dir string, newToken func() (string, error), deny []string) error {
	if _, err := LoadState(dir); err != nil {
		tok, err := newToken()
		if err != nil {
			return err
		}
		if _, err := enroll(ctx, loopbackClient(30*time.Second), serverURL, tok, "This server", dir); err != nil {
			return err
		}
	}
	a, err := New(dir)
	if err != nil {
		return err
	}
	a.st.Server = strings.TrimRight(serverURL, "/")
	a.c.base = a.st.Server
	a.c.http = loopbackClient(60 * time.Second)
	a.quiet = true
	a.deny = deny
	return a.Run(ctx)
}

func enroll(ctx context.Context, hc *http.Client, server, token, name, dir string) (*State, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	server = strings.TrimRight(server, "/")
	if hc.Transport == nil && strings.HasPrefix(server, "http://") && !strings.Contains(server, "localhost") && !strings.Contains(server, "127.0.0.1") {
		fmt.Fprintln(os.Stderr, "warning: enrolling over plain HTTP; job leases contain repository secrets. Use HTTPS.")
	}
	host, _ := os.Hostname()
	if name == "" {
		name = host
	}
	key, err := proof.LoadOrCreateKey(keyPath(dir), "agent@"+name)
	if err != nil {
		return nil, err
	}
	c := &client{base: server, http: hc}
	var resp protocol.EnrollResponse
	_, err = c.post(ctx, "/api/agent/enroll", protocol.EnrollRequest{
		Token: token, Name: name, Hostname: host, OS: runtime.GOOS + "/" + runtime.GOARCH,
		Version: engine.Version, PublicKey: key.Public().String(), Docker: drill.DockerAvailable(ctx),
	}, &resp)
	if err != nil {
		return nil, err
	}
	st := &State{Server: server, AgentID: resp.AgentID, Token: resp.AgentToken, ServerKey: resp.ServerKey, Name: name}
	b, _ := json.MarshalIndent(st, "", "  ")
	return st, os.WriteFile(statePath(dir), b, 0o600)
}

type Agent struct {
	dir    string
	quiet  bool
	deny   []string
	st     *State
	key    *proof.Key
	c      *client
	docker bool
}

func New(dir string) (*Agent, error) {
	st, err := LoadState(dir)
	if err != nil {
		return nil, err
	}
	key, err := proof.LoadOrCreateKey(keyPath(dir), "agent@"+st.Name)
	if err != nil {
		return nil, err
	}
	return &Agent{dir: dir, st: st, key: key, c: &client{base: st.Server, token: st.Token, http: &http.Client{Timeout: 60 * time.Second}}}, nil
}

// Run polls for work until ctx is cancelled. Network errors back off and
// retry; the agent never exits because the server is temporarily away.
func (a *Agent) Run(ctx context.Context) error {
	fmt.Printf("agent %s (#%d) polling %s\n", a.st.Name, a.st.AgentID, a.st.Server)
	a.docker = drill.DockerAvailable(ctx)
	go a.reportInventory(ctx)
	backoff := time.Second
	for ctx.Err() == nil {
		var lease protocol.Lease
		code, err := a.c.post(ctx, "/api/agent/poll", protocol.PollRequest{Version: engine.Version, Docker: a.docker}, &lease)
		if err != nil {
			if code == http.StatusUnauthorized {
				return errors.New("server rejected this agent (revoked?): " + err.Error())
			}
			fmt.Fprintf(os.Stderr, "poll failed: %v (retrying in %s)\n", err, backoff)
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			if backoff < time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		if code == http.StatusNoContent || lease.JobID == 0 {
			continue
		}
		a.execute(ctx, &lease)
	}
	return ctx.Err()
}

// PollOnce asks for one job and executes it. It reports whether a job ran.
func (a *Agent) PollOnce(ctx context.Context) (bool, error) {
	var lease protocol.Lease
	code, err := a.c.post(ctx, "/api/agent/poll", protocol.PollRequest{Version: engine.Version, Docker: a.docker}, &lease)
	if err != nil || code == http.StatusNoContent || lease.JobID == 0 {
		return false, err
	}
	a.execute(ctx, &lease)
	return true, nil
}

// jobLog buffers log lines and ships them with each lease heartbeat.
type jobLog struct {
	mu    sync.Mutex
	buf   strings.Builder
	a     *Agent
	jobID int64
}

func (l *jobLog) Logf(format string, args ...any) {
	line := time.Now().Format("15:04:05 ") + fmt.Sprintf(format, args...) + "\n"
	if !l.a.quiet {
		fmt.Print(line)
	}
	l.mu.Lock()
	l.buf.WriteString(line)
	l.mu.Unlock()
}

func (l *jobLog) flush(ctx context.Context) error {
	l.mu.Lock()
	lines := l.buf.String()
	l.buf.Reset()
	l.mu.Unlock()
	_, err := l.a.c.post(ctx, fmt.Sprintf("/api/agent/jobs/%d/log", l.jobID), protocol.LogRequest{Lines: lines}, nil)
	return err
}

// remoteLedger submits signed records to the control plane, which verifies
// them against this agent's enrolled key before appending to its ledger.
type remoteLedger struct {
	a     *Agent
	jobID int64
}

func (r *remoteLedger) Append(string, string, string) (proof.LedgerEntry, error) {
	return proof.LedgerEntry{}, errors.New("remote ledger requires AppendRecord")
}

func (r *remoteLedger) AppendRecord(ctx context.Context, rec *ops.Record) (proof.LedgerEntry, error) {
	var e proof.LedgerEntry
	_, err := r.a.c.post(ctx, fmt.Sprintf("/api/agent/jobs/%d/attest", r.jobID), protocol.AttestRequest{
		Kind: rec.Kind, SnapshotID: rec.SnapshotID, Source: rec.Source, Passed: rec.Passed,
		Envelope: rec.Envelope, Timestamp: rec.Timestamp,
	}, &e)
	return e, err
}

func (a *Agent) execute(parent context.Context, lease *protocol.Lease) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	jl := &jobLog{a: a, jobID: lease.JobID}
	jl.Logf("job #%d: %s %s", lease.JobID, lease.Kind, lease.Source.Name)

	// Heartbeat: flush logs and extend the lease; abort if the server says
	// the job is no longer ours.
	done := make(chan struct{})
	go func() {
		interval := time.Duration(lease.LeaseSecs) * time.Second / 3
		if interval <= 0 {
			interval = time.Minute
		}
		if interval > 10*time.Second {
			interval = 10 * time.Second
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := jl.flush(ctx); err != nil && strings.Contains(err.Error(), "409") {
					fmt.Fprintln(os.Stderr, "lease lost, cancelling job:", err)
					cancel()
					return
				}
			}
		}
	}()

	result, err := a.run(ctx, lease, jl)
	close(done)
	fin := protocol.FinishRequest{OK: err == nil, Result: result}
	if err != nil {
		fin.Error = err.Error()
		jl.Logf("job failed: %v", err)
	} else {
		jl.Logf("job finished")
	}
	fctx, fcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer fcancel()
	if err := jl.flush(fctx); err != nil {
		fmt.Fprintln(os.Stderr, "could not upload job log:", err)
	}
	if _, err := a.c.post(fctx, fmt.Sprintf("/api/agent/jobs/%d/finish", lease.JobID), fin, nil); err != nil {
		fmt.Fprintln(os.Stderr, "could not report job result:", err)
	}
}

func (a *Agent) run(ctx context.Context, lease *protocol.Lease, jl *jobLog) (any, error) {
	if lease.Repository.Type == "local" {
		if err := ops.StorageDenied(lease.Repository.Path, a.deny); err != nil {
			return nil, err
		}
	}
	be, err := backend.Open(ctx, lease.Repository, lease.Creds)
	if err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	r, err := repo.Open(ctx, be, []byte(lease.Password))
	if errors.Is(err, repo.ErrNotARepo) && lease.Kind == "backup" {
		jl.Logf("initializing new repository at %s", be.Location())
		r, err = repo.Init(ctx, be, []byte(lease.Password), chunker.DefaultParams)
	}
	if err != nil {
		be.Close()
		return nil, err
	}
	defer r.Close()
	env := &ops.Env{
		Repo: r, Signer: a.key, Ledger: &remoteLedger{a: a, jobID: lease.JobID}, TSAs: lease.TSAs, Log: jl.Logf,
		Storage: proof.StorageInfo{Location: be.Location(), ObjectLockMode: lease.Repository.ObjectLockMode, ObjectLockDays: lease.Repository.ObjectLockDays},
	}
	env.DenyPaths = a.deny
	switch lease.Kind {
	case "backup":
		if lease.Source.Kind == "import" {
			return ops.Import(ctx, env, lease.Source)
		}
		snap, _, err := ops.Backup(ctx, env, lease.Source, nil)
		if err != nil {
			return nil, err
		}
		res := map[string]any{"snapshotId": snap.ID.String(), "root": snap.Root, "stats": snap.Stats}
		forgotten, pruned, merr := ops.Maintain(ctx, env, lease.Source.Name, lease.Retention, lease.Verified)
		res["forgotten"], res["pruned"] = forgotten, pruned
		if merr != nil {
			jl.Logf("warning: retention/prune failed: %v", merr)
			res["maintenanceError"] = merr.Error()
		}
		return res, nil
	case "drill":
		work := filepath.Join(a.dir, "drills")
		if err := os.MkdirAll(work, 0o700); err != nil {
			return nil, fmt.Errorf("drill work dir: %w", err)
		}
		res, rec, err := ops.DrillAttested(ctx, env, lease.Source, lease.SnapshotID, lease.ExpectedRoot, work)
		out := map[string]any{}
		if res != nil {
			out["passed"], out["checks"], out["rtoMs"] = res.Passed, res.Checks, res.FinishedAt.Sub(res.StartedAt).Milliseconds()
		}
		if rec != nil {
			out["snapshotId"] = rec.SnapshotID
		}
		return out, err
	case "check":
		res, err := engine.Check(ctx, r, engine.CheckOptions{ReadDataPercent: 5, Seed: []byte(lease.SampleSeed), Log: jl.Logf})
		if err != nil {
			return nil, err
		}
		if !res.OK() {
			return res, fmt.Errorf("repository check found %d missing and %d corrupt blobs", res.MissingBlobs, res.CorruptBlobs)
		}
		return res, nil
	}
	return nil, fmt.Errorf("unknown job kind %q", lease.Kind)
}

// reportInventory tells the server what this machine could protect, at
// start and every 15 minutes.
func (a *Agent) reportInventory(ctx context.Context) {
	for {
		inv := discover.Collect(ctx)
		if _, err := a.c.post(ctx, "/api/agent/inventory", inv, nil); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "inventory report failed:", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Minute):
		}
	}
}
