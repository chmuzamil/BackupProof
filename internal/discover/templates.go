package discover

import (
	"path"
	"strings"

	"github.com/chmuzamil/backupproof/internal/source"
)

// AppTemplate prefills an app test. It is reviewable data: a template only
// supplies source.AppTest values, which have no way to name a command,
// privileges, a network or a port on the server, so it can't ask for more than
// the sandbox allows. A template holds no secrets, only the names of variables
// to set on the server. The person enabling it sees and can edit every value.
//
// Raise Version whenever what a template does changes: the version is recorded
// in the proof of each test that used it.
type AppTemplate struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
	// Kinds are the item types the template suits.
	Kinds []string `json:"kinds"`
	// Test prefills the app test. A blank From must be filled in by the person.
	Test source.AppTest `json:"test"`
	// ExpectPaths are files that must exist, relative to the app's first mount.
	ExpectPaths []string `json:"expectPaths,omitempty"`
	// Needs lists what the person must do or check before relying on it.
	Needs []string `json:"needs,omitempty"`
}

// Templates returns the built-in templates. Each call makes fresh values.
func Templates() []AppTemplate {
	return []AppTemplate{
		{
			Name: "wordpress", Version: 2, Title: "WordPress (Docker)",
			Summary: "The official WordPress image with its restored files read-only, and MariaDB or MySQL beside it on a throwaway copy of its restored database files. Passes when the sign-in page loads, the admin area asks for a sign-in, and the site's API answers from the restored database.",
			Kinds:   []string{"docker"},
			Test: source.AppTest{
				Template: "wordpress", TemplateVersion: 2,
				App: source.AppContainer{
					Image:  "wordpress:6-apache",
					Mounts: []source.AppDataMount{{To: "/var/www/html"}},
					Env: []source.AppEnv{
						{Name: "WORDPRESS_DB_HOST", Value: "127.0.0.1"},
						{Name: "WORDPRESS_DB_NAME", Value: "wordpress"},
						{Name: "WORDPRESS_DB_USER", Value: "wordpress"},
						{Name: "WORDPRESS_DB_PASSWORD", SecretEnv: source.AppTestSecretPrefix + "WORDPRESS_DB_PASSWORD"},
					},
				},
				Database: &source.AppContainer{
					Image:  "mariadb:11",
					Mounts: []source.AppDataMount{{To: "/var/lib/mysql", Writable: true}},
				},
				Port: 80, Path: "/wp-login.php", ExpectStatus: 200, ExpectText: "wp-submit", StartTimeoutSecs: 180,
				Checks: []source.AppCheck{
					// The admin area sends visitors to sign in: WordPress and its sessions work.
					{Name: "Admin asks for a sign-in", Path: "/wp-admin/", ExpectStatus: 302},
					// The REST API index is built from the restored options table.
					// rest_route works with and without pretty permalinks.
					{Name: "Site API answers from the database", Path: "/?rest_route=/", ExpectStatus: 200, ExpectText: "namespaces"},
				},
			},
			ExpectPaths: []string{"wp-includes/version.php", "wp-config.php"},
			Needs: []string{
				"Set " + source.AppTestSecretPrefix + "WORDPRESS_DB_PASSWORD on the server that runs the restore test, to the site's database password.",
				"Use the database and WordPress versions your site runs. A Docker backup must stop the containers so the database files are consistent.",
				"Correct the database name and user if yours differ.",
				"A site that forces HTTPS for logins redirects instead: expect status 302 and remove the expected text.",
				"To check uploaded media came back, add a check for a file you know exists, such as /wp-content/uploads/2025/01/logo.png.",
			},
		},
		{
			Name: "n8n", Version: 1, Title: "n8n (Docker)",
			Summary: "The official n8n image on a throwaway copy of its restored data folder, which holds its SQLite database, settings and encryption key. Passes when n8n has opened its restored database and its editor loads.",
			Kinds:   []string{"docker"},
			Test: source.AppTest{
				Template: "n8n", TemplateVersion: 1,
				App: source.AppContainer{
					Image:  "n8nio/n8n",
					Mounts: []source.AppDataMount{{To: "/home/node/.n8n", Writable: true}},
					Env: []source.AppEnv{
						{Name: "N8N_DIAGNOSTICS_ENABLED", Value: "false"},
						{Name: "N8N_PERSONALIZATION_ENABLED", Value: "false"},
					},
				},
				// readiness answers 200 only once n8n has opened and migrated its database.
				Port: 5678, Path: "/healthz/readiness", ExpectStatus: 200, ExpectText: "ok", StartTimeoutSecs: 240,
				Checks: []source.AppCheck{
					{Name: "Editor loads", Path: "/", ExpectStatus: 200, ExpectText: "n8n"},
				},
			},
			ExpectPaths: []string{"database.sqlite", "config"},
			Needs: []string{
				"Use the n8n version your server runs (for example n8nio/n8n:1.80.0): a newer one may change the database when it starts.",
				"This is for n8n's built-in SQLite database. If your n8n uses PostgreSQL, back up that database as its own item; its restore test checks it.",
			},
		},
		{
			Name: "generic-http", Version: 1, Title: "Any web app",
			Summary: "Starts the image you name with restored data read-only, and passes when it answers with status 200. You fill in the image, the data and the port.",
			Kinds:   []string{"files", "docker", "import"},
			Test: source.AppTest{
				Template: "generic-http", TemplateVersion: 1,
				App:  source.AppContainer{Mounts: []source.AppDataMount{{To: "/data"}}},
				Port: 8080, Path: "/", ExpectStatus: 200,
			},
			Needs: []string{
				"Name the image, the parts of the backup it needs and the port it listens on.",
				"Point the address at a page that needs the app's data, so a passed test means the data works.",
			},
		},
	}
}

