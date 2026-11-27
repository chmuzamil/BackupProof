package server

import (
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
)

func TestTestsPerMonth(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for cron, want := range map[string]int{"0 4 * * 0": 4, "0 3 * * *": 30, "manual": 0, "0 2 1 * *": 1} {
		if got := testsPerMonth(cron, now); got != want && !(cron == "0 4 * * 0" && got == 5) {
			t.Errorf("%q: %d tests, want %d", cron, got, want)
		}
	}
}

func TestStorageCharges(t *testing.T) {
	for _, c := range []struct {
		cfg              backend.Config
		provider, charge string
	}{
		{backend.Config{Type: "s3"}, "Amazon S3", "charged"},
		{backend.Config{Type: "s3", Endpoint: "https://s3.eu-west-1.amazonaws.com"}, "Amazon S3", "charged"},
		{backend.Config{Type: "s3", Endpoint: "https://abc.r2.cloudflarestorage.com"}, "Cloudflare R2", "free"},
		{backend.Config{Type: "s3", Endpoint: "https://s3.us-west-004.backblazeb2.com"}, "Backblaze B2", "free-limited"},
		{backend.Config{Type: "s3", Endpoint: "https://minio.example.com"}, "S3-compatible storage", ""},
		{backend.Config{Type: "local", Path: "/backups"}, "a disk", "none"},
		{backend.Config{Type: "sftp", Host: "nas"}, "another server (SFTP)", ""},
	} {
		p, ch := storageCharges(c.cfg)
		if p != c.provider || ch != c.charge {
			t.Errorf("%+v: %q %q, want %q %q", c.cfg, p, ch, c.provider, c.charge)
		}
	}
}
