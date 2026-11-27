package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/repo"
)

// Storage format v2 (packs and an index) needs agents from 0.4 on: an older
// agent refuses storage in a format it doesn't know. The dashboard therefore
// asks for v2 only when every server that uses a storage can read it.

var agentVersionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// agentAtLeast reports whether an agent's version string is at least major.minor.
// An unknown version counts as too old.
func agentAtLeast(version string, major, minor int) bool {
	m := agentVersionRe.FindStringSubmatch(version)
	if m == nil {
		return false
	}
	ma, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	return ma > major || ma == major && mi >= minor
}

// storageUsers are the servers that read or write a storage: each item's own
// server and its restore-test server, for items kept there or copied there.
func (s *Server) storageUsers(repoID int64) ([]Agent, error) {
	sources, err := s.store.Sources()
	if err != nil {
		return nil, err
	}
	ids := map[int64]bool{}
	for _, src := range sources {
		uses := src.RepoID == repoID || src.CopyRepoID != nil && *src.CopyRepoID == repoID
		if !uses {
			continue
		}
		ids[src.AgentID] = true
		if src.VerifierID != nil {
			ids[*src.VerifierID] = true
		}
	}
	var out []Agent
	for id := range ids {
		if a, err := s.store.Agent(id); err == nil && !a.Removed && !a.Revoked {
			out = append(out, *a)
		}
	}
	return out, nil
}

// tooOldForV2 lists the servers using a storage that can't read format v2.
func (s *Server) tooOldForV2(repoID int64) ([]string, error) {
	users, err := s.storageUsers(repoID)
	if err != nil {
		return nil, err
	}
	var old []string
	for _, a := range users {
		if !agentAtLeast(a.Version, 0, 4) {
			old = append(old, a.Name)
		}
	}
	return old, nil
}

// newStorageFormat is the format an agent should create a storage in.
func (s *Server) newStorageFormat(repoID int64) int {
	if old, err := s.tooOldForV2(repoID); err != nil || len(old) > 0 {
		return 1
	}
	return 2
}

// StorageFormatStatus is what the Storage page shows about a storage's format.
type StorageFormatStatus struct {
	Version  int      `json:"version"` // 0: not created yet, or unreadable
	TooOld   []string `json:"tooOld,omitempty"`
	Upgrade  string   `json:"upgrade,omitempty"` // running | done | failed
	Error    string   `json:"error,omitempty"`
	Location bool     `json:"reachable"`
}

var upgrades sync.Map // repository ID → *upgradeState

type upgradeState struct {
	mu    sync.Mutex
	state string
	err   string
}

func (s *Server) handleStorageFormat(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	rp, sec, err := s.store.Repository(id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	st := StorageFormatStatus{}
	st.TooOld, _ = s.tooOldForV2(id)
	if u, ok := upgrades.Load(id); ok {
		us := u.(*upgradeState)
		us.mu.Lock()
		st.Upgrade, st.Error = us.state, us.err
		us.mu.Unlock()
	}
	if rp.Backend.Type == "local" && !s.isBuiltinStorage(rp.Backend) {
		writeJSON(w, 200, st) // a folder on another server: the dashboard can't look
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	be, err := backend.Open(ctx, rp.Backend, sec.Credentials)
	if err != nil {
		writeJSON(w, 200, st)
		return
	}
	defer be.Close()
	st.Location = true
	if raw, err := be.Get(ctx, "config"); err == nil {
		var cfg repo.Config
		if json.Unmarshal(raw, &cfg) == nil {
			st.Version = cfg.Version
		}
	}
	writeJSON(w, 200, st)
}

// isBuiltinStorage: a folder storage the dashboard's own server can open.
func (s *Server) isBuiltinStorage(cfg backend.Config) bool {
	sources, err := s.store.Sources()
	if err != nil {
		return false
	}
	for _, src := range sources {
		if rp, _, err := s.store.Repository(src.RepoID); err == nil && rp.Backend.Path == cfg.Path && src.AgentID == s.builtinAgentID() {
			return true
		}
	}
	return false
}

// handleUpgradeStorage moves a storage to format v2 in the background.
func (s *Server) handleUpgradeStorage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	rp, sec, err := s.store.Repository(id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if old, err := s.tooOldForV2(id); err != nil || len(old) > 0 {
		writeErr(w, 409, fmt.Errorf("update these servers to 0.4 or later first, or they can't use the storage afterwards: %v", old))
		return
	}
	if rp.Backend.Type == "local" && !s.isBuiltinStorage(rp.Backend) {
		writeErr(w, 400, errors.New("this storage is a folder on another server: upgrade it there with `backupproof upgrade --repo <folder>`"))
		return
	}
	us := &upgradeState{state: "running"}
	if prev, loaded := upgrades.LoadOrStore(id, us); loaded {
		p := prev.(*upgradeState)
		p.mu.Lock()
		running := p.state == "running"
		p.mu.Unlock()
		if running {
			writeErr(w, 409, errors.New("this storage is already being upgraded"))
			return
		}
		upgrades.Store(id, us)
	}
	// Nothing else may use the storage while it changes format: a prune
	// could delete blobs the new index lists, and a backup could add blobs
	// it doesn't. Its jobs wait in the queue until the upgrade ends.
	running, err := s.store.PauseRepository(id)
	if err == nil && dashboardRunning.Load() && s.dashboardRepoID() == id {
		running++ // the dashboard is backing itself up to this storage
	}
	if err != nil || running > 0 {
		s.store.ResumeRepository(id)
		upgrades.Delete(id)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeErr(w, 409, fmt.Errorf("%d jobs are using this storage right now; upgrade it when they finish", running))
		return
	}
	actor := s.actor(r)
	go func() {
		defer s.store.ResumeRepository(id)
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		err := s.upgradeStorage(ctx, rp, sec)
		us.mu.Lock()
		if err != nil {
			us.state, us.err = "failed", err.Error()
		} else {
			us.state = "done"
		}
		us.mu.Unlock()
		detail := fmt.Sprintf("storage %q (#%d) upgraded to format v2", rp.Name, id)
		if err != nil {
			detail = fmt.Sprintf("upgrading storage %q (#%d) failed: %v", rp.Name, id, err)
		}
		s.audit(actor, "upgrade-storage", detail)
	}()
	writeJSON(w, 202, map[string]string{"upgrade": "running"})
}

func (s *Server) upgradeStorage(ctx context.Context, rp *Repository, sec *RepoSecret) error {
	be, err := backend.Open(ctx, rp.Backend, sec.Credentials)
	if err != nil {
		return fmt.Errorf("opening the storage: %s", friendlyStorageError(err))
	}
	r, err := repo.Open(ctx, be, []byte(sec.Password))
	if err != nil {
		be.Close()
		return err
	}
	defer r.Close()
	return r.Upgrade(ctx, func(format string, a ...any) { s.log.Printf("upgrade %s: "+format, append([]any{rp.Name}, a...)...) })
}
