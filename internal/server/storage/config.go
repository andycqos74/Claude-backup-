package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Provider identifies a storage backend type.
type Provider string

const (
	ProviderLocal     Provider = "local"
	ProviderOneDrive  Provider = "onedrive"
	ProviderGoogleDrive Provider = "gdrive"
	ProviderBox       Provider = "box"
)

// Config is the persisted storage configuration (stored as JSON in the
// settings table under key "storage.config"). OAuth secrets live here too;
// like the rest of the system's data they are stored unencrypted at rest,
// so the server host must be treated as sensitive.
type Config struct {
	Provider Provider `json:"provider"`

	// LocalDir is the on-disk root for ProviderLocal (defaults applied by
	// the server if empty).
	LocalDir string `json:"local_dir,omitempty"`

	// OAuth app credentials, entered by the admin from the provider's
	// developer console. Shared by all cloud providers.
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`

	// RefreshToken is obtained via the GUI "Connect" OAuth flow.
	RefreshToken string `json:"refresh_token,omitempty"`

	// Folder is the destination folder/path within the cloud account
	// (e.g. "central-backup"). Empty uses a provider-specific default.
	Folder string `json:"folder,omitempty"`

	// Account is a human-readable label for the connected account (email /
	// display name), shown in the GUI. Set during the OAuth flow.
	Account string `json:"account,omitempty"`
}

// Connected reports whether a cloud provider has completed OAuth.
func (c Config) Connected() bool {
	return c.Provider == ProviderLocal || c.RefreshToken != ""
}

// TokenSourceFunc yields a valid, refreshed OAuth bearer token on demand.
// The concrete implementation is provided by the server (it owns the
// oauth2 config and persistence); the storage package stays free of any
// particular OAuth library.
type TokenSourceFunc func(ctx context.Context) (string, error)

// Build constructs the active backend from cfg. For cloud providers the
// caller supplies a token source (nil is only valid for ProviderLocal).
// httpClient may be nil (defaults to http.DefaultClient) — tests inject a
// client pointed at a fake API server.
func Build(cfg Config, token TokenSourceFunc, httpClient *http.Client) (Backend, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	switch cfg.Provider {
	case "", ProviderLocal:
		dir := cfg.LocalDir
		if dir == "" {
			return nil, fmt.Errorf("local storage directory is not set")
		}
		return NewLocalFS(dir)
	case ProviderOneDrive:
		if token == nil {
			return nil, fmt.Errorf("onedrive backend requires a token source")
		}
		return newOneDrive(cfg, token, httpClient), nil
	case ProviderGoogleDrive, ProviderBox:
		return nil, fmt.Errorf("storage provider %q is not available yet", cfg.Provider)
	default:
		return nil, fmt.Errorf("unknown storage provider %q", cfg.Provider)
	}
}

// MarshalConfig / ParseConfig persist Config as JSON.
func MarshalConfig(c Config) (string, error) {
	b, err := json.Marshal(c)
	return string(b), err
}

func ParseConfig(s string) (Config, error) {
	var c Config
	if s == "" {
		return Config{Provider: ProviderLocal}, nil
	}
	err := json.Unmarshal([]byte(s), &c)
	return c, err
}
