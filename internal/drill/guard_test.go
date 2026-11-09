package drill

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/source"
)

func TestGuardDiskStopsTheTestWhenSpaceRunsOut(t *testing.T) {
	f := &fakeDocker{}
	useFake(t, f)
	oldEvery, oldFree := appDiskEvery, appFreeBytes
	t.Cleanup(func() { appDiskEvery, appFreeBytes = oldEvery, oldFree })
	appDiskEvery = time.Millisecond
	appFreeBytes = func(string) (uint64, bool) { return 1, true }
	a := &appRun{at: basicAppTest(), parent: t.TempDir(), app: "x-app", holder: "x-net"}
	stop := a.guardDisk(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for !a.diskLow.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stop()
	if !a.diskLow.Load() {
		t.Fatal("low disk wasn't noticed")
	}
	if n := len(f.find("rm")); n != 2 {
		t.Errorf("%d containers removed, want 2", n)
	}
}

func TestProbeWaitsAsLongAsTheItemsLimit(t *testing.T) {
	f := &fakeDocker{}
	useFake(t, f)
	a := &appRun{at: &source.AppTest{Port: 80, MaxResponseMs: 20000}, holder: "x-net"}
	a.probe(context.Background())
	calls := f.find("exec")
	if len(calls) != 1 {
		t.Fatalf("calls: %v", calls)
	}
	i := slices.Index(calls[0], "--max-time")
	if i < 0 || calls[0][i+1] != "21" {
		t.Errorf("args: %v", calls[0])
	}
}
