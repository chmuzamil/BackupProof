package source

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMySQLToolsFollowTheContainer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as a fake docker")
	}
	for _, c := range []struct {
		exit         string
		client, dump string
	}{{"0", "mariadb", "mariadb-dump"}, {"1", "mysql", "mysqldump"}} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nexit "+c.exit+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		client, dump := mysqlTools(context.Background(), Spec{Container: "db"})
		if client != c.client || dump != c.dump {
			t.Errorf("exit %s: %s %s", c.exit, client, dump)
		}
	}
}

func TestMissingProgramIsExplained(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	cmd := shellForTest("exit 127")
	_, err := runOutput(cmd)
	if err == nil || !strings.Contains(err.Error(), "wasn't found") {
		t.Errorf("err = %v", err)
	}
}

func shellForTest(script string) *exec.Cmd { return exec.Command("sh", "-c", script) }
