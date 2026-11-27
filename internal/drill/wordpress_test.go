package drill

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/discover"
	"github.com/chmuzamil/backupproof/internal/source"
)

// The WordPress template against real WordPress sites in four setups that
// change what its checks see. Each site is built with Docker (WordPress,
// MariaDB, WP-CLI), stopped as a backup would, and its files are app-tested
// as a restored backup. Needs Docker and the network to pull images and a
// plugin, so it runs only with BP_WORDPRESS_TEST=1 (CI: the wordpress job).
//
//	BP_WORDPRESS_TEST=1 go test ./internal/drill -run TestWordPressSetups -v
func TestWordPressSetups(t *testing.T) {
	if os.Getenv("BP_WORDPRESS_TEST") == "" {
		t.Skip("set BP_WORDPRESS_TEST=1 to build real WordPress sites with Docker")
	}
	tpl := wordpressTemplate(t)
	for _, s := range wordpressSetups {
		t.Run(s.name, func(t *testing.T) {
			dir := buildWordPress(t, s)
			for _, v := range s.variants(tpl) {
				at := v.test
				res, _ := runRealApp(t, dir, &at)
				passed := true
				var lines []string
				for _, c := range res.Checks {
					if strings.HasPrefix(c.Name, "app-") {
						mark := "PASS"
						if !c.Passed {
							mark, passed = "FAIL", false
						}
						lines = append(lines, fmt.Sprintf("    [%s] %s: %s", mark, c.Name, c.Detail))
					}
				}
				verdict := "passes"
				if !passed {
					verdict = "FAILS"
				}
				t.Logf("%s / %s: %s\n%s", s.name, v.name, verdict, strings.Join(lines, "\n"))
				if v.mustPass && !passed {
					t.Errorf("%s / %s should pass", s.name, v.name)
				}
				if v.mustFail && passed {
					t.Errorf("%s / %s should fail: the template would miss the problem", s.name, v.name)
				}
			}
		})
	}
}

type wpSetup struct {
	name     string
	siteURL  string
	commands [][]string // WP-CLI commands after install
	files    map[string]string
	config   [][]string // wp config set NAME VALUE --raw
	variants func(tpl source.AppTest) []wpVariant
}

type wpVariant struct {
	name               string
	test               source.AppTest
	mustPass, mustFail bool
}

func wordpressTemplate(t *testing.T) source.AppTest {
	for _, tp := range discover.Templates() {
		if tp.Name == "wordpress" {
			at := tp.Test
			at.App.Mounts = []source.AppDataMount{{From: "wp", To: "/var/www/html"}}
			db := *at.Database
			db.Mounts = []source.AppDataMount{{From: "db", To: "/var/lib/mysql", Writable: true}}
			at.Database = &db
			return at
		}
	}
	t.Fatal("no wordpress template")
	return source.AppTest{}
}

// withOptions applies the template's options as the dashboard form does.
func withOptions(t *testing.T, opts map[string]string) source.AppTest {
	t.Helper()
	for _, tp := range discover.Templates() {
		if tp.Name == "wordpress" {
			at, err := tp.Apply(opts)
			if err != nil {
				t.Fatal(err)
			}
			at.App.Mounts = []source.AppDataMount{{From: "wp", To: "/var/www/html"}}
			db := *at.Database
			db.Mounts = []source.AppDataMount{{From: "db", To: "/var/lib/mysql", Writable: true}}
			at.Database = &db
			return at
		}
	}
	t.Fatal("no wordpress template")
	return source.AppTest{}
}

var wordpressSetups = []wpSetup{
	{
		name:    "plain",
		siteURL: "http://site.test",
		variants: func(tpl source.AppTest) []wpVariant {
			return []wpVariant{{name: "template defaults", test: tpl, mustPass: true}}
		},
	},
	{
		// WPS Hide Login: /wp-login.php and, signed out, /wp-admin/ answer
		// 404; the sign-in page is at /secret-login.
		name:     "login moved by a security plugin",
		siteURL:  "http://site.test",
		commands: [][]string{{"plugin", "install", "wps-hide-login", "--activate"}, {"option", "update", "whl_page", "secret-login"}},
		variants: func(tpl source.AppTest) []wpVariant {
			return []wpVariant{
				{name: "template defaults", test: tpl},
				{name: "sign-in page set to /secret-login/", test: withOptionsT("loginPath", "/secret-login/"), mustPass: true},
			}
		},
	},
	{
		// A must-use plugin refusing the REST API to visitors who aren't
		// signed in, as "Disable REST API" plugins do.
		name:    "REST API off",
		siteURL: "http://site.test",
		files: map[string]string{"wp-content/mu-plugins/no-rest.php": `<?php
add_filter('rest_authentication_errors', function ($r) {
	return is_user_logged_in() ? $r : new WP_Error('rest_disabled', 'The REST API is off.', ['status' => 401]);
});
`},
		variants: func(tpl source.AppTest) []wpVariant {
			return []wpVariant{
				{name: "template defaults", test: tpl},
				{name: "site API check off", test: withOptionsT("restCheck", "off"), mustPass: true},
			}
		},
	},
	{
		// The site address is https:// and sign-ins are forced to HTTPS,
		// behind a proxy that sets X-Forwarded-Proto (which the official
		// image's wp-config.php trusts).
		name:    "HTTPS forced",
		siteURL: "https://site.test",
		config:  [][]string{{"FORCE_SSL_ADMIN", "true"}},
		variants: func(tpl source.AppTest) []wpVariant {
			noFollow := tpl
			noFollow.FollowRedirects = false
			return []wpVariant{
				{name: "template defaults", test: tpl, mustPass: tpl.FollowRedirects},
				{name: "without following redirects", test: noFollow},
				{name: "following redirects to HTTPS", test: withOptionsT("followHttps", "on"), mustPass: true},
			}
		},
	},
}

