// Command compatgen makes compatibility fixtures from a running BackupProof
// dashboard of any 0.2.x release, through its HTTP API only, so the same
// program works against every release. It protects a folder (with a second
// copy), an SQLite file and, if given, a PostgreSQL database, runs backups and
// restore tests, then saves every proof bundle, the evidence pack, the public
// keys and a manifest of what that release produced. See docs/COMPATIBILITY.md.
//
//	compatgen -url http://127.0.0.1:8420 -work /tmp/w -out fixtures/v0.2.5 [-postgres host:port:user:password:db]
package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type client struct {
	base string
	c    *http.Client
	csrf string
}

// do sends a JSON request and decodes a JSON answer into out (when not nil).
func (a *client) do(method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.base+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.csrf != "" {
		req.Header.Set("X-CSRF-Token", a.csrf)
	}
	resp, err := a.c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		if p, ok := out.(*[]byte); ok {
			*p = raw
			return resp.StatusCode, nil
		}
		return resp.StatusCode, json.Unmarshal(raw, out)
	}
	return resp.StatusCode, nil
}

type job struct {
	ID       int64
	Kind     string
	State    string
	SourceID int64
	Error    string
}

func main() {
	url := flag.String("url", "http://127.0.0.1:8420", "dashboard address")
	work := flag.String("work", "", "working folder for storage and source data")
	out := flag.String("out", "", "folder for the fixtures")
	pg := flag.String("postgres", "", "host:port:user:password:database of a PostgreSQL to protect")
	flag.Parse()
	if *work == "" || *out == "" {
		log.Fatal("-work and -out are required")
	}
	if err := run(*url, *work, *out, *pg); err != nil {
		log.Fatal(err)
	}
}

