package engine

import "runtime"

// unreadableHint says what usually causes unreadable files on this system.
func unreadableHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "On a Mac this usually means the agent hasn't been given Full Disk Access: open System Settings → Privacy & Security → Full Disk Access and add /usr/local/bin/backupproof."
	case "windows":
		return "On Windows this is usually a file another program keeps open (Outlook, accounting software, a database). Close it during the backup, or back up its own export instead."
	default:
		return "This is usually a permissions problem: the agent should run as root to read every file."
	}
}
