package server

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	bpcrypto "github.com/chmuzamil/backupproof/internal/crypto"
	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/protocol"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/snapshot"
	"github.com/chmuzamil/backupproof/internal/source"
	"lukechampine.com/blake3"
)

// Restoring from the dashboard. Everything here is for administrators: a
// restore can write files anywhere on a server, and browsing or downloading
// shows the contents of any backed-up file.
//
//   - Browsing and zip downloads: the dashboard opens the storage itself
//     (it holds the storage keys) and streams the files to the browser.
//   - Restores: a job on the chosen server, which reads straight from storage.
//
// Only backups this item's own server signed a proof for can be used, so a
// snapshot planted in shared storage can never be restored.

var restoreMigrations = []string{"ALTER TABLE jobs ADD COLUMN params TEXT", "ALTER TABLE sources ADD COLUMN copy_repo_id INTEGER"}

func restoreKind(kind string) bool { return kind == "restore" || kind == "restore-db" }

func (s *Store) EnqueueJobParams(kind string, sourceID, agentID int64, trigger string, params any) (int64, error) {
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM jobs WHERE source_id=? AND kind=? AND state IN ('queued','running')", sourceID, kind).Scan(&n); err != nil {
		return 0, err
	}
	if n > 0 {
		if restoreKind(kind) {
			return 0, errors.New("a restore of this item is already waiting or running; wait for it to finish")
		}
		return 0, errors.New("a " + kind + " job for this source is already queued or running")
	}
	b, err := json.Marshal(params)
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec("INSERT INTO jobs(kind,source_id,agent_id,state,trigger,created,params) VALUES(?,?,?,'queued',?,?,?)",
		kind, sourceID, agentID, trigger, now(), string(b))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) JobParams(id int64) (json.RawMessage, error) {
	var p sql.NullString
	if err := s.db.QueryRow("SELECT params FROM jobs WHERE id=?", id).Scan(&p); err != nil {
		return nil, err
	}
	return json.RawMessage(p.String), nil
}

// restorePoint is one backup an item can be restored from.
type restorePoint struct {
	SnapshotID string `json:"snapshotId"`
	Created    string `json:"created"`
	Tested     bool   `json:"tested"` // a restore test of exactly this backup passed
}

