package server

import (
	"context"
	"fmt"

	"golang.org/x/oauth2"

	"centralbackup/internal/server/storage"
)

// providerOAuth describes the OAuth2 particulars of a cloud storage
// provider: its endpoints and the scopes the backup app needs.
type providerOAuth struct {
	Endpoint oauth2.Endpoint
	Scopes   []string
	// AuthParams are extra query params added to the consent URL (e.g.
	// Microsoft/Google need prompt/offline to guarantee a refresh token).
	AuthParams []oauth2.AuthCodeOption
}

var providerOAuthConfig = map[storage.Provider]providerOAuth{
	storage.ProviderOneDrive: {
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
			TokenURL: "https://login.microsoftonline.com/common/oauth2/v2.0/token",
		},
		// offline_access -> refresh token; Files.ReadWrite -> the user's
		// OneDrive; User.Read -> read the account label for display.
		Scopes: []string{"offline_access", "Files.ReadWrite", "User.Read"},
	},
	storage.ProviderGoogleDrive: {
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://accounts.google.com/o/oauth2/auth",
			TokenURL: "https://oauth2.googleapis.com/token",
		},
		Scopes: []string{"https://www.googleapis.com/auth/drive.file", "openid", "email"},
		AuthParams: []oauth2.AuthCodeOption{
			oauth2.AccessTypeOffline,
			oauth2.SetAuthURLParam("prompt", "consent"), // force a refresh token every time
		},
	},
	storage.ProviderBox: {
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://account.box.com/api/oauth2/authorize",
			TokenURL: "https://api.box.com/oauth2/token",
		},
		Scopes: nil, // Box scopes are set on the app itself
	},
}

// oauthConfig builds the oauth2.Config for a provider using the admin-entered
// app credentials and this server's callback URL.
func oauthConfig(cfg storage.Config, redirectURL string) (*oauth2.Config, error) {
	p, ok := providerOAuthConfig[cfg.Provider]
	if !ok {
		return nil, fmt.Errorf("provider %q does not use OAuth", cfg.Provider)
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, fmt.Errorf("client ID and secret are not set for %s", cfg.Provider)
	}
	return &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     p.Endpoint,
		RedirectURL:  redirectURL,
		Scopes:       p.Scopes,
	}, nil
}

// storageTokenSource returns a storage.TokenSourceFunc that yields a fresh
// access token, transparently refreshing via the stored refresh token. The
// oauth2 library caches and refreshes internally; a long-lived source is
// created per build so refreshes are shared.
func (s *Server) storageTokenSource(cfg storage.Config) (storage.TokenSourceFunc, error) {
	oc, err := oauthConfig(cfg, s.oauthRedirectURL())
	if err != nil {
		return nil, err
	}
	if cfg.RefreshToken == "" {
		return nil, fmt.Errorf("%s is not connected yet", cfg.Provider)
	}
	src := oc.TokenSource(context.Background(), &oauth2.Token{RefreshToken: cfg.RefreshToken})
	return func(ctx context.Context) (string, error) {
		tok, err := src.Token()
		if err != nil {
			return "", fmt.Errorf("refresh %s token: %w", cfg.Provider, err)
		}
		return tok.AccessToken, nil
	}, nil
}
