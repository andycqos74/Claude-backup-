// Package server implements the central backup server: HTTPS endpoint
// serving the admin web GUI, the admin REST API, the agent WebSocket
// control channel and the agent data-plane endpoints (blob/manifest
// upload/download).
package server

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"centralbackup/internal/server/storage"
	"centralbackup/internal/server/store"
)

type Config struct {
	Listen     string // e.g. ":8443"
	GUIListen  string // optional plain-HTTP GUI-only listener, e.g. "127.0.0.1:8080"
	DataDir    string // sqlite db, tls certs
	StorageDir string // blob/manifest storage root (localfs backend)
	ServerName string // comma-separated extra SANs for the generated cert
	PublicURL  string // externally reachable origin for agents, e.g. https://backup.example.com:8443
	GUIURL     string // browser-facing origin of the GUI when proxied, e.g. https://backup.example.com
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
		GUIListen:  get("CB_GUI_LISTEN", ""),
		DataDir:    dataDir,
		StorageDir: get("CB_STORAGE_DIR", filepath.Join(dataDir, "storage")),
		ServerName: get("CB_SERVER_NAME", ""),
		PublicURL:  get("CB_PUBLIC_URL", ""),
		GUIURL:     get("CB_GUI_URL", ""),
		CertFile:   get("CB_TLS_CERT", ""),
		KeyFile:    get("CB_TLS_KEY", ""),
		AgentBins:  get("CB_AGENT_BIN_DIR", "./agents"),
	}
}

type Server struct {
	cfg         Config
	store       *store.Store
	hub         *Hub
	fingerprint string
	tlsCert     tls.Certificate

	// storageMu guards the active backend and its ID, which can be swapped
	// at runtime when the admin changes the storage provider. Read them via
	// backend() and backendKey().
	storageMu        sync.RWMutex
	storageActive    storage.Backend
	storageBackendID string

	// commitMu serialises snapshot commits against garbage collection:
	// commits take the read lock, GC takes the write lock.
	commitMu sync.RWMutex

	// oauth holds the in-flight storage "connect" flow state.
	oauth oauthFlow

	// docker correlates in-flight container-discovery requests with the
	// agent replies that answer them.
	docker dockerDiscovery

	// browse correlates in-flight directory-listing requests (the file
	// browser) with the agent replies that answer them.
	browse browseRequests

	web *webUI
}

// backend returns the currently active storage backend.
func (s *Server) backend() storage.Backend {
	s.storageMu.RLock()
	defer s.storageMu.RUnlock()
	return s.storageActive
}

func New(cfg Config) (*Server, error) {
	// Validate before touching disk so a typo fails fast and loudly rather
	// than silently producing unusable enrollment commands.
	publicURL, err := normalizePublicURL(cfg.PublicURL)
	if err != nil {
		return nil, err
	}
	cfg.PublicURL = publicURL

	guiURL, err := normalizeGUIURL(cfg.GUIURL)
	if err != nil {
		return nil, err
	}
	cfg.GUIURL = guiURL

	if cfg.GUIListen != "" {
		if _, _, err := net.SplitHostPort(cfg.GUIListen); err != nil {
			return nil, fmt.Errorf("CB_GUI_LISTEN is not a host:port address (got %q): %w", cfg.GUIListen, err)
		}
		// The GUI listener exists to be fronted by a TLS-terminating proxy,
		// whose hostname is not an address agents can pin. Without an
		// explicit agent address, enrollment commands would fall back to
		// CB_SERVER_NAME and quietly depend on it being right.
		if cfg.PublicURL == "" {
			return nil, fmt.Errorf("CB_GUI_LISTEN is set, so CB_PUBLIC_URL is required: " +
				"it is the direct address agents connect to (e.g. https://backup.example.com:8443), " +
				"which is never the proxied GUI address")
		}
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "server.db"))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
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
		hub:         newHub(),
		fingerprint: fp,
		tlsCert:     cert,
	}
	if err := s.initStorage(); err != nil {
		return nil, fmt.Errorf("open storage: %w", err)
	}
	s.web, err = newWebUI(s)
	if err != nil {
		return nil, fmt.Errorf("web ui: %w", err)
	}
	return s, nil
}

// registerAgentAPI registers the agent-facing endpoints (authenticated by
// agent id + secret). These are deliberately absent from the GUI listener:
// agents pin this server's certificate fingerprint, so they must always
// reach it directly rather than through a TLS-terminating proxy, and a
// proxied GUI should not expose enrollment or blob storage to the internet.
func (s *Server) registerAgentAPI(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/agent/enroll", s.handleEnroll)
	mux.HandleFunc("GET /api/agent/ws", s.handleAgentWS)
	mux.HandleFunc("POST /api/agent/blobs/check", s.agentAuth(s.handleBlobCheck))
	mux.HandleFunc("PUT /api/agent/blobs/{hash}", s.agentAuth(s.handleBlobPut))
	mux.HandleFunc("GET /api/agent/blobs/{hash}", s.agentAuth(s.handleBlobGet))
	mux.HandleFunc("POST /api/agent/snapshots", s.agentAuth(s.handleSnapshotCommit))
	mux.HandleFunc("GET /api/agent/manifests/{id}", s.agentAuth(s.handleManifestGet))
	mux.HandleFunc("GET /api/agent/transfers/{id}/content", s.agentAuth(s.handleTransferContent))
	mux.HandleFunc("PUT /api/agent/transfers/{id}/content", s.agentAuth(s.handleTransferUpload))
}

// registerGUI registers everything an operator's browser needs: the web GUI,
// the admin API (session cookie) and the install scripts and prebuilt agent
// binaries the enrollment dialog links to.
func (s *Server) registerGUI(mux *http.ServeMux) {
	s.registerAdminAPI(mux)
	s.registerStorageAPI(mux)
	s.web.register(mux)

	// Prebuilt agent binaries for the install scripts (not secret).
	mux.Handle("GET /dl/", http.StripPrefix("/dl/", http.FileServer(http.Dir(s.cfg.AgentBins))))
}

// guiHandler is the handler for the optional plain-HTTP GUI listener
// (CB_GUI_LISTEN): the GUI half of the routes, with every request tagged as
// proxied so nothing derives an agent-facing URL from the proxy's Host
// header.
func (s *Server) guiHandler() http.Handler {
	mux := http.NewServeMux()
	s.registerGUI(mux)
	return markGUIListener(mux)
}

func (s *Server) Run() error {
	mux := http.NewServeMux()
	s.registerAgentAPI(mux)
	s.registerGUI(mux)

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

	// Either listener failing is fatal, so the first error wins.
	errc := make(chan error, 2)

	if s.cfg.GUIListen != "" {
		guiSrv := &http.Server{
			Addr:              s.cfg.GUIListen,
			Handler:           s.guiHandler(),
			ReadHeaderTimeout: 30 * time.Second,
		}
		log.Printf("GUI listener (plain HTTP, for a local TLS-terminating proxy such as "+
			"cloudflared) on http://%s — do not expose this port directly", s.cfg.GUIListen)
		if s.cfg.GUIURL != "" {
			log.Printf("GUI public address: %s", s.cfg.GUIURL)
		}
		go func() { errc <- fmt.Errorf("gui listener: %w", guiSrv.ListenAndServe()) }()
	}

	log.Printf("central backup server listening on https://%s", s.cfg.Listen)
	log.Printf("TLS certificate SHA-256 fingerprint: %s", s.fingerprint)
	go func() { errc <- srv.ListenAndServeTLS("", "") }()

	return <-errc
}
