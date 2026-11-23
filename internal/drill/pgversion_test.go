package drill

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chmuzamil/backupproof/internal/snapshot"
)

func pgDump(t *testing.T, minor byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "shop.dump")
	if err := os.WriteFile(p, []byte{'P', 'G', 'D', 'M', 'P', 1, minor, 0, 4, 8, 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPostgresDrillMajor(t *testing.T) {
	snap := func(meta map[string]any) snapshot.WithID {
		return snapshot.WithID{Snapshot: &snapshot.Snapshot{SourceMeta: meta}}
	}
	cases := []struct {
		name string
		meta map[string]any
		dump string
		want int
	}{
		{"server only", map[string]any{"serverMajor": 15.0}, pgDump(t, 14), 15},
		// The bug: pg_dump 17 against a PostgreSQL 16 server made a dump
		// postgres:16 couldn't read.
		{"pg_dump newer than server", map[string]any{"serverMajor": 16.0, "pgDumpMajor": 17.0}, pgDump(t, 16), 17},
		{"older snapshot, archive 1.16", map[string]any{"serverMajor": 16.0}, pgDump(t, 16), 17},
		{"older snapshot, archive 1.15", map[string]any{"serverMajor": 14.0}, pgDump(t, 15), 16},
		{"server newer than pg_dump", map[string]any{"serverMajor": 17.0, "pgDumpMajor": 16.0}, pgDump(t, 15), 17},
		{"nothing known", nil, filepath.Join(t.TempDir(), "missing"), 17},
	}
	for _, c := range cases {
		if got := postgresDrillMajor(snap(c.meta), c.dump); got != c.want {
			t.Errorf("%s: got postgres:%d, want postgres:%d", c.name, got, c.want)
		}
	}
}