func (s *Store) restorePoints(sourceID int64, limit int) ([]restorePoint, error) {
	rows, err := s.db.Query(`SELECT b.snapshot_id, b.created,
  EXISTS(SELECT 1 FROM proofs d WHERE d.source_id=b.source_id AND d.kind='drill' AND d.passed=1 AND d.snapshot_id=b.snapshot_id)
FROM proofs b WHERE b.source_id=? AND b.kind='backup' AND b.passed=1 AND b.snapshot_id<>'' ORDER BY b.id DESC LIMIT ?`, sourceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []restorePoint{}
	seen := map[string]bool{}
	for rows.Next() {
		var p restorePoint
		if err := rows.Scan(&p.SnapshotID, &p.Created, &p.Tested); err != nil {
			return nil, err
		}
		if !seen[p.SnapshotID] {
			seen[p.SnapshotID] = true
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// attestedRoot returns the content root this item's signed backup proof
// gives for snapshot snap, or an error if there is no such proof.
func (s *Server) attestedRoot(sourceID int64, snap string) (string, error) {
	var id int64
	err := s.store.db.QueryRow("SELECT id FROM proofs WHERE source_id=? AND kind='backup' AND passed=1 AND snapshot_id=? ORDER BY id DESC LIMIT 1", sourceID, snap).Scan(&id)
	if err != nil {
		return "", errors.New("that backup isn't one of this item's signed backups")
	}
	p, err := s.store.Proof(id)
	if err != nil {
		return "", err
	}
	st, err := proof.ParseStatement(p.Envelope)
	if err != nil {
		return "", err
	}
	root := st.Subject[0].Digest["blake3"]
	if root == "" {
		return "", errors.New("the backup proof has no content root")
	}
	return root, nil
}

// --- reading backups on the dashboard ----------------------------------------

type manifestCache struct {
	mu      sync.Mutex
	entries map[string]cachedManifest
}

type cachedManifest struct {
	at      time.Time
	entries []*snapshot.Entry
}

func (c *manifestCache) get(key string) []*snapshot.Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m, ok := c.entries[key]; ok && time.Since(m.at) < 10*time.Minute {
		return m.entries
	}
	return nil
}

func (c *manifestCache) put(key string, e []*snapshot.Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]cachedManifest{}
	}
	for k, m := range c.entries { // keep it small: a few recent backups
		if time.Since(m.at) > 10*time.Minute || len(c.entries) >= 4 {
			delete(c.entries, k)
		}
	}
	c.entries[key] = cachedManifest{at: time.Now(), entries: e}
}

// openSourceRepo opens an item's storage from the dashboard. A folder on a
// disk can only be read here when it belongs to this machine's built-in server.
func (s *Server) openSourceRepo(ctx context.Context, src *Source) (*repo.Repo, error) {
	r, sec, err := s.store.Repository(src.RepoID)
	if err != nil {
		return nil, err
	}
	if r.Backend.Type == "local" && src.AgentID != s.builtinAgentID() {
		return nil, errors.New("this item's storage is a folder on another server, so the dashboard can't read it. Restore to a folder on that server instead")
	}
	be, err := backend.Open(ctx, r.Backend, sec.Credentials)
	if err != nil {
		return nil, fmt.Errorf("opening the storage: %s", friendlyStorageError(err))
	}
	rp, err := repo.Open(ctx, be, []byte(sec.Password))
	if err != nil {
		be.Close()
		return nil, err
	}
	return rp, nil
}

// manifest loads (or reuses) the file list of an attested backup.
func (s *Server) manifest(ctx context.Context, src *Source, snap string) (*repo.Repo, []*snapshot.Entry, error) {
	root, err := s.attestedRoot(src.ID, snap)
	if err != nil {
		return nil, nil, err
	}
	r, err := s.openSourceRepo(ctx, src)
	if err != nil {
		return nil, nil, err
	}
	key := strconv.FormatInt(src.RepoID, 10) + ":" + snap
	if e := s.manifests.get(key); e != nil {
		return r, e, nil
	}
	id, err := bpcrypto.ParseID(snap)
	if err != nil {
		r.Close()
		return nil, nil, err
	}
	sn, err := snapshot.Load(ctx, r, id)
	if err != nil {
		r.Close()
		return nil, nil, fmt.Errorf("reading the backup: %w", err)
	}
	if sn.Root != root {
		r.Close()
		return nil, nil, errors.New("this backup doesn't match its signed proof; it may have been tampered with")
	}
	entries, err := snapshot.ReadManifest(ctx, r, sn)
	if err != nil {
		r.Close()
		return nil, nil, err
	}
	s.manifests.put(key, entries)
	return r, entries, nil
}

type fileEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	Files int    `json:"files,omitempty"` // for folders: files inside, at any depth
	MTime int64  `json:"mtime,omitempty"`
}

