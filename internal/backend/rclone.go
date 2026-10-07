package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"
)

// Rclone reaches any of rclone's 70+ storage systems (Google Drive, Dropbox,
// OneDrive, pCloud, Mega, Box, …, including rclone "crypt" remotes, which
// rclone decrypts transparently) through the rclone program and its existing
// configuration on this computer. It is meant for importing backups that
// already live there; it is not tuned for storing many small chunks.
type Rclone struct {
	remote string // "gdrive:Backups/server1"
	config string // optional path to rclone.conf
}

func NewRclone(c Config) (*Rclone, error) {
	if _, err := exec.LookPath("rclone"); err != nil {
		return nil, errors.New("cloud drives are reached through the rclone program; install rclone on this server and set up the drive with `rclone config`")
	}
	if c.Remote == "" || !strings.Contains(c.Remote, ":") {
		return nil, errors.New(`enter the rclone remote and folder, e.g. "gdrive:Backups"`)
	}
	return &Rclone{remote: strings.TrimRight(c.Remote, "/"), config: c.RcloneConfig}, nil
}

func (r *Rclone) cmd(ctx context.Context, args ...string) *exec.Cmd {
	if r.config != "" {
		args = append([]string{"--config", r.config}, args...)
	}
	c := exec.CommandContext(ctx, "rclone", args...)
	c.Env = os.Environ()
	return c
}

func (r *Rclone) full(key string) string {
	if key == "" {
		return r.remote
	}
	sep := "/"
	if strings.HasSuffix(r.remote, ":") {
		sep = ""
	}
	return r.remote + sep + strings.TrimPrefix(key, "/")
}

func (r *Rclone) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	c := r.cmd(ctx, args...)
	c.Stdin = stdin
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "directory not found") || strings.Contains(msg, "object not found") {
			return nil, ErrNotFound
		}
		if strings.Contains(msg, "didn't find section in config file") {
			return nil, fmt.Errorf("rclone has no remote called %q on this server (run `rclone config`)", strings.SplitN(r.remote, ":", 2)[0])
		}
		return nil, fmt.Errorf("rclone %s: %v: %s", args[0], err, msg)
	}
	return out, nil
}

func (r *Rclone) Put(ctx context.Context, key string, data []byte) error {
	_, err := r.run(ctx, bytes.NewReader(data), "rcat", r.full(key))
	return err
}

func (r *Rclone) Get(ctx context.Context, key string) ([]byte, error) {
	return r.run(ctx, nil, "cat", r.full(key))
}

func (r *Rclone) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	c := r.cmd(ctx, "cat", r.full(key))
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		return nil, err
	}
	return &cmdReader{ReadCloser: out, cmd: c, stderr: &stderr}, nil
}

// cmdReader surfaces the command's failure when the stream ends.
type cmdReader struct {
	io.ReadCloser
	cmd    *exec.Cmd
	stderr *bytes.Buffer
	waited bool
}

func (c *cmdReader) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if err == io.EOF && !c.waited {
		c.waited = true
		if werr := c.cmd.Wait(); werr != nil {
			return n, fmt.Errorf("rclone cat: %v: %s", werr, strings.TrimSpace(c.stderr.String()))
		}
	}
	return n, err
}

func (c *cmdReader) Close() error {
	c.ReadCloser.Close()
	if !c.waited {
		c.waited = true
		c.cmd.Process.Kill()
		c.cmd.Wait()
	}
	return nil
}

type rcloneEntry struct {
	Path    string    `json:"Path"`
	Size    int64     `json:"Size"`
	ModTime time.Time `json:"ModTime"`
	IsDir   bool      `json:"IsDir"`
}

func (r *Rclone) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	out, err := r.run(ctx, nil, "lsjson", "--stat", r.full(key))
	if err != nil {
		return ObjectInfo{}, err
	}
	var e rcloneEntry
	if err := json.Unmarshal(out, &e); err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Key: key, Size: e.Size, Modified: e.ModTime}, nil
}

func (r *Rclone) List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error {
	out, err := r.run(ctx, nil, "lsjson", "--recursive", "--files-only", "--no-mimetype", r.full(prefix))
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var entries []rcloneEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir {
			continue
		}
		key := e.Path
		if prefix != "" {
			key = path.Join(strings.TrimSuffix(prefix, "/"), e.Path)
		}
		if err := fn(ObjectInfo{Key: key, Size: e.Size, Modified: e.ModTime}); err != nil {
			return err
		}
	}
	return nil
}

func (r *Rclone) Delete(ctx context.Context, key string) error {
	_, err := r.run(ctx, nil, "deletefile", r.full(key))
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (r *Rclone) Location() string { return "rclone:" + r.remote }
func (r *Rclone) Close() error     { return nil }
