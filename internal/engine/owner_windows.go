//go:build windows

package engine

import (
	"io/fs"
	"os"

	"github.com/chmuzamil/backupproof/internal/snapshot"
)

// Windows files inherit permissions from their folder; there is no owner to copy.
func ownerOf(fs.FileInfo) (int, int, bool) { return -1, -1, false }

func adoptParentOwner(*os.Root, string) {}

// trustedLink: links and junctions are never followed by an in-place restore.
func trustedLink(*os.Root, fs.FileInfo) bool { return false }

func pathExists(root *os.Root, name string) bool {
	_, err := root.Lstat(name)
	return err == nil
}

func entryOwner(fs.FileInfo) (*int, *int) { return nil, nil }

func restoreOwner(*os.Root, string, *snapshot.Entry) bool { return false }
