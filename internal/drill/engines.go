package drill

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/chmuzamil/backupproof/internal/snapshot"
	"github.com/chmuzamil/backupproof/internal/source"
)

func sqliteChecks(ctx context.Context, r *runner, dir string, s snapshot.WithID, spec source.Spec) {
	path, err := findStream(dir, "sqlite")
	if !r.check("locate-database", func() (string, error) { return filepath.Base(path), err }) {
		return
	}
	r.res.Sandbox = "embedded-sqlite (read-only)"
	var counts map[string]int64
	r.check("sqlite-integrity-check", func() (string, error) {
		c, integrity, err := source.SQLiteInspect(ctx, path)
		counts = c
		if err != nil {
			return "", err
		}
		if integrity != "ok" {
			return "", fmt.Errorf("PRAGMA integrity_check: %s", integrity)
		}
		return "PRAGMA integrity_check = ok", nil
	})
	if expected, exact := metaCounts(s); len(expected) > 0 && counts != nil {
		r.check("row-count-reconciliation", func() (string, error) {
			return Reconcile(expected, exact, counts, spec.Drill.RowCountTolerance)
		})
	}
	if len(spec.Drill.Assertions) > 0 {
		db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
		if err != nil {
			return
		}
		defer db.Close()
		for _, a := range spec.Drill.Assertions {
			a := a
			r.check("assert: "+a.Name, func() (string, error) {
				var v sql.NullString
				if err := db.QueryRowContext(ctx, a.SQL).Scan(&v); err != nil {
					return "", err
				}
				if !truthy(v.String) {
					return "", fmt.Errorf("returned %q", v.String)
				}
				return "returned " + v.String, nil
			})
		}
	}
}

func intMeta(s snapshot.WithID, key string) int {
	if f, ok := s.SourceMeta[key].(float64); ok {
		return int(f)
	}
	return 0
}

func postgresChecks(ctx context.Context, r *runner, dir string, s snapshot.WithID, spec source.Spec) {
	dump, err := findStream(dir, "postgres")
	if !r.check("locate-dump", func() (string, error) { return filepath.Base(dump), err }) {
		return
	}
	image := spec.Drill.Image
	if image == "" {
		major := intMeta(s, "serverMajor")
		if major == 0 {
			major = 17
		}
		image = fmt.Sprintf("postgres:%d", major)
	}
	if !DockerAvailable(ctx) {
		r.check("sandbox", func() (string, error) { return "", fmt.Errorf("docker is required for PostgreSQL drills") })
		return
	}
	var sb *Sandbox
	if !r.check("sandbox-start", func() (string, error) {
		sb, err = StartSandbox(ctx, SandboxOptions{
			Image: image, MountDir: dir, DataTmpfs: "/var/lib/postgresql",
			Env: map[string]string{"POSTGRES_HOST_AUTH_METHOD": "trust"},
		})
		if err != nil {
			return "", err
		}
		r.res.Sandbox = sb.Describe()
		// The entrypoint's init server listens on a socket only; TCP readiness
		// means the final server is up.
		return sb.Describe(), sb.WaitReady(ctx, 2*time.Minute, "psql", "-h", "127.0.0.1", "-U", "postgres", "-Atc", "select 1")
	}) {
		if sb != nil {
			sb.Remove()
		}
		return
	}
	defer sb.Remove()
	psql := func(q string) (string, error) {
		return sb.Exec(ctx, "psql", "-h", "127.0.0.1", "-U", "postgres", "-d", "drill", "-X", "-At", "-v", "ON_ERROR_STOP=1", "-c", q)
	}
	rel, _ := filepath.Rel(dir, dump)
	if !r.check("pg_restore", func() (string, error) {
		if _, err := sb.Exec(ctx, "createdb", "-h", "127.0.0.1", "-U", "postgres", "drill"); err != nil {
			return "", err
		}
		_, err := sb.Exec(ctx, "pg_restore", "-h", "127.0.0.1", "-U", "postgres", "-d", "drill",
			"--no-owner", "--no-privileges", "--exit-on-error", "/restore/"+filepath.ToSlash(rel))
		return "restored into " + image + " with --exit-on-error", err
	}) {
		return
	}
	r.check("amcheck-btree-heapallindexed", func() (string, error) {
		out, err := psql(`CREATE EXTENSION IF NOT EXISTS amcheck;
SELECT count(bt_index_check(index => c.oid, heapallindexed => true))
FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid JOIN pg_am am ON am.oid = c.relam
WHERE am.amname = 'btree' AND c.relpersistence <> 't' AND i.indisready AND i.indisvalid`)
		return fmt.Sprintf("%s B-tree indexes verified against their heaps", strings.TrimSpace(lastLine(out))), err
	})
	restored := map[string]int64{}
	r.check("exact-row-counts", func() (string, error) {
		out, err := psql(`SELECT quote_ident(table_schema)||'.'||quote_ident(table_name)||'='||
 (xpath('/row/c/text()', query_to_xml(format('select count(*) as c from %I.%I', table_schema, table_name), false, true, '')))[1]::text
FROM information_schema.tables WHERE table_type='BASE TABLE' AND table_schema NOT IN ('pg_catalog','information_schema')`)
		if err != nil {
			return "", err
		}
		for k, v := range parseLines(out) {
			restored[k] = v
		}
		return fmt.Sprintf("%d tables counted", len(restored)), nil
	})
	if expected, exact := metaCounts(s); len(expected) > 0 {
		r.check("row-count-reconciliation", func() (string, error) {
			return Reconcile(expected, exact, restored, spec.Drill.RowCountTolerance)
		})
	}
	for _, a := range spec.Drill.Assertions {
		a := a
		r.check("assert: "+a.Name, func() (string, error) {
			out, err := psql(a.SQL)
			if err != nil {
				return "", err
			}
			v := strings.TrimSpace(lastLine(out))
			if !truthy(v) {
				return "", fmt.Errorf("returned %q", v)
			}
			return "returned " + v, nil
		})
	}
}

