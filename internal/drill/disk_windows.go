package drill

import "golang.org/x/sys/windows"

// freeBytes reports the space available on the disk holding path.
func freeBytes(path string) (uint64, bool) {
	p, err := windows.UTF16PtrFromString(path)
	if path == "" || err != nil {
		return 0, false
	}
	var free, total, totalFree uint64
	if windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree) != nil {
		return 0, false
	}
	return free, true
}
