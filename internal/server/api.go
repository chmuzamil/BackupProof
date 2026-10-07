package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/engine"
	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/source"
)

const sessionCookie = "bp_session"

type ctxKey int

const userKey ctxKey = 1

var roleRank = map[string]int{"auditor": 1, "operator": 2, "admin": 3}

// auth wraps a handler with session auth, a minimum role and CSRF checks
// for state-changing requests.
func (s *Server) auth(minRole string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// API tokens (Authorization: Bearer bpt_…) for scripts. They are not
		// cookies, so CSRF doesn't apply, but they can't manage accounts or
		// tokens: a leaked token must not be able to create a way back in.
		if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			u, name, err := s.store.TokenUser(strings.TrimSpace(bearer))
			if err != nil {
				writeErr(w, http.StatusUnauthorized, err)
				return
			}
			u.ViaToken = name
			if tokenForbidden(r.Pattern) {
				writeErr(w, http.StatusForbidden, errors.New("API tokens can't manage users, tokens or two-factor sign-in; use the dashboard"))
				return
			}
			if roleRank[u.Role] < roleRank[minRole] {
				writeErr(w, http.StatusForbidden, fmt.Errorf("this token's role (%s) can't do this; it needs %s", u.Role, minRole))
				return
			}
			h(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, errors.New("not signed in"))
			return
		}
		u, csrf, err := s.store.Session(c.Value)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, err)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(csrf)) != 1 {
				writeErr(w, http.StatusForbidden, errors.New("missing or invalid CSRF token"))
				return
			}
		}
		if roleRank[u.Role] < roleRank[minRole] {
			writeErr(w, http.StatusForbidden, fmt.Errorf("requires %s role", minRole))
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	}
}

func currentUser(r *http.Request) *User {
	u, _ := r.Context().Value(userKey).(*User)
	return u
}

func (s *Server) actor(r *http.Request) string {
	if u := currentUser(r); u != nil {
		if u.ViaToken != "" {
			return u.Username + " (API token “" + u.ViaToken + "”)"
		}
		return u.Username
	}
	return "anonymous"
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/setup", s.handleSetup)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/public/keys", s.handlePublicKeys)

	mux.HandleFunc("GET /api/dashboard", s.auth("auditor", s.handleDashboard))

	mux.HandleFunc("GET /api/sources", s.auth("auditor", s.handleListSources))
	mux.HandleFunc("POST /api/sources", s.auth("operator", s.handleSaveSource))
	mux.HandleFunc("GET /api/sources/{id}", s.auth("auditor", s.handleGetSource))
	mux.HandleFunc("PUT /api/sources/{id}", s.auth("operator", s.handleSaveSource))
	mux.HandleFunc("DELETE /api/sources/{id}", s.auth("admin", s.handleDeleteSource))
	mux.HandleFunc("POST /api/sources/{id}/run", s.auth("operator", s.handleRunSource))
	mux.HandleFunc("GET /api/sources/{id}/restore-points", s.auth("admin", s.handleRestorePoints))
	mux.HandleFunc("GET /api/sources/{id}/files", s.auth("admin", s.handleBrowseBackup))
	mux.HandleFunc("GET /api/sources/{id}/download", s.auth("admin", s.handleDownloadFiles))
	mux.HandleFunc("POST /api/sources/{id}/restore", s.auth("admin", s.handleRestore))
	mux.HandleFunc("POST /api/sources/{id}/restore-db", s.auth("admin", s.handleRestoreDB))
	mux.HandleFunc("PUT /api/sources/{id}/copy", s.auth("admin", s.handleSetCopy))

	mux.HandleFunc("GET /api/jobs", s.auth("auditor", s.handleListJobs))
	mux.HandleFunc("GET /api/jobs/{id}", s.auth("auditor", s.handleGetJob))

	mux.HandleFunc("GET /api/agents", s.auth("auditor", s.handleListAgents))
	mux.HandleFunc("POST /api/agents/enroll-token", s.auth("admin", s.handleEnrollToken))
	mux.HandleFunc("POST /api/agents/{id}/revoke", s.auth("admin", s.handleRevokeAgent))
	mux.HandleFunc("PUT /api/agents/{id}/limits", s.auth("operator", s.handlePutLimits))

	mux.HandleFunc("GET /api/repositories", s.auth("auditor", s.handleListRepos))
	mux.HandleFunc("POST /api/repositories", s.auth("admin", s.handleCreateRepo))
	mux.HandleFunc("DELETE /api/repositories/{id}", s.auth("admin", s.handleDeleteRepo))
	mux.HandleFunc("POST /api/repositories/{id}/check", s.auth("operator", s.handleCheckRepo))
	mux.HandleFunc("GET /api/repositories/stats", s.auth("auditor", s.handleRepoStats))

	mux.HandleFunc("GET /api/proofs", s.auth("auditor", s.handleListProofs))
	mux.HandleFunc("GET /api/proofs/{id}", s.auth("auditor", s.handleGetProof))
	mux.HandleFunc("GET /api/proofs/{id}/bundle", s.auth("auditor", s.handleProofBundle))
	mux.HandleFunc("GET /api/ledger", s.auth("auditor", s.handleLedger))
	mux.HandleFunc("GET /api/ledger/verify", s.auth("auditor", s.handleLedgerVerify))
	mux.HandleFunc("GET /api/evidence", s.auth("auditor", s.handleEvidence))
	mux.HandleFunc("GET /api/evidence/report", s.auth("auditor", s.handleEvidenceReport))

	mux.HandleFunc("GET /api/alerts", s.auth("auditor", s.handleAlerts))

	mux.HandleFunc("GET /api/settings/notify", s.auth("admin", s.handleGetNotify))
	mux.HandleFunc("PUT /api/settings/notify", s.auth("admin", s.handlePutNotify))
	mux.HandleFunc("POST /api/settings/notify/test", s.auth("admin", s.handleTestNotify))
	mux.HandleFunc("POST /api/settings/report/send", s.auth("admin", s.handleSendReport))
	mux.HandleFunc("GET /api/settings/tsa", s.auth("auditor", s.handleGetTSA))
	mux.HandleFunc("PUT /api/settings/tsa", s.auth("admin", s.handlePutTSA))
	mux.HandleFunc("PUT /api/settings/server", s.auth("admin", s.handlePutServerSettings))

	mux.HandleFunc("GET /api/users", s.auth("admin", s.handleListUsers))
	mux.HandleFunc("POST /api/users", s.auth("admin", s.handleCreateUser))
	mux.HandleFunc("DELETE /api/users/{id}", s.auth("admin", s.handleDeleteUser))
	mux.HandleFunc("POST /api/users/password", s.auth("auditor", s.handleChangePassword))
	s.accountRoutes(mux)
}

