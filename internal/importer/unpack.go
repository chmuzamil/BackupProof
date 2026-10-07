package importer

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/chmuzamil/backupproof/internal/engine"
	"github.com/klauspost/compress/zstd"
)

// archiveKind reports how a (decrypted) file name can be unpacked.
func archiveKind(name string) (kind, base string) {
	low := strings.ToLower(name)
	for _, s := range []struct{ ext, kind string }{
		{".tar.gz", "tgz"}, {".tgz", "tgz"}, {".tar.zst", "tzst"}, {".tar", "tar"},
		{".zip", "zip"}, {".gz", "gz"}, {".zst", "zst"},
	} {
		if strings.HasSuffix(low, s.ext) {
			return s.kind, name[:len(name)-len(s.ext)]
		}
	}
	return "", name
}

// unpack adds the contents of an archive under base/ instead of the archive
// file itself, so restore tests can check individual files and unchanged
// files deduplicate across daily archives. It returns false when name is not
// an archive.
func unpack(ctx context.Context, b *engine.Builder, name string, mtime time.Time, r io.Reader, tmpDir string) (bool, error) {
	kind, base := archiveKind(name)
	switch kind {
	case "":
		return false, nil
	case "gz", "zst":
		var dr io.Reader
		if kind == "gz" {
			gz, err := gzip.NewReader(r)
			if err != nil {
				return false, err
			}
			dr = gz
		} else {
			zr, err := zstd.NewReader(r)
			if err != nil {
				return false, err
			}
			defer zr.Close()
			dr = zr
		}
		_, err := b.AddReader(ctx, base, 0o644, mtime, dr)
		return true, err
	case "tar", "tgz", "tzst":
		var tr io.Reader = r
		if kind == "tgz" {
			gz, err := gzip.NewReader(r)
			if err != nil {
				return false, err
			}
			tr = gz
		} else if kind == "tzst" {
			zr, err := zstd.NewReader(r)
			if err != nil {
				return false, err
			}
			defer zr.Close()
			tr = zr
		}
		t := tar.NewReader(tr)
		for {
			h, err := t.Next()
			if err == io.EOF {
				return true, nil
			}
			if err != nil {
				return true, err
			}
			if h.Typeflag != tar.TypeReg {
				continue
			}
			if _, err := b.AddReader(ctx, base+"/"+h.Name, uint32(h.Mode&0o777), h.ModTime, t); err != nil {
				return true, err
			}
		}
	case "zip":
		// zip needs random access: spool to a temp file first.
		f, err := os.CreateTemp(tmpDir, "bp-zip-*")
		if err != nil {
			return false, err
		}
		defer os.Remove(f.Name())
		defer f.Close()
		size, err := io.Copy(f, r)
		if err != nil {
			return false, err
		}
		zr, err := zip.NewReader(f, size)
		if err != nil {
			return false, err
		}
		for _, zf := range zr.File {
			if zf.FileInfo().IsDir() {
				continue
			}
			rc, err := zf.Open()
			if err != nil {
				return true, err
			}
			_, err = b.AddReader(ctx, base+"/"+zf.Name, 0o644, zf.Modified, rc)
			rc.Close()
			if err != nil {
				return true, err
			}
		}
		return true, nil
	}
	return false, nil
}