// withOptionsT is withOptions for the setup table, which has no *testing.T.
var withOptionsT func(key, value string) source.AppTest

func buildWordPress(t *testing.T, s wpSetup) string {
	t.Helper()
	withOptionsT = func(k, v string) source.AppTest { return withOptions(t, map[string]string{k: v}) }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dir := t.TempDir()
	wp, db := filepath.Join(dir, "wp"), filepath.Join(dir, "db")
	for _, d := range []string{wp, db} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	id := make([]byte, 4)
	_, _ = rand.Read(id)
	n := "bpwp-" + hex.EncodeToString(id)
	dbPass := "pw-" + hex.EncodeToString(id)
	t.Setenv(source.AppTestSecretPrefix+"WORDPRESS_DB_PASSWORD", dbPass)
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", "-v", n+"-db", n+"-wp").Run()
		_ = exec.Command("docker", "network", "rm", n).Run()
		// Files written by the containers' own users: remove them from a container.
		_ = exec.Command("docker", "run", "--rm", "-v", dir+":/d", "alpine:3", "sh", "-c", "rm -rf /d/wp /d/db").Run()
	})
	run("network", "create", n)
	dbEnv := []string{"-e", "MARIADB_ROOT_PASSWORD=" + dbPass, "-e", "MARIADB_DATABASE=wordpress", "-e", "MARIADB_USER=wordpress", "-e", "MARIADB_PASSWORD=" + dbPass}
	run(append(append([]string{"run", "-d", "--name", n + "-db", "--network", n, "--network-alias", "db", "-v", db + ":/var/lib/mysql"}, dbEnv...), "mariadb:11")...)
	wpEnv := []string{"-e", "WORDPRESS_DB_HOST=db", "-e", "WORDPRESS_DB_USER=wordpress", "-e", "WORDPRESS_DB_PASSWORD=" + dbPass, "-e", "WORDPRESS_DB_NAME=wordpress"}
	run(append(append([]string{"run", "-d", "--name", n + "-wp", "--network", n, "-v", wp + ":/var/www/html"}, wpEnv...), "wordpress:6-apache")...)
	waitFor(t, ctx, "the WordPress files", func() bool { _, err := os.Stat(filepath.Join(wp, "wp-config.php")); return err == nil })
	waitFor(t, ctx, "the database", func() bool {
		return exec.CommandContext(ctx, "docker", "exec", n+"-db", "healthcheck.sh", "--connect", "--innodb_initialized").Run() == nil
	})
	cli := func(args ...string) string {
		t.Helper()
		base := append([]string{"run", "--rm", "--network", n, "--volumes-from", n + "-wp", "--user", "33:33"}, wpEnv...)
		return run(append(append(base, "wordpress:cli", "wp"), args...)...)
	}
	cli("core", "install", "--url="+s.siteURL, "--title=Restore test", "--admin_user=admin",
		"--admin_password=pw-"+hex.EncodeToString(id)+"-admin", "--admin_email=admin@site.test", "--skip-email")
	for _, c := range s.config {
		cli("config", "set", c[0], c[1], "--raw")
	}
	for _, c := range s.commands {
		cli(c...)
	}
	for name, body := range s.files {
		cmd := exec.CommandContext(ctx, "docker", "exec", "-i", "--user", "33:33", n+"-wp", "sh", "-c", "mkdir -p \"$(dirname \"$1\")\" && cat > \"$1\"", "sh", "/var/www/html/"+name)
		cmd.Stdin = strings.NewReader(body)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("writing %s: %v\n%s", name, err, out)
		}
	}
	// Stopped, as a backup of a Docker app stops it, so the files are consistent.
	run("stop", "-t", "60", n+"-wp", n+"-db")
	return dir
}

func waitFor(t *testing.T, ctx context.Context, what string, ok func() bool) {
	t.Helper()
	for !ok() {
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v", what, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// runRealApp runs an app test against real Docker.
func runRealApp(t *testing.T, dir string, at *source.AppTest) (*Result, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	res := &Result{}
	parent := t.TempDir()
	appChecks(ctx, &runner{res: res, log: func(string, ...any) {}}, dir, source.Spec{Name: "wordpress", Kind: "docker", Drill: source.DrillSpec{AppTest: at}}, parent)
	return res, parent
}