// --- session ---------------------------------------------------------------

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.UserCount()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	pub := s.publicURL()
	resp := map[string]any{"version": engine.Version, "setupRequired": users == 0, "setupCodeRequired": users == 0 && !isLocalRequest(r), "user": nil, "csrf": "",
		"publicUrl": pub, "publicUrlIsLocal": strings.Contains(pub, "localhost") || strings.Contains(pub, "127.0.0.1")}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if u, csrf, err := s.store.Session(c.Value); err == nil {
			resp["user"], resp["csrf"] = u, csrf
		}
	}
	writeJSON(w, 200, resp)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *User) {
	token, csrf, err := s.store.CreateSession(u.ID, 12*time.Hour)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", SameSite: http.SameSiteStrictMode, MaxAge: 12 * 3600})
	writeJSON(w, 200, map[string]any{"user": u, "csrf": csrf})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password, SetupCode string }
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	s.store.wmu.Lock()
	users, err := s.store.UserCount()
	if err != nil {
		s.store.wmu.Unlock()
		writeErr(w, 500, err)
		return
	}
	if users > 0 {
		s.store.wmu.Unlock()
		writeErr(w, 409, errors.New("setup already completed"))
		return
	}
	if err := s.checkSetupCode(r, req.SetupCode); err != nil {
		s.store.wmu.Unlock()
		time.Sleep(500 * time.Millisecond)
		writeErr(w, http.StatusForbidden, err)
		return
	}
	id, err := s.store.CreateUser(strings.TrimSpace(req.Username), req.Password, "admin")
	s.store.wmu.Unlock()
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	s.removeSetupCode()
	s.audit(req.Username, "setup", "created initial admin account")
	s.startSession(w, r, &User{ID: id, Username: req.Username, Role: "admin"})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := s.store.DeleteSession(c.Value); err != nil {
			writeErr(w, 500, err)
			return
		}
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// --- dashboard -------------------------------------------------------------

