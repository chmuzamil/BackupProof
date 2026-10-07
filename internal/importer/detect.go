package importer

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/chmuzamil/backupproof/internal/backend"
)

// Repository describes another backup tool's repository found in a storage
// location, so people don't need to know which tool made their backups.
type Repository struct {
	Format string `json:"format"` // restic | kopia | borg (an import format)
	Tool   string `json:"tool"`   // name shown to people: restic, Kopia, BorgBackup
	// Prefix is the repository's folder relative to the listed location
	// ("" when the repository is at the top).
	Prefix string `json:"prefix"`
}

var (
	// restic: keys/<sha256>, snapshots/<sha256>, index/<sha256>, data/<2 hex>/<sha256>
	resticMarker = regexp.MustCompile(`^(.*?)(?:(?:keys|snapshots|index|locks)/[0-9a-f]{64}|data/[0-9a-f]{2}/[0-9a-f]{64})$`)
	// Kopia: kopia.repository(.f) at the repository root.
	kopiaMarker = regexp.MustCompile(`^(.*?)kopia\.repository(?:\.f)?$`)
	// Borg: numbered segment files data/<n>/<n> next to a config file.
	borgMarker = regexp.MustCompile(`^(.*?)data/\d+/\d+$`)
)

// DetectKeys recognises a repository from object keys (in listing order).
func DetectKeys(keys []string) *Repository {
	configs := map[string]bool{}
	for _, k := range keys {
		if k == "config" || strings.HasSuffix(k, "/config") {
			configs[strings.TrimSuffix(k, "config")] = true
		}
	}
	for _, k := range keys {
		if m := kopiaMarker.FindStringSubmatch(k); m != nil {
			return &Repository{Format: "kopia", Tool: "Kopia", Prefix: strings.TrimSuffix(m[1], "/")}
		}
		if m := resticMarker.FindStringSubmatch(k); m != nil && configs[m[1]] {
			return &Repository{Format: "restic", Tool: "restic", Prefix: strings.TrimSuffix(m[1], "/")}
		}
		if m := borgMarker.FindStringSubmatch(k); m != nil && configs[m[1]] {
			return &Repository{Format: "borg", Tool: "BorgBackup", Prefix: strings.TrimSuffix(m[1], "/")}
		}
	}
	return nil
}

// DetectRepository lists the first objects of a storage location and recognises a
// restic, Kopia or Borg repository there or in a subfolder.
func DetectRepository(ctx context.Context, be backend.Backend) (*Repository, error) {
	const limit = 5000
	var keys []string
	errStop := errors.New("stop")
	err := be.List(ctx, "", func(o backend.ObjectInfo) error {
		keys = append(keys, o.Key)
		if len(keys) >= limit {
			return errStop
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return nil, err
	}
	return DetectKeys(keys), nil
}

// WithPrefix returns the storage settings pointing at the repository's folder.
func (r *Repository) WithPrefix(cfg backend.Config) backend.Config {
	if r.Prefix == "" {
		return cfg
	}
	switch cfg.Type {
	case "local":
		cfg.Path = filepath.Join(cfg.Path, filepath.FromSlash(r.Prefix))
	case "sftp":
		cfg.Path = path.Join(cfg.Path, r.Prefix)
	default:
		cfg.Prefix = strings.Trim(path.Join(cfg.Prefix, r.Prefix), "/")
	}
	return cfg
}

// errRepository is returned when backup files are imported from a location
// that actually holds another tool's repository.
func errRepository(r *Repository) error {
	where := ""
	if r.Prefix != "" {
		where = fmt.Sprintf(" (in the folder %q)", r.Prefix)
	}
	return fmt.Errorf("these aren't separate backup files: this is a %s repository%s. Import it again and choose %s, with its %s password", r.Tool, where, r.Tool, r.Tool)
}
