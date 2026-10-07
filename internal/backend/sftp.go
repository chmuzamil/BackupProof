package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type SFTP struct {
	conn   *ssh.Client
	client *sftp.Client
	root   string
	loc    string
}

func NewSFTP(c Config, creds Credentials) (*SFTP, error) {
	var auth []ssh.AuthMethod
	if creds.PrivateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(creds.PrivateKey))
		if err != nil {
			return nil, fmt.Errorf("sftp: parse private key: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if creds.Password != "" {
		auth = append(auth, ssh.Password(creds.Password))
	}
	if len(auth) == 0 {
		return nil, errors.New("sftp: a password or private key is required")
	}
	if c.HostKey == "" {
		return nil, errors.New("sftp: hostKey is required (pin the server key, e.g. from ssh-keyscan)")
	}
	pinned, _, _, _, err := ssh.ParseAuthorizedKey([]byte(c.HostKey))
	if err != nil {
		return nil, fmt.Errorf("sftp: parse hostKey: %w", err)
	}
	port := c.Port
	if port == 0 {
		port = 22
	}
	conn, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", c.Host, port), &ssh.ClientConfig{
		User:            c.User,
		Auth:            auth,
		HostKeyCallback: ssh.FixedHostKey(pinned),
	})
	if err != nil {
		return nil, err
	}
	client, err := sftp.NewClient(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &SFTP{conn: conn, client: client, root: c.Path, loc: fmt.Sprintf("sftp://%s@%s:%d%s", c.User, c.Host, port, c.Path)}, nil
}

func (s *SFTP) p(key string) (string, error) {
	clean := path.Clean("/" + key)
	if strings.Contains(key, "..") {
		return "", errors.New("invalid key " + key)
	}
	return path.Join(s.root, clean), nil
}

func (s *SFTP) Put(_ context.Context, key string, data []byte) error {
	p, err := s.p(key)
	if err != nil {
		return err
	}
	if err := s.client.MkdirAll(path.Dir(p)); err != nil {
		return err
	}
	tmp := path.Join(path.Dir(p), ".tmp-"+path.Base(p))
	f, err := s.client.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return s.client.PosixRename(tmp, p)
}

func (s *SFTP) Get(_ context.Context, key string) ([]byte, error) {
	p, err := s.p(key)
	if err != nil {
		return nil, err
	}
	f, err := s.client.Open(p)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func (s *SFTP) Stat(_ context.Context, key string) (ObjectInfo, error) {
	p, err := s.p(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	fi, err := s.client.Stat(p)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
		return ObjectInfo{}, ErrNotFound
	}
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Key: key, Size: fi.Size(), Modified: fi.ModTime()}, nil
}

func (s *SFTP) List(_ context.Context, prefix string, fn func(ObjectInfo) error) error {
	base, err := s.p(prefix)
	if err != nil {
		return err
	}
	w := s.client.Walk(base)
	for w.Step() {
		if err := w.Err(); err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		fi := w.Stat()
		if fi.IsDir() || strings.HasPrefix(fi.Name(), ".tmp-") {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(w.Path(), s.root), "/")
		if err := fn(ObjectInfo{Key: rel, Size: fi.Size(), Modified: fi.ModTime()}); err != nil {
			return err
		}
	}
	return nil
}

func (s *SFTP) Delete(_ context.Context, key string) error {
	p, err := s.p(key)
	if err != nil {
		return err
	}
	err = s.client.Remove(p)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *SFTP) Location() string { return s.loc }

func (s *SFTP) Close() error {
	s.client.Close()
	return s.conn.Close()
}

func (s *SFTP) Open(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := s.p(key)
	if err != nil {
		return nil, err
	}
	f, err := s.client.Open(p)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}
