package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/chmuzamil/backupproof/internal/protocol"
)

// Second copies (3-2-1): after each backup, a copy job puts the backup into
// the item's second storage, re-encrypted with that storage's keys, and signs
// a copy proof whose content root must equal the original's.

func (s *Server) queueCopy(sourceID int64, result any) {
	src, err := s.store.Source(sourceID)
	if err != nil || src.CopyRepoID == nil || *src.CopyRepoID == src.RepoID {
		return
	}
	var res struct {
		SnapshotID string `json:"snapshotId"`
	}
	if b, err := json.Marshal(result); err == nil {
		_ = json.Unmarshal(b, &res)
	}
	if res.SnapshotID == "" {
		return
	}
	if _, err := s.store.EnqueueJobParams("copy", src.ID, src.AgentID, "after-backup", protocol.Restore{SnapshotID: res.SnapshotID}); err != nil {
		s.log.Printf("%s: copy not queued: %v", src.Name, err)
		return
	}
	s.wake(src.AgentID)
}

func (s *Server) copyLease(job *Job, src *Source, lease *protocol.Lease) error {
	if src.CopyRepoID == nil {
		return errors.New("this item has no second storage any more")
	}
	raw, err := s.store.JobParams(job.ID)
	if err != nil {
		return err
	}
	var p protocol.Restore
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	root, err := s.attestedRoot(src.ID, p.SnapshotID)
	if err != nil {
		return err
	}
	repo, sec, err := s.store.Repository(*src.CopyRepoID)
	if err != nil {
		return err
	}
	lease.SnapshotID, lease.ExpectedRoot = p.SnapshotID, root
	lease.Copy = &protocol.CopyTarget{Repository: repo.Backend, RepoID: repo.ID, Password: sec.Password, Creds: sec.Credentials}
	return nil
}

// handleSetCopy sets or clears an item's second storage.
func (s *Server) handleSetCopy(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	var req struct {
		RepoID *int64 `json:"repoId"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	src, err := s.store.Source(id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	what := "none"
	if req.RepoID != nil {
		if *req.RepoID == src.RepoID {
			writeErr(w, 400, errors.New("choose a different storage from the one the backups already go to"))
			return
		}
		repo, _, err := s.store.Repository(*req.RepoID)
		if err != nil {
			writeErr(w, 400, errors.New("choose an existing storage"))
			return
		}
		what = repo.Name
	}
	if _, err := s.store.db.Exec("UPDATE sources SET copy_repo_id=? WHERE id=?", req.RepoID, id); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.audit(s.actor(r), "set-copy", fmt.Sprintf("item %q: second copy in %s", src.Name, what))
	// Copy the newest backup straight away, so the second copy exists now.
	if req.RepoID != nil {
		if last := s.store.LastProof(src.ID, "backup", true); last != nil && last.SnapshotID != "" {
			src.CopyRepoID = req.RepoID
			s.queueCopy(src.ID, map[string]string{"snapshotId": last.SnapshotID})
		}
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
