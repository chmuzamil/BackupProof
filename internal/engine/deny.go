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
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	abs = filepath.Clean(abs)
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
