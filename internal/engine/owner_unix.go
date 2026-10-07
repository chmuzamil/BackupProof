//go:build !windows

package engine

import (
	"io/fs"
	"os"

	"syscall"

	"github.com/chmuzamil/backupproof/internal/snapshot"
)

func ownerOf(fi fs.FileInfo) (uid, gid int, ok bool) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid), true
	}
	return -1, -1, false
}

// adoptParentOwner gives a newly restored file or folder (name, directly in
// parent) the owner of that folder, as if the folder's owner had created it.
func adoptParentOwner(parent *os.Root, name string) {
	fi, err := parent.Stat(".")
	if err != nil {
		return
	}
	if uid, gid, ok := ownerOf(fi); ok {
		_ = parent.Lchown(name, uid, gid)
	}
}

// trustedLink reports whether a symlink found on the way to an in-place
// restore was made by root in a folder only root can change (such as
// /var/run -> /run), so following it can't be someone else's doing.
func trustedLink(parent *os.Root, link fs.FileInfo) bool {
	dir, err := parent.Stat(".")
	if err != nil {
		return false
	}
	luid, _, ok1 := ownerOf(link)
	duid, _, ok2 := ownerOf(dir)
	return ok1 && ok2 && luid == 0 && duid == 0 && dir.Mode().Perm()&0o022 == 0
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
