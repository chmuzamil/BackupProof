package server

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
)

// RestoreDownload estimates what restore tests download from storage: each
// test restores the whole newest backup, so a cloud provider that charges for
// downloads charges for every test.
type RestoreDownload struct {
	PerTestBytes  int64  `json:"perTestBytes"`
	TestsPerMonth int    `json:"testsPerMonth"`
	PerMonthBytes int64  `json:"perMonthBytes"`
	Provider      string `json:"provider"`
	// Charges is how the provider bills downloads: "charged", "free",
	// "free-limited" (free up to an allowance), "none" (a disk on the server
	// running the test) or "" when it isn't known.
	Charges string `json:"charges"`
	// CopyTestsPerMonth: a second copy is restore-tested monthly too.
	CopyTestsPerMonth int `json:"copyTestsPerMonth,omitempty"`
}

// storageCharges names a storage's provider and how it bills downloads.
func storageCharges(cfg backend.Config) (provider, charges string) {
	e := strings.ToLower(cfg.Endpoint)
	switch {
	case cfg.Type == "local":
		return "a disk", "none"
	case cfg.Type == "sftp":
		return "another server (SFTP)", ""
	case cfg.Type != "s3":
		return cfg.Type, ""
	case strings.Contains(e, "r2.cloudflarestorage"):
		return "Cloudflare R2", "free"
	case strings.Contains(e, "backblazeb2"):
		return "Backblaze B2", "free-limited"
	case strings.Contains(e, "wasabisys"):
		return "Wasabi", "free-limited"
	case e == "" || strings.Contains(e, "amazonaws.com"):
		return "Amazon S3", "charged"
	case strings.Contains(e, "storage.googleapis.com"):
		return "Google Cloud Storage", "charged"
	case strings.Contains(e, "digitaloceanspaces"):
		return "DigitalOcean Spaces", "free-limited"
	}
	return "S3-compatible storage", ""
}

// testsPerMonth counts the scheduled restore tests in the next 30 days.
func testsPerMonth(drillCron string, now time.Time) int {
	sched, err := ParseSchedule(drillCron)
	if err != nil {
		return 0
	}
	end := now.Add(30 * 24 * time.Hour)
	n := 0
	for t := sched.Next(now); !t.After(end) && n < 10000; t = sched.Next(t) {
		n++
	}
	return n
}

// restoreDownload is the estimate for one item, from its newest backup's size.
func (s *Server) restoreDownload(src Source, lastBackup *ProofRow, now time.Time) *RestoreDownload {
	if lastBackup == nil || len(lastBackup.Predicate) == 0 {
		return nil
	}
	var p struct {
		Bytes int64 `json:"bytes"`
	}
	if json.Unmarshal(lastBackup.Predicate, &p) != nil || p.Bytes <= 0 {
		return nil
	}
	repo, _, err := s.store.Repository(src.RepoID)
	if err != nil {
		return nil
	}
	d := &RestoreDownload{PerTestBytes: p.Bytes, TestsPerMonth: testsPerMonth(src.DrillCron, now)}
	d.Provider, d.Charges = storageCharges(repo.Backend)
	d.PerMonthBytes = d.PerTestBytes * int64(d.TestsPerMonth)
	if src.CopyRepoID != nil {
		d.CopyTestsPerMonth = 1
	}
	return d
}
