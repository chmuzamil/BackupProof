package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/engine"
)

// filesSource imports backup files that any other tool or script uploaded to
// a bucket or folder (database dumps, tar/zip archives, encrypted .gpg /
// .enc / .age files, …).
//
// Grouping decides what becomes one BackupProof backup copy:
//
//	auto    files whose names or folders carry a date (2026-09-01, 20260901)
//	        are grouped per day, each day keeping its original date;
//	        otherwise everything is one copy
//	day     always group by the date found in the name (undated → one group)
//	folder  one copy per top-level folder
//	file    one copy per file
//	all     everything is one copy
//
// For each file: download to a temp file → decrypt (auto-detected) →
// optionally unpack archives → store. Point IDs fingerprint the group's
// listing, so re-running only converts new or changed groups.
type filesSource struct {
	be       backend.Backend
	spec     Spec
	dec      *Decryptor
	groups   map[string][]backend.ObjectInfo
	detected map[string]int
}

func newFilesSource(be backend.Backend, s Spec) *filesSource {
	return &filesSource{be: be, spec: s, detected: map[string]int{},
		dec: &Decryptor{Mode: s.Decrypt, Password: s.Password, PrivateKey: s.PrivateKey, OpenSSLIter: s.OpenSSLIter}}
}

func (s *filesSource) Format() string { return "files" }
func (s *filesSource) Close() error   { return s.be.Close() }

var dateRe = regexp.MustCompile(`((?:19|20)\d\d)[-_.]?(0[1-9]|1[0-2])[-_.]?(0[1-9]|[12]\d|3[01])`)

func dateKey(key string) (string, time.Time, bool) {
	m := dateRe.FindStringSubmatch(key)
	if m == nil {
		return "", time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", m[1]+"-"+m[2]+"-"+m[3])
	if err != nil {
		return "", time.Time{}, false
	}
	return t.Format("2006-01-02"), t, true
}

func (s *filesSource) groupKey(o backend.ObjectInfo, mode string) string {
	switch mode {
	case "file":
		return o.Key
	case "folder":
		if i := strings.Index(o.Key, "/"); i > 0 {
			return o.Key[:i]
		}
		return "(top level)"
	case "day":
		if d, _, ok := dateKey(o.Key); ok {
			return d
		}
		return "undated"
	}
	return "all files"
}

func (s *filesSource) List(ctx context.Context) ([]Point, error) {
	var objs []backend.ObjectInfo
	err := s.be.List(ctx, "", func(o backend.ObjectInfo) error {
		if !strings.HasSuffix(o.Key, "/") && o.Size > 0 && !strings.HasPrefix(path.Base(o.Key), ".") {
			objs = append(objs, o)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(objs))
	for i, o := range objs {
		keys[i] = o.Key
	}
	if repo := DetectKeys(keys); repo != nil {
		return nil, errRepository(repo)
	}
	mode := s.spec.Grouping
	if mode == "" || mode == "auto" {
		dated := 0
		for _, o := range objs {
			if _, _, ok := dateKey(o.Key); ok {
				dated++
			}
		}
		mode = "all"
		if len(objs) > 1 && dated*10 >= len(objs)*8 {
			mode = "day"
		}
	}
	s.groups = map[string][]backend.ObjectInfo{}
	for _, o := range objs {
		k := s.groupKey(o, mode)
		s.groups[k] = append(s.groups[k], o)
	}
	var points []Point
	for k, list := range s.groups {
		sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })
		h := sha256.New()
		var total int64
		var newest time.Time
		for _, o := range list {
			_, _ = fmt.Fprintf(h, "%s\x00%d\x00%d\n", o.Key, o.Size, o.Modified.Unix()) // hash writes never fail
			total += o.Size
			if o.Modified.After(newest) {
				newest = o.Modified
			}
		}
		t := newest
		// Copied buckets have fresh upload times; the date in the name is the
		// truth about when the backup was taken.
		if _, d, ok := dateKey(k); ok && (newest.Before(d) || newest.Sub(d) > 48*time.Hour) {
			t = d.Add(12 * time.Hour)
		}
		label := k
		if mode == "all" {
			label = s.be.Location()
		}
		points = append(points, Point{ID: "files:" + k + ":" + hex.EncodeToString(h.Sum(nil))[:24], Label: label, Time: t.UTC(), Files: len(list), Bytes: total})
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Time.Before(points[j].Time) })
	return points, nil
}

func (s *filesSource) Fill(ctx context.Context, p Point, b *engine.Builder, log engine.Logger) error {
	k := strings.TrimPrefix(p.ID, "files:")
	k = k[:strings.LastIndex(k, ":")]
	list := s.groups[k]
	tmpDir, err := os.MkdirTemp("", "bp-import-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	for i, o := range list {
		if err := s.importOne(ctx, o, b, tmpDir, log); err != nil {
			return fmt.Errorf("%s: %w", o.Key, err)
		}
		if (i+1)%50 == 0 {
			log("converted %d of %d files", i+1, len(list))
		}
	}
	return nil
}

func (s *filesSource) importOne(ctx context.Context, o backend.ObjectInfo, b *engine.Builder, tmpDir string, log engine.Logger) error {
	rc, err := s.be.Open(ctx, o.Key)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(tmpDir, "obj-*")
	if err != nil {
		rc.Close()
		return err
	}
	n, err := io.Copy(tmp, rc)
	rc.Close()
	tmp.Close()
	defer os.Remove(tmp.Name())
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if n != o.Size {
		return fmt.Errorf("downloaded %d bytes but storage reports %d", n, o.Size)
	}
	plain, enc, err := s.dec.Open(tmp.Name(), o.Key)
	if err != nil {
		return err
	}
	defer plain.Close()
	name := o.Key
	if enc != "none" {
		s.detected[enc]++
		if s.detected[enc] == 1 {
			log("decrypting %s-encrypted files", enc)
		}
		name = PlainName(name)
	}
	if s.spec.Unpack {
		if ok, err := unpack(ctx, b, name, o.Modified, plain, tmpDir); ok || err != nil {
			return err
		}
	}
	_, err = b.AddReader(ctx, name, 0o644, o.Modified, plain)
	return err
}
