// Package drill performs restore drills: it restores a snapshot from storage
// into an empty directory (never from a local cache), proves the restored
// bytes reproduce the snapshot's Merkle root, then starts the data in an
// isolated sandbox and runs engine-native integrity checks, row-count
// reconciliation against the source, and user assertions.
package drill

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chmuzamil/backupproof/internal/engine"
	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/snapshot"
	"github.com/chmuzamil/backupproof/internal/source"
)

type Options struct {
	WorkDir string // parent for the temporary restore dir
	Log     engine.Logger
	// Keep leaves the restored files in place for inspection.
	Keep bool
}

type Result struct {
	StartedAt     time.Time
	FinishedAt    time.Time
	RestoredRoot  string
	RestoredBytes int64
	Sandbox       string
	Checks        []proof.Check
	Passed        bool
	RestoreDir    string
	// Duration is measured on the monotonic clock (wall-clock timestamps
	// can be too coarse for fast restores).
	Duration time.Duration
}

type runner struct {
	res *Result
	log engine.Logger
}

func (r *runner) check(name string, fn func() (string, error)) bool {
	start := time.Now()
	detail, err := fn()
	c := proof.Check{Name: name, Passed: err == nil, Detail: detail, DurationMs: time.Since(start).Milliseconds()}
	if err != nil {
		c.Detail = err.Error()
		if detail != "" {
			c.Detail = detail + ": " + err.Error()
		}
	}
	r.res.Checks = append(r.res.Checks, c)
	status := "PASS"
	if !c.Passed {
		status = "FAIL"
	}
	r.log("[%s] %s — %s", status, name, c.Detail)
	return c.Passed
}

// Run executes a full restore drill for one snapshot.
func Run(ctx context.Context, rp *repo.Repo, s snapshot.WithID, spec source.Spec, opts Options) (*Result, error) {
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}
	began := time.Now()
	res := &Result{StartedAt: began.UTC(), Sandbox: "host-filesystem"}
	r := &runner{res: res, log: opts.Log}

	dir, err := os.MkdirTemp(opts.WorkDir, "bp-drill-*")
	if err != nil {
		return nil, err
	}
	res.RestoreDir = dir
	if !opts.Keep {
		defer os.RemoveAll(dir)
	}

	restored := r.check("restore-from-storage", func() (string, error) {
		rr, err := engine.Restore(ctx, rp, s.Snapshot, dir, engine.RestoreOptions{Log: opts.Log})
		res.RestoredBytes = rr.Bytes
		return fmt.Sprintf("%d files, %d bytes, every chunk authenticated and every file hash verified", rr.Files, rr.Bytes), err
	})
	if restored {
		r.check("content-root", func() (string, error) {
			root, n, err := engine.TreeRoot(dir)
			res.RestoredRoot = root
			if err != nil {
				return "", err
			}
			if root != s.Root {
				return "", fmt.Errorf("restored tree root %s… does not match snapshot root %s…", root[:16], s.Root[:16])
			}
			return fmt.Sprintf("Merkle root of %d restored entries matches the snapshot (%s…)", n, root[:16]), nil
		})
		switch spec.Kind {
		case "files", "command", "import":
			filesChecks(ctx, r, dir, spec)
			dumpChecks(ctx, r, dir, spec)
		case "sqlite":
			sqliteChecks(ctx, r, dir, s, spec)
		case "postgres":
			postgresChecks(ctx, r, dir, s, spec)
		case "mysql":
			mysqlChecks(ctx, r, dir, s, spec)
		case "mongodb":
			mongoChecks(ctx, r, dir, s, spec)
		}
		if spec.Drill.Command != "" {
			r.check("custom-verify-command", func() (string, error) {
				out, err := source.Shell(ctx, spec.Drill.Command, map[string]string{"RESTORE_DIR": dir})
				return strings.TrimSpace(lastLine(out)), err
			})
		}
	}
	res.Duration = time.Since(began)
	res.FinishedAt = res.StartedAt.Add(res.Duration)
	res.Passed = len(res.Checks) > 0
	for _, c := range res.Checks {
		res.Passed = res.Passed && c.Passed
	}
	return res, nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func filesChecks(_ context.Context, r *runner, dir string, spec source.Spec) {
	if len(spec.Drill.ExpectPaths) > 0 {
		r.check("expected-paths", func() (string, error) {
			var missing []string
			for _, p := range spec.Drill.ExpectPaths {
				if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(snapshot.CleanPath(p)))); err != nil {
					missing = append(missing, p)
				}
			}
			if len(missing) > 0 {
				return "", fmt.Errorf("missing after restore: %s", strings.Join(missing, ", "))
			}
			return fmt.Sprintf("all %d expected paths present", len(spec.Drill.ExpectPaths)), nil
		})
	}
	if spec.Drill.MinFiles > 0 {
		r.check("minimum-file-count", func() (string, error) {
			n := 0
			filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
				if err == nil && d.Type().IsRegular() {
					n++
				}
				return nil
			})
			if n < spec.Drill.MinFiles {
				return "", fmt.Errorf("restored %d files, expected at least %d", n, spec.Drill.MinFiles)
			}
			return fmt.Sprintf("%d files restored (minimum %d)", n, spec.Drill.MinFiles), nil
		})
	}
}

