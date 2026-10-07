package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/engine"
	"github.com/chmuzamil/backupproof/internal/importer"
	"github.com/chmuzamil/backupproof/internal/repo"
)

func (s *Server) onboardingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/agent/inventory", s.agentAuth(s.handleInventory))
	mux.HandleFunc("GET /api/browse", s.auth("operator", s.handleBrowse))
	mux.HandleFunc("POST /api/repositories/test", s.auth("operator", s.handleTestStorage))
	mux.HandleFunc("POST /api/import/scan", s.auth("admin", s.handleImportScan))
	mux.HandleFunc("GET /install.sh", s.handleInstallScript("sh"))
	mux.HandleFunc("GET /install.ps1", s.handleInstallScript("ps1"))
	mux.HandleFunc("GET /download/{file}", s.handleDownload)
}

func (s *Server) handleInventory(w http.ResponseWriter, r *http.Request) {
	a := agentFrom(r)
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.store.SetInventory(a.ID, raw); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// markBuiltin flags the agent embedded in this server.
func (s *Server) markBuiltin(agents []Agent) []Agent {
	id := s.builtinAgentID()
	for i := range agents {
		agents[i].Builtin = agents[i].ID == id
	}
	return agents
}

// --- folder browser (this server's own disk, for the built-in agent) -----------

type browseEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size,omitempty"`
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	resp := map[string]any{"path": p, "parent": "", "entries": []browseEntry{}}
	if p == "" {
		var roots []browseEntry
		if runtime.GOOS == "windows" {
			for l := 'A'; l <= 'Z'; l++ {
				root := string(l) + `:\`
				if _, err := os.Stat(root); err == nil {
					roots = append(roots, browseEntry{Name: "Drive " + string(l) + ":", Path: root, Dir: true})
				}
			}
		} else {
			roots = append(roots, browseEntry{Name: "/", Path: "/", Dir: true})
		}
		if home, err := os.UserHomeDir(); err == nil {
			roots = append([]browseEntry{{Name: "Home (" + filepath.Base(home) + ")", Path: home, Dir: true}}, roots...)
		}
		resp["entries"] = roots
		writeJSON(w, 200, resp)
		return
	}
	p = filepath.Clean(p)
	entries, err := os.ReadDir(p)
	if err != nil {
		writeErr(w, 400, fmt.Errorf("cannot open this folder: %v", err))
		return
	}
	var out []browseEntry
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		be := browseEntry{Name: e.Name(), Path: filepath.Join(p, e.Name()), Dir: e.IsDir()}
		if !e.IsDir() {
			if fi, err := e.Info(); err == nil {
				be.Size = fi.Size()
			}
		}
		out = append(out, be)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	if len(out) > 500 {
		out = out[:500]
	}
	parent := filepath.Dir(p)
	if parent == p {
		parent = ""
	}
	resp["path"], resp["parent"], resp["entries"] = p, parent, out
	writeJSON(w, 200, resp)
}

// --- storage test ----------------------------------------------------------------

func (s *Server) handleTestStorage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Backend     backend.Config      `json:"backend"`
		Credentials backend.Credentials `json:"credentials"`
		Password    string              `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if req.Backend.Type == "local" && engine.IsDenied(req.Backend.Path, []string{s.cfg.DataDir}) {
		writeJSON(w, 200, storageTest{Message: "This folder belongs to the BackupProof server itself. Choose a different folder or disk."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res := testStorage(ctx, req.Backend, req.Credentials, req.Password)
	writeJSON(w, 200, res)
}

type storageTest struct {
	OK bool `json:"ok"`
	// Message is written for non-technical users.
	Message string `json:"message"`
	// Existing is true when a BackupProof backup storage already lives here.
	Existing bool `json:"existing"`
	// PasswordOK reports whether the given password opens the existing storage.
	PasswordOK *bool `json:"passwordOk,omitempty"`
	// OldBackups names another tool whose backups were found here (e.g. "restic").
	OldBackups string `json:"oldBackups,omitempty"`
}

func testStorage(ctx context.Context, cfg backend.Config, creds backend.Credentials, password string) storageTest {
	if cfg.Type == "local" {
		if cfg.Path == "" {
			return storageTest{Message: "Choose a folder or disk."}
		}
		if err := os.MkdirAll(cfg.Path, 0o700); err != nil {
			return storageTest{Message: "Can't use this folder: " + err.Error()}
		}
	}
	be, err := backend.Open(ctx, cfg, creds)
	if err != nil {
		return storageTest{Message: friendlyStorageError(err)}
	}
	defer be.Close()
	if _, err := be.Get(ctx, "config"); err == nil {
		res := storageTest{OK: true, Existing: true, Message: "Connected. This storage already has BackupProof backups."}
		if password != "" {
			_, err := repo.Open(ctx, be, []byte(password))
			ok := err == nil
			res.PasswordOK = &ok
			if !ok {
				res.Message = "Connected, but the password doesn't open the backups already stored here."
			}
		}
		return res
	} else if !errors.Is(err, backend.ErrNotFound) {
		return storageTest{Message: friendlyStorageError(err)}
	}
	b := make([]byte, 6)
	rand.Read(b)
	key := "backupproof-connection-test-" + hex.EncodeToString(b)
	if err := be.Put(ctx, key, []byte("test")); err != nil {
		return storageTest{Message: "Connected, but can't save files here: " + friendlyStorageError(err)}
	}
	got, err := be.Get(ctx, key)
	derr := be.Delete(ctx, key)
	if err != nil || string(got) != "test" {
		return storageTest{Message: "Saved a test file but couldn't read it back."}
	}
	res := storageTest{OK: true, Message: "Connected. Backups can be saved here."}
	if derr != nil {
		res.Message += " (The small test file " + key + " could not be removed: " + friendlyStorageError(derr) + ")"
	}
	if old := detectOldBackups(ctx, be); old != "" {
		res.OldBackups = old
		res.Message += " We also found old " + old + " backups here — you can convert them under Import."
	}
	return res
}

// detectOldBackups recognises repositories of other backup tools in the
// storage, so the dashboard can offer to convert them.
func detectOldBackups(ctx context.Context, be backend.Backend) string {
	repo, err := importer.DetectRepository(ctx, be)
	if err != nil || repo == nil {
		return ""
	}
	return repo.Tool
}

func friendlyStorageError(err error) string {
	msg := err.Error()
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "invalidaccesskeyid") || strings.Contains(low, "signaturedoesnotmatch") || strings.Contains(low, "unauthorized") || strings.Contains(low, "403"):
		return "The access key or secret key is wrong, or the key isn't allowed to use this bucket."
	case strings.Contains(low, "nosuchbucket") || strings.Contains(low, "404"):
		return "That bucket doesn't exist (check the bucket name and region)."
	case strings.Contains(low, "no such host") || strings.Contains(low, "dial tcp"):
		return "Can't reach the storage server — check the address and your internet connection."
	case strings.Contains(low, "hostkey"):
		return "The server's SSH key is needed (paste the line from `ssh-keyscan`)."
	case strings.Contains(low, "unable to authenticate") || strings.Contains(low, "handshake failed"):
		return "The username, password or SSH key was not accepted."
	}
	return msg
}

// --- old-backup scan (preview before importing) -------------------------------------

func (s *Server) handleImportScan(w http.ResponseWriter, r *http.Request) {
	var spec importer.Spec
	if err := readJSON(r, &spec); err != nil {
		writeErr(w, 400, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	// People pick "backup files" for any bucket of backups. If the location is
	// really a restic, Kopia or Borg repository, say so and where, so the
	// dashboard can switch to the right import.
	if spec.Format == "files" {
		be, err := backend.Open(ctx, spec.Storage, spec.Credentials)
		if err != nil {
			writeErr(w, 400, errors.New(friendlyStorageError(err)))
			return
		}
		repo, derr := importer.DetectRepository(ctx, be)
		_ = be.Close()
		if derr != nil {
			writeErr(w, 400, errors.New(friendlyStorageError(derr)))
			return
		}
		if repo != nil {
			writeJSON(w, 200, map[string]any{"found": 0, "groups": []any{}, "repository": repo, "storage": repo.WithPrefix(spec.Storage)})
			return
		}
	}
	src, err := importer.Open(ctx, spec)
	if err != nil {
		writeErr(w, 400, errors.New(friendlyStorageError(err)))
		return
	}
	defer src.Close()
	points, err := src.List(ctx)
	if err != nil {
		writeErr(w, 400, errors.New(friendlyStorageError(err)))
		return
	}
	type group struct {
		Label  string    `json:"label"`
		Count  int       `json:"count"`
		Oldest time.Time `json:"oldest"`
		Newest time.Time `json:"newest"`
		Bytes  int64     `json:"bytes"`
	}
	groups := map[string]*group{}
	var order []string
	for _, p := range points {
		g := groups[p.Label]
		if g == nil {
			g = &group{Label: p.Label, Oldest: p.Time}
			groups[p.Label] = g
			order = append(order, p.Label)
		}
		g.Count++
		g.Bytes += p.Bytes
		if p.Time.Before(g.Oldest) {
			g.Oldest = p.Time
		}
		if p.Time.After(g.Newest) {
			g.Newest = p.Time
		}
	}
	out := []group{}
	for _, l := range order {
		out = append(out, *groups[l])
	}
	writeJSON(w, 200, map[string]any{"found": len(points), "groups": out})
}

// --- one-line install ------------------------------------------------------------

func (s *Server) downloadsDir() string {
	if s.cfg.Downloads != "" {
		return s.cfg.Downloads
	}
	return filepath.Join(s.cfg.DataDir, "downloads")
}

var downloadName = regexp.MustCompile(`^backupproof-(linux|darwin|windows)-(amd64|arm64)(\.exe)?$`)

// handleDownload serves agent binaries: from the downloads folder (filled by
// `make dist` or the Docker image), or this server's own executable when the
// platform matches.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	m := downloadName.FindStringSubmatch(name)
	if m == nil {
		http.NotFound(w, r)
		return
	}
	candidates := []string{filepath.Join(s.downloadsDir(), name)}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "downloads", name))
		if m[1] == runtime.GOOS && m[2] == runtime.GOARCH {
			candidates = append(candidates, exe)
		}
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
			w.Header().Set("Content-Type", "application/octet-stream")
			http.ServeFile(w, r, c)
			return
		}
	}
	http.Error(w, "The agent for "+m[1]+"/"+m[2]+" is not available on this server. Build it with `make dist` and copy it into "+s.downloadsDir(), http.StatusNotFound)
}

func (s *Server) installCommands(token string) map[string]string {
	u := s.publicURL()
	return map[string]string{
		"linux":   fmt.Sprintf("curl -fsSL %s/install.sh | sudo sh -s -- %s", u, token),
		"macos":   fmt.Sprintf("curl -fsSL %s/install.sh | sudo sh -s -- %s", u, token),
		"windows": fmt.Sprintf(`powershell -ExecutionPolicy Bypass -Command "& ([scriptblock]::Create((irm %s/install.ps1))) -Token %s"`, u, token),
	}
}

var installSh = template.Must(template.New("sh").Parse(`#!/bin/sh
# BackupProof agent installer. Usage: curl -fsSL {{.URL}}/install.sh | sudo sh -s -- <TOKEN>
set -eu
TOKEN="${1:-}"
SERVER="{{.URL}}"
[ -n "$TOKEN" ] || { echo "Usage: install.sh <connection token from the dashboard>"; exit 1; }
[ "$(id -u)" = 0 ] || { echo "Please run with sudo."; exit 1; }
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in x86_64|amd64) ARCH=amd64;; aarch64|arm64) ARCH=arm64;; *) echo "Unsupported CPU: $(uname -m)"; exit 1;; esac
echo "Downloading BackupProof agent for $OS/$ARCH..."
TMP=$(mktemp)
curl -fsSL "$SERVER/download/backupproof-$OS-$ARCH" -o "$TMP"
install -m 0755 "$TMP" /usr/local/bin/backupproof && rm -f "$TMP"
STATE=/var/lib/backupproof
mkdir -p "$STATE" && chmod 700 "$STATE"
BP_STATE="$STATE" /usr/local/bin/backupproof agent enroll --server "$SERVER" --token "$TOKEN" --name "$(hostname)"
if [ "$OS" = linux ] && command -v systemctl >/dev/null 2>&1; then
cat > /etc/systemd/system/backupproof-agent.service <<'UNIT'
[Unit]
Description=BackupProof agent
After=network-online.target docker.service
Wants=network-online.target
[Service]
Environment=BP_STATE=/var/lib/backupproof
ExecStart=/usr/local/bin/backupproof agent run
Restart=always
RestartSec=10
Nice=10
[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable --now backupproof-agent
elif [ "$OS" = darwin ]; then
cat > /Library/LaunchDaemons/dev.backupproof.agent.plist <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>dev.backupproof.agent</string>
<key>ProgramArguments</key><array><string>/usr/local/bin/backupproof</string><string>agent</string><string>run</string></array>
<key>EnvironmentVariables</key><dict><key>BP_STATE</key><string>/var/lib/backupproof</string></dict>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
</dict></plist>
PLIST
  launchctl load -w /Library/LaunchDaemons/dev.backupproof.agent.plist
else
  echo "Start it with: BP_STATE=$STATE backupproof agent run"
fi
echo "Done. This server will appear in the BackupProof dashboard in a few seconds."
`))

var installPs1 = template.Must(template.New("ps1").Parse(`# BackupProof agent installer for Windows (run as Administrator)
param([Parameter(Mandatory=$true)][string]$Token)
$ErrorActionPreference = "Stop"
$Server = "{{.URL}}"
$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw "Please run PowerShell as Administrator." }
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
$dir = Join-Path $env:ProgramFiles "BackupProof"
$state = Join-Path $env:ProgramData "BackupProof"
New-Item -ItemType Directory -Force -Path $dir, $state | Out-Null
Write-Host "Downloading BackupProof agent..."
Invoke-WebRequest -UseBasicParsing -Uri "$Server/download/backupproof-windows-$arch.exe" -OutFile (Join-Path $dir "backupproof.exe")
$env:BP_STATE = $state
& (Join-Path $dir "backupproof.exe") agent enroll --server $Server --token $Token --name $env:COMPUTERNAME
if ($LASTEXITCODE -ne 0) { throw "Connecting to the dashboard failed." }
$action = New-ScheduledTaskAction -Execute (Join-Path $dir "backupproof.exe") -Argument "agent run"
$trigger = New-ScheduledTaskTrigger -AtStartup
$settings = New-ScheduledTaskSettingsSet -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
[Environment]::SetEnvironmentVariable("BP_STATE", $state, "Machine")
Register-ScheduledTask -TaskName "BackupProof Agent" -Action $action -Trigger $trigger -Settings $settings -User "SYSTEM" -RunLevel Highest -Force | Out-Null
Start-ScheduledTask -TaskName "BackupProof Agent"
Write-Host "Done. This server will appear in the BackupProof dashboard in a few seconds."
`))

func (s *Server) handleInstallScript(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		data := map[string]string{"URL": s.publicURL()}
		if kind == "sh" {
			installSh.Execute(w, data)
		} else {
			installPs1.Execute(w, data)
		}
	}
}
