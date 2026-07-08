// Package server implements the central backup server: HTTPS endpoint
// serving the admin web GUI, the admin REST API, the agent WebSocket
// control channel and the agent data-plane endpoints (blob/manifest
// upload/download).
package server

import (
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"centralbackup/internal/server/store"
	"centralbackup/internal/server/storage"
)

type Config struct {
	Listen     string // e.g. ":8443"
	DataDir    string // sqlite db, tls certs
	StorageDir string // blob/manifest storage root (localfs backend)
	ServerName string // comma-separated extra SANs for the generated cert
	CertFile   string // optional externally provided cert
	KeyFile    string
	AgentBins  string // directory of prebuilt agent binaries served at /dl/
}

func ConfigFromEnv() Config {
	get := func(key, def string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return def
	}
	dataDir := get("CB_DATA_DIR", "./data")
	return Config{
		Listen:     get("CB_LISTEN", ":8443"),
		DataDir:    dataDir,
		StorageDir: get("CB_STORAGE_DIR", filepath.Join(dataDir, "storage")),
		ServerName: get("CB_SERVER_NAME", ""),
		CertFile:   get("CB_TLS_CERT", ""),
		KeyFile:    get("CB_TLS_KEY", ""),
		AgentBins:  get("CB_AGENT_BIN_DIR", "./agents"),
	}
}

type Server struct {
	cfg         Config
	store       *store.Store
	storage     storage.Backend
	hub         *Hub
	fingerprint string
	tlsCert     tls.Certificate

	// commitMu serialises snapshot commits against garbage collection:
	// commits take the read lock, GC takes the write lock.
	commitMu sync.RWMutex

	web *webUI
}

func New(cfg Config) (*Server, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "server.db"))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	backend, err := storage.NewLocalFS(cfg.StorageDir)
	if err != nil {
		return nil, fmt.Errorf("open storage: %w", err)
	}

	certFile, keyFile := cfg.CertFile, cfg.KeyFile
	if certFile == "" {
		certFile = filepath.Join(cfg.DataDir, "tls", "cert.pem")
		keyFile = filepath.Join(cfg.DataDir, "tls", "key.pem")
	}
	cert, err := ensureTLSCert(certFile, keyFile, strings.Split(cfg.ServerName, ","))
	if err != nil {
		return nil, fmt.Errorf("tls certificate: %w", err)
	}
	fp, err := certFingerprint(cert)
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:         cfg,
		store:       st,
		storage:     backend,
		hub:         newHub(),
		fingerprint: fp,
		tlsCert:     cert,
	}
	s.web, err = newWebUI(s)
	if err != nil {
		return nil, fmt.Errorf("web ui: %w", err)
	}
	return s, nil
}

func (s *Server) Run() error {
	mux := http.NewServeMux()

	// Agent endpoints (authenticated by agent id + secret).
	mux.HandleFunc("POST /api/agent/enroll", s.handleEnroll)
	mux.HandleFunc("GET /api/agent/ws", s.handleAgentWS)
	mux.HandleFunc("POST /api/agent/blobs/check", s.agentAuth(s.handleBlobCheck))
	mux.HandleFunc("PUT /api/agent/blobs/{hash}", s.agentAuth(s.handleBlobPut))
	mux.HandleFunc("GET /api/agent/blobs/{hash}", s.agentAuth(s.handleBlobGet))
	mux.HandleFunc("POST /api/agent/snapshots", s.agentAuth(s.handleSnapshotCommit))
	mux.HandleFunc("GET /api/agent/manifests/{id}", s.agentAuth(s.handleManifestGet))

	// Admin API (session cookie).
	s.registerAdminAPI(mux)

	// Web GUI.
	s.web.register(mux)

	// Prebuilt agent binaries for the install scripts (not secret).
	mux.Handle("GET /dl/", http.StripPrefix("/dl/", http.FileServer(http.Dir(s.cfg.AgentBins))))

	srv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{s.tlsCert},
			MinVersion:   tls.VersionTLS12,
			// Capped at 1.2: .NET Framework's HttpWebRequest (used by
			// Windows PowerShell 5.1's Invoke-WebRequest) fails the
			// handshake against TLS 1.3's post-handshake NewSessionTicket
			// message ("underlying connection was closed: unexpected error
			// on a send"), even though every other client handles it fine.
			// TLS 1.2 with modern cipher suites has no practical security
			// downside here and sidesteps that whole bug class.
			MaxVersion: tls.VersionTLS12,
		},
	}

	go s.runScheduler()
	go s.runMaintenance()

	log.Printf("central backup server listening on https://%s", s.cfg.Listen)
	log.Printf("TLS certificate SHA-256 fingerprint: %s", s.fingerprint)
	return srv.ListenAndServeTLS("", "")
}
