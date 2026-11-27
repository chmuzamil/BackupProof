package drill

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Sandbox is a throwaway database container with no network, bounded
// resources and no capabilities beyond what the database entrypoint needs.
// The restored snapshot is mounted read-only.
type Sandbox struct {
	Name  string
	Image string
	// Digest is the resolved image ID, recorded in the attestation so the
	// exact restore environment is reproducible.
	Digest string
	// Memory is the container's memory limit and DataInRAM where the database
	// keeps its files, so a failure for lack of room can say which ran out.
	Memory    string
	DataInRAM bool
	hasData   bool
}

type SandboxOptions struct {
	Image      string
	Env        map[string]string
	MountDir   string // host dir mounted read-only at /restore
	DataDir    string // the database's data folder inside the container
	DataBytes  int64  // size of the dump(s) to load; places DataDir in memory or on disk
	Memory     string // e.g. "2g"; empty sizes it from DataBytes
	CPUs       string // e.g. "2"
	ExtraArgs  []string
	Entrypoint []string // optional command args after the image
	Name       string   // container name (default: generated)
	Network    string   // default "none"
}

// dockerCmd runs the Docker CLI. maxOut limits how much stdout is kept
// (0 means all of it). The tests replace it.
var dockerCmd = func(ctx context.Context, maxOut int, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &capWriter{buf: &out, max: maxOut}, &capWriter{buf: &errb, max: 64 << 10}
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// capWriter keeps at most max bytes (all of them if max is 0) and discards the
// rest, so a container can't fill this process's memory through its output.
type capWriter struct {
	buf *bytes.Buffer
	max int
}

func (c *capWriter) Write(p []byte) (int, error) {
	n := len(p)
	if c.max > 0 {
		p = p[:min(n, max(c.max-c.buf.Len(), 0))]
	}
	c.buf.Write(p)
	return n, nil
}

func dockerError(args []string, err error, stderr string) error {
	msg := strings.TrimSpace(stderr)
	if len(msg) > 800 {
		msg = "…" + msg[len(msg)-800:]
	}
	return fmt.Errorf("docker %s: %v: %s", args[0], err, msg)
}

func docker(ctx context.Context, args ...string) (string, error) {
	out, stderr, err := dockerCmd(ctx, 0, args...)
	if err != nil {
		return out, dockerError(args, err, stderr)
	}
	return out, nil
}

// DockerAvailable reports whether a usable container runtime exists.
func DockerAvailable(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := docker(ctx, "version", "--format", "{{.Server.Version}}")
	return err == nil
}

// sandboxCaps are what the official database images need: their entrypoints
// start as root to prepare the data folder, then switch to the database's user.
var sandboxCaps = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETGID", "SETUID"}

func randomHex() string {
	rb := make([]byte, 6)
	rand.Read(rb)
	return hex.EncodeToString(rb)
}

// pgData is where PostgreSQL restore tests keep the database. The images
// declare their own disk volume (here up to 17, the parent folder from 18), so
// the data folder itself is mounted and PGDATA set, or a volume would sit on
// top of the memory folder and the data would go to disk regardless.
const pgData = "/var/lib/postgresql/data"

const (
	// ramDataLimit is the largest loaded database kept in memory (tmpfs), which
	// is fast. A larger one goes on a throwaway Docker volume on disk, because
	// tmpfs counts against the container's memory limit.
	ramDataLimit = 1 << 30
	// dumpGrowth estimates a loaded database from its dump: indexes and
	// uncompressed rows make it several times larger.
	dumpGrowth = 4
)

// dataPlacement decides where the database keeps its files and the memory
// limit. memory, when set, is the item's own limit and always wins.
func dataPlacement(dumpBytes int64, memory string) (inRAM bool, mem string) {
	inRAM = dumpBytes*dumpGrowth <= ramDataLimit
	switch {
	case memory != "":
		mem = memory
	case inRAM:
		mem = "4g" // 2 GB for the database server, up to 2 GB of data in tmpfs
	default:
		mem = "2g"
	}
	return inRAM, mem
}

func StartSandbox(ctx context.Context, o SandboxOptions) (*Sandbox, error) {
	name := o.Name
	if name == "" {
		name = "bp-drill-" + randomHex()
	}
	network := o.Network
	if network == "" {
		network = "none"
	}
	inRAM, mem := dataPlacement(o.DataBytes, o.Memory)
	if o.DataDir == "" {
		inRAM = false
		if o.Memory == "" {
			mem = "2g"
		}
	}
	o.Memory = mem
	if o.CPUs == "" {
		o.CPUs = "2"
	}
	if _, err := docker(ctx, "pull", "--quiet", o.Image); err != nil {
		// A locally built or cached image is fine; only fail if it is absent.
		if _, ierr := docker(ctx, "image", "inspect", o.Image); ierr != nil {
			return nil, err
		}
	}
	args := []string{"run", "-d", "--name", name,
		"--label", "backupproof.drill=1",
		"--network", network,
		// Swap equal to memory means no swap: a test that outgrows its limit
		// stops with a clear message instead of pushing the server into swap.
		"--memory", o.Memory, "--memory-swap", o.Memory, "--cpus", o.CPUs, "--pids-limit", "512",
		"--security-opt", "no-new-privileges",
		"--cap-drop", "ALL",
	}
	for _, c := range sandboxCaps {
		args = append(args, "--cap-add", c)
	}
	if o.MountDir != "" {
		args = append(args, "-v", o.MountDir+":/restore:ro")
	}
	if o.DataDir != "" {
		if inRAM {
			args = append(args, "--tmpfs", o.DataDir+":rw,size=2g")
		} else {
			// An anonymous volume: on disk, and removed with the container (rm -v).
			args = append(args, "--mount", "type=volume,dst="+o.DataDir)
		}
	}
	for k, v := range o.Env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, o.ExtraArgs...)
	args = append(args, o.Image)
	args = append(args, o.Entrypoint...)
	if _, err := docker(ctx, args...); err != nil {
		return nil, err
	}
	sb := &Sandbox{Name: name, Image: o.Image, Memory: o.Memory, DataInRAM: inRAM, hasData: o.DataDir != ""}
	if id, err := docker(ctx, "inspect", "--format", "{{.Image}}", name); err == nil {
		sb.Digest = strings.TrimSpace(id)
	}
	return sb, nil
}