// children lists what is directly inside dir ("" for the top).
func children(entries []*snapshot.Entry, dir string) []fileEntry {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	agg := map[string]*fileEntry{}
	for _, e := range entries {
		if !strings.HasPrefix(e.Path, prefix) || e.Path == dir {
			continue
		}
		rest := e.Path[len(prefix):]
		name, deeper, _ := strings.Cut(rest, "/")
		fe := agg[name]
		if fe == nil {
			fe = &fileEntry{Name: name, Path: prefix + name}
			agg[name] = fe
		}
		isFile := e.Type == snapshot.TypeFile || e.Type == snapshot.TypeStream
		if deeper != "" || e.Type == snapshot.TypeDir {
			fe.Dir = true
		}
		if isFile {
			fe.Size += e.Size
			if deeper != "" {
				fe.Files++
			} else {
				fe.MTime = e.MTime
			}
		}
	}
	out := make([]fileEntry, 0, len(agg))
	for _, fe := range agg {
		out = append(out, *fe)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func (s *Server) sourceFromPath(w http.ResponseWriter, r *http.Request) *Source {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return nil
	}
	src, err := s.store.SourceWithSecrets(id)
	if err != nil {
		writeErr(w, 404, err)
		return nil
	}
	return src
}

func (s *Server) handleRestorePoints(w http.ResponseWriter, r *http.Request) {
	src := s.sourceFromPath(w, r)
	if src == nil {
		return
	}
	pts, err := s.store.restorePoints(src.ID, 200)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	dump, _ := source.DumpPath(src.Spec)
	writeJSON(w, 200, map[string]any{"points": pts, "database": dump != "", "dumpPath": dump})
}

func (s *Server) handleBrowseBackup(w http.ResponseWriter, r *http.Request) {
	src := s.sourceFromPath(w, r)
	if src == nil {
		return
	}
	rp, entries, err := s.manifest(r.Context(), src, r.URL.Query().Get("snapshot"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	rp.Close()
	dir := strings.Trim(r.URL.Query().Get("path"), "/")
	list := children(entries, dir)
	if len(list) > 2000 {
		list = list[:2000]
	}
	writeJSON(w, 200, map[string]any{"path": dir, "entries": list})
}

// handleDownload streams the chosen files and folders of a backup as a zip.
// Each file is checked against its content hash while it is sent; a mismatch
// aborts the download rather than deliver damaged data.
func (s *Server) handleDownloadFiles(w http.ResponseWriter, r *http.Request) {
	src := s.sourceFromPath(w, r)
	if src == nil {
		return
	}
	q := r.URL.Query()
	rp, entries, err := s.manifest(r.Context(), src, q.Get("snapshot"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	defer rp.Close()
	var picks []string
	for _, p := range q["path"] {
		if p = strings.Trim(snapshot.CleanPath(p), "/"); p != "" {
			picks = append(picks, p)
		}
	}
	match := func(p string) bool {
		if len(picks) == 0 {
			return true
		}
		for _, x := range picks {
			if p == x || strings.HasPrefix(p, x+"/") {
				return true
			}
		}
		return false
	}
	var files []*snapshot.Entry
	for _, e := range entries {
		if (e.Type == snapshot.TypeFile || e.Type == snapshot.TypeStream) && match(e.Path) && snapshot.SafeRelPath(e.Path) {
			files = append(files, e)
		}
	}
	if len(files) == 0 {
		writeErr(w, 404, errors.New("there are no files in that selection"))
		return
	}
	name := fmt.Sprintf("%s-%s.zip", safeName(src.Name), short(q.Get("snapshot")))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	s.audit(s.actor(r), "download-files", fmt.Sprintf("item %q, backup %s: %d files (%s)", src.Name, short(q.Get("snapshot")), len(files), strings.Join(picks, ", ")))
	zw := zip.NewWriter(w)
	for _, e := range files {
		hdr := &zip.FileHeader{Name: e.Path, Method: zip.Deflate}
		if e.MTime > 0 {
			hdr.Modified = time.Unix(0, e.MTime)
		}
		if e.Mode != 0 {
			hdr.SetMode(fs.FileMode(e.Mode).Perm())
		}
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			panic(http.ErrAbortHandler)
		}
		hsum := blake3.New(32, nil)
		var size int64
		for _, c := range e.Chunks {
			id, err := bpcrypto.ParseID(c)
			if err != nil {
				panic(http.ErrAbortHandler)
			}
			data, err := rp.GetBlob(r.Context(), id)
			if err != nil {
				s.log.Printf("download %s: %v", e.Path, err)
				panic(http.ErrAbortHandler)
			}
			_, _ = hsum.Write(data)
			size += int64(len(data))
			if _, err := fw.Write(data); err != nil {
				panic(http.ErrAbortHandler)
			}
		}
		if fmt.Sprintf("%x", hsum.Sum(nil)) != e.Hash || size != e.Size {
			s.log.Printf("download %s: content verification failed", e.Path)
			panic(http.ErrAbortHandler)
		}
	}
	_ = zw.Close()
}

func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, s)
	return strings.Trim(s, "-")
}

// --- restore jobs ---------------------------------------------------------------

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	src := s.sourceFromPath(w, r)
	if src == nil {
		return
	}
	var req struct {
		protocol.Restore
		AgentID int64 `json:"agentId"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if _, err := s.attestedRoot(src.ID, req.SnapshotID); err != nil {
		writeErr(w, 400, err)
		return
	}
	agentID := src.AgentID
	if req.AgentID != 0 {
		agentID = req.AgentID
	}
	a, err := s.store.Agent(agentID)
	if err != nil || a.Revoked {
		writeErr(w, 400, errors.New("choose a connected server to restore to"))
		return
	}
	req.Folder = strings.TrimSpace(req.Folder)
	if req.Folder == "" && agentID != src.AgentID {
		writeErr(w, 400, errors.New("to restore onto another server, choose a folder on it"))
		return
	}
	if req.Folder != "" && !isAbsAnyOS(req.Folder) {
		writeErr(w, 400, errors.New("give the full path of the folder, such as /root/restored or C:\\Restored"))
		return
	}
	for i, p := range req.Paths {
		req.Paths[i] = strings.Trim(snapshot.CleanPath(p), "/")
	}
	req.DBTarget, req.DBReplace = "", false
	id, err := s.store.EnqueueJobParams("restore", src.ID, agentID, "manual", req.Restore)
	if err != nil {
		writeErr(w, 409, err)
		return
	}
	where := "the original location"
	if req.Folder != "" {
		where = req.Folder
	}
	s.audit(s.actor(r), "restore", fmt.Sprintf("item %q, backup %s → %s on %q (%s), job #%d",
		src.Name, short(req.SnapshotID), where, a.Name, pathsWords(req.Paths), id))
	s.wake(agentID)
	writeJSON(w, 200, map[string]any{"jobId": id})
}

func (s *Server) handleRestoreDB(w http.ResponseWriter, r *http.Request) {
	src := s.sourceFromPath(w, r)
	if src == nil {
		return
	}
	var req protocol.Restore
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if _, err := source.DumpPath(src.Spec); err != nil {
		writeErr(w, 400, err)
		return
	}
	if _, err := s.attestedRoot(src.ID, req.SnapshotID); err != nil {
		writeErr(w, 400, err)
		return
	}
	req.DBTarget = strings.TrimSpace(req.DBTarget)
	if !req.DBReplace {
		if src.Spec.Kind == "sqlite" {
			if !isAbsAnyOS(req.DBTarget) {
				writeErr(w, 400, errors.New("give the full path for the restored database file"))
				return
			}
		} else if !source.ValidDatabaseName(req.DBTarget) {
			writeErr(w, 400, errors.New("the new database name may only use letters, digits, _ and -"))
			return
		}
	}
	req.Paths, req.Folder = nil, ""
	id, err := s.store.EnqueueJobParams("restore-db", src.ID, src.AgentID, "manual", req)
	if err != nil {
		writeErr(w, 409, err)
		return
	}
	into := "a new database " + req.DBTarget
	if req.DBReplace {
		into = "the original database (replacing it)"
	}
	s.audit(s.actor(r), "restore-db", fmt.Sprintf("item %q, backup %s → %s, job #%d", src.Name, short(req.SnapshotID), into, id))
	s.wake(src.AgentID)
	writeJSON(w, 200, map[string]any{"jobId": id})
}

// restoreLease fills in a restore job's parameters, re-checking the backup.
func (s *Server) restoreLease(job *Job, lease *protocol.Lease) error {
	raw, err := s.store.JobParams(job.ID)
	if err != nil {
		return err
	}
	var p protocol.Restore
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("restore settings: %w", err)
	}
	root, err := s.attestedRoot(job.SourceID, p.SnapshotID)
	if err != nil {
		return err
	}
	lease.Restore = &p
	lease.SnapshotID, lease.ExpectedRoot = p.SnapshotID, root
	return nil
}

func pathsWords(p []string) string {
	if len(p) == 0 {
		return "everything"
	}
	if len(p) > 3 {
		return strings.Join(p[:3], ", ") + fmt.Sprintf(" and %d more", len(p)-3)
	}
	return strings.Join(p, ", ")
}

func isAbsAnyOS(p string) bool {
	return path.IsAbs(p) || (len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/'))
}
