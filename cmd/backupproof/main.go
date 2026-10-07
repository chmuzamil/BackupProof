// Command backupproof is a single binary that is the server, the agent and a
// standalone CLI for backups whose recoverability is proven, not assumed.
package main

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/chmuzamil/backupproof/internal/agent"
	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/chunker"
	"github.com/chmuzamil/backupproof/internal/engine"
	"github.com/chmuzamil/backupproof/internal/importer"
	"github.com/chmuzamil/backupproof/internal/ops"
	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/retention"
	"github.com/chmuzamil/backupproof/internal/server"
	"github.com/chmuzamil/backupproof/internal/snapshot"
	"github.com/chmuzamil/backupproof/internal/source"
)

const usage = `BackupProof — backups with cryptographic proof of recoverability.

Control plane & fleet:
  backupproof server  [--data DIR] [--listen :8420] [--public-url URL] [--tls-cert F --tls-key F]
  backupproof agent enroll --server URL --token TOKEN [--name NAME]
  backupproof agent run

Standalone (no server needed):
  backupproof init      --repo REPO
  backupproof backup    --repo REPO (--spec spec.json | --name NAME PATH...)
  backupproof drill     --repo REPO (--spec spec.json | --name NAME) [--snapshot REF]
  backupproof snapshots --repo REPO
  backupproof restore   --repo REPO --target DIR [--include PATH] SNAPSHOT
  backupproof check     --repo REPO [--read-data PERCENT]
  backupproof forget    --repo REPO --source NAME [--keep-last N --keep-daily N ...] [--prune]
  backupproof key-add   --repo REPO            add another password slot (break-glass key)
  backupproof import    --repo REPO --from LOCATION --name NAME [--format files|restic|kopia|borg]
                       [--restic-env /etc/restic/env --restic-password-file /etc/restic/password]
                       [--decrypt auto|none|gpg|openssl|age] [--unpack] [--group auto|day|folder|file|all]
                       convert existing backups (e.g. encrypted dumps in S3/B2) into restore-tested copies.
                       Secrets: BP_IMPORT_PASSWORD, BP_IMPORT_KEY_FILE, BP_IMPORT_ACCESS_KEY, BP_IMPORT_SECRET_KEY

Locked out of the dashboard (run on the server, with access to its data folder):
  backupproof admin users          [--data DIR]
  backupproof admin reset-password [--data DIR] USERNAME   new password from BP_NEW_PASSWORD or typed in
  backupproof admin reset-2fa      [--data DIR] USERNAME   turn off two-factor sign-in
  (DIR defaults to BP_DATA, or /var/lib/backupproof when that exists)

Proofs:
  backupproof proof list   --repo REPO
  backupproof proof export --repo REPO DIGEST_PREFIX [-o bundle.json]
  backupproof proof verify BUNDLE.json [--key bpkey1:...]... [--tsa-root CA.pem] [--require-timestamp]
  backupproof proof verify-pack EVIDENCE.json [--key bpkey1:...]...
  backupproof proof key                       print this machine's public signing key

REPO is a path, s3://bucket/prefix?endpoint=URL&region=R&lock=COMPLIANCE&lockDays=30
or sftp://user@host:22/path. Secrets come from the environment:
  BP_PASSWORD / BP_PASSWORD_FILE           repository password
  AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY S3 credentials
  BP_SFTP_PASSWORD / BP_SFTP_KEY_FILE / BP_SFTP_HOSTKEY
  BP_STATE                                 state dir (default ~/.backupproof)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	args := os.Args[2:]
	switch os.Args[1] {
	case "server":
		err = cmdServer(ctx, args)
	case "agent":
		err = cmdAgent(ctx, args)
	case "init":
		err = cmdInit(ctx, args)
	case "backup":
		err = cmdBackup(ctx, args)
	case "drill":
		err = cmdDrill(ctx, args)
	case "snapshots":
		err = cmdSnapshots(ctx, args)
	case "restore":
		err = cmdRestore(ctx, args)
	case "check":
		err = cmdCheck(ctx, args)
	case "forget":
		err = cmdForget(ctx, args)
	case "key-add":
		err = cmdKeyAdd(ctx, args)
	case "import":
		err = cmdImport(ctx, args)
	case "proof":
		err = cmdProof(ctx, args)
	case "admin":
		err = cmdAdmin(args)
	case "version", "--version":
		fmt.Println(engine.Version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		err = fmt.Errorf("unknown command %q (see `backupproof help`)", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// --- shared helpers --------------------------------------------------------

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func stateDir() string {
	if d := os.Getenv("BP_STATE"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".backupproof"
	}
	return filepath.Join(home, ".backupproof")
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05 ")+format+"\n", a...)
}

func password(confirm bool) ([]byte, error) {
	if p := os.Getenv("BP_PASSWORD"); p != "" {
		return []byte(p), nil
	}
	if f := os.Getenv("BP_PASSWORD_FILE"); f != "" {
		b, err := os.ReadFile(f)
		return []byte(strings.TrimRight(string(b), "\r\n")), err
	}
	fmt.Fprint(os.Stderr, "repository password: ")
	r := bufio.NewReader(os.Stdin)
	p, err := r.ReadString('\n')
	if err != nil && p == "" {
		return nil, errors.New("no password given (set BP_PASSWORD or BP_PASSWORD_FILE)")
	}
	p = strings.TrimRight(p, "\r\n")
	if confirm {
		fmt.Fprint(os.Stderr, "repeat password: ")
		p2, _ := r.ReadString('\n')
		if strings.TrimRight(p2, "\r\n") != p {
			return nil, errors.New("passwords do not match")
		}
	}
	return []byte(p), nil
}

func openBackend(ctx context.Context, raw string) (backend.Backend, backend.Config, error) {
	if raw == "" {
		return nil, backend.Config{}, errors.New("--repo is required")
	}
	cfg, err := backend.ParseURL(raw)
	if err != nil {
		return nil, cfg, err
	}
	creds := backend.Credentials{AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"), SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"), Password: os.Getenv("BP_SFTP_PASSWORD")}
	if f := os.Getenv("BP_SFTP_KEY_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, cfg, err
		}
		creds.PrivateKey = string(b)
	}
	cfg.HostKey = os.Getenv("BP_SFTP_HOSTKEY")
	be, err := backend.Open(ctx, cfg, creds)
	return be, cfg, err
}

func openRepo(ctx context.Context, raw string) (*repo.Repo, backend.Config, error) {
	be, cfg, err := openBackend(ctx, raw)
	if err != nil {
		return nil, cfg, err
	}
	pw, err := password(false)
	if err != nil {
		return nil, cfg, err
	}
	r, err := repo.Open(ctx, be, pw)
	if err != nil {
		be.Close()
	}
	return r, cfg, err
}

// localEnv builds an ops.Env using this machine's signing key and file ledger.
func localEnv(r *repo.Repo, cfg backend.Config, tsa bool) (*ops.Env, *proof.FileLedger, error) {
	dir := stateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	host, _ := os.Hostname()
	key, err := proof.LoadOrCreateKey(filepath.Join(dir, "signing.key"), "cli@"+host)
	if err != nil {
		return nil, nil, err
	}
	ledger, err := proof.OpenFileLedger(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		return nil, nil, err
	}
	env := &ops.Env{Repo: r, Signer: key, Ledger: ledger, Log: logf,
		Storage: proof.StorageInfo{Location: r.Backend().Location(), ObjectLockMode: cfg.ObjectLockMode, ObjectLockDays: cfg.ObjectLockDays}}
	if tsa {
		env.TSAs = proof.DefaultTSAs
	}
	return env, ledger, nil
}

func loadSpec(path, name string, paths []string) (source.Spec, error) {
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return source.Spec{}, err
		}
		var s source.Spec
		if err := json.Unmarshal(b, &s); err != nil {
			return s, fmt.Errorf("%s: %w", path, err)
		}
		if s.Password == "" {
			s.Password = os.Getenv("BP_SOURCE_PASSWORD")
		}
		return s, s.Validate()
	}
	if name == "" {
		return source.Spec{}, errors.New("--spec or --name is required")
	}
	s := source.Spec{Name: name, Kind: "files", Paths: paths}
	return s, nil
}

// --- server & agent --------------------------------------------------------------

func cmdServer(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	data := fs.String("data", envOr("BP_DATA", "./backupproof-data"), "data directory")
	listen := fs.String("listen", envOr("BP_LISTEN", ":8420"), "listen address")
	public := fs.String("public-url", os.Getenv("BP_PUBLIC_URL"), "URL agents use to reach this server")
	cert := fs.String("tls-cert", "", "TLS certificate file")
	key := fs.String("tls-key", "", "TLS key file")
	noLocal := fs.Bool("no-local-agent", false, "don't protect this machine with the built-in agent")
	downloads := fs.String("downloads", os.Getenv("BP_DOWNLOADS"), "folder with agent binaries for the install scripts (default DATA/downloads)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := server.New(server.Config{DataDir: *data, Listen: *listen, PublicURL: *public, TLSCert: *cert, TLSKey: *key, NoLocalAgent: *noLocal, Downloads: *downloads})
	if err != nil {
		return err
	}
	runErr := s.Run(ctx)
	// Closing flushes the database; report that failure unless Run already failed.
	if cerr := s.Close(); cerr != nil && runErr == nil {
		return fmt.Errorf("closing server: %w", cerr)
	}
	return runErr
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func cmdAgent(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: backupproof agent enroll|run")
	}
	dir := filepath.Join(stateDir(), "agent")
	switch args[0] {
	case "enroll":
		fs := flag.NewFlagSet("agent enroll", flag.ExitOnError)
		srv := fs.String("server", "", "control plane URL")
		tok := fs.String("token", "", "single-use enrollment token")
		name := fs.String("name", "", "agent name (default: hostname)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *srv == "" || *tok == "" {
			return errors.New("--server and --token are required")
		}
		st, err := agent.Enroll(ctx, *srv, *tok, *name, dir)
		if err != nil {
			return err
		}
		fmt.Printf("enrolled as agent #%d (%s). Server ledger key: %s\nState: %s\n", st.AgentID, st.Name, st.ServerKey, dir)
		return nil
	case "run":
		a, err := agent.New(dir)
		if err != nil {
			return err
		}
		return a.Run(ctx)
	}
	return fmt.Errorf("unknown agent command %q", args[0])
}

// --- standalone ------------------------------------------------------------------

func cmdInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	repoURL := fs.String("repo", os.Getenv("BP_REPO"), "repository")
	if err := fs.Parse(args); err != nil {
		return err
	}
	be, _, err := openBackend(ctx, *repoURL)
	if err != nil {
		return err
	}
	pw, err := password(true)
	if err != nil {
		return err
	}
	r, err := repo.Init(ctx, be, pw, chunker.DefaultParams)
	if err != nil {
		return err
	}
	defer r.Close()
	fmt.Printf("created repository %s at %s\n", r.Config().ID, be.Location())
	fmt.Println("IMPORTANT: store the password offline. Without it no one — including you — can restore.")
	return nil
}

func cmdBackup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	repoURL := fs.String("repo", os.Getenv("BP_REPO"), "repository")
	specPath := fs.String("spec", "", "source spec JSON")
	name := fs.String("name", "", "source name (files)")
	tsa := fs.Bool("timestamp", false, "obtain RFC 3161 timestamps from public TSAs")
	var tags multi
	fs.Var(&tags, "tag", "tag (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	spec, err := loadSpec(*specPath, *name, fs.Args())
	if err != nil {
		return err
	}
	r, cfg, err := openRepo(ctx, *repoURL)
	if err != nil {
		return err
	}
	defer r.Close()
	env, _, err := localEnv(r, cfg, *tsa)
	if err != nil {
		return err
	}
	snap, rec, err := ops.Backup(ctx, env, spec, tags)
	if err != nil {
		return err
	}
	fmt.Printf("snapshot %s  root %s…  ledger #%d\n", snap.ID.Short(), snap.Root[:16], rec.Ledger.Seq)
	return nil
}

func cmdDrill(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("drill", flag.ExitOnError)
	repoURL := fs.String("repo", os.Getenv("BP_REPO"), "repository")
	specPath := fs.String("spec", "", "source spec JSON")
	name := fs.String("name", "", "source name")
	ref := fs.String("snapshot", "", "snapshot (default latest of source)")
	tsa := fs.Bool("timestamp", false, "obtain RFC 3161 timestamps from public TSAs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	spec, err := loadSpec(*specPath, *name, nil)
	if err != nil {
		return err
	}
	r, cfg, err := openRepo(ctx, *repoURL)
	if err != nil {
		return err
	}
	defer r.Close()
	if spec.Kind == "files" && *specPath == "" {
		spec.Kind = "" // take the kind from the snapshot
	}
	env, _, err := localEnv(r, cfg, *tsa)
	if err != nil {
		return err
	}
	res, rec, err := ops.Drill(ctx, env, spec, *ref, "")
	if res != nil {
		fmt.Println()
		for _, c := range res.Checks {
			mark := "PASS"
			if !c.Passed {
				mark = "FAIL"
			}
			fmt.Printf("  [%s] %-30s %s\n", mark, c.Name, c.Detail)
		}
		fmt.Printf("\nRTO %s · restored %d bytes · sandbox %s\n", res.FinishedAt.Sub(res.StartedAt).Round(time.Millisecond), res.RestoredBytes, res.Sandbox)
	}
	if rec != nil && rec.Ledger != nil {
		fmt.Printf("attestation %s… recorded at ledger #%d\n", rec.Envelope.Digest()[:16], rec.Ledger.Seq)
	}
	return err
}

func cmdSnapshots(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("snapshots", flag.ExitOnError)
	repoURL := fs.String("repo", os.Getenv("BP_REPO"), "repository")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r, _, err := openRepo(ctx, *repoURL)
	if err != nil {
		return err
	}
	defer r.Close()
	all, err := snapshot.List(ctx, r)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tTIME\tSOURCE\tKIND\tHOST\tENTRIES\tSIZE\tROOT"); err != nil {
		return err
	}
	for _, s := range all {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s…\n", s.ID.Short(), s.Time.Local().Format("2006-01-02 15:04"), s.Source.Name, s.Source.Kind, s.Host, s.Entries, human(s.Stats.Bytes), s.Root[:12]); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func human(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

func cmdRestore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	repoURL := fs.String("repo", os.Getenv("BP_REPO"), "repository")
	target := fs.String("target", "", "target directory")
	var include multi
	fs.Var(&include, "include", "restore only this path (repeatable)")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if *target == "" || fs.NArg() != 1 {
		return errors.New("usage: backupproof restore --repo REPO --target DIR SNAPSHOT")
	}
	r, _, err := openRepo(ctx, *repoURL)
	if err != nil {
		return err
	}
	defer r.Close()
	s, err := snapshot.Resolve(ctx, r, fs.Arg(0))
	if err != nil {
		return err
	}
	res, err := engine.Restore(ctx, r, s.Snapshot, *target, engine.RestoreOptions{Include: include, Log: logf})
	if err != nil {
		return err
	}
	fmt.Printf("restored %d files (%s) in %s — every file verified against its content hash\n", res.Files, human(res.Bytes), time.Duration(res.DurationMs)*time.Millisecond)
	if len(include) == 0 {
		if root, _, err := engine.TreeRoot(*target); err == nil {
			if root == s.Root {
				fmt.Println("restored tree matches snapshot Merkle root", root[:16]+"…")
			} else {
				fmt.Println("note: target contains other files, so its tree root differs from the snapshot")
			}
		}
	}
	return nil
}

func cmdCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	repoURL := fs.String("repo", os.Getenv("BP_REPO"), "repository")
	pct := fs.Float64("read-data", 0, "percent of data to download and verify (0-100)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r, _, err := openRepo(ctx, *repoURL)
	if err != nil {
		return err
	}
	defer r.Close()
	seed := []byte(time.Now().String())
	if l, err := proof.OpenFileLedger(filepath.Join(stateDir(), "ledger.jsonl")); err == nil && l.Head() != nil {
		seed = []byte(l.Head().Hash)
	}
	res, err := engine.Check(ctx, r, engine.CheckOptions{ReadDataPercent: *pct, Seed: seed, Log: logf})
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(b))
	if !res.OK() {
		return errors.New("repository check FAILED")
	}
	return nil
}

func cmdForget(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("forget", flag.ExitOnError)
	repoURL := fs.String("repo", os.Getenv("BP_REPO"), "repository")
	src := fs.String("source", "", "source name")
	var p retention.Policy
	fs.IntVar(&p.KeepLast, "keep-last", 0, "")
	fs.IntVar(&p.KeepHourly, "keep-hourly", 0, "")
	fs.IntVar(&p.KeepDaily, "keep-daily", 0, "")
	fs.IntVar(&p.KeepWeekly, "keep-weekly", 0, "")
	fs.IntVar(&p.KeepMonthly, "keep-monthly", 0, "")
	fs.IntVar(&p.KeepYearly, "keep-yearly", 0, "")
	prune := fs.Bool("prune", false, "also delete unreferenced data")
	dry := fs.Bool("dry-run", false, "only show what would be forgotten")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *src == "" || p.Empty() {
		return errors.New("--source and at least one --keep-* flag are required")
	}
	r, _, err := openRepo(ctx, *repoURL)
	if err != nil {
		return err
	}
	defer r.Close()
	all, err := snapshot.List(ctx, r)
	if err != nil {
		return err
	}
	verified := map[string]bool{}
	if recs, err := ops.LoadRecords(ctx, r); err == nil {
		for _, rec := range recs {
			if rec.Kind == "drill" && rec.Passed {
				verified[rec.SnapshotID] = true
			}
		}
	}
	var items []retention.Item
	for _, s := range all {
		if s.Source.Name == *src {
			items = append(items, retention.Item{ID: s.ID.String(), Time: s.Time, Verified: verified[s.ID.String()]})
		}
	}
	var forget []string
	for _, d := range retention.Apply(items, p, time.Local) {
		state := "keep  "
		if !d.Keep {
			state = "forget"
			forget = append(forget, d.ID)
		}
		fmt.Printf("%s %s %s  %s\n", state, d.ID[:8], d.Time.Local().Format("2006-01-02 15:04"), strings.Join(d.Reasons, ", "))
	}
	if *dry {
		return nil
	}
	if err := engine.Forget(ctx, r, forget); err != nil {
		return err
	}
	if *prune {
		_, err = engine.Prune(ctx, r, engine.PruneOptions{Log: logf})
	}
	return err
}

func cmdKeyAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("key-add", flag.ExitOnError)
	repoURL := fs.String("repo", os.Getenv("BP_REPO"), "repository")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r, _, err := openRepo(ctx, *repoURL)
	if err != nil {
		return err
	}
	defer r.Close()
	fmt.Fprintln(os.Stderr, "new password for the additional key slot:")
	if err := os.Unsetenv("BP_PASSWORD"); err != nil {
		return err
	}
	if err := os.Unsetenv("BP_PASSWORD_FILE"); err != nil {
		return err
	}
	pw, err := password(true)
	if err != nil {
		return err
	}
	if err := r.AddPassword(ctx, pw); err != nil {
		return err
	}
	fmt.Println("added key slot")
	return nil
}

// --- proofs --------------------------------------------------------------------

func cmdProof(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: backupproof proof list|export|verify|verify-pack|key")
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "key":
		host, _ := os.Hostname()
		if err := os.MkdirAll(stateDir(), 0o700); err != nil {
			return err
		}
		k, err := proof.LoadOrCreateKey(filepath.Join(stateDir(), "signing.key"), "cli@"+host)
		if err != nil {
			return err
		}
		fmt.Println(k.Public().String())
		return nil
	case "list":
		fs := flag.NewFlagSet("proof list", flag.ExitOnError)
		repoURL := fs.String("repo", os.Getenv("BP_REPO"), "repository")
		if err := fs.Parse(args); err != nil {
			return err
		}
		r, _, err := openRepo(ctx, *repoURL)
		if err != nil {
			return err
		}
		defer r.Close()
		recs, err := ops.LoadRecords(ctx, r)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(tw, "DIGEST\tCREATED\tKIND\tSOURCE\tSNAPSHOT\tRESULT\tLEDGER\tTIMESTAMP"); err != nil {
			return err
		}
		for _, rec := range recs {
			res, seq, ts := "pass", "-", "-"
			if !rec.Passed {
				res = "FAIL"
			}
			if rec.Ledger != nil {
				seq = fmt.Sprint(rec.Ledger.Seq)
			}
			if rec.Timestamp != nil {
				ts = rec.Timestamp.Time.Format(time.RFC3339)
			}
			if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", rec.Envelope.Digest()[:12], rec.Created.Local().Format("2006-01-02 15:04"), rec.Kind, rec.Source, rec.SnapshotID[:8], res, seq, ts); err != nil {
				return err
			}
		}
		return tw.Flush()
	case "export":
		fs := flag.NewFlagSet("proof export", flag.ExitOnError)
		repoURL := fs.String("repo", os.Getenv("BP_REPO"), "repository")
		out := fs.String("o", "", "output file (default stdout)")
		if err := fs.Parse(reorder(args)); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("usage: backupproof proof export --repo REPO DIGEST_PREFIX")
		}
		r, cfg, err := openRepo(ctx, *repoURL)
		if err != nil {
			return err
		}
		defer r.Close()
		recs, err := ops.LoadRecords(ctx, r)
		if err != nil {
			return err
		}
		var match []*ops.Record
		for i := range recs {
			if strings.HasPrefix(recs[i].Envelope.Digest(), fs.Arg(0)) {
				match = append(match, &recs[i])
			}
		}
		if len(match) != 1 {
			return fmt.Errorf("%d proofs match %q", len(match), fs.Arg(0))
		}
		env, ledger, err := localEnv(r, cfg, false)
		if err != nil {
			return err
		}
		entries, err := ledger.All()
		if err != nil {
			return err
		}
		host, _ := os.Hostname()
		b, err := ops.BuildBundle(match[0], entries, env.Signer, "backupproof-cli@"+host, []proof.PublicKey{env.Signer.Public()})
		if err != nil {
			return err
		}
		raw, _ := json.MarshalIndent(b, "", "  ")
		if *out == "" {
			fmt.Println(string(raw))
			return nil
		}
		return os.WriteFile(*out, raw, 0o644)
	case "verify":
		fs := flag.NewFlagSet("proof verify", flag.ExitOnError)
		var keys multi
		fs.Var(&keys, "key", "trusted public key bpkey1:... (repeatable)")
		tsaRoot := fs.String("tsa-root", "", "PEM file with trusted TSA root certificates")
		reqTS := fs.Bool("require-timestamp", false, "fail without an RFC 3161 timestamp")
		jsonOut := fs.Bool("json", false, "print the report as JSON")
		if err := fs.Parse(reorder(args)); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("usage: backupproof proof verify BUNDLE.json [--key bpkey1:...]")
		}
		raw, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			return err
		}
		b, err := proof.ParseBundle(raw)
		if err != nil {
			return err
		}
		opts := proof.VerifyOptions{RequireTimestamp: *reqTS}
		for _, k := range keys {
			pk, err := proof.ParsePublicKey(k)
			if err != nil {
				return err
			}
			opts.TrustedKeys = append(opts.TrustedKeys, pk)
		}
		if *tsaRoot != "" {
			pool, err := loadPool(*tsaRoot)
			if err != nil {
				return err
			}
			opts.TSARoots = pool
		}
		rep := proof.VerifyBundle(b, opts)
		if *jsonOut {
			out, _ := json.MarshalIndent(rep, "", "  ")
			fmt.Println(string(out))
		} else {
			printReport(rep)
		}
		if !rep.Valid {
			return errors.New("proof is NOT valid")
		}
		return nil
	case "verify-pack":
		fs := flag.NewFlagSet("proof verify-pack", flag.ExitOnError)
		var keys multi
		fs.Var(&keys, "key", "trusted public key (repeatable)")
		if err := fs.Parse(reorder(args)); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("usage: backupproof proof verify-pack EVIDENCE.json [--key bpkey1:...]")
		}
		raw, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			return err
		}
		var pack server.EvidencePack
		if err := json.Unmarshal(raw, &pack); err != nil {
			return err
		}
		var trusted []proof.PublicKey
		for _, k := range keys {
			pk, err := proof.ParsePublicKey(k)
			if err != nil {
				return err
			}
			trusted = append(trusted, pk)
		}
		if len(trusted) == 0 {
			fmt.Println("warning: no --key given; trusting keys embedded in the pack")
		}
		errs := server.VerifyPack(&pack, trusted)
		pass, fail := 0, 0
		for _, p := range pack.Proofs {
			if p.Passed {
				pass++
			} else {
				fail++
			}
		}
		fmt.Printf("evidence %s – %s: %d attestations (%d pass, %d fail), ledger #%d–#%d\n",
			pack.From.Format("2006-01-02"), pack.To.Format("2006-01-02"), len(pack.Proofs), pass, fail, firstSeq(pack.Ledger), lastSeq(pack.Ledger))
		for _, e := range errs {
			fmt.Println("  ✗", e)
		}
		if len(errs) > 0 {
			return errors.New("evidence pack is NOT valid")
		}
		fmt.Println("  ✓ all signatures valid, ledger chain unbroken, checkpoint verified")
		return nil
	}
	return fmt.Errorf("unknown proof command %q", sub)
}

// reorder moves flags before positional args so `verify FILE --key K` works.
func reorder(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) && a != "--require-timestamp" && a != "-require-timestamp" && a != "--json" && a != "-json" {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

func firstSeq(l []proof.LedgerEntry) int64 {
	if len(l) == 0 {
		return 0
	}
	return l[0].Seq
}

func lastSeq(l []proof.LedgerEntry) int64 {
	if len(l) == 0 {
		return 0
	}
	return l[len(l)-1].Seq
}

func loadPool(path string) (*x509.CertPool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	for {
		var blk *pem.Block
		blk, raw = pem.Decode(raw)
		if blk == nil {
			break
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, err
		}
		pool.AddCert(c)
	}
	return pool, nil
}

func printReport(r *proof.Report) {
	status := "VALID"
	if !r.Valid {
		status = "INVALID"
	}
	fmt.Printf("%s  %s\n", status, r.PredicateType)
	fmt.Printf("  subject   %s\n", r.Subject.Name)
	fmt.Printf("  root      blake3:%s\n", r.Subject.Digest["blake3"])
	if r.Signer != "" {
		fmt.Printf("  signer    %s\n", r.Signer)
	}
	if r.Passed != nil {
		fmt.Printf("  drill     passed=%v\n", *r.Passed)
	}
	if r.LedgerSeq > 0 {
		fmt.Printf("  ledger    entry #%d, chained to signed checkpoint #%d\n", r.LedgerSeq, r.CheckpointSeq)
	}
	if r.TimestampedAt != nil {
		fmt.Printf("  time      %s (RFC 3161, %s)\n", r.TimestampedAt.Format(time.RFC3339), r.TimestampTSA)
	}
	for _, w := range r.Warnings {
		fmt.Println("  ! ", w)
	}
	for _, e := range r.Errors {
		fmt.Println("  ✗ ", e)
	}
}

func cmdImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	repoURL := fs.String("repo", os.Getenv("BP_REPO"), "BackupProof repository to convert into")
	from := fs.String("from", "", "where the old backups are (path, s3://bucket/prefix?endpoint=..., sftp://...)")
	name := fs.String("name", "", "name for the converted backups")
	format := fs.String("format", "files", "files | restic | kopia | borg")
	decrypt := fs.String("decrypt", "auto", "auto | none | gpg | openssl | age")
	unpack := fs.Bool("unpack", false, "open .zip/.tar.gz/.gz archives into individual files")
	group := fs.String("group", "auto", "auto | day | folder | file | all")
	iter := fs.Int("openssl-iter", 0, "PBKDF2 iterations used with openssl enc -iter")
	resticEnv := fs.String("restic-env", "", "restic: reuse an existing env file (e.g. /etc/restic/env)")
	resticPw := fs.String("restic-password-file", "", "restic: password file (e.g. /etc/restic/password)")
	resticRepo := fs.String("restic-repo", "", "restic: repository address (e.g. b2:bucket:path)")
	tsa := fs.Bool("timestamp", false, "obtain RFC 3161 timestamps")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" || (*from == "" && *resticEnv == "" && *resticRepo == "") {
		return errors.New("--name and --from (or --restic-env / --restic-repo) are required")
	}
	var cfg backend.Config
	if *from != "" {
		c, err := backend.ParseURL(*from)
		if err != nil {
			return err
		}
		cfg = c
	}
	if cfg.Type == "sftp" {
		cfg.HostKey = os.Getenv("BP_SFTP_HOSTKEY")
	}
	spec := source.Spec{Name: *name, Kind: "import", Import: &importer.Spec{
		Format: *format, Storage: cfg, Decrypt: *decrypt, Unpack: *unpack, Grouping: *group, OpenSSLIter: *iter,
		ResticEnvFile: *resticEnv, ResticPasswordFile: *resticPw, ResticRepository: *resticRepo,
		Password: os.Getenv("BP_IMPORT_PASSWORD"),
		Credentials: backend.Credentials{AccessKey: os.Getenv("BP_IMPORT_ACCESS_KEY"), SecretKey: os.Getenv("BP_IMPORT_SECRET_KEY"),
			Password: os.Getenv("BP_SFTP_PASSWORD")},
	}}
	if f := os.Getenv("BP_IMPORT_KEY_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		spec.Import.PrivateKey = string(b)
	}
	r, rcfg, err := openRepo(ctx, *repoURL)
	if err != nil {
		return err
	}
	defer r.Close()
	env, _, err := localEnv(r, rcfg, *tsa)
	if err != nil {
		return err
	}
	if *resticEnv != "" || *resticRepo != "" {
		spec.Import.Format = "restic"
	}
	res, err := ops.Import(ctx, env, spec)
	if res != nil {
		fmt.Printf("found %d old backup copies: %d converted, %d already done, %d failed (%s)\n", res.Found, res.Imported, res.Skipped, len(res.Failed), human(res.Bytes))
		for _, f := range res.Failed {
			fmt.Println("  ✗", f)
		}
		if res.Imported > 0 {
			fmt.Printf("next: backupproof drill --repo %s --name %s   (prove the converted copies restore)\n", *repoURL, *name)
		}
	}
	return err
}

// cmdAdmin recovers access to the dashboard from the server's own console.
func cmdAdmin(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: backupproof admin users|reset-password|reset-2fa [--data DIR] [USERNAME]")
	}
	sub := args[0]
	def := os.Getenv("BP_DATA")
	if def == "" {
		def = "./backupproof-data"
		if fi, err := os.Stat("/var/lib/backupproof"); err == nil && fi.IsDir() {
			def = "/var/lib/backupproof"
		}
	}
	fs := flag.NewFlagSet("admin "+sub, flag.ExitOnError)
	data := fs.String("data", def, "dashboard data folder")
	if err := fs.Parse(reorder(args[1:])); err != nil {
		return err
	}
	switch sub {
	case "users":
		users, err := server.ListAccounts(*data)
		if err != nil {
			return err
		}
		for _, u := range users {
			tf := ""
			if u.TwoFactor {
				tf = "  (two-factor on)"
			}
			fmt.Printf("%-24s %s%s\n", u.Username, u.Role, tf)
		}
		return nil
	case "reset-password", "reset-2fa":
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: backupproof admin %s [--data DIR] USERNAME", sub)
		}
		user := fs.Arg(0)
		if sub == "reset-2fa" {
			if err := server.RecoverAccount(*data, user, "", true); err != nil {
				return err
			}
			fmt.Printf("Two-factor sign-in is off for %s. They can sign in with their password and set it up again under Settings.\n", user)
			return nil
		}
		pw := os.Getenv("BP_NEW_PASSWORD")
		if pw == "" {
			fmt.Fprint(os.Stderr, "new password (at least 10 characters): ")
			in := bufio.NewReader(os.Stdin)
			line, _ := in.ReadString('\n')
			pw = strings.TrimRight(line, "\r\n")
		}
		if err := server.RecoverAccount(*data, user, pw, false); err != nil {
			return err
		}
		fmt.Printf("New password set for %s. Their existing sessions were signed out.\n", user)
		return nil
	}
	return fmt.Errorf("unknown admin command %q (users, reset-password or reset-2fa)", sub)
}
