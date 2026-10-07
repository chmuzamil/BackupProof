package source

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/chmuzamil/backupproof/internal/engine"
	_ "modernc.org/sqlite"
)

func pgConnArgs(s Spec) []string {
	var a []string
	if s.Host != "" {
		a = append(a, "-h", s.Host)
	}
	if s.Port != 0 {
		a = append(a, "-p", strconv.Itoa(s.Port))
	}
	if s.User != "" {
		a = append(a, "-U", s.User)
	}
	return a
}

func pgEnv(s Spec) map[string]string {
	if s.Password == "" {
		return nil
	}
	return map[string]string{"PGPASSWORD": s.Password}
}

// PostgresDumpArgs returns the pg_dump arguments: custom format (compressed
// by the repository instead), one consistent REPEATABLE READ snapshot.
func PostgresDumpArgs(s Spec) []string {
	args := append(pgConnArgs(s), "--format=custom", "--compress=0", "--no-password", "--dbname="+s.Database)
	return args
}

func backupPostgres(ctx context.Context, s Spec, b *engine.Builder, log engine.Logger) (map[string]any, error) {
	meta := map[string]any{"database": s.Database}
	// Server version and per-table row estimates for drill reconciliation.
	q := `SELECT current_setting('server_version_num') UNION ALL ` +
		`SELECT quote_ident(schemaname)||'.'||quote_ident(relname)||'='||n_live_tup FROM pg_stat_user_tables`
	psql := toolCmd(ctx, s, pgEnv(s), "psql", append(pgConnArgs(s), "-X", "-At", "--no-password", "-d", s.Database, "-c", q)...)
	if out, err := runOutput(psql); err != nil {
		log("warning: could not read table statistics: %v", err)
	} else {
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) > 0 {
			if v, err := strconv.Atoi(strings.TrimSpace(lines[0])); err == nil {
				meta["serverVersionNum"] = v
				meta["serverMajor"] = v / 10000
			}
			meta["tables"] = parseCounts(lines[1:])
			meta["countsExact"] = false
		}
	}
	log("dumping PostgreSQL database %s", s.Database)
	n, err := streamCommand(ctx, b, "postgres/"+s.Database+".dump", toolCmd(ctx, s, pgEnv(s), "pg_dump", PostgresDumpArgs(s)...))
	if err != nil {
		return nil, err
	}
	meta["dumpBytes"] = n
	meta["dumpFormat"] = "pg_dump-custom"
	if s.Globals {
		log("dumping PostgreSQL globals")
		if _, err := streamCommand(ctx, b, "postgres/globals.sql", toolCmd(ctx, s, pgEnv(s), "pg_dumpall", append(pgConnArgs(s), "--globals-only", "--no-password")...)); err != nil {
			return nil, err
		}
	}
	return meta, nil
}

func parseCounts(lines []string) map[string]int64 {
	out := map[string]int64{}
	for _, l := range lines {
		l = strings.TrimSpace(l)
		i := strings.LastIndex(l, "=")
		if i <= 0 {
			i = strings.LastIndex(l, "\t")
		}
		if i <= 0 {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(l[i+1:]), 10, 64); err == nil {
			out[strings.TrimSpace(l[:i])] = n
		}
	}
	return out
}

func myConnArgs(s Spec) []string {
	var a []string
	if s.Host != "" {
		a = append(a, "-h", s.Host)
	}
	if s.Port != 0 {
		a = append(a, "-P", strconv.Itoa(s.Port))
	}
	if s.User != "" {
		a = append(a, "-u", s.User)
	}
	return a
}

func myEnv(s Spec) map[string]string {
	if s.Password == "" {
		return nil
	}
	return map[string]string{"MYSQL_PWD": s.Password}
}

// MySQLDumpArgs: --single-transaction gives a consistent InnoDB snapshot
// without locking; routines, triggers and events are included so the
// restored schema is complete.
func MySQLDumpArgs(s Spec) []string {
	return append(myConnArgs(s), "--single-transaction", "--quick", "--routines", "--triggers", "--events",
		"--hex-blob", "--set-gtid-purged=OFF", "--databases", s.Database)
}

