// Package agent implements the client-side backup agent: an outbound-only
// connection to the central server, the file backup/restore engine, and
// local job configuration via agent.yaml.
package agent

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Credentials are issued once at enrollment and stored in the agent state
// directory. Fingerprint pins the server's TLS certificate: with it, the
// self-signed server cert is safe against man-in-the-middle.
type Credentials struct {
	ServerURL   string `json:"server_url"`
	AgentID     string `json:"agent_id"`
	Secret      string `json:"secret"`
	Fingerprint string `json:"fingerprint"`
}

func credsPath(stateDir string) string { return filepath.Join(stateDir, "creds.json") }

func LoadCredentials(stateDir string) (*Credentials, error) {
	b, err := os.ReadFile(credsPath(stateDir))
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Credentials) Save(stateDir string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(credsPath(stateDir), b, 0o600)
}

// tlsConfig verifies the server certificate exclusively by its pinned
// SHA-256 fingerprint (the server typically uses a self-signed cert).
func pinnedTLSConfig(fingerprint string) *tls.Config {
	want := strings.ToLower(strings.ReplaceAll(fingerprint, ":", ""))
	return &tls.Config{
		InsecureSkipVerify: true, // verification is done by pin below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("server presented no certificate")
			}
			sum := sha256.Sum256(rawCerts[0])
			if hex.EncodeToString(sum[:]) != want {
				return fmt.Errorf("server certificate fingerprint mismatch: got %s, pinned %s",
					hex.EncodeToString(sum[:]), want)
			}
			return nil
		},
		MinVersion: tls.VersionTLS12,
	}
}

// FetchFingerprint connects once (without verification) and returns the
// server certificate's SHA-256 fingerprint — trust-on-first-use for
// enrollment when no --fingerprint was provided.
func FetchFingerprint(serverURL string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", err
	}
	host := u.Host
	if u.Port() == "" {
		host += ":443"
	}
	conn, err := tls.Dial("tcp", host, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		return "", err
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", fmt.Errorf("server presented no certificate")
	}
	sum := sha256.Sum256(certs[0].Raw)
	return hex.EncodeToString(sum[:]), nil
}
