package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/chmuzamil/backupproof/internal/engine"
)

// Storage health: a check job per storage every week (or on demand) reads a
// sample of the data back and measures the storage's size, so problems are
// found between restore tests and growth is visible.

const checkEvery = 7 * 24 * time.Hour

var storageMigrations = []string{`CREATE TABLE IF NOT EXISTS repo_stats (
  id INTEGER PRIMARY KEY, repo_id INTEGER NOT NULL, time TEXT NOT NULL, ok INTEGER NOT NULL,
  bytes INTEGER NOT NULL DEFAULT 0, blobs INTEGER NOT NULL DEFAULT 0, snapshots INTEGER NOT NULL DEFAULT 0,
  read_blobs INTEGER NOT NULL DEFAULT 0, corrupt INTEGER NOT NULL DEFAULT 0, missing INTEGER NOT NULL DEFAULT 0)`,
	`CREATE INDEX IF NOT EXISTS repo_stats_repo ON repo_stats(repo_id, id)`}

type RepoStat struct {
	Time      string `json:"time"`
	OK        bool   `json:"ok"`
	Bytes     int64  `json:"bytes"`
	Blobs     int    `json:"blobs"`
	Snapshots int    `json:"snapshots"`
	ReadBlobs int    `json:"readBlobs"`
	Corrupt   int    `json:"corrupt"`
	Missing   int    `json:"missing"`
}

func (s *Store) AddRepoStat(repoID int64, ok bool, r engine.CheckResult) error {
	_, err := s.db.Exec("INSERT INTO repo_stats(repo_id,time,ok,bytes,blobs,snapshots,read_blobs,corrupt,missing) VALUES(?,?,?,?,?,?,?,?,?)",
		repoID, now(), ok, r.StoredBytes, r.StoredBlobs, r.Snapshots, r.ReadBlobs, r.CorruptBlobs, r.MissingBlobs)
	return err
}

// RepoStats returns up to limit results per storage, oldest first.
func (s *Store) RepoStats(limit int) (map[int64][]RepoStat, error) {
	rows, err := s.db.Query(`SELECT repo_id,time,ok,bytes,blobs,snapshots,read_blobs,corrupt,missing FROM (
  SELECT *, ROW_NUMBER() OVER (PARTITION BY repo_id ORDER BY id DESC) AS n FROM repo_stats) WHERE n <= ? ORDER BY repo_id, id`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]RepoStat{}
	for rows.Next() {
		var id int64
		var st RepoStat
		if err := rows.Scan(&id, &st.Time, &st.OK, &st.Bytes, &st.Blobs, &st.Snapshots, &st.ReadBlobs, &st.Corrupt, &st.Missing); err != nil {
			return nil, err
		}
		out[id] = append(out[id], st)
	}
	return out, rows.Err()
}

// checkParams says which storage a check job is for: the item's own, or
// its second storage.
type checkParams struct {
	RepoID int64 `json:"repoId"`
}

// checkRepo is the storage a check job checks.
func (s *Server) checkRepo(jobID int64, src *Source) int64 {
	var p checkParams
	if raw, err := s.store.JobParams(jobID); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &p)
	}
	if p.RepoID != 0 && src.CopyRepoID != nil && *src.CopyRepoID == p.RepoID {
		return p.RepoID
	}
	return src.RepoID
}

// recordCheck stores the outcome of a finished storage check.
func (s *Server) recordCheck(jobID, sourceID int64, ok bool, result any) {
	src, err := s.store.Source(sourceID)
	if err != nil {
		return
	}
	var r engine.CheckResult
	if b, err := json.Marshal(result); err == nil {
		_ = json.Unmarshal(b, &r)
	}
	if err := s.store.AddRepoStat(s.checkRepo(jobID, src), ok, r); err != nil {
		s.log.Printf("storage check: recording result: %v", err)
	}
}

// checkSource picks an enabled item that uses repoID, as its own storage or
// else as its second storage, to run its check.
func (s *Server) checkSource(repoID int64, sources []Source) *Source {
	for i := range sources {
		if sources[i].RepoID == repoID && sources[i].Enabled {
			return &sources[i]
		}
	}
	for i := range sources {
		if c := sources[i].CopyRepoID; c != nil && *c == repoID && sources[i].Enabled {
			return &sources[i]
		}
	}
	return nil
}

// scheduleChecks queues a health check for every storage that hasn't had
// one for a week (counting from when it was added).
func (s *Server) scheduleChecks(now time.Time, sources []Source) {
	repos, err := s.store.Repositories()
	if err != nil {
		return
	}
	for _, repo := range repos {
		key := "repo_check_at:" + strconv.FormatInt(repo.ID, 10)
		last, err := time.Parse(time.RFC3339Nano, s.store.Setting(key))
		if err != nil {
			last, _ = time.Parse(time.RFC3339Nano, repo.Created)
		}
		if now.Sub(last) < checkEvery {
			continue
		}
		src := s.checkSource(repo.ID, sources)
		if src == nil {
			continue
		}
		if _, err := s.store.EnqueueJobParams("check", src.ID, src.DrillAgent(), "schedule", checkParams{repo.ID}); err != nil {
			continue // another check through the same item is waiting; try again next time
		}
		s.log.Printf("scheduled storage health check for %s (via %s)", repo.Name, src.Name)
		s.wake(src.DrillAgent())
		_ = s.store.SetSetting(key, now.UTC().Format(time.RFC3339Nano))
	}
}

func (s *Server) handleCheckRepo(w http.ResponseWriter, r *http.Request) {
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
	sources, err := s.store.Sources()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	src := s.checkSource(id, sources)
	if src == nil {
		writeErr(w, 409, errors.New("no item uses this storage yet, so there is nothing to check"))
		return
	}
	jobID, err := s.store.EnqueueJobParams("check", src.ID, src.DrillAgent(), "manual", checkParams{id})
	if err != nil {
		writeErr(w, 409, err)
		return
	}
	_ = s.store.SetSetting("repo_check_at:"+strconv.FormatInt(id, 10), now())
	s.wake(src.DrillAgent())
	s.audit(s.actor(r), "run-check", fmt.Sprintf("storage %q health check, job #%d", repo.Name, jobID))
	writeJSON(w, 200, map[string]any{"jobId": jobID})
}

func (s *Server) handleRepoStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.RepoStats(26)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	out := map[string][]RepoStat{}
	for id, v := range st {
		out[strconv.FormatInt(id, 10)] = v
	}
	writeJSON(w, 200, out)
}
