// Package discover finds things worth protecting on a machine (common
// folders, websites, WordPress installs, databases in Docker or on standard
// ports) and places to store backups (extra disks), so the dashboard can
// offer one-click choices instead of asking users to type paths.
package discover

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type Item struct {
	Kind  string `json:"kind"` // folder | website | wordpress
	Label string `json:"label"`
	Path  string `json:"path"`
	// WPConfig is set for WordPress sites (their database is suggested too).
	WPConfig string `json:"wpConfig,omitempty"`
}

type Database struct {
	Kind      string `json:"kind"` // postgres | mysql | mongodb
	Label     string `json:"label"`
	Container string `json:"container,omitempty"`
	Host      string `json:"host,omitempty"`
	Port      int    `json:"port,omitempty"`
	WPConfig  string `json:"wpConfig,omitempty"`
	// NeedsLogin is true when the user must enter a username and password.
	NeedsLogin bool `json:"needsLogin"`
}

type Drive struct {
	Path  string `json:"path"`
	Label string `json:"label"`
	Free  uint64 `json:"free"`
	Total uint64 `json:"total"`
}

type Inventory struct {
	Collected time.Time  `json:"collected"`
	OS        string     `json:"os"`
	Hostname  string     `json:"hostname"`
	Docker    bool       `json:"docker"`
	Items     []Item     `json:"items"`
	Databases []Database `json:"databases"`
	Drives    []Drive    `json:"drives"`
}

func Collect(ctx context.Context) *Inventory {
	host, _ := os.Hostname()
	inv := &Inventory{Collected: time.Now().UTC(), OS: runtime.GOOS, Hostname: host, Items: []Item{}, Databases: []Database{}, Drives: []Drive{}}
	inv.collectFolders()
	inv.collectWebsites()
	inv.collectDocker(ctx)
	inv.collectPorts()
	inv.Drives = drives()
	return inv
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func (inv *Inventory) add(kind, label, path string) {
	for _, it := range inv.Items {
		if it.Path == path {
			return
		}
	}
	inv.Items = append(inv.Items, Item{Kind: kind, Label: label, Path: path})
}

func (inv *Inventory) collectFolders() {
	if home, err := os.UserHomeDir(); err == nil {
		for _, name := range []string{"Documents", "Desktop", "Pictures", "Downloads", "Videos", "Music"} {
			if p := filepath.Join(home, name); isDir(p) {
				inv.add("folder", name, p)
			}
		}
	}
	if runtime.GOOS == "windows" {
		return
	}
	for _, base := range []string{"/srv", "/opt"} {
		entries, _ := os.ReadDir(base)
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && e.Name() != "containerd" {
				inv.add("folder", "App data: "+e.Name(), filepath.Join(base, e.Name()))
			}
		}
	}
	entries, _ := os.ReadDir("/home")
	for _, e := range entries {
		if e.IsDir() {
			inv.add("folder", "Home of "+e.Name(), filepath.Join("/home", e.Name()))
		}
	}
	if isDir("/etc") {
		inv.add("folder", "System settings (/etc)", "/etc")
	}
}

func (inv *Inventory) collectWebsites() {
	roots := []string{"/var/www", "/usr/share/nginx/html", `C:\inetpub\wwwroot`, `C:\xampp\htdocs`}
	for _, root := range roots {
		if !isDir(root) {
			continue
		}
		candidates := []string{root}
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if e.IsDir() {
				candidates = append(candidates, filepath.Join(root, e.Name()))
			}
		}
		for _, dir := range candidates {
			for _, docroot := range []string{dir, filepath.Join(dir, "public_html"), filepath.Join(dir, "htdocs")} {
				wp := filepath.Join(docroot, "wp-config.php")
				if _, err := os.Stat(wp); err == nil {
					name := filepath.Base(dir)
					inv.Items = append(inv.Items, Item{Kind: "wordpress", Label: "WordPress site: " + name, Path: dir, WPConfig: wp})
					inv.Databases = append(inv.Databases, Database{Kind: "mysql", Label: "WordPress database: " + name, WPConfig: wp})
					goto next
				}
			}
			if dir != root {
				inv.add("website", "Website: "+filepath.Base(dir), dir)
			}
		next:
		}
	}
}

func (inv *Inventory) collectDocker(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "--format", "{{json .}}").Output()
	if err != nil {
		return
	}
	inv.Docker = true
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var c struct{ Names, Image string }
		if json.Unmarshal([]byte(line), &c) != nil {
			continue
		}
		img := strings.ToLower(c.Image)
		kind := ""
		switch {
		case strings.Contains(img, "postgres") || strings.Contains(img, "postgis") || strings.Contains(img, "timescale"):
			kind = "postgres"
		case strings.Contains(img, "mysql") || strings.Contains(img, "mariadb") || strings.Contains(img, "percona"):
			kind = "mysql"
		case strings.Contains(img, "mongo"):
			kind = "mongodb"
		}
		if kind != "" {
			inv.Databases = append(inv.Databases, Database{Kind: kind, Label: niceKind(kind) + " in Docker: " + c.Names, Container: c.Names})
		}
	}
}

func niceKind(k string) string {
	return map[string]string{"postgres": "PostgreSQL", "mysql": "MySQL/MariaDB", "mongodb": "MongoDB"}[k]
}

func (inv *Inventory) collectPorts() {
	for _, p := range []struct {
		kind string
		port int
	}{{"postgres", 5432}, {"mysql", 3306}, {"mongodb", 27017}} {
		// Skip ports already explained by a Docker container we found.
		dup := false
		for _, d := range inv.Databases {
			if d.Kind == p.kind && d.Container != "" {
				dup = true
			}
		}
		if dup {
			continue
		}
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(p.port)), 300*time.Millisecond)
		if err != nil {
			continue
		}
		conn.Close()
		inv.Databases = append(inv.Databases, Database{Kind: p.kind, Label: niceKind(p.kind) + " on this server (port " + itoa(p.port) + ")", Host: "127.0.0.1", Port: p.port, NeedsLogin: true})
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func sortDrives(d []Drive) []Drive {
	sort.Slice(d, func(i, j int) bool { return d[i].Path < d[j].Path })
	return d
}
