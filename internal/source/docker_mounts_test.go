package source

import (
	"reflect"
	"testing"
)

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
