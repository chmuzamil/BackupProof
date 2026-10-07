package backend

import (
	"context"
	"testing"
	"time"
)

func TestThrottlePacesUploads(t *testing.T) {
	be, err := Open(context.Background(), Config{Type: "local", Path: t.TempDir()}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	slow := Throttle(be, 200*1024, 0) // 200 KB/s
	start := time.Now()
	for i := 0; i < 4; i++ { // 4 × 100 KB = 400 KB → about 1.5 s after the first
		if err := slow.Put(context.Background(), "data/x"+string(rune('a'+i)), make([]byte, 100*1024)); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 1300*time.Millisecond || d > 4*time.Second {
		t.Errorf("400 KB at 200 KB/s took %v", d)
	}
	if Throttle(be, 0, 0) != be {
		t.Error("no limit should return the storage unchanged")
	}
}
