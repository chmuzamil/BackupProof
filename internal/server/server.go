// Package server is the BackupProof control plane: it stores configuration,
// schedules backups and restore drills, leases jobs to outbound-only agents,
// verifies and ledgers their signed attestations, watches for what did not
// happen, and serves the dashboard and evidence exports.
package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chmuzamil/backupproof/internal/agent"
	"github.com/chmuzamil/backupproof/internal/engine"
	"github.com/chmuzamil/backupproof/internal/proof"
)

//go:embed web
var webFS embed.FS

type Config struct {
	DataDir   string
	Listen    string
	PublicURL string // how agents reach the server, e.g. https://backup.example.com
	TLSCert   string
	TLSKey    string
	// NoLocalAgent disables the built-in agent that protects this machine.
	NoLocalAgent bool
	// Downloads is a folder of agent binaries served to the install scripts.
	Downloads string
}

type Server struct {
	cfg   Config
	store *Store
	key   *proof.Key // signs ledger checkpoints
	log   *log.Logger

	pollMu  sync.Mutex
	waiters map[int64]chan struct{} // agent long-poll wake-ups
}

func loadSecret(dataDir string) ([]byte, error) {
	if env := os.Getenv("BP_SECRET_KEY"); env != "" {
		b, err := hex.DecodeString(strings.TrimSpace(env))
		if err != nil || len(b) != 32 {
			return nil, errors.New("BP_SECRET_KEY must be 64 hex characters")
		}
		return b, nil
	}
	p := filepath.Join(dataDir, "secret.key")
	if b, err := os.ReadFile(p); err == nil {
		k, err := hex.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(k) != 32 {
			return nil, fmt.Errorf("%s is malformed", p)
		}
		return k, nil
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, os.WriteFile(p, []byte(hex.EncodeToString(k)), 0o600)
}

func New(cfg Config) (*Server, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	secret, err := loadSecret(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	st, err := OpenStore(cfg.DataDir, secret)
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	key, err := proof.LoadOrCreateKey(filepath.Join(cfg.DataDir, "server.key"), "server@"+host)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, store: st, key: key, log: log.New(os.Stderr, "backupproof ", log.LstdFlags), waiters: map[int64]chan struct{}{}}
	if st.Setting("tsa") == "" {
		b, _ := json.Marshal(proof.DefaultTSAs)
		st.SetSetting("tsa", string(b))
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)
	s.agentRoutes(mux)
	s.onboardingRoutes(mux)
	static, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(static))
	mux.Handle("GET /", files)
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) Run(ctx context.Context) error {
	go s.schedulerLoop(ctx)
	if !s.cfg.NoLocalAgent {
		go s.runBuiltinAgent(ctx)
	}
	srv := &http.Server{Addr: s.cfg.Listen, Handler: s.Handler(), ReadHeaderTimeout: 15 * time.Second}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sc)
	}()
	if s.store.UserCount() == 0 {
		s.log.Printf("first run: open %s to create the admin account", s.publicURL())
	}
	s.log.Printf("%s listening on %s (data: %s)", engine.Version, s.cfg.Listen, s.cfg.DataDir)
	var err error
	if s.cfg.TLSCert != "" {
		err = srv.ListenAndServeTLS(s.cfg.TLSCert, s.cfg.TLSKey)
	} else {
		s.log.Printf("warning: serving plain HTTP; put BackupProof behind a TLS reverse proxy or pass --tls-cert/--tls-key")
		err = srv.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) publicURL() string {
	if s.cfg.PublicURL != "" {
		return strings.TrimRight(s.cfg.PublicURL, "/")
	}
	if u := s.store.Setting("publicUrl"); u != "" {
		return strings.TrimRight(u, "/")
	}
	addr := s.cfg.Listen
	if strings.HasPrefix(addr, ":") {
		addr = "localhost" + addr
	}
	scheme := "http"
	if s.cfg.TLSCert != "" {
		scheme = "https"
	}
	return scheme + "://" + addr
}

func (s *Server) Close() error { return s.store.Close() }

// --- helpers -------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

func (s *Server) builtinDir() string { return filepath.Join(s.cfg.DataDir, "builtin-agent") }

// loopbackURL is how the built-in agent reaches this process.
func (s *Server) loopbackURL() string {
	host, port, err := net.SplitHostPort(s.cfg.Listen)
	if err != nil {
		host, port = "127.0.0.1", "8420"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	scheme := "http"
	if s.cfg.TLSCert != "" {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

func (s *Server) runBuiltinAgent(ctx context.Context) {
	url := s.loopbackURL()
	for i := 0; i < 50; i++ { // wait for the listener
		if resp, err := (&http.Client{Timeout: time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}).Get(url + "/api/status"); err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	err := agent.StartBuiltin(ctx, url, s.builtinDir(), func() (string, error) {
		return s.store.CreateEnrollToken("system", 5*time.Minute)
	}, []string{s.cfg.DataDir})
	if err != nil && ctx.Err() == nil {
		s.log.Printf("built-in agent stopped: %v", err)
	}
}

// builtinAgentID returns the ID of the embedded agent (0 if none).
func (s *Server) builtinAgentID() int64 {
	if st, err := agent.LoadState(s.builtinDir()); err == nil {
		return st.AgentID
	}
	return 0
}
