package drill

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestDataPlacement(t *testing.T) {
	for _, c := range []struct {
		dump   int64
		memory string
		inRAM  bool
		mem    string
	}{
		{0, "", true, "4g"},            // size unknown: the old behaviour, in memory
		{100 << 20, "", true, "4g"},    // 100 MB dump, about 400 MB loaded
		{256 << 20, "", true, "4g"},    // right at the 1 GB loaded limit
		{256<<20 + 1, "", false, "2g"}, // just over: on disk
		{20 << 30, "", false, "2g"},    // 20 GB dump: on disk
		{20 << 30, "8g", false, "8g"},  // the item's own limit wins
		{10 << 20, "512m", true, "512m"},
	} {
		inRAM, mem := dataPlacement(c.dump, c.memory)
		if inRAM != c.inRAM || mem != c.mem {
			t.Errorf("dataPlacement(%d, %q) = %v, %q; want %v, %q", c.dump, c.memory, inRAM, mem, c.inRAM, c.mem)
		}
	}
}

// runArgs starts a sandbox against a fake Docker and returns the run command.
func runArgs(t *testing.T, o SandboxOptions) ([]string, *Sandbox) {
	t.Helper()
	var run []string
	defer UseFakeDocker(func(_ context.Context, _ int, args ...string) (string, string, error) {
		if args[0] == "run" {
			run = args
		}
		return "", "", nil
	})()
	sb, err := StartSandbox(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return run, sb
}

func flagValue(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func TestSandboxPutsLargeDatabasesOnDisk(t *testing.T) {
	small, sb := runArgs(t, SandboxOptions{Image: "postgres:17", DataDir: pgData, DataBytes: 50 << 20})
	if flagValue(small, "--tmpfs") != pgData+":rw,size=2g" || flagValue(small, "--memory") != "4g" || slices.Contains(small, "--mount") {
		t.Errorf("small database: %v", small)
	}
	if !strings.Contains(sb.Describe(), "data in memory, memory 4g") {
		t.Errorf("describe: %s", sb.Describe())
	}

	large, sb := runArgs(t, SandboxOptions{Image: "postgres:17", DataDir: pgData, DataBytes: 5 << 30})
	if flagValue(large, "--mount") != "type=volume,dst="+pgData || flagValue(large, "--memory") != "2g" || slices.Contains(large, "--tmpfs") {
		t.Errorf("large database: %v", large)
	}
	if !strings.Contains(sb.Describe(), "data on disk, memory 2g") {
		t.Errorf("describe: %s", sb.Describe())
	}

	own, _ := runArgs(t, SandboxOptions{Image: "postgres:17", DataDir: pgData, DataBytes: 5 << 30, Memory: "16g"})
	if flagValue(own, "--memory") != "16g" || flagValue(own, "--memory-swap") != "16g" {
		t.Errorf("the item's memory setting was ignored: %v", own)
	}

	// App containers have no database folder and keep the plain 2g limit.
	app, _ := runArgs(t, SandboxOptions{Image: "nginx:1.27-alpine"})
	if flagValue(app, "--memory") != "2g" || slices.Contains(app, "--tmpfs") || slices.Contains(app, "--mount") {
		t.Errorf("app container: %v", app)
	}
}

func TestExplainOutOfRoom(t *testing.T) {
	events := "low 0\nhigh 0\nmax 12\noom 3\noom_kill 1\n"
	defer UseFakeDocker(func(_ context.Context, _ int, args ...string) (string, string, error) {
		if args[0] == "exec" && slices.Contains(args, "/sys/fs/cgroup/memory.events") {
			return events, "", nil
		}
		return "false", "", nil
	})()
	sb := &Sandbox{Name: "bp-drill-x", Memory: "4g", DataInRAM: true}
	err := sb.Explain(context.Background(), errors.New("docker exec: exit status 137"))
	if err == nil || !strings.Contains(err.Error(), "ran out of memory (limit 4g)") || !strings.Contains(err.Error(), "not a fault in the backup") {
		t.Errorf("out of memory: %v", err)
	}

	err = sb.Explain(context.Background(), errors.New(`pg_restore: error: could not extend file "base/16384/16390": No space left on device`))
	if err == nil || !strings.Contains(err.Error(), "ran out of space in memory") {
		t.Errorf("tmpfs full: %v", err)
	}

	events = "oom 0\noom_kill 0\n"
	plain := errors.New("pg_restore: error: relation \"x\" does not exist")
	if got := sb.Explain(context.Background(), plain); got != plain {
		t.Errorf("an ordinary failure was changed: %v", got)
	}
	if sb.Explain(context.Background(), nil) != nil {
		t.Error("no error became one")
	}
}
