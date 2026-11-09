package drill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chmuzamil/backupproof/internal/source"
)

// The app test starts the restored application and checks it responds. It
// runs after every other check of a restore test has passed, and reports as
// app-* checks in the signed proof. Those fail only the app test: the restore
// test itself has already passed on the data.
//
// Restored data is untrusted, and a restored Compose file could mount the
// host's disk or run privileged, so nothing from the restored tree is read for
// instructions or run:
//
//   - Only the image the operator named is started, with its own entrypoint.
//   - Restored data is mounted read-only. A path the operator marks writable
//     gets a throwaway copy, never the restored tree.
//   - A small holder container is started with --network none, and every other
//     container joins its network namespace. They share one loopback and have
//     no interface at all, so a database is reachable beside the app without a
//     Docker network (an internal network still reaches the host's gateway
//     address). No port is ever published.
//   - The holder is also the probe: requests run from inside it with docker
//     exec, so the test needs nothing from the app's own image.
//   - Values of environment settings go in a private file and never on a
//     command line. Response bodies and container logs are never kept: a proof
//     must not repeat what a restored app says.
const (
	// appProbeImage holds the network namespace and makes the requests.
	// Pinned by digest, because it runs on every server with an app test.
	appProbeImage  = "curlimages/curl:8.17.0@sha256:935d9100e9ba842cdb060de42472c7ca90cfe9a7c96e4dacb55e79e560b3ff40"
	appPullTimeout = 10 * time.Minute
	appStepTimeout = 90 * time.Second
	// appDefaultStart is how long an app gets to answer unless the item says otherwise.
	appDefaultStart = 2 * time.Minute
	appProbeTimeout = 15 * time.Second
	appBodyLimit    = 1 << 20
	appTmpfsOptions = "rw,nosuid,nodev,size=1g"
	appCopyLimit    = int64(64 << 30)
	appExpiryLabel  = "backupproof.apptest.expires"
	// appMinFreeBytes is the free disk a test stops at, so a restored app that
	// writes without limit can't fill the server it runs on.
	appMinFreeBytes = uint64(1 << 30)
	// appLeftoverAfter is when a container counts as left over from an agent
	// that died mid-test, comfortably past the longest possible test.
	appLeftoverAfter = 45 * time.Minute
)

// appPollEvery is how often the app is asked whether it is up yet.
var (
	appPollEvery = 2 * time.Second
	appDiskEvery = 3 * time.Second
	appFreeBytes = freeBytes
)

type appRun struct {
	at   *source.AppTest
	kind string
	dir  string // the restored tree
	// parent is where the throwaway folder goes; work is that folder.
	parent, work string
	holder       string
	app, db      string
	secrets      []string // values to keep out of anything recorded
	diskLow      atomic.Bool
}

// appChecks runs the app test and always removes what it started.
func appChecks(ctx context.Context, r *runner, dir string, spec source.Spec, parent string) {
	base := "bp-apptest-" + randomHex()
	a := &appRun{at: spec.Drill.AppTest, kind: spec.Kind, dir: dir, parent: parent, holder: base + "-net", app: base + "-app"}
	if a.at.Database != nil {
		a.db = base + "-db"
	}
	// Containers are named before they exist, so one that Docker created while
	// this process was being cancelled is still found and removed.
	defer a.cleanup()
	a.sweepLeftovers(ctx)
	if !a.check(r, "app-sandbox-start", func() (string, error) { return a.start(ctx) }) {
		return
	}
	defer func() {
		if a.diskLow.Load() {
			a.check(r, "app-disk-space", func() (string, error) {
				return "", errors.New("the test was stopped because the server was running out of disk space")
			})
		}
	}()
	defer a.guardDisk(ctx)()
	var last probeResult
	ready := false
	dbStopped := ""
	if !a.check(r, "app-running", func() (string, error) {
		began := time.Now()
		var err error
		ready, dbStopped, last, err = a.waitUntilUp(ctx)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("the app container is running (waited %s)", time.Since(began).Round(time.Second)), nil
	}) {
		return
	}
	if a.db != "" {
		a.check(r, "app-database-running", func() (string, error) {
			if dbStopped != "" {
				return "", errors.New(dbStopped)
			}
			return "the database container is running", nil
		})
	}
	var final probeResult
	if !a.check(r, "app-http-status", func() (string, error) {
		if !ready {
			return "", a.notReady(last)
		}
		final = a.probe(ctx)
		if final.err != nil {
			return "", final.err
		}
		if final.status != a.wantStatus() {
			return "", fmt.Errorf("the app answered with status %d, expected %d", final.status, a.wantStatus())
		}
		return fmt.Sprintf("GET %s answered with status %d", a.path(), final.status), nil
	}) {
		return
	}
	if a.at.ExpectText != "" {
		a.check(r, "app-body-text", func() (string, error) {
			if !bytes.Contains(final.body, []byte(a.at.ExpectText)) {
				return "", fmt.Errorf("the expected text %q wasn't in the first %d bytes of the response", a.at.ExpectText, appBodyLimit)
			}
			return fmt.Sprintf("the response contains %q", a.at.ExpectText), nil
		})
	}
	if a.at.MaxResponseMs > 0 {
		a.check(r, "app-response-time", func() (string, error) {
			if final.ms > int64(a.at.MaxResponseMs) {
				return "", fmt.Errorf("the app took %d ms, over the limit of %d ms", final.ms, a.at.MaxResponseMs)
			}
			return fmt.Sprintf("answered in %d ms (limit %d ms)", final.ms, a.at.MaxResponseMs), nil
		})
	}
}

