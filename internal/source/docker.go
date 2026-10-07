package source

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/chmuzamil/backupproof/internal/engine"
)

// Docker apps and volumes. A backup holds, under docker/:
//
//	volumes/<name>/…            the files of each named volume (with owners)
//	mounts/<host path>…         folders and files mounted into the containers
//	containers/<name>.json      each container's settings (docker inspect)
//	compose/<project>/<file>    the Compose files and .env of a project
//
// Volumes are read straight from Docker's folder when this server can (Linux,
// as root); otherwise, as on Docker Desktop, through a small helper container.

// DockerSpec says what to back up: a Compose project, containers and/or
// volumes. Stop pauses the containers that use the volumes during the backup
// so databases and other apps leave their files consistent.
type DockerSpec struct {
	Project    string   `json:"project,omitempty"`
	Containers []string `json:"containers,omitempty"`
	Volumes    []string `json:"volumes,omitempty"`
	Stop       bool     `json:"stop,omitempty"`
	// Mounts also backs up the folders and files on this server that are
	// mounted into the containers (bind mounts), such as ./data in Compose.
	Mounts bool `json:"mounts,omitempty"`
}

// systemMounts are parts of the machine itself that containers often mount
// (the Docker socket, /proc, the time zone…): never app data to back up.
var systemMounts = []string{"/proc", "/sys", "/dev", "/run", "/var/run", "/var/lib/docker", "/tmp", "/boot",
	"/usr", "/bin", "/sbin", "/lib", "/lib64", "/lib/modules",
	"/etc/localtime", "/etc/timezone", "/etc/hosts", "/etc/hostname", "/etc/resolv.conf", "/etc/machine-id"}

// AppMount reports whether a bind mount's source on this server looks like
// app data worth backing up. Only absolute paths qualify, so Docker Desktop's
// Windows paths are left out.
func AppMount(src string) bool {
	if !strings.HasPrefix(src, "/") {
		return false
	}
	p := path.Clean(src)
	if p == "/" || strings.HasSuffix(p, ".sock") {
		return false
	}
	for _, s := range systemMounts {
		if p == s || strings.HasPrefix(p, s+"/") {
			return false
		}
	}
	return true
}