type SourceStatus struct {
	Source           Source    `json:"source"`
	AgentName        string    `json:"agentName"`
	VerifierName     string    `json:"verifierName,omitempty"`
	RepoName         string    `json:"repoName"`
	Status           string    `json:"status"`
	Reason           string    `json:"reason"`
	LastBackup       *ProofRow `json:"lastBackup"`
	LastDrill        *ProofRow `json:"lastDrill"`
	LastPassingDrill *ProofRow `json:"lastPassingDrill"`
	Running          *Job      `json:"running"`
}

func (s *Server) sourceStatus(src Source, agents map[int64]string, repos map[int64]string) SourceStatus {
	st := SourceStatus{Source: src, AgentName: agents[src.AgentID], RepoName: repos[src.RepoID]}
	if src.VerifierID != nil {
		st.VerifierName = agents[*src.VerifierID]
	}
	st.LastBackup = s.store.LastProof(src.ID, "backup", false)
	st.LastDrill = s.store.LastProof(src.ID, "drill", false)
	st.LastPassingDrill = s.store.LastProof(src.ID, "drill", true)
	for _, state := range []string{"running", "queued"} {
		for _, kind := range []string{"backup", "drill", "check"} {
			if j := s.store.LastJob(src.ID, kind, state); j != nil && st.Running == nil {
				st.Running = j
			}
		}
	}
	lastFailed := s.store.LastJob(src.ID, "backup", "failed")
	lastOK := s.store.LastJob(src.ID, "backup", "succeeded")
	maxAge := time.Duration(src.ProofMaxAgeHours) * time.Hour
	switch {
	case st.LastDrill != nil && !st.LastDrill.Passed:
		st.Status, st.Reason = "failing", "The last restore test failed, so this backup may not be usable. Open it to see what went wrong."
	case lastFailed != nil && (lastOK == nil || lastFailed.ID > lastOK.ID):
		st.Status, st.Reason = "failing", "The last backup didn't finish: "+lastFailed.Error
	case st.LastPassingDrill == nil:
		if st.LastBackup == nil {
			st.Status, st.Reason = "unproven", "No backup yet."
		} else {
			st.Status, st.Reason = "unproven", "Backed up, but not restore-tested yet."
		}
	default:
		t, _ := time.Parse(time.RFC3339Nano, st.LastPassingDrill.Created)
		age := time.Since(t)
		if age > maxAge {
			st.Status, st.Reason = "at-risk", fmt.Sprintf("Last restore test was %s ago, so a new one is overdue.", humanDur(age))
		} else {
			st.Status, st.Reason = "proven", fmt.Sprintf("Restore tested %s ago.", humanDur(age))
		}
	}
	return st
}

func (s *Server) nameMaps() (map[int64]string, map[int64]string) {
	agents, repos := map[int64]string{}, map[int64]string{}
	if as, err := s.store.Agents(); err == nil {
		for _, a := range as {
			agents[a.ID] = a.Name
		}
	}
	if rs, err := s.store.Repositories(); err == nil {
		for _, r := range rs {
			repos[r.ID] = r.Name
		}
	}
	return agents, repos
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	sources, err := s.store.Sources()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	agentNames, repoNames := s.nameMaps()
	out := []SourceStatus{}
	counts := map[string]int{"sources": len(sources)}
	for _, src := range sources {
		st := s.sourceStatus(src, agentNames, repoNames)
		out = append(out, st)
		switch st.Status {
		case "proven":
			counts["proven"]++
		case "at-risk", "unproven":
			counts["atRisk"]++
		case "failing":
			counts["failing"]++
		}
	}
	agents, _ := s.store.Agents()
	agents = s.markBuiltin(agents)
	for _, a := range agents {
		if !a.Revoked {
			counts["agentsTotal"]++
			if a.Online {
				counts["agentsOnline"]++
			}
		}
	}
	alerts, _ := s.store.Alerts(true, 50)
	writeJSON(w, 200, map[string]any{"sources": out, "agents": nonNil(agents), "alerts": nonNil(alerts), "counts": counts})
}

func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// --- sources ---------------------------------------------------------------

func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	src, err := s.store.Sources()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, nonNil(src))
}

