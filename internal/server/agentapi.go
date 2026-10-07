package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/protocol"
)

const leaseDuration = 3 * time.Minute

type agentCtxKey struct{}

func (s *Server) agentAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		a, err := s.store.AgentByToken(tok)
		if tok == "" || err != nil {
			writeErr(w, http.StatusUnauthorized, errors.New("unknown or revoked agent"))
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), agentCtxKey{}, a)))
	}
}

func agentFrom(r *http.Request) *Agent { return r.Context().Value(agentCtxKey{}).(*Agent) }

func (s *Server) agentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/agent/enroll", s.handleEnroll)
	mux.HandleFunc("POST /api/agent/poll", s.agentAuth(s.handlePoll))
	mux.HandleFunc("POST /api/agent/jobs/{id}/log", s.agentAuth(s.handleJobLog))
	mux.HandleFunc("POST /api/agent/jobs/{id}/attest", s.agentAuth(s.handleAttest))
	mux.HandleFunc("POST /api/agent/jobs/{id}/finish", s.agentAuth(s.handleFinish))
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req protocol.EnrollRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if req.Name == "" {
		req.Name = req.Hostname
	}
	id, tok, err := s.store.Enroll(req.Token, Agent{Name: req.Name, Hostname: req.Hostname, OS: req.OS, Version: req.Version, PublicKey: req.PublicKey, Docker: req.Docker})
	if err != nil {
		time.Sleep(500 * time.Millisecond)
		writeErr(w, 403, err)
		return
	}
	pk, _ := proof.ParsePublicKey(req.PublicKey)
	s.audit("agent:"+req.Name, "enroll", fmt.Sprintf("agent #%d host=%s os=%s key=%s", id, req.Hostname, req.OS, pk.KeyID))
	writeJSON(w, 200, protocol.EnrollResponse{AgentID: id, AgentToken: tok, ServerKey: s.key.Public().String()})
}

func (s *Server) waiter(agentID int64) chan struct{} {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	ch, ok := s.waiters[agentID]
	if !ok {
		ch = make(chan struct{}, 1)
		s.waiters[agentID] = ch
	}
	return ch
}

// wake interrupts an agent's long poll so manual runs start immediately.
func (s *Server) wake(agentID int64) {
	select {
	case s.waiter(agentID) <- struct{}{}:
	default:
	}
}

// handlePoll long-polls for up to 25s and returns a lease or 204.
func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	a := agentFrom(r)
	var req protocol.PollRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.store.TouchAgent(a.ID, req.Version, req.Docker); err != nil {
		s.log.Printf("agent #%d: could not record heartbeat: %v", a.ID, err)
	}
	deadline := time.NewTimer(25 * time.Second)
	defer deadline.Stop()
	for {
		job, err := s.store.LeaseJob(a.ID, leaseDuration)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		if job != nil {
			lease, err := s.buildLease(job)
			if err != nil {
				if ferr := s.store.FinishJob(job.ID, a.ID, false, err.Error(), nil); ferr != nil {
					s.log.Printf("job #%d: could not record failure: %v", job.ID, ferr)
				}
				sid := job.SourceID
				s.alert("job-failed", &sid, nil, fmt.Sprintf("A %s couldn't start: %v", plainKind(job.Kind), err))
				continue
			}
			writeJSON(w, 200, lease)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-s.waiter(a.ID):
		case <-time.After(5 * time.Second):
		}
	}
}

func (s *Server) buildLease(job *Job) (*protocol.Lease, error) {
	src, err := s.store.SourceWithSecrets(job.SourceID)
	if err != nil {
		return nil, err
	}
	repo, sec, err := s.store.Repository(src.RepoID)
	if err != nil {
		return nil, err
	}
	verified, err := s.store.VerifiedSnapshots(src.ID)
	if err != nil {
		return nil, fmt.Errorf("listing restore-tested snapshots: %w", err)
	}
	lease := &protocol.Lease{
		JobID: job.ID, Kind: job.Kind, Source: src.Spec, Repository: repo.Backend, RepoID: repo.ID,
		Password: sec.Password, Creds: sec.Credentials, Retention: src.Retention,
		Verified: verified, TSAs: s.tsaURLs(), LeaseSecs: int(leaseDuration.Seconds()),
	}
	if head, _ := s.store.LedgerHead(); head != nil {
		lease.SampleSeed = head.Hash
	}
	if job.Kind == "drill" {
		id, root, err := s.attestedSnapshot(src.ID)
		if err != nil {
			return nil, err
		}
		lease.SnapshotID, lease.ExpectedRoot = id, root
	}
	return lease, nil
}

