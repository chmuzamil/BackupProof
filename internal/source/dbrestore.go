package source

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Restoring a database backup into a live database. The dump in the backup
// is loaded with the database's own tools, through the same container (or
// local tools and login) the backup used. By default it goes into a new
// database next to the original; replacing the original is explicit.

var dbNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,62}$`)

// ValidDatabaseName reports whether name is safe to create as a database.
func ValidDatabaseName(name string) bool { return dbNameRe.MatchString(name) }

// DumpPath is where a database item's dump sits in its backup.
func DumpPath(s Spec) (string, error) {
	switch s.Kind {
	case "postgres":
		return "postgres/" + s.Database + ".dump", nil
	case "mysql":
		return "mysql/" + s.Database + ".sql", nil
	case "mongodb":
		name := s.Database
		if name == "" {
			name = "all"
		}
		return "mongodb/" + name + ".archive", nil
	case "sqlite":
		if len(s.Paths) == 0 {
			return "", errors.New("the SQLite item has no file")
		}
		return "sqlite/" + filepath.Base(s.Paths[0]), nil
	}
	return "", fmt.Errorf("%s items aren't databases", s.Kind)
}

// RestoreDatabase loads the dump file into target (a new database name), or
// into the original database when replace is true. For SQLite, target is a
// file path and replace overwrites the original file. It returns where the
// data went, in words.
func RestoreDatabase(ctx context.Context, s Spec, dump, target string, replace bool, log func(string, ...any)) (string, error) {
	if !replace && s.Kind != "sqlite" && !ValidDatabaseName(target) {
		return "", fmt.Errorf("%q isn't a valid database name: use letters, digits, _ and -", target)
	}
	switch s.Kind {
	case "postgres":
		return restorePostgres(ctx, s, dump, target, replace, log)
	case "mysql":
		return restoreMySQL(ctx, s, dump, target, replace, log)
	case "mongodb":
		return restoreMongo(ctx, s, dump, target, replace, log)
	case "sqlite":
		return restoreSQLite(s, dump, target, replace, log)
	}
	return "", fmt.Errorf("%s items can't be restored as a database", s.Kind)
}

// runWithInput runs cmd with in as standard input and returns a readable
// error that includes the tool's own message.
func runWithInput(cmd *exec.Cmd, in io.Reader) error {
	var stderr bytes.Buffer
	cmd.Stdin = in
	cmd.Stderr = &limitedBuffer{buf: &stderr, max: 16 << 10}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return err
		}
		return fmt.Errorf("%v: %s", err, msg)
	}
	return nil
}

func restorePostgres(ctx context.Context, s Spec, dump, target string, replace bool, log func(string, ...any)) (string, error) {
	db := s.Database
	if !replace {
		db = target
		log("creating PostgreSQL database %s", db)
		args := append(pgConnArgs(s), "--no-password", "--maintenance-db="+s.Database, db)
		if err := runWithInput(toolCmd(ctx, s, pgEnv(s), "createdb", args...), nil); err != nil {
			return "", fmt.Errorf("creating database %s: %w", db, err)
		}
	}
	f, err := os.Open(dump)
	if err != nil {
		return "", err
	}
	defer f.Close()
	args := append(pgConnArgs(s), "--no-password", "--no-owner", "--no-privileges", "--exit-on-error", "--dbname="+db)
	if replace {
		args = append(args, "--clean", "--if-exists")
	}
	log("loading the backup into PostgreSQL database %s", db)
	// pg_restore reads the archive from standard input, so this works the
	// same way inside a container.
	if err := runWithInput(toolCmd(ctx, s, pgEnv(s), "pg_restore", args...), f); err != nil {
		return "", fmt.Errorf("pg_restore: %w", err)
	}
	return "PostgreSQL database " + db, nil
}

func restoreMySQL(ctx context.Context, s Spec, dump, target string, replace bool, log func(string, ...any)) (string, error) {
	tool := "mysql"
	if s.Container != "" && mariaDB(ctx, s) {
		tool = "mariadb"
	}
	f, err := os.Open(dump)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if replace {
		log("loading the backup into MySQL database %s (tables in the backup are replaced)", s.Database)
		if err := runWithInput(toolCmd(ctx, s, myEnv(s), tool, myConnArgs(s)...), f); err != nil {
			return "", fmt.Errorf("%s: %w", tool, err)
		}
		return "MySQL database " + s.Database, nil
	}
	log("creating MySQL database %s", target)
	create := append(myConnArgs(s), "-e", "CREATE DATABASE `"+target+"`")
	if err := runWithInput(toolCmd(ctx, s, myEnv(s), tool, create...), nil); err != nil {
		return "", fmt.Errorf("creating database %s: %w", target, err)
	}
	// The dump names its own database (--databases); drop those lines so it
	// loads into the new one instead.
	pr, pw := io.Pipe()
	go func() {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		w := bufio.NewWriter(pw)
		for sc.Scan() {
			line := sc.Bytes()
			if bytes.HasPrefix(line, []byte("CREATE DATABASE ")) || bytes.HasPrefix(line, []byte("USE `")) {
				continue
			}
			w.Write(line)
			w.WriteByte('\n')
		}
		if err := sc.Err(); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.CloseWithError(w.Flush())
	}()
	log("loading the backup into MySQL database %s", target)
	if err := runWithInput(toolCmd(ctx, s, myEnv(s), tool, append(myConnArgs(s), target)...), pr); err != nil {
		return "", fmt.Errorf("%s: %w", tool, err)
	}
	return "MySQL database " + target, nil
}