// Exec runs a command inside the sandbox.
func (s *Sandbox) Exec(ctx context.Context, args ...string) (string, error) {
	return docker(ctx, append([]string{"exec", s.Name}, args...)...)
}

// WaitReady polls probe until it succeeds or the timeout elapses.
func (s *Sandbox) WaitReady(ctx context.Context, timeout time.Duration, probe ...string) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if _, err := s.Exec(ctx, probe...); err == nil {
			return nil
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	logs, _ := docker(context.Background(), "logs", "--tail", "30", s.Name)
	return fmt.Errorf("sandbox not ready after %s: %v\n%s", timeout, last, logs)
}

func (s *Sandbox) Remove() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Best-effort cleanup: there is no caller that could act on a failure here.
	_, _ = docker(ctx, "rm", "-f", "-v", s.Name)
}

// SweepContainers removes restore-test containers older than age, which an
// agent that was killed outright can leave running.
func SweepContainers(ctx context.Context, age time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := docker(ctx, "ps", "-a", "--filter", "label=backupproof.drill=1", "--format", "{{.ID}}|{{.CreatedAt}}")
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		id, created, ok := strings.Cut(line, "|")
		if !ok {
			continue
		}
		if t, err := time.Parse("2006-01-02 15:04:05 -0700 MST", created); err == nil && time.Since(t) > age {
			_, _ = docker(ctx, "rm", "-f", "-v", id)
		}
	}
}

func (s *Sandbox) Describe() string {
	d := s.Image
	if s.Digest != "" {
		d += "@" + s.Digest
	}
	extra := ""
	if s.hasData {
		extra = ", data on disk"
		if s.DataInRAM {
			extra = ", data in memory"
		}
	}
	if s.Memory != "" {
		extra += ", memory " + s.Memory
	}
	return "docker:" + d + " (network=none" + extra + ")"
}

// Explain adds why a step failed when the test container ran out of memory or
// space, which is a limit of the restore test and says nothing about the backup.
func (s *Sandbox) Explain(ctx context.Context, err error) error {
	if err == nil || s == nil {
		return err
	}
	msg := err.Error()
	if strings.Contains(msg, "No space left on device") || strings.Contains(msg, "could not extend file") {
		where := "on this server's disk"
		if s.DataInRAM {
			where = "in memory"
		}
		return fmt.Errorf("%w. The test database ran out of space %s: a limit of the restore test, %s. Raise the item's restore-test memory or free disk space", err, where, NotTheBackup)
	}
	if s.outOfMemory(ctx) {
		return fmt.Errorf("%w. The restore-test container ran out of memory (limit %s): a limit of the restore test, %s. Raise the item's restore-test memory", err, s.Memory, NotTheBackup)
	}
	return err
}

// outOfMemory reports whether the kernel killed a process in the container
// for lack of memory.
func (s *Sandbox) outOfMemory(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if out, err := docker(ctx, "exec", s.Name, "cat", "/sys/fs/cgroup/memory.events"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) == 2 && f[0] == "oom_kill" && f[1] != "0" {
				return true
			}
		}
	}
	out, err := docker(ctx, "inspect", "--format", "{{.State.OOMKilled}}", s.Name)
	return err == nil && strings.TrimSpace(out) == "true"
}
