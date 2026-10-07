package source

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// resolveCredentials fills in database credentials that live on the
// protected machine itself, so users never have to type or store them:
//
//   - WordPress: DB_NAME/DB_USER/DB_PASSWORD/DB_HOST from wp-config.php
//   - Docker:    POSTGRES_* / MYSQL_* / MARIADB_* environment of the container
//
// Values are read at backup time on the agent and never sent to the server.
func resolveCredentials(ctx context.Context, s *Spec) error {
	if s.WPConfig != "" {
		cfg, err := ParseWPConfig(s.WPConfig)
		if err != nil {
			return err
		}
		if s.Database == "" {
			s.Database = cfg["DB_NAME"]
		}
		if s.User == "" {
			s.User = cfg["DB_USER"]
		}
		if s.Password == "" {
			s.Password = cfg["DB_PASSWORD"]
		}
		if s.Host == "" && s.Container == "" {
			host := cfg["DB_HOST"]
			if h, p, ok := strings.Cut(host, ":"); ok {
				host = h
				if n, err := strconv.Atoi(p); err == nil && s.Port == 0 {
					s.Port = n
				}
			}
			if host == "localhost" {
				host = "127.0.0.1"
			}
			s.Host = host
		}
	}
	if s.Container != "" && s.Password == "" {
		env, err := containerEnv(ctx, s.Container)
		if err != nil {
			return err
		}
		switch s.Kind {
		case "postgres":
			if s.User == "" {
				s.User = firstNonEmpty(env["POSTGRES_USER"], "postgres")
			}
			if s.Database == "" {
				s.Database = firstNonEmpty(env["POSTGRES_DB"], s.User)
			}
			s.Password = env["POSTGRES_PASSWORD"]
		case "mysql":
			root := firstNonEmpty(env["MYSQL_ROOT_PASSWORD"], env["MARIADB_ROOT_PASSWORD"])
			if (s.User == "" || s.User == "root") && root != "" {
				s.User, s.Password = "root", root
			} else {
				s.User = firstNonEmpty(s.User, env["MYSQL_USER"], env["MARIADB_USER"])
				s.Password = firstNonEmpty(env["MYSQL_PASSWORD"], env["MARIADB_PASSWORD"])
			}
			if s.Database == "" {
				s.Database = firstNonEmpty(env["MYSQL_DATABASE"], env["MARIADB_DATABASE"])
			}
		}
	}
	if (s.Kind == "postgres" || s.Kind == "mysql") && s.Database == "" {
		return fmt.Errorf("could not work out which %s database to back up; enter its name", s.Kind)
	}
	return nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func containerEnv(ctx context.Context, container string) (map[string]string, error) {
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .Config.Env}}", container).Output()
	if err != nil {
		return nil, fmt.Errorf("cannot inspect container %s: %v", container, err)
	}
	var list []string
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, kv := range list {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env, nil
}

var wpDefine = regexp.MustCompile(`define\s*\(\s*['"](DB_NAME|DB_USER|DB_PASSWORD|DB_HOST)['"]\s*,\s*(?:'((?:[^'\\]|\\.)*)'|"((?:[^"\\]|\\.)*)")\s*\)`)

// ParseWPConfig extracts the database settings from a wp-config.php.
func ParseWPConfig(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("wp-config: %w", err)
	}
	out := map[string]string{}
	for _, m := range wpDefine.FindAllStringSubmatch(string(raw), -1) {
		v := m[2]
		if v == "" {
			v = m[3]
		}
		out[m[1]] = strings.NewReplacer(`\'`, `'`, `\"`, `"`, `\\`, `\`).Replace(v)
	}
	if out["DB_NAME"] == "" {
		return nil, fmt.Errorf("wp-config: DB_NAME not found in %s", path)
	}
	return out, nil
}