func (s *Server) handleGetSource(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	src, err := s.store.Source(id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	a, rp := s.nameMaps()
	writeJSON(w, 200, s.sourceStatus(*src, a, rp))
}

func (s *Server) handleSaveSource(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source
		Secret *SourceSecret `json:"secret"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	src := req.Source
	var existing *Source
	if r.Method == http.MethodPut {
		id, err := pathID(r)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		existing, err = s.store.Source(id)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		src.ID, src.Name = id, existing.Name
	}
	if u := currentUser(r); u == nil || u.Role != "admin" {
		if err := operatorMayChange(existing, &src); err != nil {
			writeErr(w, http.StatusForbidden, err)
			return
		}
	}
	if _, err := s.store.Agent(src.AgentID); err != nil {
		writeErr(w, 400, errors.New("agent not found"))
		return
	}
	if src.VerifierID != nil && *src.VerifierID == 0 {
		src.VerifierID = nil
	}
	if src.VerifierID != nil {
		if _, err := s.store.Agent(*src.VerifierID); err != nil {
			writeErr(w, 400, errors.New("verifier agent not found"))
			return
		}
	}
	if _, _, err := s.store.Repository(src.RepoID); err != nil {
		writeErr(w, 400, err)
		return
	}
	// Secrets arriving in the spec are moved to the encrypted secret column.
	sec := req.Secret
	if src.Spec.Password != "" || (src.Spec.URI != "" && src.Spec.URI != "(redacted)") {
		if sec == nil {
			sec = &SourceSecret{}
		}
		if sec.Password == "" {
			sec.Password = src.Spec.Password
		}
		if sec.URI == "" {
			sec.URI = src.Spec.URI
		}
	}
	if im := src.Spec.Import; im != nil && (im.Password != "" || im.PrivateKey != "" || im.Credentials != (backend.Credentials{})) {
		if sec == nil {
			sec = &SourceSecret{}
		}
		if sec.ImportPassword == "" {
			sec.ImportPassword = im.Password
		}
		if sec.ImportCredentials == (backend.Credentials{}) {
			sec.ImportCredentials = im.Credentials
		}
		if sec.ImportPrivateKey == "" {
			sec.ImportPrivateKey = im.PrivateKey
		}
	}
	id, err := s.store.SaveSource(&src, sec)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	s.audit(s.actor(r), "save-source", fmt.Sprintf("source %q (#%d) kind=%s agent=%d repo=%d backup=%q drill=%q", src.Name, id, src.Spec.Kind, src.AgentID, src.RepoID, src.BackupCron, src.DrillCron))
	writeJSON(w, 200, map[string]int64{"id": id})
}

func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	src, err := s.store.Source(id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if err := s.store.DeleteSource(id); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.audit(s.actor(r), "delete-source", fmt.Sprintf("source %q (#%d); repository data and proofs are retained", src.Name, id))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleRunSource(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	var req struct{ Kind string }
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	src, err := s.store.Source(id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	agent := src.AgentID
	switch req.Kind {
	case "backup":
	case "drill", "check":
		agent = src.DrillAgent()
	default:
		writeErr(w, 400, errors.New("kind must be backup, drill or check"))
		return
	}
	jobID, err := s.store.EnqueueJob(req.Kind, id, agent, "manual:"+s.actor(r))
	if err != nil {
		writeErr(w, 409, err)
		return
	}
	s.wake(agent)
	s.audit(s.actor(r), "run-"+req.Kind, fmt.Sprintf("source %q job #%d", src.Name, jobID))
	writeJSON(w, 200, map[string]int64{"jobId": jobID})
}

// --- jobs ------------------------------------------------------------------

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	sid, _ := strconv.ParseInt(r.URL.Query().Get("source"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	jobs, err := s.store.Jobs(sid, limit)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, nonNil(jobs))
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	j, err := s.store.Job(id)
	if err != nil {
		writeErr(w, 404, errors.New("job not found"))
		return
	}
	writeJSON(w, 200, j)
}

// --- agents ----------------------------------------------------------------

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.Agents()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, s.markBuiltin(nonNil(a)))
}

func (s *Server) handleEnrollToken(w http.ResponseWriter, r *http.Request) {
	ttl := time.Hour
	tok, err := s.store.CreateEnrollToken(s.actor(r), ttl)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	s.audit(s.actor(r), "create-enroll-token", "single-use, expires in 1h")
	url := s.publicURL()
	writeJSON(w, 200, map[string]any{
		"token":    tok,
		"expires":  time.Now().Add(ttl).UTC(),
		"command":  fmt.Sprintf("backupproof agent enroll --server %s --token %s && backupproof agent run", url, tok),
		"commands": s.installCommands(tok),
	})
}

func (s *Server) handleRevokeAgent(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.store.RevokeAgent(id); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.audit(s.actor(r), "revoke-agent", fmt.Sprintf("agent #%d", id))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// --- repositories ------------------------------------------------------------

func (s *Server) handleListRepos(w http.ResponseWriter, r *http.Request) {
	rs, err := s.store.Repositories()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, nonNil(rs))
}

func (s *Server) handleDeleteRepo(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	repo, _, err := s.store.Repository(id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if err := s.store.DeleteRepository(id); err != nil {
		var inUse ErrRepositoryInUse
		if errors.As(err, &inUse) {
			writeErr(w, http.StatusConflict, err)
			return
		}
		writeErr(w, 500, err)
		return
	}
	s.audit(s.actor(r), "delete-repository", fmt.Sprintf("storage %q (#%d) removed from the dashboard; the backups in it were not deleted", repo.Name, id))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string              `json:"name"`
		Backend     backend.Config      `json:"backend"`
		Password    string              `json:"password"`
		Credentials backend.Credentials `json:"credentials"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	switch req.Backend.Type {
	case "local", "s3", "sftp":
	default:
		writeErr(w, 400, errors.New("backend type must be local, s3 or sftp"))
		return
	}
	if m := req.Backend.ObjectLockMode; m != "" && m != "GOVERNANCE" && m != "COMPLIANCE" {
		writeErr(w, 400, errors.New("objectLockMode must be GOVERNANCE or COMPLIANCE"))
		return
	}
	id, err := s.store.CreateRepository(strings.TrimSpace(req.Name), req.Backend, RepoSecret{Password: req.Password, Credentials: req.Credentials})
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	s.audit(s.actor(r), "create-repository", fmt.Sprintf("repository %q type=%s lock=%s/%dd", req.Name, req.Backend.Type, req.Backend.ObjectLockMode, req.Backend.ObjectLockDays))
	writeJSON(w, 200, map[string]int64{"id": id})
}

// --- proofs & ledger -----------------------------------------------------------

func (s *Server) handleListProofs(w http.ResponseWriter, r *http.Request) {
	sid, _ := strconv.ParseInt(r.URL.Query().Get("source"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	ps, err := s.store.Proofs(sid, limit)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, nonNil(ps))
}

func (s *Server) handleGetProof(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, 200, p)
}

func (s *Server) handleLedger(w http.ResponseWriter, r *http.Request) {
	from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	if from <= 0 {
		from = 1
	}
	rows, err := s.store.Ledger(from, limit)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, nonNil(rows))
}