func run(url, work, out, pg string) error {
	jar, _ := cookiejar.New(nil)
	a := &client{base: url, c: &http.Client{Jar: jar, Timeout: 2 * time.Minute}}
	for i := 0; ; i++ {
		var st struct{ Version string }
		if _, err := a.do("GET", "/api/status", nil, &st); err == nil {
			log.Printf("dashboard %s", st.Version)
			break
		}
		if i == 60 {
			return errors.New("dashboard didn't start")
		}
		time.Sleep(time.Second)
	}
	var setup struct{ CSRF string }
	if _, err := a.do("POST", "/api/setup", map[string]string{"username": "admin", "password": "compat-fixture-admin"}, &setup); err != nil {
		return err
	}
	a.csrf = setup.CSRF

	var agents []struct{ ID int64 }
	for i := 0; len(agents) == 0; i++ {
		if _, err := a.do("GET", "/api/agents", nil, &agents); err != nil {
			return err
		}
		if i == 60 {
			return errors.New("the built-in agent never connected")
		}
		time.Sleep(time.Second)
	}
	agentID := agents[0].ID

	repo := func(name string) (int64, error) {
		var r struct{ ID int64 }
		_, err := a.do("POST", "/api/repositories", map[string]any{"name": name,
			"backend": map[string]any{"type": "local", "path": filepath.Join(work, name)}, "password": "compat-fixture-repository"}, &r)
		return r.ID, err
	}
	main, err := repo("main")
	if err != nil {
		return err
	}
	second, err := repo("second")
	if err != nil {
		return err
	}

	// What to protect.
	files := filepath.Join(work, "files")
	if err := os.MkdirAll(filepath.Join(files, "docs"), 0o755); err != nil {
		return err
	}
	_ = os.WriteFile(filepath.Join(files, "readme.txt"), []byte("BackupProof compatibility fixture\n"), 0o644)
	_ = os.WriteFile(filepath.Join(files, "docs", "notes.txt"), []byte("a nested file\n"), 0o644)
	dbPath := filepath.Join(work, "app.db")
	if err := makeSQLite(dbPath); err != nil {
		return err
	}
	type item struct {
		name   string
		spec   map[string]any
		secret map[string]string
	}
	items := []item{
		{"files", map[string]any{"kind": "files", "paths": []string{files}}, nil},
		{"sqlite", map[string]any{"kind": "sqlite", "paths": []string{dbPath},
			"drill": map[string]any{"assertions": []map[string]string{{"name": "invoices present", "sql": "SELECT count(*) = 50 FROM invoices"}}}}, nil},
	}
	if pg != "" {
		p := strings.SplitN(pg, ":", 5)
		if len(p) != 5 {
			return errors.New("-postgres needs host:port:user:password:database")
		}
		var port int
		fmt.Sscan(p[1], &port)
		items = append(items, item{"postgres", map[string]any{"kind": "postgres", "host": p[0], "port": port, "user": p[2], "database": p[4],
			"drill": map[string]any{"assertions": []map[string]string{{"name": "orders present", "sql": "SELECT count(*) = 20 FROM orders"}}}},
			map[string]string{"password": p[3]}})
	}

	ids := map[string]int64{}
	for _, it := range items {
		body := map[string]any{"name": it.name, "agentId": agentID, "repoId": main, "spec": it.spec,
			"backupCron": "@daily", "drillCron": "0 4 * * 0", "enabled": true}
		if it.secret != nil {
			body["secret"] = it.secret
		}
		var s struct{ ID int64 }
		if _, err := a.do("POST", "/api/sources", body, &s); err != nil {
			return err
		}
		ids[it.name] = s.ID
	}
	if _, err := a.do("PUT", fmt.Sprintf("/api/sources/%d/copy", ids["files"]), map[string]any{"repoId": second}, nil); err != nil {
		return err
	}

	runJob := func(id int64, kind string) error {
		var r struct{ JobID int64 }
		if _, err := a.do("POST", fmt.Sprintf("/api/sources/%d/run", id), map[string]string{"kind": kind}, &r); err != nil {
			return err
		}
		return waitIdle(a)
	}
	// An older release may not support everything; what it couldn't do is
	// recorded in the manifest instead of stopping the run.
	var problems []string
	for _, it := range items {
		// The first backup queues a restore test (and, for files, a copy).
		for _, kind := range []string{"backup", "drill"} {
			if err := runJob(ids[it.name], kind); err != nil {
				problems = append(problems, fmt.Sprintf("%s %s: %v", it.name, kind, err))
			}
		}
	}
	if err := runJob(ids["files"], "copy-drill"); err != nil {
		problems = append(problems, fmt.Sprintf("files copy-drill: %v", err))
	}

	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	var keys []byte
	if _, err := a.do("GET", "/api/public/keys", nil, &keys); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "keys.json"), keys, 0o644); err != nil {
		return err
	}
	var proofs []struct {
		ID         int64
		Kind       string
		SourceName string
	}
	if _, err := a.do("GET", "/api/proofs", nil, &proofs); err != nil {
		return err
	}
	kinds := map[string]bool{}
	seen := map[string]int{}
	for _, p := range proofs {
		var b []byte
		if _, err := a.do("GET", fmt.Sprintf("/api/proofs/%d/bundle", p.ID), nil, &b); err != nil {
			return err
		}
		name := p.Kind + "-" + p.SourceName
		seen[name]++
		if seen[name] > 1 {
			name = fmt.Sprintf("%s-%d", name, seen[name])
		}
		if err := os.WriteFile(filepath.Join(out, name+".bundle.json"), b, 0o644); err != nil {
			return err
		}
		kinds[p.Kind+"/"+p.SourceName] = true
		if strings.Contains(string(b), `"timestamp":`) {
			kinds["timestamp"] = true
		}
	}
	var pack []byte
	if _, err := a.do("GET", "/api/evidence", nil, &pack); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "evidence.json"), pack, 0o644); err != nil {
		return err
	}
	kinds["evidence"] = true
	var list []string
	for k := range kinds {
		list = append(list, k)
	}
	sort.Strings(list)
	man, _ := json.MarshalIndent(map[string]any{"categories": list, "proofs": len(proofs), "problems": problems}, "", "  ")
	log.Printf("%d proofs: %s", len(proofs), strings.Join(list, " "))
	return os.WriteFile(filepath.Join(out, "manifest.json"), append(man, '\n'), 0o644)
}

// waitIdle waits until no job is queued or running.
func waitIdle(a *client) error {
	deadline := time.Now().Add(5 * time.Minute)
	for {
		time.Sleep(time.Second)
		var jobs []job
		if _, err := a.do("GET", "/api/jobs", nil, &jobs); err != nil {
			return err
		}
		busy := false
		for _, j := range jobs {
			if j.State == "queued" || j.State == "running" {
				busy = true
			}
		}
		if !busy {
			for _, j := range jobs {
				if j.State == "failed" {
					return fmt.Errorf("job %d (%s) failed: %s", j.ID, j.Kind, j.Error)
				}
			}
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("jobs still running after 5 minutes")
		}
	}
}

func makeSQLite(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE invoices (id INTEGER PRIMARY KEY, amount INTEGER)"); err != nil {
		return err
	}
	for i := 1; i <= 50; i++ {
		if _, err := db.Exec("INSERT INTO invoices (amount) VALUES (?)", i*100); err != nil {
			return err
		}
	}
	return nil
}
