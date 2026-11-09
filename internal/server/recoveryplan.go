package server

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/source"
)

// A recovery plan is a per-server document to download or print before it is
// needed: what to restore, in what order, from which backup, and what is left
// to a person. It is built only from fields that hold no secrets: never an
// item's passwords, a storage password or credentials, a container's
// environment, or the contents of a backed-up .env file. Literal app-test
// settings are left out too, as only the names of secrets to set are useful.

// planItem is one thing to bring back.
type planItem struct {
	Order int    `json:"order"`
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	// Stage is the reason for its place in the order.
	Stage string `json:"stage"`

	Storage     string `json:"storage"`
	StorageWhat string `json:"storageWhat"`

	// Tested is false when no backup has passed a restore test. The fields
	// after it describe the newest backup that has.
	Tested       bool       `json:"tested"`
	SnapshotID   string     `json:"snapshotId,omitempty"`
	BackupTime   *time.Time `json:"backupTime,omitempty"`
	TestedAt     string     `json:"testedAt,omitempty"`
	RestoreMs    *int64     `json:"restoreMs,omitempty"`
	LastRestore  string     `json:"lastRestore,omitempty"` // when a real restore of this item last finished
	AppTestState string     `json:"appTestState,omitempty"`

	UISteps  []string `json:"uiSteps"`
	CLISteps []string `json:"cliSteps"`
	// AutoChecks passed in that restore test. Confirm is left to a person.
	AutoChecks []string `json:"autoChecks"`
	Confirm    []string `json:"confirm"`
	NotCovered []string `json:"notCovered"`
}

type planStorage struct {
	Name string `json:"name"`
	What string `json:"what"`
	Note string `json:"note,omitempty"`
}

type recoveryPlan struct {
	Generated string `json:"generated"`
	Server    string `json:"server"`
	Hostname  string `json:"hostname,omitempty"`
	OS        string `json:"os,omitempty"`
	OrderNote string `json:"orderNote"`

	Items    []planItem    `json:"items"`
	Storages []planStorage `json:"storages"`
	Warnings []string      `json:"warnings"`

	BeforeYouStart []string `json:"beforeYouStart"`
	DNSAndFirewall []string `json:"dnsAndFirewall"`
	AnotherServer  []string `json:"anotherServer"`
	KnownGaps      []string `json:"knownGaps"`
	LostDashboard  []string `json:"lostDashboard"`
}

func kindWords(s source.Spec) string {
	switch s.Kind {
	case "files":
		return "Folders and files"
	case "postgres":
		return "PostgreSQL database"
	case "mysql":
		return "MySQL or MariaDB database"
	case "mongodb":
		return "MongoDB database"
	case "sqlite":
		return "SQLite database"
	case "docker":
		return "Docker app"
	case "command":
		return "Output of a command"
	case "import":
		return "Imported backups"
	}
	return s.Kind
}

// planStage puts databases first, because the apps that need them expect them
// to be ready. BackupProof doesn't record which item an app really uses.
func planStage(kind string) (int, string) {
	switch kind {
	case "postgres", "mysql", "mongodb", "sqlite":
		return 0, "Database: restore before the apps that use it"
	case "docker":
		return 1, "App: restore after its databases"
	}
	return 2, "Files: restore last, unless an app above needs them first"
}

// storageWhat describes where backups are, with nothing secret in it.
func storageWhat(b backend.Config) string {
	switch b.Type {
	case "s3":
		w := "S3-compatible bucket " + b.Bucket
		if b.Prefix != "" {
			w += ", folder " + b.Prefix
		}
		if b.Endpoint != "" {
			w += " at " + b.Endpoint
		}
		return w
	case "sftp":
		return fmt.Sprintf("SFTP %s@%s:%s", b.User, b.Host, b.Path)
	case "rclone":
		return "rclone remote " + b.Remote
	}
	return "folder " + b.Path
}

// planRepoURL is the storage's address for the command line, or "" if the
// command line can't open this kind of storage.
func planRepoURL(b backend.Config) string {
	switch b.Type {
	case "local":
		return b.Path
	case "s3":
		q := url.Values{}
		if b.Endpoint != "" {
			q.Set("endpoint", b.Endpoint)
		}
		if b.Region != "" {
			q.Set("region", b.Region)
		}
		out := "s3://" + b.Bucket + "/" + b.Prefix
		if len(q) > 0 {
			out += "?" + q.Encode()
		}
		return out
	case "sftp":
		port := b.Port
		if port == 0 {
			port = 22
		}
		return fmt.Sprintf("sftp://%s@%s:%d%s", b.User, b.Host, port, b.Path)
	}
	return ""
}

