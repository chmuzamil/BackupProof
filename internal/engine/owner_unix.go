//go:build !windows

package engine

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

func ownerOf(fi fs.FileInfo) (uid, gid int, ok bool) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid), true
	}
	return -1, -1, false
}

// adoptParentOwner gives a newly restored file or folder the owner of the
// folder it was created in, as if the folder's owner had created it.
func adoptParentOwner(root *os.Root, name string) {
	dir := filepath.Dir(name)
	fi, err := root.Stat(dir)
	if err != nil {
		return
	}
	if uid, gid, ok := ownerOf(fi); ok {
		_ = root.Lchown(name, uid, gid)
	}
}

func pathExists(root *os.Root, name string) bool {
	_, err := root.Lstat(name)
	return err == nil
}