func (s *Server) handleJobLog(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	var req protocol.LogRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	a := agentFrom(r)
	if err := s.store.TouchAgent(a.ID, a.Version, a.Docker); err != nil {
		s.log.Printf("agent #%d: could not record heartbeat: %v", a.ID, err)
	}
	if err := s.store.ExtendLease(id, a.ID, leaseDuration, req.Lines); err != nil {
		writeErr(w, 409, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleAttest verifies an agent-signed attestation, checks it describes the
// leased job's source, and appends it to the ledger atomically with the
// proof row. Agents cannot forge evidence for sources they were not given.
func (s *Server) handleAttest(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	a := agentFrom(r)
	job, err := s.store.Job(id)
	if err != nil || job.AgentID != a.ID || job.State != "running" {
		writeErr(w, 409, errors.New("job is not running on this agent"))
		return
	}
	var req protocol.AttestRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if req.Envelope == nil {
		writeErr(w, 400, errors.New("envelope required"))
		return
	}
	pk, err := proof.ParsePublicKey(a.PublicKey)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if _, err := req.Envelope.Verify([]proof.PublicKey{pk}); err != nil {
		writeErr(w, 400, fmt.Errorf("attestation signature: %w", err))
		return
	}
	st, err := proof.ParseStatement(req.Envelope)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	src, err := s.store.Source(job.SourceID)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	var pred struct {
		Source     string `json:"source"`
		SnapshotID string `json:"snapshotId"`
		RepoID     string `json:"repoId"`
		Passed     *bool  `json:"passed"`
		RTOMs      *int64 `json:"rtoMs"`
	}
	if err := json.Unmarshal(st.Predicate, &pred); err != nil {
		writeErr(w, 400, fmt.Errorf("attestation predicate: %w", err))
		return
	}
	wantType := map[string]string{"backup": proof.PredicateBackup, "drill": proof.PredicateDrill}[req.Kind]
	if st.PredicateType != wantType || req.Kind != job.Kind {
		writeErr(w, 400, errors.New("attestation kind does not match job"))
		return
	}
	if pred.Source != src.Name || pred.SnapshotID != req.SnapshotID {
		writeErr(w, 400, errors.New("attestation does not describe this job's source"))
		return
	}
	passed := true
	if req.Kind == "drill" {
		passed = pred.Passed != nil && *pred.Passed
	}
	if req.Timestamp != nil {
		env, _ := json.Marshal(req.Envelope)
		if _, err := req.Timestamp.Verify(env, nil); err != nil {
			req.Timestamp = nil // keep the proof, drop an invalid timestamp
		}
	}
	if err := s.store.SetRepoID(src.RepoID, pred.RepoID); err != nil {
		s.log.Printf("repository #%d: could not record repository ID: %v", src.RepoID, err)
	}
	sid := src.ID
	row := &ProofRow{Kind: req.Kind, SourceID: &sid, SourceName: src.Name, SnapshotID: req.SnapshotID, Passed: passed,
		RTOMs: pred.RTOMs, Envelope: req.Envelope, Timestamp: req.Timestamp, Signer: a.Name + " (" + pk.KeyID + ")"}
	entry, err := s.store.AppendLedger(req.Kind, req.SnapshotID, req.Envelope.Digest(), src.Name, func(tx *sql.Tx, e proof.LedgerEntry) error {
		row.LedgerSeq = e.Seq
		return insertProof(tx, row)
	})
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if req.Kind == "drill" {
		if passed {
			s.resolve("drill-failed", &sid, nil)
		} else {
			s.alert("drill-failed", &sid, nil, fmt.Sprintf("%s: the restore test FAILED. The backup may not be usable.", src.Name))
		}
	}
	writeJSON(w, 200, entry)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (s *Server) handleFinish(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	a := agentFrom(r)
	var req protocol.FinishRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	job, err := s.store.Job(id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if err := s.store.FinishJob(id, a.ID, req.OK, req.Error, req.Result); err != nil {
		writeErr(w, 409, err)
		return
	}
	sid := job.SourceID
	kind := "job-failed"
	if req.OK {
		s.resolve(kind, &sid, nil)
	} else {
		name := ""
		if src, err := s.store.Source(sid); err == nil {
			name = src.Name + ": "
		}
		s.alert(kind, &sid, nil, fmt.Sprintf("%sthe %s failed: %s", name, plainKind(job.Kind), req.Error))
	}
	// The first successful backup of a source is immediately restore-tested,
	// so new users see "Restore tested ✓" without waiting for the schedule.
	if job.Kind == "backup" && req.OK && s.store.LastProof(sid, "drill", false) == nil {
		if src, err := s.store.Source(sid); err == nil {
			if _, err := s.store.EnqueueJob("drill", sid, src.DrillAgent(), "first-backup"); err == nil {
				s.wake(src.DrillAgent())
			}
		}
	}
	if job.Kind == "check" {
		s.recordCheck(sid, req.OK, req.Result)
		digest := fmt.Sprintf("%x", jsonDigest(req.Result))
		if _, err := s.store.AppendLedger("check", fmt.Sprintf("source#%d job#%d ok=%v", sid, id, req.OK), digest, "", nil); err != nil {
			s.log.Printf("job #%d: could not append check to ledger: %v", id, err)
		}
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// attestedSnapshot returns the newest snapshot of a source for which the
// server holds a backup attestation it verified against the source's agent
// key, together with the content root that attestation commits to.
func (s *Server) attestedSnapshot(sourceID int64) (string, string, error) {
	last := s.store.LastProof(sourceID, "backup", false)
	if last == nil {
		return "", "", errors.New("there is no verified backup to restore-test yet")
	}
	p, err := s.store.Proof(last.ID)
	if err != nil {
		return "", "", err
	}
	st, err := proof.ParseStatement(p.Envelope)
	if err != nil {
		return "", "", err
	}
	root := st.Subject[0].Digest["blake3"]
	if root == "" {
		return "", "", errors.New("backup proof has no content root")
	}
	return p.SnapshotID, root, nil
}
