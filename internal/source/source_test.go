package source

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPostgresDumpArgs(t *testing.T) {
	args := strings.Join(PostgresDumpArgs(Spec{Host: "db", Port: 5433, User: "app", Database: "shop", Password: "s3cret"}), " ")
	for _, want := range []string{"-h db", "-p 5433", "-U app", "--format=custom", "--dbname=shop", "--no-password"} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q in %q", want, args)
		}
	}
	if strings.Contains(args, "s3cret") {
		t.Fatal("password must never appear in arguments")
	}
}

func TestMySQLDumpArgs(t *testing.T) {
	args := strings.Join(MySQLDumpArgs(Spec{Host: "db", User: "root", Database: "wp", Password: "pw"}), " ")
	for _, want := range []string{"--single-transaction", "--routines", "--triggers", "--events", "--databases wp"} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(args, "pw") && !strings.Contains(args, "--set-gtid") {
		t.Fatal("password must never appear in arguments")
	}
}

func TestValidateAndRedact(t *testing.T) {
	if (Spec{Name: "x", Kind: "postgres"}).Validate() == nil {
		t.Fatal("postgres without database must fail")
	}
	if (Spec{Name: "x", Kind: "nope"}).Validate() == nil {
		t.Fatal("unknown kind must fail")
	}
	r := Spec{Name: "x", Kind: "mongodb", URI: "mongodb://u:p@h", Password: "p"}.Redacted()
	if r.Password != "" || strings.Contains(r.URI, "u:p") {
		t.Fatal("redaction failed")
	}
}

func TestParseCounts(t *testing.T) {
	got := parseCounts([]string{"public.users=10", "public.\"Order\"=3", "garbage"})
	if got["public.users"] != 10 || got[`public."Order"`] != 3 || len(got) != 2 {
		t.Fatalf("%v", got)
	}
}

func TestExampleSpecsAreValid(t *testing.T) {
	files, _ := filepath.Glob("../../examples/*.json")
	if len(files) == 0 {
		t.Fatal("no example specs found")
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var s Spec
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if err := s.Validate(); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}

func TestContainerDatabaseNeedsNoName(t *testing.T) {
	if err := (Spec{Name: "db", Kind: "postgres", Container: "app-db-1"}).Validate(); err != nil {
		t.Fatalf("a database found in Docker must be accepted without a name: %v", err)
	}
}
