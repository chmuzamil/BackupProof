package source

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeDocker puts a docker command on PATH that logs its arguments and
// fails for the given "verb name" pairs.
func fakeDocker(t *testing.T, failing ...string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as docker")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\n"
	for _, f := range failing {
		script += fmt.Sprintf("[ \"$*\" = %q ] && { echo 'Error response from daemon' >&2; exit 1; }\n", f)
	}
	script += "exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func calls(t *testing.T, log string) string {
	b, _ := os.ReadFile(log)
	return strings.Join(strings.Split(strings.TrimSpace(string(b)), "\n"), "; ")
}

// If a container won't stop, the ones already stopped start again and the
// backup fails, rather than copying a live database's files.
func TestStopFailureFailsAndRestarts(t *testing.T) {
	log := fakeDocker(t, "stop db")
	var warned []string
	warn := func(f string, a ...any) { warned = append(warned, fmt.Sprintf(f, a...)) }
	_, err := stopContainers(context.Background(), []string{"web", "db"}, warn, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "couldn't stop the container db") {
		t.Fatalf("err = %v", err)
	}
	if got := calls(t, log); got != "stop web; stop db; start web" {
		t.Fatalf("docker calls: %s", got)
	}
}

// A container that doesn't start again after the backup is a warning, which
// the dashboard turns into an alert.
func TestRestartFailureWarns(t *testing.T) {
	fakeDocker(t, "start db")
	var warned []string
	warn := func(f string, a ...any) { warned = append(warned, fmt.Sprintf(f, a...)) }
	restart, err := stopContainers(context.Background(), []string{"web", "db"}, warn, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	restart()
	if len(warned) != 1 || !strings.Contains(warned[0], "the container db didn't start again") {
		t.Fatalf("warnings: %v", warned)
	}
}
