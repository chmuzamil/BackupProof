//go:build windows

package drill

import "io/fs"

// copyOwner does nothing on Windows, where files have no numeric owners.
func copyOwner(string, fs.FileInfo) {}