var mysqlVersionRe = regexp.MustCompile(`^(\d+)\.(\d+)`)

// MySQLImage picks a sandbox image matching the server that was backed up.
func MySQLImage(serverVersion string) string {
	m := mysqlVersionRe.FindStringSubmatch(serverVersion)
	isMaria := strings.Contains(strings.ToLower(serverVersion), "mariadb")
	switch {
	case m == nil && isMaria:
		return "mariadb:11"
	case m == nil:
		return "mysql:8.4"
	case isMaria:
		return "mariadb:" + m[1] + "." + m[2]
	}
	return "mysql:" + m[1] + "." + m[2]
}

func mysqlChecks(ctx context.Context, r *runner, dir string, s snapshot.WithID, spec source.Spec) {
	dump, err := findStream(dir, "mysql")
	if !r.check("locate-dump", func() (string, error) { return filepath.Base(dump), err }) {
		return
	}
	version, _ := s.SourceMeta["serverVersion"].(string)
	image := spec.Drill.Image
	if image == "" {
		image = MySQLImage(version)
	}
	client, check := "mysql", "mysqlcheck"
	if strings.HasPrefix(image, "mariadb") {
		client, check = "mariadb", "mariadb-check"
	}
	if !DockerAvailable(ctx) {
		r.check("sandbox", func() (string, error) { return "", fmt.Errorf("docker is required for MySQL drills") })
		return
	}
	var sb *Sandbox
	if !r.check("sandbox-start", func() (string, error) {
		sb, err = StartSandbox(ctx, SandboxOptions{
			Image: image, MountDir: dir, DataTmpfs: "/var/lib/mysql",
			Env: map[string]string{"MYSQL_ALLOW_EMPTY_PASSWORD": "yes", "MARIADB_ALLOW_EMPTY_ROOT_PASSWORD": "1"},
		})
		if err != nil {
			return "", err
		}
		r.res.Sandbox = sb.Describe()
		return sb.Describe(), sb.WaitReady(ctx, 3*time.Minute, client, "-h127.0.0.1", "-uroot", "-e", "SELECT 1")
	}) {
		if sb != nil {
			sb.Remove()
		}
		return
	}
	defer sb.Remove()
	q := func(sqlText string) (string, error) {
		return sb.Exec(ctx, client, "-h127.0.0.1", "-uroot", "-N", "-B", "-e", sqlText)
	}
	rel, _ := filepath.Rel(dir, dump)
	if !r.check("import-dump", func() (string, error) {
		_, err := sb.Exec(ctx, "sh", "-c", client+" -h127.0.0.1 -uroot < /restore/"+filepath.ToSlash(rel))
		return "imported into " + image, err
	}) {
		return
	}
	db := spec.Database
	r.check("check-table", func() (string, error) {
		out, err := sb.Exec(ctx, check, "-h127.0.0.1", "-uroot", "--check", "--databases", db)
		if err != nil {
			return "", err
		}
		var bad []string
		for _, l := range strings.Split(out, "\n") {
			l = strings.TrimSpace(l)
			if l != "" && !strings.HasSuffix(l, "OK") && !strings.Contains(l, "doesn't support check") {
				bad = append(bad, l)
			}
		}
		if len(bad) > 0 {
			return "", fmt.Errorf("%s", strings.Join(bad, "; "))
		}
		return "CHECK TABLE reports OK for every table", nil
	})
	restored := map[string]int64{}
	r.check("exact-row-counts", func() (string, error) {
		gen, err := q(fmt.Sprintf("SELECT GROUP_CONCAT(CONCAT('SELECT CONCAT(''', table_name, '='', COUNT(*)) FROM `%s`.`', table_name, '`') SEPARATOR ' UNION ALL ') FROM information_schema.tables WHERE table_schema='%s' AND table_type='BASE TABLE'",
			strings.ReplaceAll(db, "`", ""), strings.ReplaceAll(db, "'", "''")))
		if err != nil {
			return "", err
		}
		gen = strings.TrimSpace(gen)
		if gen == "" || gen == "NULL" {
			return "no tables", nil
		}
		out, err := q("SET SESSION group_concat_max_len = 1048576; " + gen)
		if err != nil {
			return "", err
		}
		for k, v := range parseLines(out) {
			restored[k] = v
		}
		return fmt.Sprintf("%d tables counted", len(restored)), nil
	})
	if expected, exact := metaCounts(s); len(expected) > 0 {
		r.check("row-count-reconciliation", func() (string, error) {
			return Reconcile(expected, exact, restored, spec.Drill.RowCountTolerance)
		})
	}
	for _, a := range spec.Drill.Assertions {
		a := a
		r.check("assert: "+a.Name, func() (string, error) {
			out, err := sb.Exec(ctx, client, "-h127.0.0.1", "-uroot", "-N", "-B", db, "-e", a.SQL)
			if err != nil {
				return "", err
			}
			v := strings.TrimSpace(lastLine(out))
			if !truthy(v) {
				return "", fmt.Errorf("returned %q", v)
			}
			return "returned " + v, nil
		})
	}
}

