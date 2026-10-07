//go:build !windows

package discover

import (
	"bufio"
	"os"
	"strings"
	"syscall"
)

// drives lists mounted disks that are good backup targets: anything under
// /mnt, /media, /run/media or /Volumes, plus the root filesystem.
func drives() []Drive {
	var mounts []string
	if f, err := os.Open("/proc/mounts"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) > 1 {
				mounts = append(mounts, fields[1])
			}
		}
		f.Close()
	}
	if entries, err := os.ReadDir("/Volumes"); err == nil {
		for _, e := range entries {
			mounts = append(mounts, "/Volumes/"+e.Name())
		}
	}
	var out []Drive
	seen := map[string]bool{}
	for _, m := range append([]string{"/"}, mounts...) {
		if seen[m] {
			continue
		}
		if m != "/" && !strings.HasPrefix(m, "/mnt") && !strings.HasPrefix(m, "/media") && !strings.HasPrefix(m, "/run/media") && !strings.HasPrefix(m, "/Volumes/") {
			continue
		}
		seen[m] = true
		var st syscall.Statfs_t
		if syscall.Statfs(m, &st) != nil {
			continue
		}
		bs := uint64(st.Bsize)
		label := "Disk " + m
		if m == "/" {
			label = "Main disk (/)"
		}
		out = append(out, Drive{Path: m, Label: label, Free: uint64(st.Bavail) * bs, Total: uint64(st.Blocks) * bs})
	}
	return sortDrives(out)
}
