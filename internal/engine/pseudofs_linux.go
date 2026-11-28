package engine

import (
	"io/fs"
	"syscall"
)

// Filesystem magic numbers (statfs f_type) of Linux pseudo filesystems.
var pseudoTypes = map[int64]string{
	0x9fa0:     "proc",
	0x62656572: "sysfs",
	0x1cd1:     "devpts",
	0x27e0eb:   "cgroup",
	0x63677270: "cgroup2",
	0x73636673: "securityfs",
	0x64626720: "debugfs",
	0x74726163: "tracefs",
	0x6165676c: "pstore",
	0xcafe4a11: "bpf",
	0x62656570: "configfs",
	0x65735543: "fusectl",
	0x19800202: "mqueue",
	0x42494e4d: "binfmt_misc",
	0x958458f6: "hugetlbfs",
	0x01021994: "tmpfs-dev", // devtmpfs reports tmpfs's magic; see below
}

// pseudoFS names the pseudo filesystem a folder is the top of, or "".
// statfs runs once per device, so a walk pays for it only at mount points.
func (b *Builder) pseudoFS(p string, info fs.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	dev := uint64(st.Dev)
	if b.pseudo == nil {
		b.pseudo = map[uint64]string{}
	}
	if t, seen := b.pseudo[dev]; seen {
		return t
	}
	var sfs syscall.Statfs_t
	t := ""
	if err := syscall.Statfs(p, &sfs); err == nil {
		t = pseudoTypes[int64(sfs.Type)]
		// tmpfs holds real (if temporary) files, except /dev, which is devtmpfs.
		if t == "tmpfs-dev" {
			t = ""
			if p == "/dev" {
				t = "devtmpfs"
			}
		}
	}
	b.pseudo[dev] = t
	return t
}
