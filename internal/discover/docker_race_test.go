package discover

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A container removed between listing and inspecting must not hide the rest.
func TestDockerAppsSurviveARemovedContainer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as a fake docker")
	}
	script := `#!/bin/sh
case "$1" in
ps) echo gone; echo kept ;;
inspect)
	echo '[{"Name":"/kept","Mounts":[],"Config":{"Image":"nginx","Labels":{}},"State":{"Running":true}}]'
	echo 'Error: No such object: gone' >&2
	exit 1 ;;
esac
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	inv := &Inventory{}
	inv.collectDockerApps(context.Background())
	if len(inv.DockerApps) != 1 || inv.DockerApps[0].Name != "kept" {
		t.Errorf("apps: %+v", inv.DockerApps)
	}
}
