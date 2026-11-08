//go:build !windows

package drill

import (
	"io/fs"
	"os"
	"syscall"
)

// copyOwner gives path the owner of fi.
func copyOwner(path string, fi fs.FileInfo) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		_ = os.Lchown(path, int(st.Uid), int(st.Gid))
	}
}
