package server

import (
	"fmt"
	"log"
	"sync"

	"centralbackup/internal/server/storage"
)

// The active storage backend is chosen by the admin and persisted as JSON
// under the "storage.config" setting. On startup the server rebuilds it;
// if a configured cloud backend can't be built (e.g. revoked token) the
// server logs the problem and falls back to local storage so backups keep
// working and the admin can fix it in the GUI.

const storageConfigKey = "storage.config"

// oauthState holds the pending OAuth "connect" flow between the redirect
// and the callback (CSRF state -> in-progress config).
type oauthFlow struct {
	mu      sync.Mutex
	state   string
	pending storage.Config
}

func (s *Server) loadStorageConfig() storage.Config {
	raw, err := s.store.GetSetting(storageConfigKey)
	if err != nil {
		log.Printf("storage: read config: %v", err)
	}
	cfg, err := storage.ParseConfig(raw)
	if err != nil {
		log.Printf("storage: parse config: %v; using local", err)
		cfg = storage.Config{Provider: storage.ProviderLocal}
	}
	if cfg.Provider == "" {
		cfg.Provider = storage.ProviderLocal
	}
	if cfg.Provider == storage.ProviderLocal && cfg.LocalDir == "" {
		cfg.LocalDir = s.cfg.StorageDir
	}
	return cfg
}

func (s *Server) saveStorageConfig(cfg storage.Config) error {
	raw, err := storage.MarshalConfig(cfg)
	if err != nil {
		return err
	}
	return s.store.SetSetting(storageConfigKey, raw)
}

// buildBackend constructs a backend from cfg, supplying an OAuth token
// source for cloud providers.
func (s *Server) buildBackend(cfg storage.Config) (storage.Backend, error) {
	var token storage.TokenSourceFunc
	if cfg.Provider != storage.ProviderLocal {
		ts, err := s.storageTokenSource(cfg)
		if err != nil {
			return nil, err
		}
		token = ts
	}
	return storage.Build(cfg, token, nil)
}

// initStorage builds the active backend at startup, falling back to local
// storage if a configured cloud backend can't be constructed.
func (s *Server) initStorage() error {
	cfg := s.loadStorageConfig()
	backend, err := s.buildBackend(cfg)
	if err != nil {
		log.Printf("storage: %s backend unavailable (%v); falling back to local", cfg.Provider, err)
		local, lerr := storage.NewLocalFS(s.cfg.StorageDir)
		if lerr != nil {
			return lerr
		}
		s.setBackend(local, storage.Config{Provider: storage.ProviderLocal})
		return nil
	}
	s.setBackend(backend, cfg)
	if cfg.Provider != storage.ProviderLocal {
		log.Printf("storage: using %s (account %q, folder %q)", cfg.Provider, cfg.Account, cfg.Folder)
	} else {
		log.Printf("storage: using local filesystem at %s", cfg.LocalDir)
	}
	return nil
}

func (s *Server) setBackend(b storage.Backend, cfg storage.Config) {
	s.storageMu.Lock()
	s.storageActive = b
	s.storageBackendID = backendID(cfg)
	s.storageMu.Unlock()
}

// backendKey returns the identifier of the active backend, used to scope
// the blob dedup index and snapshot set. Blobs and snapshots recorded under
// one backend are invisible to another, so switching backends starts a
// clean namespace (and the first backup re-uploads everything).
func (s *Server) backendKey() string {
	s.storageMu.RLock()
	defer s.storageMu.RUnlock()
	return s.storageBackendID
}

// backendID derives a stable identifier for a storage configuration.
func backendID(cfg storage.Config) string {
	switch cfg.Provider {
	case "", storage.ProviderLocal:
		return "local"
	default:
		// provider + account + folder distinguishes distinct destinations.
		return string(cfg.Provider) + "/" + cfg.Account + "/" + cfg.Folder
	}
}

// applyStorageConfig validates, persists and activates a new configuration.
func (s *Server) applyStorageConfig(cfg storage.Config) error {
	if cfg.Provider == storage.ProviderLocal && cfg.LocalDir == "" {
		cfg.LocalDir = s.cfg.StorageDir
	}
	backend, err := s.buildBackend(cfg)
	if err != nil {
		return err
	}
	if err := s.saveStorageConfig(cfg); err != nil {
		return err
	}
	s.setBackend(backend, cfg)
	log.Printf("storage: switched to %s", cfg.Provider)
	return nil
}

// oauthRedirectURL is where providers send the user back after consent.
func (s *Server) oauthRedirectURL() string {
	host := s.cfg.ServerName
	if i := indexByte(host, ','); i >= 0 {
		host = host[:i]
	}
	if host == "" {
		// Best effort: without a configured public name we can't know the
		// externally reachable host here; the connect handler overrides
		// this with the actual request Host, which is what matters.
		host = "localhost"
	}
	return fmt.Sprintf("https://%s%s/api/admin/storage/oauth/callback", host, portSuffix(s.cfg.Listen))
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// portSuffix returns ":port" from a listen address unless it's the standard
// 443, so the redirect URL is clean.
func portSuffix(listen string) string {
	// listen looks like ":8443" or "0.0.0.0:8443".
	i := indexByteLast(listen, ':')
	if i < 0 {
		return ""
	}
	port := listen[i+1:]
	if port == "443" || port == "" {
		return ""
	}
	return ":" + port
}

func indexByteLast(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}