// KnownTemplate reports whether a saved item names a real template version.
func KnownTemplate(name string, version int) bool {
	for _, t := range Templates() {
		if t.Name == name {
			return version >= 1 && version <= t.Version
		}
	}
	return false
}

// TemplateHint is what discovery found that can fill in a template: image names
// and paths in the backup. It never holds a container's environment.
type TemplateHint struct {
	Template      string `json:"template"`
	AppImage      string `json:"appImage,omitempty"`
	AppFrom       string `json:"appFrom,omitempty"`
	DatabaseImage string `json:"databaseImage,omitempty"`
	DatabaseFrom  string `json:"databaseFrom,omitempty"`
}

// containerFacts is the part of a container that hints are worked out from.
// It leaves out the container's environment on purpose.
type containerFacts struct {
	Image  string
	Mounts []mountFacts
}

type mountFacts struct{ Type, Name, Source, Destination string }

// templateHint recognises a Docker app a template fits.
func templateHint(containers []containerFacts) *TemplateHint {
	var hint *TemplateHint
	for _, c := range containers {
		img := strings.ToLower(c.Image)
		if strings.Contains(img, "n8nio/n8n") || strings.HasPrefix(img, "n8n:") || img == "n8n" {
			return &TemplateHint{Template: "n8n", AppImage: c.Image, AppFrom: backupPath(c.Mounts, "/home/node/.n8n")}
		}
	}
	for _, c := range containers {
		img := strings.ToLower(c.Image)
		if strings.Contains(img, "wordpress") && !strings.Contains(img, ":cli") {
			if hint == nil {
				hint = &TemplateHint{Template: "wordpress"}
			}
			hint.AppImage, hint.AppFrom = c.Image, backupPath(c.Mounts, "/var/www/html")
		}
	}
	if hint == nil {
		return nil
	}
	for _, c := range containers {
		img := strings.ToLower(c.Image)
		if strings.Contains(img, "mysql") || strings.Contains(img, "mariadb") || strings.Contains(img, "percona") {
			hint.DatabaseImage, hint.DatabaseFrom = c.Image, backupPath(c.Mounts, "/var/lib/mysql")
		}
	}
	return hint
}

// backupPath is where a container folder's data is kept inside a Docker
// backup (see internal/source/docker.go), or "" if it isn't part of one.
func backupPath(mounts []mountFacts, destination string) string {
	for _, m := range mounts {
		if m.Destination != destination {
			continue
		}
		switch {
		case m.Type == "volume" && m.Name != "":
			return "docker/volumes/" + m.Name
		case m.Type == "bind" && source.AppMount(m.Source):
			return "docker/mounts" + path.Clean(m.Source)
		}
	}
	return ""
}