// topLevel drops paths that are inside another path in the list.
func topLevel(paths []string) []string {
	var out []string
	for _, p := range uniq(paths) {
		if len(out) > 0 {
			last := out[len(out)-1]
			if p == last || strings.HasPrefix(p, last+"/") {
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

// HelperImage reads and writes volumes when Docker's folder isn't reachable.
const HelperImage = "busybox:1.37"

var dockerNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func (d *DockerSpec) validate() error {
	if d == nil || (d.Project == "" && len(d.Containers) == 0 && len(d.Volumes) == 0) {
		return errors.New("choose a Docker app, containers or volumes to back up")
	}
	for _, n := range append(append([]string{d.Project}, d.Containers...), d.Volumes...) {
		if n != "" && !dockerNameRe.MatchString(n) {
			return fmt.Errorf("%q isn't a valid Docker name", n)
		}
	}
	return nil
}

type dockerMount struct {
	Type, Name, Source, Destination string
}

type dockerContainer struct {
	Name   string
	Raw    json.RawMessage
	Mounts []dockerMount
	Labels map[string]string
	Run    bool
}

func docker(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, fmt.Errorf("docker %s: %s", args[0], msg)
		}
		return out, fmt.Errorf("docker %s: %w", args[0], err)
	}
	return out, nil
}

// inspectContainers returns the named containers (and, for a project, all of
// its containers) with their mounts and labels.
func inspectContainers(ctx context.Context, d *DockerSpec) ([]dockerContainer, error) {
	names := append([]string(nil), d.Containers...)
	if d.Project != "" {
		out, err := docker(ctx, "ps", "-a", "--filter", "label=com.docker.compose.project="+d.Project, "--format", "{{.Names}}")
		if err != nil {
			return nil, err
		}
		names = append(names, strings.Fields(string(out))...)
		if len(names) == 0 && len(d.Volumes) == 0 {
			return nil, fmt.Errorf("no containers belong to the Docker app %q", d.Project)
		}
	}
	names = uniq(names)
	if len(names) == 0 {
		return nil, nil
	}
	out, err := docker(ctx, append([]string{"inspect"}, names...)...)
	if err != nil {
		return nil, err
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	var cs []dockerContainer
	for _, r := range raw {
		var c struct {
			Name   string
			Mounts []dockerMount
			Config struct{ Labels map[string]string }
			State  struct{ Running bool }
		}
		if err := json.Unmarshal(r, &c); err != nil {
			return nil, err
		}
		cs = append(cs, dockerContainer{Name: strings.TrimPrefix(c.Name, "/"), Raw: r, Mounts: c.Mounts, Labels: c.Config.Labels, Run: c.State.Running})
	}
	return cs, nil
}

func uniq(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

// volumeUsers lists running containers that use volume v.
func volumeUsers(ctx context.Context, v string) []string {
	out, err := docker(ctx, "ps", "--filter", "volume="+v, "--format", "{{.Names}}")
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// stopContainers stops the given running containers and returns a function
// that starts them again, in reverse order.
func stopContainers(ctx context.Context, names []string, log func(string, ...any)) func() {
	var stopped []string
	for _, n := range names {
		log("stopping container %s", n)
		if _, err := docker(ctx, "stop", n); err != nil {
			log("warning: could not stop %s: %v", n, err)
			continue
		}
		stopped = append(stopped, n)
	}
	return func() {
		for i := len(stopped) - 1; i >= 0; i-- {
			log("starting container %s", stopped[i])
			if _, err := docker(context.Background(), "start", stopped[i]); err != nil {
				log("warning: could not start %s again: %v", stopped[i], err)
			}
		}
	}
}

// volumeDir is a volume's folder on this server, if this process can read it.
func volumeDir(ctx context.Context, v string) string {
	out, err := docker(ctx, "volume", "inspect", "-f", "{{.Mountpoint}}", v)
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(string(out))
	if _, err := os.ReadDir(dir); err != nil {
		return ""
	}
	return dir
}

func backupDocker(ctx context.Context, s Spec, b *engine.Builder, log engine.Logger) (map[string]any, error) {
	d := s.Docker
	cs, err := inspectContainers(ctx, d)
	if err != nil {
		return nil, err
	}
	var vols []string
	vols = append(vols, d.Volumes...)
	for _, c := range cs {
		for _, m := range c.Mounts {
			if m.Type == "volume" && m.Name != "" {
				vols = append(vols, m.Name)
			}
		}
	}
	vols = uniq(vols)
	if d.Stop {
		var users []string
		for _, c := range cs {
			if c.Run {
				users = append(users, c.Name)
			}
		}
		for _, v := range vols {
			users = append(users, volumeUsers(ctx, v)...)
		}
		defer stopContainers(ctx, uniq(users), log)()
	}
	var names []string
	for _, c := range cs {
		names = append(names, c.Name)
		if _, err := b.AddReader(ctx, "docker/containers/"+c.Name+".json", 0o600, time.Now(), strings.NewReader(string(c.Raw))); err != nil {
			return nil, err
		}
	}
	// Compose files: from the project's labels, plus .env next to them.
	var composeFiles []string
	for _, c := range cs {
		if f := c.Labels["com.docker.compose.project.config_files"]; f != "" {
			composeFiles = append(composeFiles, strings.Split(f, ",")...)
			if wd := c.Labels["com.docker.compose.project.working_dir"]; wd != "" {
				composeFiles = append(composeFiles, filepath.Join(wd, ".env"))
			}
		}
	}
	project := d.Project
	if project == "" && len(cs) > 0 {
		project = cs[0].Labels["com.docker.compose.project"]
	}
	for _, f := range uniq(composeFiles) {
		fh, err := os.Open(f)
		if err != nil {
			continue // a missing .env is normal
		}
		fi, _ := fh.Stat()
		_, err = b.AddReader(ctx, "docker/compose/"+project+"/"+filepath.Base(f), uint32(fi.Mode().Perm()), fi.ModTime(), fh)
		fh.Close()
		if err != nil {
			return nil, err
		}
	}
	for _, v := range vols {
		if dir := volumeDir(ctx, v); dir != "" {
			log("backing up volume %s", v)
			if err := b.AddTree(ctx, dir, "docker/volumes/"+v); err != nil {
				return nil, fmt.Errorf("volume %s: %w", v, err)
			}
			continue
		}
		log("backing up volume %s through a helper container", v)
		if err := addVolumeViaHelper(ctx, b, v); err != nil {
			return nil, fmt.Errorf("volume %s: %w", v, err)
		}
	}
	var mounts []string
	if d.Mounts {
		var srcs []string
		for _, c := range cs {
			for _, m := range c.Mounts {
				if m.Type == "bind" && AppMount(m.Source) {
					srcs = append(srcs, path.Clean(m.Source))
				}
			}
		}
		for _, m := range topLevel(srcs) {
			if b.Denied(m) {
				log("skipping %s: it holds this server's own BackupProof data", m)
				continue
			}
			fi, err := os.Stat(m)
			if err != nil {
				log("warning: skipping mounted %s: %v", m, err)
				continue
			}
			log("backing up mounted %s", m)
			prefix := "docker/mounts" + m
			switch {
			case fi.IsDir():
				if err := b.AddTree(ctx, m, prefix); err != nil {
					return nil, fmt.Errorf("mounted folder %s: %w", m, err)
				}
			case fi.Mode().IsRegular():
				f, err := os.Open(m)
				if err != nil {
					return nil, err
				}
				_, err = b.AddReader(ctx, prefix, uint32(fi.Mode().Perm()), fi.ModTime(), f)
				f.Close()
				if err != nil {
					return nil, fmt.Errorf("mounted file %s: %w", m, err)
				}
			default:
				continue
			}
			mounts = append(mounts, m)
		}
	}
	return map[string]any{"volumes": vols, "mounts": mounts, "containers": names, "project": project}, nil
}

// ContainersUsing lists running containers with a bind mount at, inside or
// above one of the given paths on this server.
func ContainersUsing(ctx context.Context, paths []string) []string {
	out, err := docker(ctx, "ps", "-q")
	if err != nil || len(strings.Fields(string(out))) == 0 {
		return nil
	}
	out, err = docker(ctx, append([]string{"inspect"}, strings.Fields(string(out))...)...)
	if err != nil {
		return nil
	}
	var cs []struct {
		Name   string
		Mounts []dockerMount
	}
	if json.Unmarshal(out, &cs) != nil {
		return nil
	}
	overlaps := func(a, b string) bool {
		return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
	}
	var names []string
	for _, c := range cs {
		for _, m := range c.Mounts {
			if m.Type != "bind" {
				continue
			}
			for _, p := range paths {
				if overlaps(path.Clean(m.Source), p) {
					names = append(names, strings.TrimPrefix(c.Name, "/"))
				}
			}
		}
	}
	return uniq(names)
}

// StopContainers stops the given running containers and returns a function
// that starts them again.
func StopContainers(ctx context.Context, names []string, log func(string, ...any)) func() {
	return stopContainers(ctx, names, log)
}

// addVolumeViaHelper copies a volume out with tar in a throwaway container
// (no network), unpacks it into a temporary folder, and backs that up.
func addVolumeViaHelper(ctx context.Context, b *engine.Builder, v string) error {
	tmp, err := os.MkdirTemp("", "bp-volume-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "-v", v+":/v:ro", HelperImage, "tar", "-C", "/v", "-cf", "-", ".")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	owners, xerr := untar(out, tmp)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("helper container: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	if xerr != nil {
		return xerr
	}
	if err := b.AddTree(ctx, tmp, "docker/volumes/"+v); err != nil {
		return err
	}
	// Without root the unpacked files belong to this user; keep the real owners.
	b.SetOwners("docker/volumes/"+v, owners)
	return nil
}

// untar unpacks regular files, folders and symlinks into dir, refusing any
// path that would leave it. It returns each path's owner from the archive.
func untar(r io.Reader, dir string) (map[string][2]int, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	owners := map[string][2]int{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return owners, nil
		}
		if err != nil {
			return nil, err
		}
		name := filepath.FromSlash(strings.TrimPrefix(filepath.ToSlash(filepath.Clean(h.Name)), "./"))
		if name == "." || name == "" {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o700); err != nil {
				return nil, err
			}
			_ = root.Chmod(name, os.FileMode(h.Mode).Perm())
		case tar.TypeReg:
			if d := filepath.Dir(name); d != "." {
				if err := root.MkdirAll(d, 0o700); err != nil {
					return nil, err
				}
			}
			f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode).Perm())
			if err != nil {
				return nil, err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return nil, err
			}
			f.Close()
		case tar.TypeSymlink:
			_ = root.Symlink(h.Linkname, name)
		default:
			continue
		}
		owners[filepath.ToSlash(name)] = [2]int{h.Uid, h.Gid}
		_ = root.Lchown(name, h.Uid, h.Gid)
		_ = root.Chtimes(name, h.ModTime, h.ModTime)
	}
}

// RestoreVolume replaces the contents of volume v with the restored files in
// src (a folder), stopping and restarting the containers that use it.
// Owner is a file's owner as recorded in the backup.
type Owner struct{ UID, GID int }

// RestoreVolume replaces volume v's contents using restore. owners maps each
// path inside the volume (slash-separated) to its backed-up owner, for when
// the files go in through a helper container: then they are written here
// first, possibly without the right to set owners, and the tar stream sets
// them instead.
func RestoreVolume(ctx context.Context, v, src string, owners map[string]Owner, restore func(target string) error, log func(string, ...any)) error {
	if !dockerNameRe.MatchString(v) {
		return fmt.Errorf("%q isn't a valid volume name", v)
	}
	if _, err := docker(ctx, "volume", "create", v); err != nil {
		return err
	}
	defer stopContainers(ctx, volumeUsers(ctx, v), log)()
	if dir := volumeDir(ctx, v); dir != "" {
		log("replacing the contents of volume %s", v)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
		return restore(dir)
	}
	// Through a helper container: restore into src, then stream it in.
	if err := restore(src); err != nil {
		return err
	}
	log("replacing the contents of volume %s through a helper container", v)
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeTar(pw, src, owners)) }()
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-i", "--network", "none", "-v", v+":/v", HelperImage,
		"sh", "-c", "find /v -mindepth 1 -delete && tar -C /v -xf -")
	cmd.Stdin = pr
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helper container: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func writeTar(w io.Writer, dir string, owners map[string]Owner) error {
	tw := tar.NewWriter(w)
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		link := ""
		if fi.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		h, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		if o, ok := owners[h.Name]; ok {
			h.Uid, h.Gid, h.Uname, h.Gname = o.UID, o.GID, "", ""
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}
