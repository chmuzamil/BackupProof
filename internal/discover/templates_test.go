package discover

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/chmuzamil/backupproof/internal/source"
)

// filled completes the blanks a person fills in, so a template can be validated.
func filled(t AppTemplate) source.AppTest {
	at := t.Test
	if at.App.Image == "" {
		at.App.Image = "example/app:1"
	}
	for i := range at.App.Mounts {
		if at.App.Mounts[i].From == "" {
			at.App.Mounts[i].From = "docker/volumes/data"
		}
	}
	if at.Database != nil {
		for i := range at.Database.Mounts {
			if at.Database.Mounts[i].From == "" {
				at.Database.Mounts[i].From = "docker/volumes/db"
			}
		}
	}
	return at
}

func TestTemplatesAreValid(t *testing.T) {
	seen := map[string]bool{}
	for _, tpl := range Templates() {
		if seen[tpl.Name] {
			t.Errorf("%s appears twice", tpl.Name)
		}
		seen[tpl.Name] = true
		if tpl.Test.Template != tpl.Name || tpl.Test.TemplateVersion != tpl.Version || tpl.Version < 1 {
			t.Errorf("%s records %s v%d, but is version %d", tpl.Name, tpl.Test.Template, tpl.Test.TemplateVersion, tpl.Version)
		}
		at := filled(tpl)
		for _, k := range tpl.Kinds {
			if err := at.Validate(k); err != nil {
				t.Errorf("%s for %s: %v", tpl.Name, k, err)
			}
		}
		if !KnownTemplate(tpl.Name, tpl.Version) {
			t.Errorf("%s isn't known", tpl.Name)
		}
	}
	if !seen["wordpress"] || !seen["generic-http"] {
		t.Errorf("missing templates: %v", seen)
	}
}

func TestKnownTemplate(t *testing.T) {
	for _, c := range []struct {
		name    string
		version int
	}{{"wordpress", 0}, {"wordpress", 99}, {"nonsense", 1}, {"", 1}} {
		if KnownTemplate(c.name, c.version) {
			t.Errorf("%q v%d should be unknown", c.name, c.version)
		}
	}
}

// A template is data for a type with no permission fields (the field list is
// pinned in internal/source), so this checks only what templates carry.
func TestTemplatesCarryNoSecretsOrCommands(t *testing.T) {
	for _, tpl := range Templates() {
		containers := []source.AppContainer{tpl.Test.App}
		if tpl.Test.Database != nil {
			containers = append(containers, *tpl.Test.Database)
		}
		for _, c := range containers {
			for _, e := range c.Env {
				if strings.Contains(strings.ToLower(e.Name), "password") && (e.Value != "" || e.SecretEnv == "") {
					t.Errorf("%s: %s must be a reference", tpl.Name, e.Name)
				}
			}
			for _, m := range c.Mounts {
				if m.From != "" {
					t.Errorf("%s: %s should be filled in by the person", tpl.Name, m.From)
				}
			}
		}
		b, err := json.Marshal(tpl.Test)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"privileged", "capAdd", "entrypoint", `"command"`, "network"} {
			if strings.Contains(string(b), banned) {
				t.Errorf("%s has %s: %s", tpl.Name, banned, b)
			}
		}
	}
}

func TestTemplatesAreCopies(t *testing.T) {
	a := Templates()
	a[0].Test.App.Image = "changed"
	a[0].Test.App.Env[0].Value = "changed"
	if b := Templates(); b[0].Test.App.Image == "changed" || b[0].Test.App.Env[0].Value == "changed" {
		t.Error("the built-in templates can be changed")
	}
}

func TestTemplateHintFindsWordPress(t *testing.T) {
	hint := templateHint([]containerFacts{
		{Image: "wordpress:6.8-apache", Mounts: []mountFacts{{Type: "volume", Name: "shop_wp", Destination: "/var/www/html"}}},
		{Image: "mariadb:11.4", Mounts: []mountFacts{{Type: "volume", Name: "shop_db", Destination: "/var/lib/mysql"}}},
	})
	want := TemplateHint{Template: "wordpress", AppImage: "wordpress:6.8-apache", AppFrom: "docker/volumes/shop_wp", DatabaseImage: "mariadb:11.4", DatabaseFrom: "docker/volumes/shop_db"}
	if hint == nil || *hint != want {
		t.Fatalf("hint = %+v", hint)
	}
}

func TestTemplateHintCases(t *testing.T) {
	if h := templateHint([]containerFacts{{Image: "nginx:1"}, {Image: "mariadb:11"}}); h != nil {
		t.Errorf("no WordPress here: %+v", h)
	}
	if h := templateHint([]containerFacts{{Image: "wordpress:cli"}}); h != nil {
		t.Errorf("the CLI image isn't the site: %+v", h)
	}
	h := templateHint([]containerFacts{{Image: "wordpress:6", Mounts: []mountFacts{{Type: "bind", Source: "/srv/site//html/", Destination: "/var/www/html"}}}})
	if h == nil || h.AppFrom != "docker/mounts/srv/site/html" {
		t.Errorf("bind mount: %+v", h)
	}
	if h := templateHint([]containerFacts{{Image: "wordpress:6", Mounts: []mountFacts{{Type: "bind", Source: "/proc/x", Destination: "/var/www/html"}}}}); h == nil || h.AppFrom != "" {
		t.Errorf("a system path isn't in a backup: %+v", h)
	}
}

// Docker lists passwords in a container's settings. Nothing about them may
// reach what discovery reports.
func TestHintHasNoEnvironment(t *testing.T) {
	b, err := json.Marshal(TemplateHint{Template: "wordpress"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(b)), "env") {
		t.Errorf("hint has an environment field: %s", b)
	}
}