func planCLIEnv(b backend.Config) string {
	switch b.Type {
	case "s3":
		return "export AWS_ACCESS_KEY_ID=<access key> AWS_SECRET_ACCESS_KEY=<secret key>"
	case "sftp":
		key := b.HostKey
		if key == "" {
			key = "<the server's key, from ssh-keyscan>"
		}
		return "export BP_SFTP_HOSTKEY='" + key + "'\nexport BP_SFTP_PASSWORD=<server password>  # or BP_SFTP_KEY_FILE=<path to the private key>"
	}
	return ""
}

var planCheckWords = map[string]string{
	"restore-from-storage":         "Every file was downloaded and decrypted",
	"content-root":                 "Every file matches the original",
	"expected-paths":               "Important files were present",
	"minimum-file-count":           "Enough files came back",
	"sqlite-integrity-check":       "Database structure was healthy",
	"amcheck-btree-heapallindexed": "Database structure was healthy",
	"check-table":                  "Database structure was healthy",
	"validate-collections":         "Database structure was healthy",
	"row-count-reconciliation":     "Row counts matched the backup",
	"pg_restore":                   "The database dump loaded",
	"import-dump":                  "The database dump loaded",
	"mongorestore":                 "The database dump loaded",
	"custom-verify-command":        "Your own check passed",
	"app-sandbox-start":            "The app started in isolation",
	"app-running":                  "The app stayed running",
	"app-database-running":         "The app's database stayed running",
	"app-http-status":              "The app answered",
	"app-body-text":                "The app's answer had the expected text",
	"app-response-time":            "The app answered quickly enough",
}

func planChecks(checks []proof.Check) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, c := range checks {
		if !c.Passed {
			continue
		}
		w := planCheckWords[c.Name]
		if w == "" {
			if strings.HasPrefix(c.Name, "assert:") {
				w = "Your check: " + strings.TrimSpace(strings.TrimPrefix(c.Name, "assert:"))
			} else {
				continue
			}
		}
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// newestTested picks the backup a restore should start from: the newest by
// when it was taken, among those that passed a restore test.
func newestTested(tests []ProofRow) *ProofRow {
	var best *ProofRow
	var bestTime time.Time
	for i := range tests {
		p := &tests[i]
		if !p.Passed {
			continue
		}
		if t := snapshotTime(p); best == nil || t.After(bestTime) {
			best, bestTime = p, t
		}
	}
	return best
}

func (s *Server) buildRecoveryPlan(a *Agent, at time.Time) (*recoveryPlan, error) {
	sources, err := s.store.Sources()
	if err != nil {
		return nil, err
	}
	repos, err := s.store.Repositories()
	if err != nil {
		return nil, err
	}
	repoByID := map[int64]Repository{}
	for _, r := range repos {
		repoByID[r.ID] = r
	}
	plan := &recoveryPlan{
		Generated: at.UTC().Format(time.RFC3339), Server: a.Name, Hostname: a.Hostname, OS: a.OS,
		OrderNote: "Databases come first, then Docker apps, then files. BackupProof doesn't record which item an app really uses, so change the order if yours needs it.",
		Items:     []planItem{}, Storages: []planStorage{}, Warnings: []string{},
	}
	var mine []Source
	for _, src := range sources {
		if src.AgentID == a.ID {
			mine = append(mine, src)
		}
	}
	sort.SliceStable(mine, func(i, j int) bool {
		x, _ := planStage(mine[i].Spec.Kind)
		y, _ := planStage(mine[j].Spec.Kind)
		if x != y {
			return x < y
		}
		return mine[i].Name < mine[j].Name
	})
	seenRepo := map[int64]bool{}
	for i, src := range mine {
		r := repoByID[src.RepoID]
		item, err := s.planItem(src, r, i+1)
		if err != nil {
			return nil, err
		}
		plan.Items = append(plan.Items, item)
		if !item.Tested {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s has no backup that passed a restore test. Run one before you need it.", src.Name))
		}
		if src.Spec.Drill.AppTest != nil {
			for _, name := range appTestSecretNames(src.Spec.Drill.AppTest) {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: the app test reads %s on the server running it. On a new server, set it again or the app test will fail.", src.Name, name))
			}
		}
		if seenRepo[r.ID] {
			continue
		}
		seenRepo[r.ID] = true
		ps := planStorage{Name: r.Name, What: storageWhat(r.Backend), Note: "You need this storage's recovery kit: it has the encryption password. Without it nobody can open these backups."}
		if r.Backend.Type == "local" {
			ps.Note += " This is a folder on a disk: if it is on the lost server, the backups are lost with it."
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("Storage %q is a folder on a disk (%s). If that disk is on this server, it won't survive losing the server. Add a second storage elsewhere.", r.Name, r.Backend.Path))
		}
		if r.Backend.Type == "rclone" {
			ps.Note += " The backupproof command line can't open rclone storage by address, so restore from the dashboard, or copy the storage to a folder with rclone and restore from there."
		}
		plan.Storages = append(plan.Storages, ps)
	}
	plan.BeforeYouStart = planBeforeYouStart
	plan.DNSAndFirewall = planDNSAndFirewall
	plan.AnotherServer = planAnotherServer
	plan.KnownGaps = planKnownGaps
	plan.LostDashboard = planLostDashboard
	return plan, nil
}