// mariaDB reports whether a container has the mariadb client (newer MariaDB
// images no longer ship a "mysql" command).
func mariaDB(ctx context.Context, s Spec) bool {
	return exec.CommandContext(ctx, "docker", "exec", s.Container, "sh", "-c", "command -v mariadb").Run() == nil
}

func restoreMongo(ctx context.Context, s Spec, dump, target string, replace bool, log func(string, ...any)) (string, error) {
	args := []string{"--archive"}
	if s.URI != "" {
		args = append(args, "--uri="+s.URI)
	} else if s.Host != "" {
		args = append(args, "--host="+s.Host)
		if s.Port != 0 {
			args = append(args, fmt.Sprintf("--port=%d", s.Port))
		}
		if s.User != "" {
			args = append(args, "--username="+s.User, "--authenticationDatabase=admin")
		}
	}
	if s.Password != "" && s.URI == "" && s.Container == "" {
		// As for the backup: the password goes in a config file, not on the process list.
		cfg, err := os.CreateTemp("", "bp-mongo-*.yaml")
		if err != nil {
			return "", err
		}
		defer os.Remove(cfg.Name())
		if _, err := fmt.Fprintf(cfg, "password: %q\n", s.Password); err != nil {
			cfg.Close()
			return "", err
		}
		if err := cfg.Close(); err != nil {
			return "", err
		}
		args = append(args, "--config="+cfg.Name())
	}
	where := "MongoDB"
	switch {
	case replace:
		args = append(args, "--drop")
		if s.Database != "" {
			where = "MongoDB database " + s.Database
		}
	case s.Database == "":
		return "", errors.New("this backup holds every MongoDB database, so it can only replace them; choose “Replace the original”")
	default:
		args = append(args, "--nsFrom="+s.Database+".*", "--nsTo="+target+".*")
		where = "MongoDB database " + target
	}
	f, err := os.Open(dump)
	if err != nil {
		return "", err
	}
	defer f.Close()
	log("loading the backup into %s", where)
	if err := runWithInput(toolCmd(ctx, s, nil, "mongorestore", args...), f); err != nil {
		return "", fmt.Errorf("mongorestore: %w", err)
	}
	return where, nil
}

func restoreSQLite(s Spec, dump, target string, replace bool, log func(string, ...any)) (string, error) {
	if len(s.Paths) == 0 {
		return "", errors.New("the SQLite item has no file")
	}
	dst := s.Paths[0]
	if !replace {
		dst = target
		if dst == "" || !filepath.IsAbs(dst) {
			return "", errors.New("give the full path for the restored database file")
		}
		if _, err := os.Stat(dst); err == nil {
			return "", fmt.Errorf("%s already exists; choose another name", dst)
		}
	}
	in, err := os.Open(dump)
	if err != nil {
		return "", err
	}
	defer in.Close()
	tmp := dst + ".bp-restore"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if fi, err := os.Stat(dst); err == nil {
		_ = os.Chmod(tmp, fi.Mode().Perm())
		copyOwner(tmp, fi)
	}
	// A database open while it's replaced keeps using the old file; the
	// dashboard asks people to stop the app first.
	_ = os.Remove(dst + "-wal")
	_ = os.Remove(dst + "-shm")
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", err
	}
	log("restored SQLite database to %s", dst)
	return "SQLite file " + dst, nil
}