func (s *Server) handleLedgerVerify(w http.ResponseWriter, r *http.Request) {
	entries, err := s.store.LedgerEntries(1)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	resp := map[string]any{"ok": true, "entries": len(entries), "head": ""}
	if len(entries) > 0 {
		resp["head"] = entries[len(entries)-1].Hash
	}
	if err := proof.VerifyChain(entries); err != nil {
		resp["ok"], resp["error"] = false, err.Error()
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.Alerts(r.URL.Query().Get("open") == "1", 200)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, nonNil(a))
}

// --- settings ------------------------------------------------------------------

// handleGetNotify never returns secrets; "saved" lists the ones that are set.
func (s *Server) handleGetNotify(w http.ResponseWriter, r *http.Request) {
	n := s.notifySettings()
	saved := []string{}
	for k, p := range n.secretFields() {
		if *p != "" {
			saved = append(saved, k)
		}
		*p = ""
	}
	writeJSON(w, 200, map[string]any{"settings": n, "saved": saved})
}

func (s *Server) handlePutNotify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NotifySettings
		Clear []string `json:"clear"` // secrets to remove
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	n := req.NotifySettings
	// An empty secret keeps the saved one, but only while it still goes to
	// the same place: otherwise changing the address and sending a test
	// would hand the saved password or token to another server.
	old := s.notifySettings()
	oldSecrets := old.secretFields()
	moved := map[string]bool{
		"smtpPass":    n.SMTPHost != old.SMTPHost || n.SMTPPort != old.SMTPPort,
		"ntfyToken":   n.NtfyURL != old.NtfyURL,
		"gotifyToken": n.GotifyURL != old.GotifyURL,
	}
	clear := req
	for k, p := range n.secretFields() {
		if *p == "" && !moved[k] {
			*p = *oldSecrets[k]
		}
	}
	for _, k := range clear.Clear {
		if p, ok := n.secretFields()[k]; ok {
			*p = ""
		}
	}
	if n.ReportDay < 0 || n.ReportDay > 6 || n.ReportHour < 0 || n.ReportHour > 23 {
		writeErr(w, 400, errors.New("choose a day of the week and an hour between 0 and 23 for the weekly summary"))
		return
	}
	b, _ := jsonMarshal(n)
	if err := s.store.SetSetting("notify", string(b)); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.audit(s.actor(r), "update-notifications", "")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleTestNotify(w http.ResponseWriter, r *http.Request) {
	if err := s.send("BackupProof test alert", "If you can read this, BackupProof alerts will reach you here."); err != nil {
		writeErr(w, 502, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) tsaURLs() []string {
	raw := s.store.Setting("tsa")
	if raw == "" {
		return nil
	}
	var urls []string
	if err := jsonUnmarshal([]byte(raw), &urls); err != nil {
		s.log.Printf("warning: ignoring unreadable TSA setting: %v", err)
		return nil
	}
	return urls
}

func (s *Server) handleGetTSA(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"urls": nonNil(s.tsaURLs())})
}

