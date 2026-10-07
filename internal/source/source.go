// Package source captures consistent data from servers and applications:
// file trees, database dumps (PostgreSQL, MySQL/MariaDB, MongoDB, SQLite)
// and arbitrary commands, with pre/post hooks for application quiescing.
package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/engine"
	"github.com/chmuzamil/backupproof/internal/importer"
)

// Spec describes one protected source. It is stored in the control plane
// (without secrets) and sent to agents with each job.
type Spec struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // files | postgres | mysql | mongodb | sqlite | command

	// files
	Paths    []string `json:"paths,omitempty"`
	Excludes []string `json:"excludes,omitempty"`

	// databases
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	User     string `json:"user,omitempty"`
	Database string `json:"database,omitempty"`
	Password string `json:"password,omitempty"` // secret; never logged or attested
	// URI is used by MongoDB (mongodb://...). Secret.
	URI string `json:"uri,omitempty"`
	// Container runs dump tools inside an existing container via `docker exec`,
	// which guarantees client/server version match (the GitLab 2017 lesson).
	Container string `json:"container,omitempty"`
	// Globals also dumps PostgreSQL roles and tablespaces (pg_dumpall -g).
	Globals bool `json:"globals,omitempty"`

	// sqlite: Paths[0] is the database file.

	// command: stdout is stored as a stream.
	Command string `json:"command,omitempty"`

	// import: convert backups made by another tool (kind "import").
	Import *importer.Spec `json:"import,omitempty"`

	// WPConfig reads MySQL credentials from a WordPress wp-config.php at
	// backup time, so the password never has to be typed or stored.
	WPConfig string `json:"wpConfig,omitempty"`

	PreHook  string `json:"preHook,omitempty"`
	PostHook string `json:"postHook,omitempty"`

	// Drill configuration.
	Drill DrillSpec `json:"drill,omitempty"`
}

type DrillSpec struct {
	// Image overrides the sandbox image (default derived from kind and server version).
	Image string `json:"image,omitempty"`
	// Assertions are SQL queries that must return a single truthy value
	// (non-zero number, "t", "true", or a non-empty string) on the restored database.
	Assertions []Assertion `json:"assertions,omitempty"`
	// ExpectPaths must exist in the restored tree (manifest-relative or absolute source paths).
	ExpectPaths []string `json:"expectPaths,omitempty"`
	// MinFiles fails the drill if fewer files were restored.
	MinFiles int `json:"minFiles,omitempty"`
	// Command runs after restore with RESTORE_DIR set; exit 0 means pass.
	Command string `json:"command,omitempty"`
	// RowCountTolerance is the allowed relative difference between row-count
	// estimates captured at backup time and exact counts after restore (default 0.2).
	// Sources with exact counts (SQLite) are always compared exactly.
	RowCountTolerance float64 `json:"rowCountTolerance,omitempty"`
	// SkipDumps turns off loading PostgreSQL dumps found inside file backups.
	SkipDumps bool `json:"skipDumps,omitempty"`
}

type Assertion struct {
	Name string `json:"name"`
	SQL  string `json:"sql"`
}

func (s Spec) Validate() error {
	if s.Name == "" {
		return errors.New("source name is required")
	}
	switch s.Kind {
	case "files":
		if len(s.Paths) == 0 {
			return errors.New("files source needs at least one path")
		}
	case "postgres", "mysql":
		if s.Database == "" && s.WPConfig == "" && s.Container == "" {
			return fmt.Errorf("%s source needs a database", s.Kind)
		}
	case "mongodb":
		if s.URI == "" && s.Host == "" && s.Container == "" {
			return errors.New("mongodb source needs a uri, host or container")
		}
	case "sqlite":
		if len(s.Paths) != 1 {
			return errors.New("sqlite source needs exactly one database path")
		}
	case "command":
		if s.Command == "" {
			return errors.New("command source needs a command")
		}
	case "import":
		if s.Import == nil || !s.Import.HasLocation() {
			return errors.New("import needs the location of the old backups")
		}
		switch s.Import.Format {
		case "files", "restic", "kopia", "borg":
		default:
			return errors.New("import format must be files, restic, kopia or borg")
		}
	default:
		return fmt.Errorf("unknown source kind %q", s.Kind)
	}
	return nil
}

