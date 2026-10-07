package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chmuzamil/backupproof/internal/engine"
)

// kopiaSource imports Kopia snapshots with the kopia program, which verifies
// its own encryption while restoring. It either reuses an existing
// repository connection on the converting computer (its repository.config)
// or connects to S3/B2/a folder with a private, temporary config.
type kopiaSource struct {
	spec   Spec
	config string
	tmpCfg string
	env    []string
}

func newKopia(ctx context.Context, s Spec) (*kopiaSource, error) {
	if _, err := exec.LookPath("kopia"); err != nil {
		return nil, errors.New("importing a Kopia repository needs the kopia program installed on this server")
	}
	pw := s.Password
	if pw == "" && s.PasswordFile != "" {
		p, err := readSecretFile(s.PasswordFile)
		if err != nil {
			return nil, err
		}
		pw = p
	}
	k := &kopiaSource{spec: s, env: os.Environ()}
	if pw != "" {
		k.env = append(k.env, "KOPIA_PASSWORD="+pw)
	}
	k.env = append(k.env, "KOPIA_CHECK_FOR_UPDATES=false")
	if s.KopiaConfigFile != "" {
		if _, err := os.Stat(s.KopiaConfigFile); err != nil {
			return nil, fmt.Errorf("kopia settings file: %w", err)
		}
		k.config = s.KopiaConfigFile
		return k, nil
	}
	if pw == "" {
		return nil, errors.New("the Kopia repository password is required")
	}
	dir, err := os.MkdirTemp("", "bp-kopia-*")
	if err != nil {
		return nil, err
	}
	k.tmpCfg = dir
	k.config = filepath.Join(dir, "repository.config")
	args, err := KopiaConnectArgs(s)
	if err != nil {
		_ = k.Close() // removes the temp config dir; the original error is what matters
		return nil, err
	}
	args = append(args, "--cache-directory="+filepath.Join(dir, "cache"))
	if _, err := k.run(ctx, args...); err != nil {
		_ = k.Close() // removes the temp config dir; the connect error is what matters
		return nil, err
	}
	return k, nil
}

// KopiaConnectArgs builds `kopia repository connect` for the storage given.
func KopiaConnectArgs(s Spec) ([]string, error) {
	c := s.Storage
	switch c.Type {
	case "local":
		return []string{"repository", "connect", "filesystem", "--path=" + c.Path, "--readonly"}, nil
	case "s3":
		ep := strings.TrimPrefix(strings.TrimPrefix(c.Endpoint, "https://"), "http://")
		if ep == "" {
			ep = "s3.amazonaws.com"
		}
		args := []string{"repository", "connect", "s3", "--bucket=" + c.Bucket, "--endpoint=" + strings.TrimRight(ep, "/"),
			"--access-key=" + s.Credentials.AccessKey, "--secret-access-key=" + s.Credentials.SecretKey, "--readonly"}
		if c.Prefix != "" {
			args = append(args, "--prefix="+strings.TrimSuffix(c.Prefix, "/")+"/")
		}
		if c.Region != "" {
			args = append(args, "--region="+c.Region)
		}
		return args, nil
	case "sftp":
		args := []string{"repository", "connect", "sftp", "--host=" + c.Host, "--username=" + c.User, "--path=" + c.Path, "--readonly"}
		if c.Port != 0 {
			args = append(args, fmt.Sprintf("--port=%d", c.Port))
		}
		if c.HostKey != "" {
			args = append(args, "--known-hosts-data="+c.Host+" "+c.HostKey)
		}
		if s.Credentials.Password != "" {
			args = append(args, "--sftp-password="+s.Credentials.Password)
		}
		if s.Credentials.PrivateKey != "" {
			args = append(args, "--key-data="+s.Credentials.PrivateKey)
		}
		return args, nil
	}
	return nil, fmt.Errorf("kopia import supports a folder, S3/B2 or SFTP (got %q)", c.Type)
}

func (k *kopiaSource) Format() string { return "kopia" }

func (k *kopiaSource) Close() error {
	if k.tmpCfg != "" {
		return os.RemoveAll(k.tmpCfg)
	}
	return nil
}

func (k *kopiaSource) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "kopia", append([]string{"--config-file=" + k.config}, args...)...)
	cmd.Env = k.env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(strings.ToLower(msg), "invalid repository password") {
			msg = "wrong Kopia repository password"
		}
		return nil, fmt.Errorf("kopia %s: %v: %s", args[0], err, msg)
	}
	return out, nil
}

type kopiaSnap struct {
	ID     string `json:"id"`
	Source struct {
		Host     string `json:"host"`
		UserName string `json:"userName"`
		Path     string `json:"path"`
	} `json:"source"`
	StartTime time.Time `json:"startTime"`
	Stats     struct {
		TotalSize int64 `json:"totalSize"`
		FileCount int   `json:"fileCount"`
	} `json:"stats"`
}

func (k *kopiaSource) List(ctx context.Context) ([]Point, error) {
	out, err := k.run(ctx, "snapshot", "list", "--all", "--json")
	if err != nil {
		return nil, err
	}
	var snaps []kopiaSnap
	if err := json.Unmarshal(out, &snaps); err != nil {
		return nil, fmt.Errorf("kopia snapshot list: %w", err)
	}
	var pts []Point
	for _, s := range snaps {
		pts = append(pts, Point{
			ID:    "kopia:" + s.ID,
			Label: s.Source.UserName + "@" + s.Source.Host + ":" + s.Source.Path,
			Time:  s.StartTime.UTC(), Files: s.Stats.FileCount, Bytes: s.Stats.TotalSize,
		})
	}
	return pts, nil
}

func (k *kopiaSource) Fill(ctx context.Context, p Point, b *engine.Builder, log engine.Logger) error {
	tmp, err := os.MkdirTemp("", "bp-kopia-restore-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	target := filepath.Join(tmp, "data")
	id := strings.TrimPrefix(p.ID, "kopia:")
	log("restoring Kopia snapshot %s to a staging folder", id[:min(8, len(id))])
	if _, err := k.run(ctx, "snapshot", "restore", id, target, "--skip-owners", "--skip-permissions", "--ignore-permission-errors"); err != nil {
		return err
	}
	// Kopia restores the snapshot's root directly into target; keep the
	// original path so restored copies look like the source machine.
	prefix := ""
	if i := strings.LastIndex(p.Label, ":"); i >= 0 {
		prefix = p.Label[i+1:]
	}
	return b.AddTree(ctx, target, prefix)
}
