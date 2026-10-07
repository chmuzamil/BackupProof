//go:build windows

package discover

import (
	"os"

	"golang.org/x/sys/windows"
)

func drives() []Drive {
	var out []Drive
	for l := 'A'; l <= 'Z'; l++ {
		root := string(l) + `:\`
		if _, err := os.Stat(root); err != nil {
			continue
		}
		var free, total, totalFree uint64
		p, _ := windows.UTF16PtrFromString(root)
		if windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree) != nil || total == 0 {
			continue
		}
		label := "Drive " + string(l) + ":"
		if t := windows.GetDriveType(p); t == windows.DRIVE_REMOVABLE {
			label += " (removable)"
		} else if t == windows.DRIVE_REMOTE {
			label += " (network)"
		}
		out = append(out, Drive{Path: root, Label: label, Free: free, Total: total})
	}
	return sortDrives(out)
}