// Redacted returns a copy without secrets, safe for logs and attestations.
func (s Spec) Redacted() Spec {
	s.Password = ""
	if s.URI != "" {
		s.URI = "(redacted)"
	}
	if s.Import != nil {
		im := *s.Import
		im.Password, im.Credentials, im.PrivateKey = "", backend.Credentials{}, ""
		s.Import = &im
	}
	return s
}

// Backup captures the source into b and returns metadata that is stored in
// the snapshot and used later by restore drills for reconciliation.
func Backup(ctx context.Context, s Spec, b *engine.Builder, log engine.Logger) (map[string]any, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if s.PreHook != "" {
		log("running pre-hook")
		if out, err := Shell(ctx, s.PreHook, nil); err != nil {
			return nil, fmt.Errorf("pre-hook failed: %v: %s", err, tail(out))
		}
	}
	meta, err := capture(ctx, s, b, log)
	if s.PostHook != "" {
		log("running post-hook")
		if out, herr := Shell(ctx, s.PostHook, nil); herr != nil {
			if err == nil {
				err = fmt.Errorf("post-hook failed: %v: %s", herr, tail(out))
			}
		}
	}
	return meta, err
}

func capture(ctx context.Context, s Spec, b *engine.Builder, log engine.Logger) (map[string]any, error) {
	switch s.Kind {
	case "files":
		for _, p := range s.Paths {
			log("scanning %s", p)
			if err := b.AddPath(ctx, p); err != nil {
				return nil, err
			}
		}
		return map[string]any{"paths": s.Paths}, nil
	case "import":
		return nil, errors.New("imports run through ops.Import")
	case "postgres", "mysql":
		if err := resolveCredentials(ctx, &s); err != nil {
			return nil, err
		}
		if s.Kind == "mysql" {
			return backupMySQL(ctx, s, b, log)
		}
		return backupPostgres(ctx, s, b, log)
	case "mongodb":
		return backupMongo(ctx, s, b, log)
	case "sqlite":
		return backupSQLite(ctx, s, b, log)
	case "command":
		log("running command source")
		cmd := shellCmd(ctx, s.Command)
		if _, err := streamCommand(ctx, b, "command/"+s.Name+".out", cmd); err != nil {
			return nil, err
		}
		return map[string]any{}, nil
	}
	return nil, fmt.Errorf("unknown kind %q", s.Kind)
}

// toolCmd builds a command for a database client tool, optionally inside
// the database's own container.
func toolCmd(ctx context.Context, s Spec, env map[string]string, tool string, args ...string) *exec.Cmd {
	if s.Container != "" {
		full := []string{"exec", "-i"}
		for k, v := range env {
			full = append(full, "-e", k+"="+v)
		}
		full = append(full, s.Container, tool)
		return exec.CommandContext(ctx, "docker", append(full, args...)...)
	}
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return cmd
}

// streamCommand pipes cmd's stdout into a snapshot stream and fails if the
// command exits non-zero, so a truncated dump can never become a snapshot.
func streamCommand(ctx context.Context, b *engine.Builder, name string, cmd *exec.Cmd) (int64, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &limitedBuffer{buf: &stderr, max: 16 << 10}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start %s: %w", cmd.Path, err)
	}
	e, serr := b.AddStream(ctx, name, stdout)
	if serr != nil {
		io.Copy(io.Discard, stdout)
	}
	werr := cmd.Wait()
	if werr != nil {
		return 0, fmt.Errorf("%s failed: %v: %s", cmd.Args[0], werr, tail(stderr.String()))
	}
	if serr != nil {
		return 0, serr
	}
	if e.Size == 0 {
		return 0, fmt.Errorf("%s produced an empty dump", cmd.Args[0])
	}
	return e.Size, nil
}

type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
		} else {
			l.buf.Write(p)
		}
	}
	return len(p), nil
}

func shellCmd(ctx context.Context, script string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	}
	return exec.CommandContext(ctx, "sh", "-c", script)
}

// Shell runs a hook script and returns its combined output.
func Shell(ctx context.Context, script string, env map[string]string) (string, error) {
	cmd := shellCmd(ctx, script)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 600 {
		s = "…" + s[len(s)-600:]
	}
	return s
}

func runOutput(cmd *exec.Cmd) (string, error) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, tail(stderr.String()))
	}
	return string(out), nil
}
