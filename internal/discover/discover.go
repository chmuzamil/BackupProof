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
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/chmuzamil/backupproof/internal/source"
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
	// Docker apps (Compose projects and containers started on their own) and
	// named volumes found on this server.
	DockerApps    []DockerApp    `json:"dockerApps"`
	DockerVolumes []DockerVolume `json:"dockerVolumes"`
}

type DockerApp struct {
	Name       string   `json:"name"`
	Containers []string `json:"containers"`
	Volumes    []string `json:"volumes"`
	// Mounts are folders and files on this server mounted into the containers.
	Mounts  []string `json:"mounts"`
	Running bool     `json:"running"`
	// Standalone is a container started without Compose (docker run).
	Standalone bool `json:"standalone"`
	// Database is true when one of its containers runs a database image.
	Database bool `json:"database"`
}

type DockerVolume struct {
	Name   string   `json:"name"`
	UsedBy []string `json:"usedBy"`
}

func Collect(ctx context.Context) *Inventory {
	host, _ := os.Hostname()
	inv := &Inventory{Collected: time.Now().UTC(), OS: runtime.GOOS, Hostname: host, Items: []Item{}, Databases: []Database{}, Drives: []Drive{}, DockerApps: []DockerApp{}, DockerVolumes: []DockerVolume{}}
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
	inv.collectDockerApps(ctx)
}

// collectDockerApps groups containers into Compose projects (a container
// started on its own is an app by itself) and lists named volumes with the
// containers that use them.
func (inv *Inventory) collectDockerApps(ctx context.Context) {
	ids, err := exec.CommandContext(ctx, "docker", "ps", "-aq").Output()
	if err != nil {
		return
	}
	apps := map[string]*DockerApp{}
	used := map[string][]string{}
	if f := strings.Fields(string(ids)); len(f) > 0 {
		out, err := exec.CommandContext(ctx, "docker", append([]string{"inspect"}, f...)...).Output()
		if err != nil {
			return
		}
		var cs []struct {
			Name   string
			Mounts []struct{ Type, Name, Source string }
			Config struct {
				Image  string
				Labels map[string]string
			}
			State struct{ Running bool }
		}
		if json.Unmarshal(out, &cs) != nil {
			return
		}
		for _, c := range cs {
			name := strings.TrimPrefix(c.Name, "/")
			var vols, mounts []string
			for _, m := range c.Mounts {
				if m.Type == "volume" && m.Name != "" {
					vols = append(vols, m.Name)
					used[m.Name] = append(used[m.Name], name)
				}
				if m.Type == "bind" && source.AppMount(m.Source) {
					mounts = append(mounts, path.Clean(m.Source))
				}
			}
			proj := c.Config.Labels["com.docker.compose.project"]
			key := "compose:" + proj
			if proj == "" {
				key = "container:" + name
			}
			a := apps[key]
			if a == nil {
				a = &DockerApp{Name: proj, Containers: []string{}, Volumes: []string{}, Mounts: []string{}, Standalone: proj == ""}
				if proj == "" {
					a.Name = name
				}
				apps[key] = a
			}
			a.Containers = append(a.Containers, name)
			a.Volumes = append(a.Volumes, vols...)
			a.Mounts = append(a.Mounts, mounts...)
			a.Running = a.Running || c.State.Running
			img := strings.ToLower(c.Config.Image)
			for _, db := range []string{"postgres", "postgis", "mysql", "mariadb", "mongo", "redis", "timescale"} {
				if strings.Contains(img, db) {
					a.Database = true
				}
			}
		}
	}
	for _, a := range apps {
		sort.Strings(a.Containers)
		a.Volumes = sortedUniq(a.Volumes)
		a.Mounts = sortedUniq(a.Mounts)
		inv.DockerApps = append(inv.DockerApps, *a)
	}
	// Compose apps first, then containers started on their own.
	sort.Slice(inv.DockerApps, func(i, j int) bool {
		x, y := inv.DockerApps[i], inv.DockerApps[j]
		if x.Standalone != y.Standalone {
			return !x.Standalone
		}
		return x.Name < y.Name
	})
	out, err := exec.CommandContext(ctx, "docker", "volume", "ls", "-q").Output()
	if err != nil {
		return
	}
	for _, v := range strings.Fields(string(out)) {
		// Anonymous volumes are long random hashes; they belong to their container.
		if len(v) == 64 && strings.Trim(v, "0123456789abcdef") == "" {
			continue
		}
		u := used[v]
		if u == nil {
			u = []string{}
		}
		inv.DockerVolumes = append(inv.DockerVolumes, DockerVolume{Name: v, UsedBy: u})
	}
}

func sortedUniq(s []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
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