func mongoChecks(ctx context.Context, r *runner, dir string, s snapshot.WithID, spec source.Spec) {
	dump, err := findStream(dir, "mongodb")
	if !r.check("locate-dump", func() (string, error) { return filepath.Base(dump), err }) {
		return
	}
	image := spec.Drill.Image
	if image == "" {
		image = "mongo:7"
	}
	if !DockerAvailable(ctx) {
		r.check("sandbox", func() (string, error) { return "", fmt.Errorf("docker is required for MongoDB drills") })
		return
	}
	var sb *Sandbox
	if !r.check("sandbox-start", func() (string, error) {
		sb, err = StartSandbox(ctx, SandboxOptions{Image: image, MountDir: dir, DataTmpfs: "/data/db"})
		if err != nil {
			return "", err
		}
		r.res.Sandbox = sb.Describe()
		return sb.Describe(), sb.WaitReady(ctx, 2*time.Minute, "mongosh", "--quiet", "--eval", "db.runCommand({ping:1}).ok")
	}) {
		if sb != nil {
			sb.Remove()
		}
		return
	}
	defer sb.Remove()
	rel, _ := filepath.Rel(dir, dump)
	oplog, _ := s.SourceMeta["oplog"].(bool)
	if !r.check("mongorestore", func() (string, error) {
		args := []string{"mongorestore", "--archive=/restore/" + filepath.ToSlash(rel), "--stopOnError"}
		if oplog {
			args = append(args, "--oplogReplay")
		}
		_, err := sb.Exec(ctx, args...)
		return "restored into " + image, err
	}) {
		return
	}
	r.check("validate-collections", func() (string, error) {
		out, err := sb.Exec(ctx, "mongosh", "--quiet", "--eval", `
let bad=[], n=0, docs=0;
db.adminCommand({listDatabases:1}).databases.forEach(d=>{
  if(['admin','local','config'].includes(d.name)) return;
  const x=db.getSiblingDB(d.name);
  x.getCollectionNames().forEach(c=>{ n++; const v=x.runCommand({validate:c, full:true}); docs+=x[c].countDocuments({}); if(!v.valid) bad.push(d.name+'.'+c); });
});
print(JSON.stringify({collections:n, docs:docs, invalid:bad}));`)
		if err != nil {
			return "", err
		}
		line := lastLine(out)
		if strings.Contains(line, `"invalid":[]`) {
			return "validate(full) passed: " + line, nil
		}
		return "", fmt.Errorf("invalid collections: %s", line)
	})
}

func parseLines(out string) map[string]int64 {
	res := map[string]int64{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		i := strings.LastIndex(l, "=")
		if i <= 0 {
			continue
		}
		var n int64
		if _, err := fmt.Sscan(strings.TrimSpace(l[i+1:]), &n); err == nil {
			res[strings.TrimSpace(l[:i])] = n
		}
	}
	return res
}