// guardDisk removes the test's containers if the disk they write to runs low.
// Docker has no size limit that works on every filesystem. It returns a
// function that stops watching.
func (a *appRun) guardDisk(ctx context.Context) func() {
	paths := []string{a.parent}
	if root, err := docker(ctx, "info", "--format", "{{.DockerRootDir}}"); err == nil && strings.TrimSpace(root) != "" {
		paths = append(paths, strings.TrimSpace(root))
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(appDiskEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			for _, p := range paths {
				if free, ok := appFreeBytes(p); ok && free < appMinFreeBytes {
					a.diskLow.Store(true)
					for _, name := range []string{a.app, a.db, a.holder} {
						if name != "" {
							_, _ = docker(context.WithoutCancel(ctx), "rm", "-f", "-v", name)
						}
					}
					return
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// check records a check with any secret value removed from what it says.
func (a *appRun) check(r *runner, name string, fn func() (string, error)) bool {
	return r.check(name, func() (string, error) {
		detail, err := fn()
		if err != nil {
			err = errors.New(a.scrub(err.Error()))
		}
		return a.scrub(detail), err
	})
}

func (a *appRun) scrub(s string) string {
	for _, v := range a.secrets {
		if v != "" {
			s = strings.ReplaceAll(s, v, "(hidden)")
		}
	}
	return s
}

func (a *appRun) wantStatus() int {
	if a.at.ExpectStatus != 0 {
		return a.at.ExpectStatus
	}
	return 200
}

func (a *appRun) path() string {
	if a.at.Path != "" {
		return a.at.Path
	}
	return "/"
}

func (a *appRun) startTimeout() time.Duration {
	if a.at.StartTimeoutSecs > 0 {
		return time.Duration(a.at.StartTimeoutSecs) * time.Second
	}
	return appDefaultStart
}

// start prepares the data and starts the holder, the database and the app.
func (a *appRun) start(ctx context.Context) (string, error) {
	if err := a.at.Validate(a.kind); err != nil {
		return "", err
	}
	stepCtx, cancel := context.WithTimeout(ctx, appPullTimeout+appStepTimeout)
	defer cancel()
	if _, err := docker(stepCtx, "version", "--format", "{{.Server.Version}}"); err != nil {
		return "", errors.New("the app test needs Docker on the server that runs the restore test")
	}
	abs, err := filepath.Abs(a.dir)
	if err != nil {
		return "", err
	}
	a.dir = abs
	parent := a.parent
	if a.work, err = os.MkdirTemp(parent, "bp-apptest-*"); err != nil {
		return "", err
	}
	if a.work, err = filepath.Abs(a.work); err != nil {
		return "", err
	}
	// Resolved first so a missing secret stops the test before anything starts.
	appArgs, err := a.containerArgs(ctx, "app", &a.at.App)
	if err != nil {
		return "", err
	}
	var dbArgs []string
	if a.db != "" {
		if dbArgs, err = a.containerArgs(ctx, "db", a.at.Database); err != nil {
			return "", err
		}
	}
	if err := a.startHolder(stepCtx); err != nil {
		return "", err
	}
	detail := "started with no network and no published ports; restored data is read-only unless listed as writable"
	if a.db != "" {
		sb, err := a.startContainer(stepCtx, a.db, a.at.Database.Image, dbArgs)
		if err != nil {
			return "", fmt.Errorf("starting the database: %w", err)
		}
		detail = "database " + imageWords(sb) + "; " + detail
	}
	sb, err := a.startContainer(stepCtx, a.app, a.at.App.Image, appArgs)
	if err != nil {
		return "", fmt.Errorf("starting the app: %w", err)
	}
	detail = "app " + imageWords(sb) + "; " + detail
	if a.at.Template != "" {
		detail = fmt.Sprintf("template %s v%d; %s", a.at.Template, a.at.TemplateVersion, detail)
	}
	return detail, nil
}

func imageWords(sb *Sandbox) string {
	if sb.Digest != "" {
		return sb.Image + " (" + sb.Digest + ")"
	}
	return sb.Image
}

func (a *appRun) expiry() string {
	return strconv.FormatInt(time.Now().Add(appLeftoverAfter).Unix(), 10)
}

// startHolder starts the container that owns the private network. Nothing of
// the restored data reaches it, and it has no privileges at all.
func (a *appRun) startHolder(ctx context.Context) error {
	if _, err := docker(ctx, "pull", "--quiet", appProbeImage); err != nil {
		// A cached image is fine; only fail if it is absent.
		if _, ierr := docker(ctx, "image", "inspect", appProbeImage); ierr != nil {
			return err
		}
	}
	_, err := docker(ctx, "run", "-d", "--name", a.holder,
		"--label", "backupproof.drill=1", "--label", appExpiryLabel+"="+a.expiry(),
		"--network", "none", "--pull", "never", "--restart", "no", "--log-driver", "none",
		"--memory", "128m", "--cpus", "0.5", "--pids-limit", "64",
		"--security-opt", "no-new-privileges", "--cap-drop", "ALL",
		"--read-only", "--user", "65534:65534",
		"--entrypoint", "/bin/sleep", appProbeImage, strconv.Itoa(int(appLeftoverAfter.Seconds())))
	return err
}

func (a *appRun) startContainer(ctx context.Context, name, image string, args []string) (*Sandbox, error) {
	return StartSandbox(ctx, SandboxOptions{
		Image: image, Name: name, Network: "container:" + a.holder,
		ExtraArgs: append([]string{"--label", appExpiryLabel + "=" + a.expiry(), "--pull", "never", "--restart", "no", "--log-driver", "none"}, args...),
	})
}

// containerArgs turns one container's settings into Docker options, making the
// throwaway copies and the private settings file on the way.
func (a *appRun) containerArgs(ctx context.Context, name string, c *source.AppContainer) ([]string, error) {
	var args []string
	for i, m := range c.Mounts {
		src, err := restoredPath(a.dir, m.From)
		if err != nil {
			return nil, err
		}
		// A folder the backup has no entry for is still private to the restore.
		if fi, err := os.Stat(src); err == nil && fi.IsDir() && fi.Mode().Perm() == 0o700 {
			_ = os.Chmod(src, 0o755)
		}
		opt := ",readonly"
		if m.Writable {
			copied := filepath.Join(a.work, name+"-"+strconv.Itoa(i))
			if err := copyRestored(ctx, src, copied); err != nil {
				return nil, fmt.Errorf("making a throwaway copy of %s: %w", m.From, err)
			}
			src, opt = copied, ""
		}
		if strings.Contains(src, ",") {
			return nil, fmt.Errorf("the folder %s has a comma in its path, so Docker can't mount it", src)
		}
		args = append(args, "--mount", "type=bind,src="+src+",dst="+m.To+opt)
	}
	for _, p := range c.Tmpfs {
		args = append(args, "--tmpfs", p+":"+appTmpfsOptions)
	}
	if len(c.Env) == 0 {
		return args, nil
	}
	var b strings.Builder
	for _, e := range c.Env {
		v := e.Value
		if e.SecretEnv != "" {
			v = os.Getenv(e.SecretEnv)
			if v == "" {
				return nil, fmt.Errorf("the setting %s needs %s, which isn't set on the server running this test", e.Name, e.SecretEnv)
			}
			if strings.ContainsAny(v, "\r\n") {
				return nil, fmt.Errorf("the value of %s has a line break, which Docker can't pass on", e.SecretEnv)
			}
			a.secrets = append(a.secrets, v)
		}
		b.WriteString(e.Name + "=" + v + "\n")
	}
	file := filepath.Join(a.work, name+".env")
	if err := os.WriteFile(file, []byte(b.String()), 0o600); err != nil {
		return nil, err
	}
	return append(args, "--env-file", file), nil
}

// restoredPath finds a path inside the restored tree. Restored symlinks are
// untrusted: Docker would follow one, so a backup holding "data -> /" could
// otherwise put the server's disk in the container. No part of the path may
// be a symlink.
func restoredPath(root, rel string) (string, error) {
	cur := root
	for _, part := range strings.Split(filepath.FromSlash(rel), string(filepath.Separator)) {
		if part == "." {
			continue
		}
		if part == ".." || part == "" {
			return "", fmt.Errorf("%q isn't a usable path inside the backup", rel)
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return "", fmt.Errorf("%s isn't in this backup", rel)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("%s is a symbolic link in the backup, so it can't be mounted", rel)
		}
	}
	return cur, nil
}

// copyRestored makes the throwaway copy of a writable path, keeping owners
// (apps refuse data they don't own). Symlinks are copied as links and never
// followed, and special files are left out.
func copyRestored(ctx context.Context, src, dst string) error {
	var total int64
	var dirs []string
	dirMode := map[string]fs.FileMode{}
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if err := os.MkdirAll(out, 0o700); err != nil {
				return err
			}
			dirs, dirMode[out] = append(dirs, out), fi.Mode().Perm()
			copyOwner(out, fi)
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, out); err != nil {
				return err
			}
			copyOwner(out, fi)
		case d.Type().IsRegular():
			if total += fi.Size(); total > appCopyLimit {
				return fmt.Errorf("more than %d GiB would have to be copied", appCopyLimit>>30)
			}
			if err := copyFile(ctx, p, out, fi.Mode().Perm()); err != nil {
				return err
			}
			copyOwner(out, fi)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Deepest first, so a read-only folder doesn't block its own contents.
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Chmod(dirs[i], dirMode[dirs[i]])
	}
	return nil
}

func copyFile(ctx context.Context, src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return err
}

type appState struct {
	running  bool
	exitCode int
	oom      bool
}

func (s appState) words(what string) string {
	if s.oom {
		return "the " + what + " container ran out of memory and stopped"
	}
	return fmt.Sprintf("the %s container stopped (exit code %d)", what, s.exitCode)
}

func (a *appRun) state(ctx context.Context, name string) (appState, error) {
	ctx, cancel := context.WithTimeout(ctx, appStepTimeout)
	defer cancel()
	out, err := docker(ctx, "inspect", "--format", "{{.State.Running}} {{.State.ExitCode}} {{.State.OOMKilled}}", name)
	if err != nil {
		return appState{}, err
	}
	f := strings.Fields(out)
	if len(f) != 3 {
		return appState{}, fmt.Errorf("unexpected answer from docker inspect: %q", out)
	}
	code, _ := strconv.Atoi(f[1])
	return appState{running: f[0] == "true", exitCode: code, oom: f[2] == "true"}, nil
}

// waitUntilUp polls until the app gives the expected status, a container
// stops, or the time runs out. An app still starting when time runs out is
// not an error here: app-http-status reports it.
func (a *appRun) waitUntilUp(ctx context.Context) (ready bool, dbStopped string, last probeResult, err error) {
	waitCtx, cancel := context.WithTimeout(ctx, a.startTimeout())
	defer cancel()
	for {
		st, err := a.state(ctx, a.app)
		if err != nil {
			return false, "", last, err
		}
		if !st.running {
			return false, "", last, errors.New(st.words("app"))
		}
		if a.db != "" {
			ds, err := a.state(ctx, a.db)
			if err != nil {
				return false, "", last, err
			}
			if !ds.running {
				return false, ds.words("database"), last, nil
			}
		}
		last = a.probe(waitCtx)
		if last.err == nil && last.status == a.wantStatus() {
			return true, "", last, nil
		}
		select {
		case <-ctx.Done():
			return false, "", last, ctx.Err()
		case <-waitCtx.Done():
			return false, "", last, nil
		case <-time.After(appPollEvery):
		}
	}
}

func (a *appRun) notReady(last probeResult) error {
	if last.answered {
		return fmt.Errorf("after %s the app was still answering with status %d, expected %d", a.startTimeout(), last.status, a.wantStatus())
	}
	return fmt.Errorf("nothing answered on port %d within %s", a.at.Port, a.startTimeout())
}

type probeResult struct {
	answered bool
	status   int
	ms       int64
	body     []byte
	err      error
}

const probeMarker = "BPPROBE"

// probe requests the app from inside the holder's network. curl sends the
// measurements to stderr so a large body can't push them out of the stdout limit.
func (a *appRun) probe(ctx context.Context) probeResult {
	limit := appProbeTimeout
	if d := time.Duration(a.at.MaxResponseMs)*time.Millisecond + time.Second; d > limit {
		limit = d // long enough to measure an answer that breaks the item's own limit
	}
	ctx, cancel := context.WithTimeout(ctx, limit+5*time.Second)
	defer cancel()
	args := []string{"exec", a.holder, "curl", "-q", "--silent", "--show-error", "--globoff",
		"--proto", "=http", "--noproxy", "*",
		"--max-time", strconv.Itoa(int(limit.Seconds())),
		"--output", "-",
		"--write-out", "%{stderr}" + probeMarker + " %{http_code} %{time_total}"}
	if a.at.Host != "" {
		args = append(args, "--header", "Host: "+a.at.Host)
	}
	args = append(args, "http://127.0.0.1:"+strconv.Itoa(a.at.Port)+a.path())
	body, stderr, err := dockerCmd(ctx, appBodyLimit, args...)
	status, ms, ok := parseProbe(stderr)
	if err != nil || !ok || status == 0 {
		return probeResult{err: fmt.Errorf("nothing answered on port %d", a.at.Port)}
	}
	return probeResult{answered: true, status: status, ms: ms, body: []byte(body)}
}

func parseProbe(stderr string) (status int, ms int64, ok bool) {
	i := strings.LastIndex(stderr, probeMarker)
	if i < 0 {
		return 0, 0, false
	}
	f := strings.Fields(stderr[i:])
	if len(f) != 3 {
		return 0, 0, false
	}
	status, err := strconv.Atoi(f[1])
	if err != nil {
		return 0, 0, false
	}
	secs, err := strconv.ParseFloat(f[2], 64)
	if err != nil {
		return 0, 0, false
	}
	return status, int64(secs * 1000), true
}

// cleanup removes everything the test started. It can't use the test's own
// context, which may be the reason for stopping.
func (a *appRun) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range []string{a.app, a.db, a.holder} {
		if name != "" {
			// Best-effort: a failure here can't change the result, and the sweep
			// below takes it on the next test.
			_, _ = docker(ctx, "rm", "-f", "-v", name)
		}
	}
	if a.work != "" {
		if err := os.RemoveAll(a.work); err != nil {
			fixPermissions(a.work)
			_ = os.RemoveAll(a.work)
		}
	}
}

// RemoveLeftover deletes a folder a killed job left behind, including one
// holding read-only folders.
func RemoveLeftover(dir string) {
	if os.RemoveAll(dir) != nil {
		fixPermissions(dir)
		_ = os.RemoveAll(dir)
	}
}

// fixPermissions makes a throwaway copy removable if it came with read-only folders.
func fixPermissions(root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
}

// sweepLeftovers removes containers an earlier test couldn't, for instance
// because the agent was killed in the middle of one.
func (a *appRun) sweepLeftovers(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, appStepTimeout)
	defer cancel()
	out, err := docker(ctx, "ps", "-a", "--filter", "label="+appExpiryLabel, "--format", `{{.ID}} {{.Label "`+appExpiryLabel+`"}}`)
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		if until, err := strconv.ParseInt(f[1], 10, 64); err == nil && time.Now().Unix() > until {
			_, _ = docker(ctx, "rm", "-f", "-v", f[0])
		}
	}
}
