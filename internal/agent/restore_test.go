package agent

import (
	"reflect"
	"testing"

	"github.com/chmuzamil/backupproof/internal/snapshot"
)

func TestDockerRestoreParts(t *testing.T) {
	var entries []*snapshot.Entry
	for _, p := range []string{"docker/volumes/db/x", "docker/volumes/web/y", "docker/mounts/srv/app/data/a.txt", "docker/containers/app.json"} {
		entries = append(entries, &snapshot.Entry{Path: p})
	}
	vols, mounts, err := dockerRestoreParts(entries, nil)
	if err != nil || !reflect.DeepEqual(vols, []string{"db", "web"}) || !reflect.DeepEqual(mounts, []string{"docker/mounts"}) {
		t.Fatalf("everything: %v %v %v", vols, mounts, err)
	}
	vols, mounts, err = dockerRestoreParts(entries, []string{"docker/volumes/db"})
	if err != nil || !reflect.DeepEqual(vols, []string{"db"}) || mounts != nil {
		t.Fatalf("one volume: %v %v %v", vols, mounts, err)
	}
	vols, mounts, err = dockerRestoreParts(entries, []string{"docker/mounts/srv/app/data"})
	if err != nil || vols != nil || !reflect.DeepEqual(mounts, []string{"docker/mounts/srv/app/data"}) {
		t.Fatalf("one mounted folder: %v %v %v", vols, mounts, err)
	}
	if _, _, err := dockerRestoreParts(entries, []string{"docker/volumes/db/x"}); err == nil {
		t.Error("part of a volume must be refused")
	}
	if _, _, err := dockerRestoreParts(entries, []string{"docker/containers/app.json"}); err == nil {
		t.Error("container settings can't be put back")
	}
	if got := mountRoot("docker/mounts/srv/app/data/a.txt", []string{"docker/mounts/srv/app"}); got != "/srv/app" {
		t.Errorf("mountRoot: %q", got)
	}
}
