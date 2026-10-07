package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/chmuzamil/backupproof/internal/engine"
)

// resticSource imports restic snapshots by restoring each one to a temporary
// directory with the restic binary (which verifies restic's own MACs) and
// adding the tree. It needs free disk space for one snapshot at a time.
type resticSource struct {
	spec Spec
	env  []string
}

func ResticRepoString(s Spec) string {
	c := s.Storage
	switch c.Type {
	case "local":
		return c.Path
	case "s3":
		ep := strings.TrimPrefix(strings.TrimPrefix(c.Endpoint, "https://"), "http://")
		if ep == "" {
			ep = "s3.amazonaws.com"
		}
		return "s3:https://" + strings.TrimRight(ep, "/") + "/" + strings.Trim(c.Bucket+"/"+c.Prefix, "/")
	case "sftp":
		return fmt.Sprintf("sftp:%s@%s:%s", c.User, c.Host, c.Path)
	}
	return ""
}

func newRestic(s Spec) (*resticSource, error) {
	if _, err := exec.LookPath("restic"); err != nil {
		return nil, errors.New("importing a restic repository needs the restic program installed on this server")
	}
	env := os.Environ()
	vars := map[string]string{}
	// Reuse the env file the existing restic job already uses (e.g.
	// /etc/restic/env): repository address and storage keys are read on this
	// computer and never sent to the BackupProof server.
	if s.ResticEnvFile != "" {
		parsed, err := ParseEnvFile(s.ResticEnvFile)
		if err != nil {
			return nil, err
		}
		vars = parsed
	}
	if s.ResticRepository != "" {
		vars["RESTIC_REPOSITORY"] = s.ResticRepository
	}
	if vars["RESTIC_REPOSITORY"] == "" && vars["RESTIC_REPOSITORY_FILE"] == "" {
		if s.Storage.Type == "" {
			return nil, errors.New("tell us where the restic repository is (or the path of its env file, e.g. /etc/restic/env)")
		}
		vars["RESTIC_REPOSITORY"] = ResticRepoString(s)
	}
	switch {
	case s.Password != "":
		vars["RESTIC_PASSWORD"] = s.Password
		delete(vars, "RESTIC_PASSWORD_FILE")
	case s.ResticPasswordFile != "":
		vars["RESTIC_PASSWORD_FILE"] = s.ResticPasswordFile
		delete(vars, "RESTIC_PASSWORD")
	}
	if vars["RESTIC_PASSWORD"] == "" && vars["RESTIC_PASSWORD_FILE"] == "" && vars["RESTIC_PASSWORD_COMMAND"] == "" {
		return nil, errors.New("the restic repository password is required (or the path of its password file, e.g. /etc/restic/password)")
	}
	if s.Credentials.AccessKey != "" {
		vars["AWS_ACCESS_KEY_ID"], vars["AWS_SECRET_ACCESS_KEY"] = s.Credentials.AccessKey, s.Credentials.SecretKey
	}
	if s.Storage.Region != "" && vars["AWS_DEFAULT_REGION"] == "" {
		vars["AWS_DEFAULT_REGION"] = s.Storage.Region
	}
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	return &resticSource{spec: s, env: env}, nil
}

func (r *resticSource) Format() string { return "restic" }
func (r *resticSource) Close() error   { return nil }

func (r *resticSource) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "restic", args...)
	cmd.Env = r.env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("restic %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (r *resticSource) List(ctx context.Context) ([]Point, error) {
	out, err := r.run(ctx, "snapshots", "--json", "--no-lock")
	if err != nil {
		return nil, err
	}
	var snaps []struct {
		ID       string    `json:"id"`
		Time     time.Time `json:"time"`
		Hostname string    `json:"hostname"`
		Paths    []string  `json:"paths"`
	}
	if err := json.Unmarshal(out, &snaps); err != nil {
		return nil, err
	}
	var pts []Point
	for _, s := range snaps {
		pts = append(pts, Point{ID: "restic:" + s.ID, Label: s.Hostname + " " + strings.Join(s.Paths, ", "), Time: s.Time.UTC()})
	}
	return pts, nil
}

func (r *resticSource) Fill(ctx context.Context, p Point, b *engine.Builder, log engine.Logger) error {
	tmp, err := os.MkdirTemp("", "bp-restic-import-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	id := strings.TrimPrefix(p.ID, "restic:")
	log("restoring restic snapshot %s to a staging folder", id[:8])
	if _, err := r.run(ctx, "restore", id, "--target", tmp, "--no-lock"); err != nil {
		if !metadataOnly(err.Error()) {
			return err
		}
		// restic verified every byte; only timestamps/owners/attributes of
		// the staging copy could not be set (normal when not running as
		// root, or on Windows). That does not affect the data.
		log("note: restic could not restore some file metadata (timestamps/owners) on the staging copy; the data itself was restored and verified")
	}
	return b.AddTree(ctx, tmp, "")
}

// ParseEnvFile reads KEY=VALUE lines as written for `set -a; . file`:
// comments, blank lines, "export " prefixes and quotes are handled.
func ParseEnvFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, nil
}

var metadataErr = regexp.MustCompile(`(?i)(timestamp|lchown|chown|xattr|extended attribute|chmod|security descriptor|file attributes|lutimes)`)

// metadataOnly reports whether every per-file error restic printed concerns
// metadata rather than content.
func metadataOnly(stderr string) bool {
	sawIgnored := false
	for _, line := range strings.Split(stderr, "\n") {
		if !strings.Contains(line, "ignoring error for") {
			continue
		}
		sawIgnored = true
		if !metadataErr.MatchString(line) {
			return false
		}
	}
	return sawIgnored
}
