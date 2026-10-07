// Package importer converts backups made by other tools into BackupProof
// snapshots, so old restore points become restore-testable and provable:
//
//   - files:  backup files in a bucket, folder or cloud drive (dumps,
//     archives, GPG/OpenSSL/age-encrypted files from any tool or script)
//   - restic: restic repositories (uses the restic program)
//   - kopia:  Kopia repositories (uses the kopia program)
//   - borg:   BorgBackup repositories (uses the borg program)
//
// Cloud drives (Google Drive, Dropbox, OneDrive, …) are reached through the
// "rclone" storage type and imported with the files format.
//
// The original tools verify their own encryption and checksums while
// restoring; any error aborts that restore point instead of importing
// damaged data silently.
package importer

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/engine"
)

// Point is one restore point in the foreign backup set.
type Point struct {
	ID    string    // stable ID in the original tool (used to skip re-imports)
	Label string    // human readable
	Time  time.Time // when the original backup was taken
	Files int
	Bytes int64
}

// Source reads restore points from a foreign backup set.
type Source interface {
	List(ctx context.Context) ([]Point, error)
	// Fill adds the content of point p to b.
	Fill(ctx context.Context, p Point, b *engine.Builder, log engine.Logger) error
	Format() string
	Close() error
}

type Spec struct {
	Format  string         `json:"format"` // files | restic | kopia | borg
	Storage backend.Config `json:"storage"`
	// Credentials and Password are secrets (stored encrypted, never attested).
	Credentials backend.Credentials `json:"credentials,omitempty"`
	Password    string              `json:"password,omitempty"`
	// PasswordFile is read on the converting computer (kopia, borg).
	PasswordFile string `json:"passwordFile,omitempty"`

	// For "files": how encrypted files are decrypted (auto | none | gpg |
	// openssl | age), an optional private key (secret), whether archives are
	// unpacked, and how files are grouped into backup copies.
	Decrypt     string `json:"decrypt,omitempty"`
	PrivateKey  string `json:"privateKey,omitempty"`
	OpenSSLIter int    `json:"opensslIter,omitempty"`
	Unpack      bool   `json:"unpack,omitempty"`
	Grouping    string `json:"grouping,omitempty"`

	// For "restic": reuse the existing job's files on the protected computer
	// (e.g. /etc/restic/env and /etc/restic/password) or give the repository
	// address directly (e.g. "b2:my-bucket:server1").
	ResticEnvFile      string `json:"resticEnvFile,omitempty"`
	ResticPasswordFile string `json:"resticPasswordFile,omitempty"`
	ResticRepository   string `json:"resticRepository,omitempty"`

	// For "kopia": reuse an existing repository connection
	// (e.g. /root/.config/kopia/repository.config) or connect to Storage.
	KopiaConfigFile string `json:"kopiaConfigFile,omitempty"`

	// For "borg": repository address (e.g. ssh://u123@u123.your-storagebox.de:23/./backups
	// or /mnt/backup/borg) and an optional SSH key file on the converting computer.
	BorgRepository string `json:"borgRepository,omitempty"`
	BorgSSHKeyFile string `json:"borgSshKeyFile,omitempty"`
}

// HasLocation reports whether the spec says where the old backups are.
func (s Spec) HasLocation() bool {
	switch s.Format {
	case "restic":
		return s.Storage.Type != "" || s.ResticEnvFile != "" || s.ResticRepository != ""
	case "kopia":
		return s.Storage.Type != "" || s.KopiaConfigFile != ""
	case "borg":
		return s.BorgRepository != ""
	}
	return s.Storage.Type != ""
}

func Open(ctx context.Context, s Spec) (Source, error) {
	if s.Storage.Type == "local" && s.Storage.Path != "" {
		if fi, err := os.Stat(s.Storage.Path); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("folder not found: %s", s.Storage.Path)
		}
	}
	switch s.Format {
	case "files":
		be, err := backend.Open(ctx, s.Storage, s.Credentials)
		if err != nil {
			return nil, err
		}
		return newFilesSource(be, s), nil
	case "restic":
		return newRestic(s)
	case "kopia":
		return newKopia(ctx, s)
	case "borg":
		return newBorg(s)
	}
	return nil, errUnknownFormat(s.Format)
}

type errUnknownFormat string

func (e errUnknownFormat) Error() string {
	return "unknown import format " + string(e) + " (use files, restic, kopia or borg)"
}

// readSecretFile returns the trimmed contents of a password file.
func readSecretFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", path, err)
	}
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s, nil
}