func (s *Server) handlePutTSA(w http.ResponseWriter, r *http.Request) {
	var req struct{ URLs []string }
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	for _, u := range req.URLs {
		if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			writeErr(w, 400, fmt.Errorf("invalid TSA URL %q", u))
			return
		}
	}
	b, _ := jsonMarshal(nonNil(req.URLs))
	if err := s.store.SetSetting("tsa", string(b)); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.audit(s.actor(r), "update-tsa", strings.Join(req.URLs, ", "))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// --- users ---------------------------------------------------------------------

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	u, err := s.store.Users()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, nonNil(u))
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password, Role string }
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if _, ok := roleRank[req.Role]; !ok {
		writeErr(w, 400, errors.New("role must be admin, operator or auditor"))
		return
	}
	id, err := s.store.CreateUser(strings.TrimSpace(req.Username), req.Password, req.Role)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	s.audit(s.actor(r), "create-user", fmt.Sprintf("%s (%s)", req.Username, req.Role))
	writeJSON(w, 200, map[string]int64{"id": id})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.store.DeleteUser(id); err != nil {
		writeErr(w, 400, err)
		return
	}
	s.audit(s.actor(r), "delete-user", fmt.Sprintf("user #%d", id))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handlePutServerSettings sets the address other computers use to reach
// this dashboard (needed for the one-line install commands).
func (s *Server) handlePutServerSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PublicURL string `json:"publicUrl"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	u := strings.TrimRight(strings.TrimSpace(req.PublicURL), "/")
	if u != "" && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		writeErr(w, 400, errors.New("the address must start with https:// (or http:// for testing)"))
		return
	}
	if err := s.store.SetSetting("publicUrl", u); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.audit(s.actor(r), "update-public-url", u)
	writeJSON(w, 200, map[string]any{"ok": true, "publicUrl": s.publicURL()})
}

// humanDur renders durations the way people say them.
func humanDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour")
	}
	return plural(int(d.Hours()/24), "day")
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// operatorMayChange enforces what non-admin users may configure. Anything
// that runs commands on a server (hooks, command sources, custom restore
// checks), moves an item to a different server (which would hand that
// server the item's secrets), or reads arbitrary local paths through rclone
// can turn "set up backups" into control of the server, so it is admin-only.
// Values that are unchanged from what an admin already saved are allowed.
func operatorMayChange(old *Source, src *Source) error {
	var o source.Spec
	if old != nil {
		o = old.Spec
	}
	n := src.Spec
	deny := func(what string) error {
		return fmt.Errorf("only an administrator can change %s", what)
	}
	if n.Kind == "command" && o.Kind != "command" {
		return deny("command-based backups")
	}
	if n.Kind == "command" && n.Command != o.Command {
		return deny("backup commands")
	}
	if n.PreHook != o.PreHook || n.PostHook != o.PostHook {
		return deny("commands that run before or after a backup")
	}
	if n.Drill.Command != o.Drill.Command {
		return deny("custom restore-test commands")
	}
	if old != nil {
		if src.AgentID != old.AgentID {
			return deny("which server an item runs on")
		}
		ov, nv := int64(0), int64(0)
		if old.VerifierID != nil {
			ov = *old.VerifierID
		}
		if src.VerifierID != nil {
			nv = *src.VerifierID
		}
		if ov != nv {
			return deny("which server runs restore tests")
		}
	}
	if n.Import != nil && n.Import.Storage.Type == "rclone" && strings.HasPrefix(strings.TrimSpace(n.Import.Storage.Remote), ":") {
		if o.Import == nil || o.Import.Storage.Remote != n.Import.Storage.Remote {
			return deny("on-the-fly rclone remotes (use a remote set up with rclone config)")
		}
	}
	return nil
}
