package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"

	"centralbackup/internal/proto"
)

// serverClient is the agent's HTTPS data-plane client: enrollment, blob
// check/upload/download and manifest transfer. All requests carry the agent
// credentials and verify the server by pinned certificate fingerprint.
type serverClient struct {
	base  string
	creds *Credentials
	http  *http.Client
}

func newServerClient(creds *Credentials) *serverClient {
	return &serverClient{
		base:  creds.ServerURL,
		creds: creds,
		http: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: pinnedTLSConfig(creds.Fingerprint),
			},
			// No overall timeout: blob uploads/downloads can be large.
		},
	}
}

func (c *serverClient) do(method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Agent-Id", c.creds.AgentID)
	req.Header.Set("X-Agent-Key", c.creds.Secret)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 400 {
		defer res.Body.Close()
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&e)
		if e.Error == "" {
			e.Error = res.Status
		}
		return nil, fmt.Errorf("%s %s: %s", method, path, e.Error)
	}
	return res, nil
}

func (c *serverClient) doJSON(method, path string, reqBody, respBody any) error {
	var r io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	res, err := c.do(method, path, r, "application/json")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if respBody != nil {
		return json.NewDecoder(res.Body).Decode(respBody)
	}
	io.Copy(io.Discard, res.Body)
	return nil
}

// Enroll exchanges a one-time token for permanent credentials.
// creds.Fingerprint and ServerURL must already be set.
func Enroll(serverURL, fingerprint, token, name string) (*Credentials, error) {
	hostname, _ := os.Hostname()
	creds := &Credentials{ServerURL: serverURL, Fingerprint: fingerprint}
	c := newServerClient(creds)
	var resp proto.EnrollResponse
	err := c.doJSON("POST", "/api/agent/enroll", proto.EnrollRequest{
		Token: token, Name: name, Hostname: hostname,
		OS: runtime.GOOS, Arch: runtime.GOARCH,
	}, &resp)
	if err != nil {
		return nil, err
	}
	creds.AgentID = resp.AgentID
	creds.Secret = resp.Secret
	return creds, nil
}

func (c *serverClient) checkBlobs(hashes []string) (missing []string, err error) {
	var resp proto.BlobCheckResponse
	if err := c.doJSON("POST", "/api/agent/blobs/check", proto.BlobCheckRequest{Hashes: hashes}, &resp); err != nil {
		return nil, err
	}
	return resp.Missing, nil
}

// putBlob uploads one zstd-compressed blob stream.
func (c *serverClient) putBlob(hash string, compressed io.Reader) error {
	res, err := c.do("PUT", "/api/agent/blobs/"+hash, compressed, "application/zstd")
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

// getBlob returns the zstd-compressed blob stream (caller closes).
func (c *serverClient) getBlob(hash string) (io.ReadCloser, error) {
	res, err := c.do("GET", "/api/agent/blobs/"+hash, nil, "")
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// getManifest returns the zstd-compressed manifest stream (caller closes).
func (c *serverClient) getManifest(snapshotID string) (io.ReadCloser, error) {
	res, err := c.do("GET", "/api/agent/manifests/"+snapshotID, nil, "")
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// commitSnapshot uploads the zstd-compressed JSONL manifest and records the
// snapshot server-side.
func (c *serverClient) commitSnapshot(jobID, runID, mode string, files, bytesTotal int64, manifest io.Reader) (string, error) {
	path := fmt.Sprintf("/api/agent/snapshots?job_id=%s&run_id=%s&mode=%s&files=%d&bytes=%d",
		jobID, runID, mode, files, bytesTotal)
	res, err := c.do("POST", path, manifest, "application/zstd")
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var resp proto.SnapshotCommitResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		return "", err
	}
	return resp.SnapshotID, nil
}
