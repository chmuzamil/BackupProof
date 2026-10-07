package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// normPath returns an absolute, symlink-resolved, comparable form of p.
func normPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	abs = filepath.Clean(abs)
	// Resolve symlinks (and, on Windows, 8.3 short names such as RUNNER~1)
	// on the deepest part of the path that exists, then re-append the rest.
	// Without this, a path that doesn't exist yet would keep an unresolved
	// prefix and fail to match a resolved denied directory.
	rest := ""
	for dir := abs; ; {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			abs = filepath.Join(real, rest)
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		abs = strings.ToLower(abs)
	}
	return abs
}

// IsDenied reports whether p is, or is inside, one of the denied paths.
// The built-in agent denies the server's data directory so that nobody who
// can configure backups can copy the server's keys and database.
func IsDenied(p string, deny []string) bool {
	if len(deny) == 0 || p == "" {
		return false
	}
	np := normPath(p)
	for _, d := range deny {
		nd := normPath(d)
		if np == nd || strings.HasPrefix(np, strings.TrimSuffix(nd, string(os.PathSeparator))+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}
