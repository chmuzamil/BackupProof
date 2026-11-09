//go:build !windows

package drill

import "syscall"

// freeBytes reports the space available on the disk holding path.
func freeBytes(path string) (uint64, bool) {
	var st syscall.Statfs_t
	if path == "" || syscall.Statfs(path, &st) != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}
