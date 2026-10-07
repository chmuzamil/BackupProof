package drill

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/chmuzamil/backupproof/internal/source"
)

// dumpChecks finds PostgreSQL dumps inside a restored file backup (for
// example the pg_dump files a nightly script writes before restic runs) and
// proves each one loads into a real database. This closes the classic gap of
// "the dump file exists, but has anyone ever restored it?".
func dumpChecks(ctx context.Context, r *runner, dir string, spec source.Spec) {
	if spec.Drill.SkipDumps {
		return
	}
	var dumps []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if isPgCustomDump(p) {
			dumps = append(dumps, p)
		}
		return nil
	})
	if len(dumps) == 0 {
		return
	}
	if !DockerAvailable(ctx) {
		r.check("database-dumps", func() (string, error) {
			return fmt.Sprintf("Found %d PostgreSQL dump(s) but this server has no Docker, so they were not loaded into a test database. Install Docker (or choose a server with Docker for restore tests) to test them.", len(dumps)), nil
		})
		return
	}
	image := spec.Drill.Image
	if image == "" {
		image = "postgres:17" // pg_restore reads dumps from all older versions
	}
	var sb *Sandbox
	var err error
	if !r.check("sandbox-start", func() (string, error) {
		sb, err = StartSandbox(ctx, SandboxOptions{
			Image: image, MountDir: dir, DataTmpfs: "/var/lib/postgresql",
			Env: map[string]string{"POSTGRES_HOST_AUTH_METHOD": "trust"},
		})
		if err != nil {
			return "", err
		}
		r.res.Sandbox = sb.Describe()
		return sb.Describe(), sb.WaitReady(ctx, 2*time.Minute, "psql", "-h", "127.0.0.1", "-U", "postgres", "-Atc", "select 1")
	}) {
		if sb != nil {
			sb.Remove()
		}
		return
	}
	defer sb.Remove()
	for i, dump := range dumps {
		rel, _ := filepath.Rel(dir, dump)
		rel = filepath.ToSlash(rel)
		db := fmt.Sprintf("dump%d", i+1)
		name := filepath.Base(dump)
		psql := func(q string) (string, error) {
			return sb.Exec(ctx, "psql", "-h", "127.0.0.1", "-U", "postgres", "-d", db, "-X", "-At", "-v", "ON_ERROR_STOP=1", "-c", q)
		}
		loaded := r.check("database-dump: "+name, func() (string, error) {
			if _, err := sb.Exec(ctx, "createdb", "-h", "127.0.0.1", "-U", "postgres", db); err != nil {
				return "", err
			}
			// Without --exit-on-error: dumps from managed services (Supabase,
			// RDS, …) reference extensions and roles a plain server lacks.
			// Those are counted and reported; the data itself must load.
			out, rerr := sb.Exec(ctx, "sh", "-c", "pg_restore -h 127.0.0.1 -U postgres -d "+db+" --no-owner --no-privileges '/restore/"+strings.ReplaceAll(rel, "'", "'\\''")+"' 2>&1; echo EXIT=$?")
			ignored := ""
			if m := ignoredRe.FindStringSubmatch(out); m != nil {
				ignored = m[1]
			}
			if rerr != nil {
				return "", rerr
			}
			counts, err := psql(`SELECT count(*)||' '||coalesce(sum((xpath('/row/c/text()', query_to_xml(format('select count(*) as c from %I.%I', table_schema, table_name), false, true, '')))[1]::text::bigint),0)
FROM information_schema.tables WHERE table_type='BASE TABLE' AND table_schema NOT IN ('pg_catalog','information_schema')`)
			if err != nil {
				return "", err
			}
			var tables, rows int64
			fmt.Sscan(strings.TrimSpace(lastLine(counts)), &tables, &rows)
			if tables == 0 {
				return "", fmt.Errorf("the dump loaded but contains no tables: %s", strings.TrimSpace(lastLine(strings.TrimSuffix(out, "\n"))))
			}
			detail := fmt.Sprintf("loaded into PostgreSQL: %d tables, %d rows", tables, rows)
			if ignored != "" && ignored != "0" {
				detail += fmt.Sprintf(" (%s statements skipped, usually extensions or roles that only exist on the original server)", ignored)
			}
			return detail, nil
		})
		if loaded {
			r.check("database-dump-integrity: "+name, func() (string, error) {
				out, err := psql(`CREATE EXTENSION IF NOT EXISTS amcheck;
SELECT count(bt_index_check(index => c.oid, heapallindexed => true))
FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid JOIN pg_am am ON am.oid = c.relam
WHERE am.amname = 'btree' AND c.relpersistence <> 't' AND i.indisready AND i.indisvalid`)
				return fmt.Sprintf("%s indexes verified against their tables", strings.TrimSpace(lastLine(out))), err
			})
		}
	}
}

var ignoredRe = regexp.MustCompile(`errors ignored on restore: (\d+)`)

func isPgCustomDump(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 5)
	if _, err := io.ReadFull(f, head); err != nil {
		return false
	}
	return string(head) == "PGDMP"
}