func appTestSecretNames(at *source.AppTest) []string {
	var out []string
	add := func(c *source.AppContainer) {
		for _, e := range c.Env {
			if e.SecretEnv != "" {
				out = append(out, e.SecretEnv)
			}
		}
	}
	add(&at.App)
	if at.Database != nil {
		add(at.Database)
	}
	return out
}

func (s *Server) planItem(src Source, r Repository, order int) (planItem, error) {
	tests, err := s.store.RecoveryTests(src.ID, recoveryTests)
	tests = s.withLastPass(src.ID, tests)
	if err != nil {
		return planItem{}, err
	}
	_, stage := planStage(src.Spec.Kind)
	item := planItem{Order: order, Name: src.Name, Kind: kindWords(src.Spec), Stage: stage, Storage: r.Name, StorageWhat: storageWhat(r.Backend),
		AutoChecks: []string{}, Confirm: []string{}, NotCovered: []string{}}
	snap := "<backup ID from the list above>"
	best := newestTested(tests)
	if best != nil {
		item.Tested, item.SnapshotID, item.TestedAt = true, best.SnapshotID, best.Created
		if t := snapshotTime(best); !t.IsZero() {
			item.BackupTime = &t
		}
		snap = best.SnapshotID
		item.AutoChecks = planChecks(lastChecks(best))
	}
	item.RestoreMs = recoveryFrom(src, tests, time.Now()).RestoreMs
	for _, kind := range []string{"restore", "restore-db"} {
		if j := s.store.LastJob(src.ID, kind, "succeeded"); j != nil && j.Finished != nil && item.LastRestore < j.Finished.UTC().Format(time.RFC3339) {
			item.LastRestore = j.Finished.UTC().Format(time.RFC3339)
		}
	}
	if src.Spec.Drill.AppTest != nil {
		item.AppTestState = "The app test is on for this item, so its restore tests also start the app in isolation."
		if best != nil && appTestFailed(lastChecks(best)) {
			item.AppTestState = "The app test is on for this item, and the app didn't come back up in that restore test. The data passed, but expect to fix the app after restoring it."
		}
	}
	planSteps(&item, src, r, snap)
	return item, nil
}