// metaCounts reads the per-table counts captured at backup time.
func metaCounts(s snapshot.WithID) (map[string]int64, bool) {
	out := map[string]int64{}
	raw, ok := s.SourceMeta["tables"].(map[string]any)
	if !ok {
		return out, false
	}
	for k, v := range raw {
		if f, ok := v.(float64); ok {
			out[k] = int64(f)
		}
	}
	exact, _ := s.SourceMeta["countsExact"].(bool)
	return out, exact
}

// Reconcile compares counts captured at backup time with exact counts after
// restore. Exact sources must match exactly. Estimates (pg_stat, InnoDB) only
// fail when the restore has materially fewer rows than expected.
func Reconcile(expected map[string]int64, exact bool, restored map[string]int64, tolerance float64) (string, error) {
	if tolerance <= 0 {
		tolerance = 0.2
	}
	var problems []string
	names := make([]string, 0, len(expected))
	for t := range expected {
		names = append(names, t)
	}
	sort.Strings(names)
	var total int64
	for _, t := range names {
		want := expected[t]
		got, ok := restored[t]
		if !ok {
			problems = append(problems, t+": table missing after restore")
			continue
		}
		total += got
		if exact {
			if got != want {
				problems = append(problems, fmt.Sprintf("%s: %d rows restored, %d at backup", t, got, want))
			}
			continue
		}
		if want >= 100 && float64(got) < float64(want)*(1-tolerance) {
			problems = append(problems, fmt.Sprintf("%s: %d rows restored, ~%d estimated at backup (-%.0f%%)", t, got, want, 100*(1-float64(got)/float64(want))))
		}
	}
	if len(problems) > 0 {
		return "", fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	kind := "exact"
	if !exact {
		kind = fmt.Sprintf("estimate within %.0f%%", tolerance*100)
	}
	return fmt.Sprintf("%d tables, %d rows reconciled with backup-time counts (%s)", len(names), total, kind), nil
}

func truthy(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "", "0", "f", "false", "null", "no":
		return false
	}
	if f, err := parseFloat(v); err == nil {
		return f != 0 && !math.IsNaN(f)
	}
	return true
}

func parseFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(s, "%g", &f)
	return f, err
}

func findStream(dir, prefix string) (string, error) {
	var found string
	filepath.WalkDir(filepath.Join(dir, prefix), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && found == "" && !strings.HasSuffix(p, "globals.sql") {
			found = p
		}
		return nil
	})
	if found == "" {
		return "", fmt.Errorf("no %s dump in snapshot", prefix)
	}
	return found, nil
}
