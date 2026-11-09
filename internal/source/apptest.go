package source

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// AppTest starts the restored application in isolation after a restore test
// has passed, and checks that it responds. It is deliberately small: there is
// no field for a command, an entrypoint, a port on the server, a network,
// capabilities or any Docker option, so neither a saved item nor a template
// can ask for more than the fixed sandbox in internal/drill/apptest.go gives.
//
// Restored data is untrusted. Only the image named here is run, and nothing
// in the restored tree (Compose files included) is ever read or executed.
type AppTest struct {
	// Template and TemplateVersion record the built-in template these
	// settings started from. They are a label for the proof and grant nothing.
	Template        string `json:"template,omitempty"`
	TemplateVersion int    `json:"templateVersion,omitempty"`

	App AppContainer `json:"app"`
	// Database optionally runs beside the app. It shares the app's private
	// loopback, so the app reaches it at 127.0.0.1.
	Database *AppContainer `json:"database,omitempty"`

	Port int `json:"port"`
	// Path is requested on the app (default "/"). Host optionally sets the
	// Host header, for apps that refuse names they don't know.
	Path string `json:"path,omitempty"`
	Host string `json:"host,omitempty"`
	// ExpectStatus is the HTTP status that counts as working (default 200).
	ExpectStatus int `json:"expectStatus,omitempty"`
	// ExpectText must appear in the first 1 MiB of the response, if set.
	ExpectText string `json:"expectText,omitempty"`
	// MaxResponseMs fails the test if the response takes longer, if set.
	MaxResponseMs int `json:"maxResponseMs,omitempty"`
	// StartTimeoutSecs is how long the app gets to answer (default 120).
	StartTimeoutSecs int `json:"startTimeoutSecs,omitempty"`
}

// AppContainer is one container of an app test.
type AppContainer struct {
	Image  string         `json:"image"`
	Mounts []AppDataMount `json:"mounts,omitempty"`
	// Tmpfs lists container paths that get an empty in-memory folder.
	Tmpfs []string `json:"tmpfs,omitempty"`
	Env   []AppEnv `json:"env,omitempty"`
}

// AppDataMount puts a part of the restored data into the container.
// It is read-only unless Writable is set, which mounts a throwaway copy:
// the restored tree itself is never writable.
type AppDataMount struct {
	From     string `json:"from"` // path inside the backup, e.g. docker/volumes/wp_data
	To       string `json:"to"`   // path inside the container
	Writable bool   `json:"writable,omitempty"`
}

// AppEnv sets one environment variable in the container, either to a literal
// that isn't secret or to the value of SecretEnv: the name of a variable set
// on the server that runs the test. Secrets are never stored in the item.
type AppEnv struct {
	Name      string `json:"name"`
	Value     string `json:"value,omitempty"`
	SecretEnv string `json:"secretEnv,omitempty"`
}

// AppTestSecretPrefix limits which server variables a test may read, so that
// an item can't reach the agent's own settings or the storage password.
const AppTestSecretPrefix = "BP_APPTEST_"

const (
	maxAppMounts   = 16
	maxAppTmpfs    = 8
	maxAppEnv      = 32
	maxAppStartSec = 900
	// MaxAppResponseMs is also the longest a single request may take.
	MaxAppResponseMs = 60000
)

var (
	appImageRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)
	appEnvNameRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	appSecretRe    = regexp.MustCompile(`^` + AppTestSecretPrefix + `[A-Z0-9_]+$`)
	appSecretish   = regexp.MustCompile(`(?i)pass|secret|token|key|credential|auth|private`)
	appURLPassword = regexp.MustCompile(`://[^/\s:@]+:[^/\s@]+@`)
	appWebPathRe   = regexp.MustCompile(`^/[!-~]*$`)
	appHostRe      = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)
	appInsidePath  = regexp.MustCompile(`^/[A-Za-z0-9._/-]*$`)
	appTemplateRe  = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
)

// appEnvSwitches are database image settings that only sound like secrets:
// their values are switches such as 1 or trust.
var appEnvSwitches = map[string]bool{"MARIADB_ALLOW_EMPTY_ROOT_PASSWORD": true, "MYSQL_ALLOW_EMPTY_PASSWORD": true,
	"MARIADB_RANDOM_ROOT_PASSWORD": true, "MYSQL_RANDOM_ROOT_PASSWORD": true, "POSTGRES_HOST_AUTH_METHOD": true}

