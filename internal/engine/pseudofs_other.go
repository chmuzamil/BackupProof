//go:build !linux

package engine

import "io/fs"

func (b *Builder) pseudoFS(string, fs.FileInfo) string {
	_ = b.pseudo // only Linux has pseudo filesystems to cache
	return ""
}
