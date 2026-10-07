// Package backend abstracts the object storage a repository lives on.
// Keys are slash-separated relative paths such as "data/ab/abcd…".
package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

var ErrNotFound = errors.New("object not found")

type ObjectInfo struct {
	Key      string
	Size     int64
	Modified time.Time
}

type Backend interface {
	// Put stores data under key atomically. Existing keys are overwritten.
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	// Open streams an object; used for large foreign objects during import.
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	// List calls fn for every object below prefix.
	List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error
	Delete(ctx context.Context, key string) error
	// Location is a human readable, credential-free description.
	Location() string
	Close() error
}

// Config describes a storage target. Credentials are passed separately so
// that Config can be logged and stored in plaintext.
type Config struct {
	Type     string `json:"type"` // local | s3 | sftp | rclone
	Path     string `json:"path,omitempty"`
	Bucket   string `json:"bucket,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	Region   string `json:"region,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	User     string `json:"user,omitempty"`
	// ObjectLock applies S3 Object Lock retention to every new object.
	ObjectLockMode string `json:"objectLockMode,omitempty"` // GOVERNANCE | COMPLIANCE
	ObjectLockDays int    `json:"objectLockDays,omitempty"`
	// HostKey pins the SFTP server key (authorized_keys format).
	HostKey string `json:"hostKey,omitempty"`
	// Remote is an rclone remote and folder ("gdrive:Backups") for type rclone;
	// RcloneConfig optionally points at a specific rclone.conf.
	Remote       string `json:"remote,omitempty"`
	RcloneConfig string `json:"rcloneConfig,omitempty"`
}

type Credentials struct {
	AccessKey  string `json:"accessKey,omitempty"`
	SecretKey  string `json:"secretKey,omitempty"`
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"privateKey,omitempty"`
}

// ParseURL accepts local paths, s3://bucket/prefix and sftp://user@host:port/path.
func ParseURL(raw string) (Config, error) {
	if !strings.Contains(raw, "://") {
		return Config{Type: "local", Path: raw}, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Config{}, err
	}
	switch u.Scheme {
	case "file":
		return Config{Type: "local", Path: u.Path}, nil
	case "s3":
		c := Config{Type: "s3", Bucket: u.Host, Prefix: strings.Trim(u.Path, "/")}
		q := u.Query()
		c.Endpoint, c.Region = q.Get("endpoint"), q.Get("region")
		c.ObjectLockMode = strings.ToUpper(q.Get("lock"))
		if d := q.Get("lockDays"); d != "" {
			if _, err := fmt.Sscanf(d, "%d", &c.ObjectLockDays); err != nil {
				return Config{}, fmt.Errorf("invalid lockDays %q: %w", d, err)
			}
		}
		return c, nil
	case "sftp":
		c := Config{Type: "sftp", Host: u.Hostname(), Path: u.Path, User: u.User.Username(), Port: 22}
		if p := u.Port(); p != "" {
			if _, err := fmt.Sscanf(p, "%d", &c.Port); err != nil {
				return Config{}, fmt.Errorf("invalid port %q: %w", p, err)
			}
		}
		return c, nil
	}
	return Config{}, fmt.Errorf("unsupported storage scheme %q", u.Scheme)
}

func Open(ctx context.Context, c Config, creds Credentials) (Backend, error) {
	switch c.Type {
	case "local":
		return NewLocal(c.Path)
	case "s3":
		return NewS3(ctx, c, creds)
	case "sftp":
		return NewSFTP(c, creds)
	case "rclone":
		return NewRclone(c)
	}
	return nil, fmt.Errorf("unsupported storage type %q", c.Type)
}

func ReadAllLimit(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("object larger than %d bytes", limit)
	}
	return b, nil
}
