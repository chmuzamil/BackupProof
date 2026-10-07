//go:build !windows

package engine

import (
	"io/fs"
	"os"

	"github.com/chmuzamil/backupproof/internal/snapshot"
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

// entryOwner records a file's owner for the backup.
func entryOwner(fi fs.FileInfo) (*int, *int) {
	uid, gid, ok := ownerOf(fi)
	if !ok {
		return nil, nil
	}
	return &uid, &gid
}

// restoreOwner sets the owner recorded in the backup, when there is one.
func restoreOwner(root *os.Root, name string, e *snapshot.Entry) bool {
	if e.UID == nil || e.GID == nil {
		return false
	}
	return root.Lchown(name, *e.UID, *e.GID) == nil
}
