package source

import (
	"reflect"
	"testing"
)

func TestPutBackAllowed(t *testing.T) {
	for p, want := range map[string]bool{
		"/srv/app/data/index.html":      true,
		"/root/n8n/data/database.db":    true,
		"/etc/nginx/conf.d/site.conf":   true,
		"/etc/cron.d/backdoor":          false,
		"/etc/sudoers.d/x":              false,
		"/etc/passwd":                   false,
		"/etc/systemd/system/x.service": false,
		"/root/.ssh/authorized_keys":    false,
		"/home/me/.ssh/config":          false,
		"/home/me/.bashrc":              false,
		"/var/spool/cron/crontabs/root": false,
		"/usr/local/bin/tool":           false,
		"/srv/app/../../etc/cron.d/x":   false,
	} {
		if got := PutBackAllowed(p); got != want {
			t.Errorf("PutBackAllowed(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestAppMount(t *testing.T) {
	for src, want := range map[string]bool{
		"/srv/app/data":           true,
		"/home/me/site":           true,
		"/etc/nginx/conf.d":       true,
		"/var/run/docker.sock":    false,
		"/run/user/1000":          false,
		"/proc":                   false,
		"/etc/localtime":          false,
		"/var/lib/docker/volumes": false,
		"/opt/app/agent.sock":     false,
		"/":                       false,
		`C:\Users\me\site`:        false,
		"relative/path":           false,
	} {
		if got := AppMount(src); got != want {
			t.Errorf("AppMount(%q) = %v, want %v", src, got, want)
		}
	}
	if got := topLevel([]string{"/srv/app/data/uploads", "/srv/app/data", "/opt/x", "/srv/app/database"}); !reflect.DeepEqual(got, []string{"/opt/x", "/srv/app/data", "/srv/app/database"}) {
		t.Errorf("topLevel: %v", got)
	}
}
