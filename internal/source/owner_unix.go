//go:build !windows

package source

import (
	"io/fs"
	"os"
	"syscall"
)

// copyOwner gives path the owner of fi (the file it replaces).
func copyOwner(path string, fi fs.FileInfo) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		_ = os.Lchown(path, int(st.Uid), int(st.Gid))
	}
}