// planSteps fills in how to restore each kind of item, and says what isn't covered.
func planSteps(item *planItem, src Source, r Repository, snap string) {
	open := fmt.Sprintf("In the dashboard, open Protected, then %q, then Restore, and choose the newest backup. None has passed a restore test yet.", src.Name)
	if item.Tested {
		open = fmt.Sprintf("In the dashboard, open Protected, then %q, then Restore, and choose the backup marked as tested (%s).", src.Name, short(snap))
	}
	base := fmt.Sprintf("backupproof restore --repo '%s' --target /root/restored", planRepoURL(r.Backend))
	env := "export BP_PASSWORD='<password from the recovery kit>'"
	if extra := planCLIEnv(r.Backend); extra != "" {
		env += "\n" + extra
	}
	item.CLISteps = []string{env, "backupproof snapshots --repo '" + planRepoURL(r.Backend) + "'"}
	if planRepoURL(r.Backend) == "" {
		item.CLISteps = []string{"The command line can't open this storage by address. Restore from the dashboard, or copy the storage to a folder with rclone and restore from that folder."}
	}
	cli := func(extra string) {
		if planRepoURL(r.Backend) != "" {
			item.CLISteps = append(item.CLISteps, strings.TrimSpace(base+" "+extra+" "+snap))
		}
	}
	switch src.Spec.Kind {
	case "postgres", "mysql", "mongodb", "sqlite":
		dump, _ := source.DumpPath(src.Spec)
		item.UISteps = []string{open, "Choose a new database next to the original, or replace the original after typing its name."}
		item.NotCovered = append(item.NotCovered, "A database restore from the dashboard runs on the original server only. For a replacement server, restore the dump to a folder with the command below and load it with the database's own tools.")
		if dump == "" || strings.HasSuffix(dump, "/.dump") || strings.HasSuffix(dump, "/.sql") {
			cli("")
			item.NotCovered = append(item.NotCovered, "The name of this database is worked out at backup time, so look in the restored folder for its dump.")
		} else {
			cli("--include " + dump)
		}
		item.Confirm = append(item.Confirm, "Connect the app to the restored database and check recent records are there.")
	case "docker":
		item.UISteps = []string{open, "Choose Put the app's data back. The containers using it stop, its volumes and mounted folders are replaced, and the containers start again."}
		cli("")
		item.NotCovered = append(item.NotCovered,
			"Docker put-back restores volumes and mounted folders only. It doesn't recreate Compose files, images or containers, so on a new server they have to exist first.",
			"The Compose files are in the backup under docker/compose/ and container settings under docker/containers/. Restore them to a folder, read them before using them (they may include an .env with passwords, and a restored file can do anything Docker can), then start the app yourself with docker compose up -d.")
		item.Confirm = append(item.Confirm, "Open the app in a browser and do something that depends on its data.")
	case "command":
		item.UISteps = []string{open, "Restore to a new folder: the backup is the command's saved output."}
		cli("")
		item.NotCovered = append(item.NotCovered, "BackupProof saved what the command printed. Putting that back into the system is up to you.")
		item.Confirm = append(item.Confirm, "Load the output where it belongs and check it works.")
	default:
		item.UISteps = []string{open, "Choose where it came from, or a new folder, on this server or another."}
		cli("")
		item.Confirm = append(item.Confirm, "Check the files are where the app expects, with the right owners, and start whatever uses them.")
	}
}

var planBeforeYouStart = []string{
	"Keep a copy of this plan and the recovery kits somewhere that doesn't depend on this server.",
	"Have a replacement server, or the original with enough free disk space for every item below.",
	"Have the storage access details: the recovery kit, and the storage's access keys or login. Neither is in this plan.",
	"For the command-line steps, download the backupproof program for the replacement server from backupproof.dev.",
}

var planDNSAndFirewall = []string{
	"Lower the TTL of your DNS records ahead of time, if you can.",
	"Open the same ports on the replacement server's firewall as on the old one. BackupProof doesn't back up firewall rules.",
	"Install or renew TLS certificates for the replacement server if they aren't part of what you restored.",
	"Once the apps respond on the new server's address, point DNS at it. BackupProof doesn't change DNS.",
	"Check from outside your network that each site loads over HTTPS and that mail or webhooks still arrive.",
}

var planAnotherServer = []string{
	"Files can be restored onto any connected server from the Restore page: choose A new folder.",
	"Databases and Docker put-back can't target another server from the Restore page.",
	"An item's server can be changed under Edit (advanced), which makes new jobs run there. This is undocumented, and has been tested only as far as the dashboard accepting it: a full restore on a new server after moving hasn't been tried. Test it on something unimportant first.",
}

var planKnownGaps = []string{
	"Database restores from the dashboard go to the original server only.",
	"Docker put-back restores volumes and mounted folders, not Compose files.",
	"Passing a restore test shows the data can be restored. Unless the item's app test is on and passed, it doesn't show the app works.",
	"BackupProof doesn't configure DNS, firewalls, certificates or the operating system of a replacement server.",
}

