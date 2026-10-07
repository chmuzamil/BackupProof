package importer

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/chmuzamil/backupproof/internal/engine"
)

// borgSource imports BorgBackup archives with the borg program. Each archive
// is streamed with `borg export-tar` (borg verifies its own encryption and
// chunk MACs) straight into BackupProof, so no staging disk space is needed.
type borgSource struct {
	spec Spec
	env  []string
}

func newBorg(s Spec) (*borgSource, error) {
	if _, err := exec.LookPath("borg"); err != nil {
		return nil, errors.New("importing a BorgBackup repository needs the borg program installed on this server")
	}
	if s.BorgRepository == "" {
		return nil, errors.New("enter the Borg repository address (e.g. ssh://user@host:23/./backups or /mnt/backup/borg)")
	}
	return &borgSource{spec: s, env: BorgEnv(s, os.Environ())}, nil
}

// BorgEnv returns the environment for non-interactive, read-only borg use.
func BorgEnv(s Spec, base []string) []string {
	env := append(base,
		"BORG_REPO="+s.BorgRepository,
		// Never stop to ask questions: this runs unattended.
		"BORG_RELOCATED_REPO_ACCESS_IS_OK=yes",
		"BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes",
		"BORG_DISPLAY_PASSPHRASE=no",
	)
	switch {
	case s.Password != "":
		env = append(env, "BORG_PASSPHRASE="+s.Password)
	case s.PasswordFile != "":
		env = append(env, "BORG_PASSCOMMAND=cat "+shellQuote(s.PasswordFile))
	}
	if s.BorgSSHKeyFile != "" {
		env = append(env, "BORG_RSH=ssh -i "+shellQuote(s.BorgSSHKeyFile)+" -o BatchMode=yes -o StrictHostKeyChecking=accept-new")
	} else {
		env = append(env, "BORG_RSH=ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new")
	}
	return env
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (g *borgSource) Format() string { return "borg" }
func (g *borgSource) Close() error   { return nil }

func (g *borgSource) cmd(ctx context.Context, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, "borg", args...)
	c.Env = g.env
	return c
}

func borgError(op string, err error, stderr string) error {
	msg := strings.TrimSpace(stderr)
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "passphrase supplied") && strings.Contains(low, "incorrect"):
		msg = "wrong Borg passphrase"
	case strings.Contains(low, "permission denied (publickey"):
		msg = "the SSH key was not accepted by the Borg server"
	case strings.Contains(low, "does not exist") || strings.Contains(low, "is not a valid repository"):
		msg = "no Borg repository at that address"
	}
	return fmt.Errorf("borg %s: %v: %s", op, err, msg)
}

func (g *borgSource) List(ctx context.Context) ([]Point, error) {
	c := g.cmd(ctx, "list", "--json")
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return nil, borgError("list", err, stderr.String())
	}
	return ParseBorgList(out)
}

// ParseBorgList reads `borg list --json` output.
func ParseBorgList(out []byte) ([]Point, error) {
	var res struct {
		Archives []struct {
			Name  string `json:"name"`
			ID    string `json:"id"`
			Start string `json:"start"`
			Time  string `json:"time"`
		} `json:"archives"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("borg list: %w", err)
	}
	var pts []Point
	for _, a := range res.Archives {
		ts := a.Start
		if ts == "" {
			ts = a.Time
		}
		t, err := time.Parse("2006-01-02T15:04:05.000000", ts)
		if err != nil {
			t, _ = time.Parse(time.RFC3339Nano, ts)
		}
		pts = append(pts, Point{ID: "borg:" + a.ID + ":" + a.Name, Label: a.Name, Time: t.UTC()})
	}
	return pts, nil
}

func (g *borgSource) Fill(ctx context.Context, p Point, b *engine.Builder, log engine.Logger) error {
	name := p.Label
	log("streaming Borg archive %s", name)
	c := g.cmd(ctx, "export-tar", "::"+name, "-")
	var stderr bytes.Buffer
	c.Stderr = &stderr
	stdout, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	if err := c.Start(); err != nil {
		return err
	}
	ferr := AddTarStream(ctx, b, stdout, "")
	if ferr != nil {
		// Drain the pipe so borg can exit; its own failure is reported by Wait.
		_, _ = io.Copy(io.Discard, stdout)
	}
	if err := c.Wait(); err != nil {
		return borgError("export-tar", err, stderr.String())
	}
	return ferr
}

// AddTarStream adds every regular file of a tar stream under prefix.
func AddTarStream(ctx context.Context, b *engine.Builder, r io.Reader, prefix string) error {
	t := tar.NewReader(r)
	n := 0
	for {
		h, err := t.Next()
		if err == io.EOF {
			if n == 0 {
				return errors.New("the archive contained no files")
			}
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		name := h.Name
		if prefix != "" {
			name = prefix + "/" + name
		}
		if _, err := b.AddReader(ctx, name, uint32(h.Mode&0o777), h.ModTime, t); err != nil {
			return err
		}
		n++
	}
}