func backupMySQL(ctx context.Context, s Spec, b *engine.Builder, log engine.Logger) (map[string]any, error) {
	meta := map[string]any{"database": s.Database}
	q := fmt.Sprintf("SELECT VERSION() UNION ALL SELECT CONCAT(table_name,'=',IFNULL(table_rows,0)) FROM information_schema.tables WHERE table_schema='%s' AND table_type='BASE TABLE'",
		strings.ReplaceAll(s.Database, "'", "''"))
	cli := toolCmd(ctx, s, myEnv(s), "mysql", append(myConnArgs(s), "-N", "-B", "-e", q)...)
	if out, err := runOutput(cli); err != nil {
		log("warning: could not read table statistics: %v", err)
	} else {
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) > 0 {
			meta["serverVersion"] = strings.TrimSpace(lines[0])
			meta["tables"] = parseCounts(lines[1:])
			meta["countsExact"] = false
		}
	}
	log("dumping MySQL database %s", s.Database)
	tool := "mysqldump"
	if v, _ := meta["serverVersion"].(string); strings.Contains(strings.ToLower(v), "mariadb") {
		tool = "mariadb-dump"
	}
	args := MySQLDumpArgs(s)
	if tool == "mariadb-dump" {
		args = removeArg(args, "--set-gtid-purged=OFF")
	}
	n, err := streamCommand(ctx, b, "mysql/"+s.Database+".sql", toolCmd(ctx, s, myEnv(s), tool, args...))
	if err != nil {
		return nil, err
	}
	meta["dumpBytes"] = n
	meta["dumpTool"] = tool
	return meta, nil
}

func removeArg(args []string, drop string) []string {
	out := args[:0:0]
	for _, a := range args {
		if a != drop {
			out = append(out, a)
		}
	}
	return out
}

func backupMongo(ctx context.Context, s Spec, b *engine.Builder, log engine.Logger) (map[string]any, error) {
	args := []string{"--archive"}
	if s.URI != "" {
		args = append(args, "--uri="+s.URI)
	} else if s.Host != "" {
		args = append(args, "--host="+s.Host)
		if s.Port != 0 {
			args = append(args, "--port="+strconv.Itoa(s.Port))
		}
		if s.User != "" {
			args = append(args, "--username="+s.User, "--authenticationDatabase=admin")
		}
	}
	if s.Password != "" && s.URI == "" && s.Container == "" {
		// Keep the password off the process list: mongodump reads it from --config.
		cfg, err := os.CreateTemp("", "bp-mongo-*.yaml")
		if err != nil {
			return nil, err
		}
		defer os.Remove(cfg.Name())
		if _, err := fmt.Fprintf(cfg, "password: %q\n", s.Password); err != nil {
			cfg.Close()
			return nil, err
		}
		if err := cfg.Close(); err != nil {
			return nil, err
		}
		args = append(args, "--config="+cfg.Name())
	}
	if s.Database != "" {
		args = append(args, "--db="+s.Database)
	} else {
		// Point-in-time consistency across the whole replica set.
		args = append(args, "--oplog")
	}
	name := s.Database
	if name == "" {
		name = "all"
	}
	log("dumping MongoDB %s", name)
	n, err := streamCommand(ctx, b, "mongodb/"+name+".archive", toolCmd(ctx, s, nil, "mongodump", args...))
	if err != nil {
		return nil, err
	}
	return map[string]any{"database": s.Database, "dumpBytes": n, "oplog": s.Database == ""}, nil
}

// backupSQLite takes a transactionally consistent copy with VACUUM INTO
// (safe while the application is writing), records exact row counts from
// that copy, verifies it, then stores it.
func backupSQLite(ctx context.Context, s Spec, b *engine.Builder, log engine.Logger) (map[string]any, error) {
	src, err := filepath.Abs(s.Paths[0])
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(src); err != nil {
		return nil, fmt.Errorf("sqlite database: %w", err)
	}
	tmpDir, err := os.MkdirTemp("", "bp-sqlite-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)
	copyPath := filepath.Join(tmpDir, "snapshot.db")

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(src)+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, err
	}
	_, err = db.ExecContext(ctx, "VACUUM INTO ?", copyPath)
	db.Close()
	if err != nil {
		return nil, fmt.Errorf("VACUUM INTO: %w", err)
	}
	counts, integrity, err := SQLiteInspect(ctx, copyPath)
	if err != nil {
		return nil, err
	}
	if integrity != "ok" {
		return nil, fmt.Errorf("source database failed integrity_check: %s", integrity)
	}
	f, err := os.Open(copyPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	name := "sqlite/" + filepath.Base(src)
	log("storing consistent SQLite copy of %s (%d tables)", src, len(counts))
	if _, err := b.AddStream(ctx, name, f); err != nil {
		return nil, err
	}
	return map[string]any{"path": src, "stream": name, "tables": counts, "countsExact": true, "integrity": integrity}, nil
}

// SQLiteInspect returns exact row counts per table and the integrity_check result.
func SQLiteInspect(ctx context.Context, path string) (map[string]int64, string, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		return nil, "", err
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return nil, "", fmt.Errorf("integrity_check: %w", err)
	}
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, "", err
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, "", err
		}
		tables = append(tables, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	counts := map[string]int64{}
	for _, t := range tables {
		var n int64
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "`+strings.ReplaceAll(t, `"`, `""`)+`"`).Scan(&n); err != nil {
			return nil, "", err
		}
		counts[t] = n
	}
	return counts, integrity, nil
}