// Validate checks an app test for an item of the given kind.
func (a *AppTest) Validate(kind string) error {
	switch kind {
	case "files", "docker", "import":
	default:
		return errors.New("the app test works on files and Docker apps; database items are already started and checked by their restore test")
	}
	if a.Template != "" && !appTemplateRe.MatchString(a.Template) {
		return errors.New("the app test's template name isn't valid")
	}
	if a.TemplateVersion < 0 {
		return errors.New("the app test's template version isn't valid")
	}
	if a.Port < 1 || a.Port > 65535 {
		return errors.New("the app test needs the port the app listens on inside its container")
	}
	if a.Path != "" && !appWebPathRe.MatchString(a.Path) {
		return errors.New("the app test's address must start with / and contain no spaces")
	}
	if a.Host != "" && !appHostRe.MatchString(a.Host) {
		return errors.New("the app test's Host header must be a plain host name")
	}
	if a.ExpectStatus != 0 && (a.ExpectStatus < 100 || a.ExpectStatus > 599) {
		return errors.New("the app test's expected status must be between 100 and 599")
	}
	if len(a.ExpectText) > 200 {
		return errors.New("the app test's expected text can be at most 200 characters")
	}
	if a.MaxResponseMs < 0 || a.MaxResponseMs > MaxAppResponseMs {
		return fmt.Errorf("the app test's response limit must be between 0 (no limit) and %d ms", MaxAppResponseMs)
	}
	if a.StartTimeoutSecs < 0 || a.StartTimeoutSecs > maxAppStartSec {
		return fmt.Errorf("the app test's start time must be between 0 (the default) and %d seconds", maxAppStartSec)
	}
	if err := a.App.validate("app", kind); err != nil {
		return err
	}
	if len(a.App.Mounts) == 0 {
		return errors.New("the app test needs at least one part of the backup to give the app")
	}
	if a.Database != nil {
		return a.Database.validate("database", kind)
	}
	return nil
}

func (c *AppContainer) validate(what, kind string) error {
	if len(c.Image) > 255 || !appImageRe.MatchString(c.Image) {
		return fmt.Errorf("the app test's %s image name isn't valid", what)
	}
	if len(c.Mounts) > maxAppMounts || len(c.Tmpfs) > maxAppTmpfs || len(c.Env) > maxAppEnv {
		return fmt.Errorf("the app test's %s has too many folders or settings", what)
	}
	seen := map[string]bool{}
	dest := func(p string) error {
		if err := validInsidePath(p); err != nil {
			return err
		}
		if seen[p] {
			return fmt.Errorf("the app test uses %s twice", p)
		}
		seen[p] = true
		return nil
	}
	for _, m := range c.Mounts {
		if err := validBackupPath(m.From, kind); err != nil {
			return err
		}
		if err := dest(m.To); err != nil {
			return err
		}
	}
	for _, p := range c.Tmpfs {
		if err := dest(p); err != nil {
			return err
		}
	}
	names := map[string]bool{}
	for _, e := range c.Env {
		if !appEnvNameRe.MatchString(e.Name) {
			return fmt.Errorf("%q isn't a valid setting name", e.Name)
		}
		if names[e.Name] {
			return fmt.Errorf("the setting %s appears twice", e.Name)
		}
		names[e.Name] = true
		if err := validAppEnv(e); err != nil {
			return err
		}
	}
	return nil
}

func validAppEnv(e AppEnv) error {
	if e.SecretEnv != "" {
		if e.Value != "" {
			return fmt.Errorf("%s can have a value or a reference to a secret, not both", e.Name)
		}
		if !appSecretRe.MatchString(e.SecretEnv) {
			return fmt.Errorf("%s must refer to a server variable whose name starts with %s", e.Name, AppTestSecretPrefix)
		}
		return nil
	}
	if strings.ContainsAny(e.Value, "\r\n\x00") || len(e.Value) > 4096 {
		return fmt.Errorf("the value of %s can't be used", e.Name)
	}
	if e.Value != "" && ((appSecretish.MatchString(e.Name) && !appEnvSwitches[e.Name]) || appURLPassword.MatchString(e.Value)) {
		return fmt.Errorf("%s looks like a secret, so it can't be saved here: set it on the server as %s… and refer to that instead", e.Name, AppTestSecretPrefix)
	}
	return nil
}

// validInsidePath checks a path inside a container. It goes into a Docker
// option, so only plain characters are allowed.
func validInsidePath(p string) error {
	if !appInsidePath.MatchString(p) || path.Clean(p) != p || len(p) > 200 {
		return fmt.Errorf("%q isn't a usable path inside the container", p)
	}
	if p == "/" {
		return errors.New("the app test can't replace the whole container")
	}
	for _, root := range []string{"/proc", "/sys", "/dev"} {
		if p == root || strings.HasPrefix(p, root+"/") {
			return fmt.Errorf("the app test can't use %s", p)
		}
	}
	return nil
}

// validBackupPath checks a path inside the backup: relative, canonical and
// safe to place in a Docker option. Where a Docker item keeps its Compose
// files and container settings can't be mounted at all.
func validBackupPath(p, kind string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, ",\"\\:\r\n\x00") || path.Clean(p) != p || len(p) > 1024 ||
		p == ".." || strings.HasPrefix(p, "../") {
		return fmt.Errorf("%q isn't a usable path inside the backup", p)
	}
	if kind == "docker" {
		for _, private := range []string{"docker/compose", "docker/containers"} {
			if p == "." || p == "docker" || p == private || strings.HasPrefix(p, private+"/") {
				return fmt.Errorf("the app test can't use %q: a Docker backup's Compose files and container settings are never given to the test", p)
			}
		}
	}
	return nil
}
