package source

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func goodAppTest() AppTest {
	return AppTest{
		App:  AppContainer{Image: "wordpress:6-apache", Mounts: []AppDataMount{{From: "docker/volumes/wp", To: "/var/www/html"}}},
		Port: 80,
	}
}

func TestAppTestValidate(t *testing.T) {
	if a := goodAppTest(); a.Validate("docker") != nil {
		t.Fatal(a.Validate("docker"))
	}
	bad := map[string]func(*AppTest){
		"no port":                func(a *AppTest) { a.Port = 0 },
		"huge port":              func(a *AppTest) { a.Port = 70000 },
		"no image":               func(a *AppTest) { a.App.Image = "" },
		"option as image":        func(a *AppTest) { a.App.Image = "--privileged" },
		"image with space":       func(a *AppTest) { a.App.Image = "nginx --privileged" },
		"no data":                func(a *AppTest) { a.App.Mounts = nil },
		"escaping path":          func(a *AppTest) { a.App.Mounts[0].From = "../../etc" },
		"absolute path":          func(a *AppTest) { a.App.Mounts[0].From = "/etc" },
		"mount option injection": func(a *AppTest) { a.App.Mounts[0].From = "x,dst=/host" },
		"colon":                  func(a *AppTest) { a.App.Mounts[0].From = "x:/host" },
		"compose files":          func(a *AppTest) { a.App.Mounts[0].From = "docker/compose/shop" },
		"container settings":     func(a *AppTest) { a.App.Mounts[0].From = "docker/containers" },
		"whole docker backup":    func(a *AppTest) { a.App.Mounts[0].From = "docker" },
		"whole backup":           func(a *AppTest) { a.App.Mounts[0].From = "." },
		"mount on root":          func(a *AppTest) { a.App.Mounts[0].To = "/" },
		"mount on proc":          func(a *AppTest) { a.App.Mounts[0].To = "/proc/sys" },
		"relative target":        func(a *AppTest) { a.App.Mounts[0].To = "var/www" },
		"target with comma":      func(a *AppTest) { a.App.Mounts[0].To = "/a,readonly=false" },
		"tmpfs on sys":           func(a *AppTest) { a.App.Tmpfs = []string{"/sys"} },
		"tmpfs options":          func(a *AppTest) { a.App.Tmpfs = []string{"/x:rw,exec"} },
		"same target twice":      func(a *AppTest) { a.App.Tmpfs = []string{"/var/www/html"} },
		"web path with space":    func(a *AppTest) { a.Path = "/a b" },
		"web path no slash":      func(a *AppTest) { a.Path = "a" },
		"host injection":         func(a *AppTest) { a.Host = "x\r\nEvil: 1" },
		"status":                 func(a *AppTest) { a.ExpectStatus = 1000 },
		"negative limit":         func(a *AppTest) { a.MaxResponseMs = -1 },
		"long limit":             func(a *AppTest) { a.MaxResponseMs = MaxAppResponseMs + 1 },
		"long start":             func(a *AppTest) { a.StartTimeoutSecs = maxAppStartSec + 1 },
		"long text":              func(a *AppTest) { a.ExpectText = strings.Repeat("x", 201) },
		"template name":          func(a *AppTest) { a.Template = "../x" },
		"env name":               func(a *AppTest) { a.App.Env = []AppEnv{{Name: "A=B"}} },
		"env twice":              func(a *AppTest) { a.App.Env = []AppEnv{{Name: "A"}, {Name: "A"}} },
		"env newline":            func(a *AppTest) { a.App.Env = []AppEnv{{Name: "A", Value: "x\nB=y"}} },
		"literal password":       func(a *AppTest) { a.App.Env = []AppEnv{{Name: "WORDPRESS_DB_PASSWORD", Value: "hunter2"}} },
		"literal token":          func(a *AppTest) { a.App.Env = []AppEnv{{Name: "API_TOKEN", Value: "abc"}} },
		"password in a URL":      func(a *AppTest) { a.App.Env = []AppEnv{{Name: "DATABASE_URL", Value: "mysql://u:hunter2@db/x"}} },
		"value and reference":    func(a *AppTest) { a.App.Env = []AppEnv{{Name: "A", Value: "x", SecretEnv: "BP_APPTEST_A"}} },
		"reference to the password": func(a *AppTest) {
			a.App.Env = []AppEnv{{Name: "X", SecretEnv: "BP_PASSWORD"}}
		},
		"reference to anything": func(a *AppTest) { a.App.Env = []AppEnv{{Name: "X", SecretEnv: "HOME"}} },
		"bad database": func(a *AppTest) {
			a.Database = &AppContainer{Image: "--network=host"}
		},
	}
	for name, change := range bad {
		a := goodAppTest()
		change(&a)
		if err := a.Validate("docker"); err == nil {
			t.Errorf("%s: should be refused", name)
		}
	}
	a := goodAppTest()
	if err := a.Validate("postgres"); err == nil {
		t.Error("a database item doesn't take an app test")
	}
	// Outside Docker backups the whole restored tree is a fair thing to mount.
	a.App.Mounts[0].From = "."
	if err := a.Validate("files"); err != nil {
		t.Error(err)
	}
	// References, and empty values for anything, are fine.
	a = goodAppTest()
	a.App.Env = []AppEnv{{Name: "WORDPRESS_DB_PASSWORD", SecretEnv: "BP_APPTEST_WP_DB"}, {Name: "SECRET_FROM_NOWHERE"}, {Name: "WORDPRESS_DB_HOST", Value: "127.0.0.1"}}
	if err := a.Validate("docker"); err != nil {
		t.Error(err)
	}
}