// planLostDashboard is the procedure when the dashboard itself is gone. It
// follows from the code: the snapshots and restore commands need only the
// storage and its password (cmd/backupproof), proofs are written to the storage
// as well (internal/ops), a dashboard restores only backups it holds a signed proof
// for (attestedRoot), and the built-in server refuses its own data folder
// (engine.IsDenied).
var planLostDashboard = []string{
	"Your backups don't depend on the dashboard. Each storage holds the encrypted data and a copy of the signed proofs.",
	"On any computer, download the backupproof program, then set BP_PASSWORD from the recovery kit, and the storage's access keys or login.",
	"List the backups with: backupproof snapshots --repo '<storage address>'",
	"Restore one with: backupproof restore --repo '<storage address>' --target /root/restored SNAPSHOT-ID. Every file is checked against its hash.",
	"Check the proofs with: backupproof proof list --repo '<storage address>'",
	"To go back to using a dashboard, install a new one and connect each server again with a new code, because a server's connection belongs to the old dashboard.",
	"Add each storage to the new dashboard with its recovery kit. Backups made before the dashboard was lost can't be restored from the new dashboard, because it has no signed proofs for them: use the command line for those. New backups work as usual.",
	"The dashboard can't back up its own data folder. If you want to keep users, schedules and the proof history, copy the data folder (BP_DATA) somewhere else yourself.",
}

func (s *Server) handleRecoveryPlan(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	a, err := s.store.Agent(id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	plan, err := s.buildRecoveryPlan(a, time.Now())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"plan": plan, "text": plan.markdown()})
}

// markdown renders the plan for downloading.
func (p *recoveryPlan) markdown() string {
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	list := func(items []string) {
		for _, i := range items {
			line("- %s", i)
		}
		line("")
	}
	steps := func(items []string) {
		for n, i := range items {
			line("%d. %s", n+1, i)
		}
		line("")
	}
	line("# Recovery plan: %s", p.Server)
	line("")
	if p.Hostname != "" {
		line("Server: %s (%s)  ", p.Hostname, p.OS)
	}
	line("Made: %s. The tested backups below change as new tests pass, so refresh this plan regularly.", p.Generated)
	line("")
	if len(p.Warnings) > 0 {
		line("## Warnings")
		line("")
		list(p.Warnings)
	}
	line("## Before you start")
	line("")
	list(p.BeforeYouStart)
	line("## Storage you need")
	line("")
	for _, s := range p.Storages {
		line("- **%s**: %s. %s", s.Name, s.What, s.Note)
	}
	line("")
	line("## What to restore, in order")
	line("")
	line("%s", p.OrderNote)
	line("")
	if len(p.Items) == 0 {
		line("Nothing on this server is protected yet.")
		line("")
	}
	for _, it := range p.Items {
		line("### %d. %s (%s)", it.Order, it.Name, it.Kind)
		line("")
		line("%s. Stored in %s (%s).", it.Stage, it.Storage, it.StorageWhat)
		line("")
		if it.Tested {
			line("- Newest backup that passed a restore test: %s", it.SnapshotID)
			if it.BackupTime != nil {
				line("- Taken: %s", it.BackupTime.UTC().Format(time.RFC3339))
			}
			line("- Tested: %s", it.TestedAt)
		} else {
			line("- **No backup has passed a restore test.**")
		}
		if it.RestoreMs != nil {
			line("- Time to restore in the last passed test: %s", humanDur(time.Duration(*it.RestoreMs)*time.Millisecond))
		}
		if it.LastRestore != "" {
			line("- Last real restore: %s", it.LastRestore)
		}
		if it.AppTestState != "" {
			line("- %s", it.AppTestState)
		}
		line("")
		line("In the dashboard:")
		line("")
		steps(it.UISteps)
		line("From the command line:")
		line("")
		line("```")
		for _, c := range it.CLISteps {
			line("%s", c)
		}
		line("```")
		line("")
		if len(it.AutoChecks) > 0 {
			line("Checked automatically by that restore test:")
			line("")
			list(it.AutoChecks)
		}
		line("You need to confirm:")
		line("")
		for _, c := range it.Confirm {
			line("- [ ] %s", c)
		}
		line("")
		if len(it.NotCovered) > 0 {
			line("Not covered:")
			line("")
			list(it.NotCovered)
		}
	}
	line("## DNS and firewall (you do these)")
	line("")
	for _, c := range p.DNSAndFirewall {
		line("- [ ] %s", c)
	}
	line("")
	line("## Moving to another server")
	line("")
	list(p.AnotherServer)
	line("## Known gaps")
	line("")
	list(p.KnownGaps)
	line("## If the dashboard is lost too")
	line("")
	steps(p.LostDashboard)
	return b.String()
}
