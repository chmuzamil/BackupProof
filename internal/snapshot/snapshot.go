// Package snapshot defines snapshot records and their file manifests.
package snapshot

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/chmuzamil/backupproof/internal/chunker"
	bpcrypto "github.com/chmuzamil/backupproof/internal/crypto"
	"github.com/chmuzamil/backupproof/internal/repo"
)

const (
	TypeFile    = "file"
	TypeDir     = "dir"
	TypeSymlink = "symlink"
	// TypeStream is a generated artifact such as a database dump.
	TypeStream = "stream"
)

type Entry struct {
	Path   string   `json:"p"`
	Type   string   `json:"t"`
	Mode   uint32   `json:"m,omitempty"`
	MTime  int64    `json:"mt,omitempty"`
	Size   int64    `json:"s"`
	Hash   string   `json:"h,omitempty"`
	Link   string   `json:"l,omitempty"`
	Chunks []string `json:"c,omitempty"`
	// UID and GID record the owner on Unix-like systems, so a restore in place
	// can put it back exactly (needed for Docker volumes). Not part of the
	// content root, like Mode and MTime.
	UID *int `json:"u,omitempty"`
	GID *int `json:"g,omitempty"`
}

type Stats struct {
	Files      int   `json:"files"`
	Dirs       int   `json:"dirs"`
	Streams    int   `json:"streams"`
	Bytes      int64 `json:"bytes"`
	Chunks     int   `json:"chunks"`
	NewChunks  int   `json:"newChunks"`
	Uploaded   int64 `json:"uploadedBytes"`
	Unchanged  int   `json:"unchangedFiles"`
	DurationMs int64 `json:"durationMs"`
}

type Source struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // files | postgres | mysql | mongodb | sqlite | command
}

// Snapshot is stored encrypted under snapshots/<id>.
type Snapshot struct {
	Time       time.Time      `json:"time"`
	Host       string         `json:"host"`
	Source     Source         `json:"source"`
	Tags       []string       `json:"tags,omitempty"`
	Paths      []string       `json:"paths,omitempty"`
	Manifest   []string       `json:"manifest"`
	Entries    int            `json:"entries"`
	Root       string         `json:"root"`
	Stats      Stats          `json:"stats"`
	Parent     string         `json:"parent,omitempty"`
	SourceMeta map[string]any `json:"sourceMeta,omitempty"`
	Engine     string         `json:"engine"`
}

// WithID pairs a snapshot with its repository ID.
type WithID struct {
	ID bpcrypto.ID
	*Snapshot
}

// CleanPath turns an OS path into a safe, relative, slash-separated
// manifest path ("C:\\data\\x" -> "C/data/x", "/var/www" -> "var/www").
func CleanPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.Replace(p, ":", "", 1)
	p = path.Clean("/" + p)
	return strings.TrimPrefix(p, "/")
}

// SafeRelPath reports whether a manifest path can be joined under a restore
// target without escaping it.
func SafeRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.Contains(p, ":") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return false
		}
	}
	return true
}

// WriteManifest stores sorted entries as JSON lines in content-defined
// blobs, so unchanged parts of large manifests deduplicate between snapshots.
func WriteManifest(ctx context.Context, r *repo.Repo, entries []*Entry) ([]string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return nil, err
		}
	}
	ch := chunker.New(&buf, r.Gear(), r.ChunkerParams())
	var ids []string
	for {
		c, err := ch.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		id, _, err := r.PutBlob(ctx, c)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id.String())
	}
	return ids, nil
}

// ReadManifest loads and verifies all manifest entries of a snapshot.
func ReadManifest(ctx context.Context, r *repo.Repo, s *Snapshot) ([]*Entry, error) {
	var all bytes.Buffer
	for _, sid := range s.Manifest {
		id, err := bpcrypto.ParseID(sid)
		if err != nil {
			return nil, err
		}
		b, err := r.GetBlob(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("manifest: %w", err)
		}
		all.Write(b)
	}
	var entries []*Entry
	sc := bufio.NewScanner(&all)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("manifest: %w", err)
		}
		entries = append(entries, &e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(entries) != s.Entries {
		return nil, fmt.Errorf("manifest has %d entries, snapshot records %d", len(entries), s.Entries)
	}
	if got := Root(entries).String(); got != s.Root {
		return nil, fmt.Errorf("manifest root %s does not match snapshot root %s", got[:16], s.Root[:16])
	}
	return entries, nil
}

func Save(ctx context.Context, r *repo.Repo, s *Snapshot) (bpcrypto.ID, error) {
	return r.PutJSON(ctx, "snapshots", s)
}

func Load(ctx context.Context, r *repo.Repo, id bpcrypto.ID) (*Snapshot, error) {
	var s Snapshot
	if err := r.GetJSON(ctx, "snapshots", id, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// List loads all snapshots, newest first.
func List(ctx context.Context, r *repo.Repo) ([]WithID, error) {
	ids, err := r.ListIDs(ctx, "snapshots")
	if err != nil {
		return nil, err
	}
	out := make([]WithID, 0, len(ids))
	for _, id := range ids {
		s, err := Load(ctx, r, id)
		if err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", id.Short(), err)
		}
		out = append(out, WithID{ID: id, Snapshot: s})
	}
	sortNewestFirst(out)
	return out, nil
}

func sortNewestFirst(s []WithID) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Time.After(s[j-1].Time); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Resolve accepts "latest", "latest:<source>" or a hex ID prefix.
func Resolve(ctx context.Context, r *repo.Repo, ref string) (WithID, error) {
	if ref == "latest" || strings.HasPrefix(ref, "latest:") {
		source := strings.TrimPrefix(strings.TrimPrefix(ref, "latest"), ":")
		all, err := List(ctx, r)
		if err != nil {
			return WithID{}, err
		}
		for _, s := range all {
			if source == "" || s.Source.Name == source {
				return s, nil
			}
		}
		return WithID{}, fmt.Errorf("no snapshot found for %q", ref)
	}
	id, err := r.ResolveID(ctx, "snapshots", ref)
	if err != nil {
		return WithID{}, err
	}
	s, err := Load(ctx, r, id)
	if err != nil {
		return WithID{}, err
	}
	return WithID{ID: id, Snapshot: s}, nil
}
