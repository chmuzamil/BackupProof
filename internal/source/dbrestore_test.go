package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForRestoreWorksOutTheDatabase(t *testing.T) {
	ctx := context.Background()
	wp := filepath.Join(t.TempDir(), "wp-config.php")
	config := `<?php define('DB_NAME', 'renamed_since'); define('DB_USER', 'wp'); define('DB_PASSWORD', 'secret'); define('DB_HOST', 'localhost:3307');`
	if err := os.WriteFile(wp, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	// The name comes from the dump in the backup, the login from wp-config.
	s, dump, err := ForRestore(ctx, Spec{Kind: "mysql", WPConfig: wp}, []string{"mysql/shop.sql", "other/file.sql"})
	if err != nil {
		t.Fatal(err)
	}
	if dump != "mysql/shop.sql" || s.Database != "shop" || s.User != "wp" || s.Password != "secret" || s.Host != "127.0.0.1" || s.Port != 3307 {
		t.Fatalf("dump=%s spec=%+v", dump, s)
	}

	// PostgreSQL's globals dump isn't the database.
	s, dump, err = ForRestore(ctx, Spec{Kind: "postgres", WPConfig: wp}, []string{"postgres/globals.sql", "postgres/app.dump"})
	if err != nil || dump != "postgres/app.dump" || s.Database != "app" {
		t.Fatalf("dump=%s spec=%+v err=%v", dump, s, err)
	}

	// A named item keeps its name.
	if s, dump, err = ForRestore(ctx, Spec{Kind: "postgres", Database: "named", Host: "db"}, nil); err != nil || dump != "postgres/named.dump" || s.Database != "named" {
		t.Fatalf("dump=%s spec=%+v err=%v", dump, s, err)
	}

	if _, _, err := ForRestore(ctx, Spec{Kind: "mysql", WPConfig: wp}, []string{"mysql/a.sql", "mysql/b.sql"}); err == nil {
		t.Error("two dumps: no error")
	}
	if _, _, err := ForRestore(ctx, Spec{Kind: "mysql", WPConfig: wp}, nil); err == nil {
		t.Error("no dump: no error")
	}
	_, _, err = ForRestore(ctx, Spec{Kind: "mysql", WPConfig: wp + ".gone"}, []string{"mysql/shop.sql"})
	if err == nil || !strings.Contains(err.Error(), "put them back first") {
		t.Errorf("lost wp-config: %v", err)
	}
}
