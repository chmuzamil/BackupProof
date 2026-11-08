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
}

type SandboxOptions struct {
	Image      string
	Env        map[string]string
	MountDir   string // host dir mounted read-only at /restore
	DataTmpfs  string // tmpfs path for the database data dir
	Memory     string // e.g. "2g"
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

func StartSandbox(ctx context.Context, o SandboxOptions) (*Sandbox, error) {
	name := o.Name
	if name == "" {
		name = "bp-drill-" + randomHex()
	}
	network := o.Network
	if network == "" {
		network = "none"
	}
	if o.Memory == "" {
		o.Memory = "2g"
	}
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
		"--memory", o.Memory, "--cpus", o.CPUs, "--pids-limit", "512",
		"--security-opt", "no-new-privileges",
		"--cap-drop", "ALL",
	}
	for _, c := range sandboxCaps {
		args = append(args, "--cap-add", c)
	}
	if o.MountDir != "" {
		args = append(args, "-v", o.MountDir+":/restore:ro")
	}
	if o.DataTmpfs != "" {
		args = append(args, "--tmpfs", o.DataTmpfs+":rw,size=8g")
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
	sb := &Sandbox{Name: name, Image: o.Image}
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

func (s *Sandbox) Describe() string {
	d := s.Image
	if s.Digest != "" {
		d += "@" + s.Digest
	}
	return "docker:" + d + " (network=none)"
}
