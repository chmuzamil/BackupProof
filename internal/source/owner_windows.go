//go:build windows

package source

import "io/fs"

func copyOwner(string, fs.FileInfo) {}