// Spec.Validate only looks at the app test when there is one.
func TestSpecValidatesAppTest(t *testing.T) {
	s := Spec{Name: "x", Kind: "files", Paths: []string{"/a"}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := goodAppTest()
	bad.Port = 0
	s.Drill.AppTest = &bad
	if err := s.Validate(); err == nil {
		t.Fatal("an invalid app test must fail the item")
	}
}

// Old items have no appTest key at all, and an item without one stays that way.
func TestAppTestAbsentFromOldItems(t *testing.T) {
	var s Spec
	if err := json.Unmarshal([]byte(`{"name":"x","kind":"files","paths":["/a"],"drill":{"minFiles":1}}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.Drill.AppTest != nil {
		t.Error("an old item must have no app test")
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "appTest") {
		t.Errorf("an item without an app test changed: %s", b)
	}
}

// A new field here widens what a saved item or template can ask Docker for.
// Adding one must be a decision made while looking at this list.
func TestAppTestHasNoPermissionFields(t *testing.T) {
	for _, c := range []struct {
		typ  reflect.Type
		want []string
	}{
		{reflect.TypeFor[AppTest](), []string{"Template", "TemplateVersion", "App", "Database", "Port", "Path", "Host", "ExpectStatus", "ExpectText", "MaxResponseMs", "StartTimeoutSecs"}},
		{reflect.TypeFor[AppContainer](), []string{"Image", "Mounts", "Tmpfs", "Env"}},
		{reflect.TypeFor[AppDataMount](), []string{"From", "To", "Writable"}},
		{reflect.TypeFor[AppEnv](), []string{"Name", "Value", "SecretEnv"}},
	} {
		var got []string
		for f := range c.typ.Fields() {
			got = append(got, f.Name)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s has fields %v, want %v", c.typ.Name(), got, c.want)
		}
	}
}

// Unknown options in a saved item have no effect.
func TestAppTestIgnoresExtraJSON(t *testing.T) {
	var a AppTest
	if err := json.Unmarshal([]byte(`{"port":80,"privileged":true,"capAdd":["SYS_ADMIN"],"network":"host","command":"sh","app":{"image":"x","entrypoint":"sh","volumes":["/:/host"]}}`), &a); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(a)
	for _, w := range []string{"privileged", "SYS_ADMIN", "host", "command", "entrypoint", "volumes"} {
		if strings.Contains(string(b), w) {
			t.Errorf("%s survived: %s", w, b)
		}
	}
}
