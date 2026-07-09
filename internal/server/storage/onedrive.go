package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// oneDrive implements Backend over the Microsoft Graph OneDrive API. Blobs
// and manifests are stored as files under a folder in the connected
// account's drive, keyed by the same "blobs/ab/<hash>.zst" style keys used
// everywhere else (slashes become nested folders, which Graph creates
// automatically on upload).
//
// The Graph base URL is a field so tests can point it at a fake server; in
// production it is https://graph.microsoft.com/v1.0.
type oneDrive struct {
	base   string // graph base, no trailing slash
	folder string
	token  TokenSourceFunc
	http   *http.Client
}

const graphBase = "https://graph.microsoft.com/v1.0"

// oneDriveSimpleUploadMax is the size below which a single PUT is used;
// larger blobs go through a chunked upload session. Graph allows simple
// uploads up to 250 MiB but recommends sessions above 4 MiB.
const oneDriveSimpleUploadMax = 4 << 20

// oneDriveChunk is the upload-session chunk size. Graph requires chunks to
// be a multiple of 320 KiB; 10 MiB satisfies that.
const oneDriveChunk = 10 << 20

func newOneDrive(cfg Config, token TokenSourceFunc, hc *http.Client) *oneDrive {
	folder := strings.Trim(cfg.Folder, "/")
	if folder == "" {
		folder = "central-backup"
	}
	return &oneDrive{base: graphBase, folder: folder, token: token, http: hc}
}

// itemPath builds the "root:/<folder>/<key>:" drive-item selector for a key.
func (o *oneDrive) itemPath(key string) string {
	full := o.folder + "/" + strings.TrimLeft(key, "/")
	return "/me/drive/root:/" + encodePath(full) + ":"
}

func (o *oneDrive) authReq(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	tok, err := o.token(ctx)
	if err != nil {
		return nil, fmt.Errorf("obtain access token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return req, nil
}

// ctx is not part of the Backend interface (kept minimal for the local
// backend), so cloud backends use a background context with a generous
// timeout per operation.
func (o *oneDrive) opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 6*time.Hour)
}

func (o *oneDrive) Put(key string, r io.Reader) (int64, error) {
	ctx, cancel := o.opCtx()
	defer cancel()

	// Buffer to a temp file to learn the size (the Backend interface gives
	// us an unsized reader, and the chunked upload protocol needs the total
	// length up front).
	tmp, err := os.CreateTemp("", "od-up-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	size, err := io.Copy(tmp, r)
	if err != nil {
		return 0, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}

	if size <= oneDriveSimpleUploadMax {
		// Small enough to hold in memory; buffering here lets each retry
		// send a fresh reader (the HTTP client closes the request body
		// after each attempt, so we can't reuse the temp file directly).
		data, err := io.ReadAll(tmp)
		if err != nil {
			return 0, err
		}
		return size, o.simpleUpload(ctx, key, data)
	}
	return size, o.sessionUpload(ctx, key, tmp, size)
}

func (o *oneDrive) simpleUpload(ctx context.Context, key string, data []byte) error {
	url := o.base + o.itemPath(key) + "/content"
	return retry(ctx, func() error {
		req, err := o.authReq(ctx, "PUT", url, bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.ContentLength = int64(len(data))
		req.Header.Set("Content-Type", "application/octet-stream")
		res, err := o.http.Do(req)
		if err != nil {
			return err
		}
		defer drain(res)
		if res.StatusCode/100 != 2 {
			return respErr("onedrive upload", res)
		}
		return nil
	})
}

func (o *oneDrive) sessionUpload(ctx context.Context, key string, r io.ReadSeeker, size int64) error {
	// Create the upload session.
	createURL := o.base + o.itemPath(key) + "/createUploadSession"
	var session struct {
		UploadURL string `json:"uploadUrl"`
	}
	err := retry(ctx, func() error {
		req, err := o.authReq(ctx, "POST", createURL, strings.NewReader(`{"item":{"@microsoft.graph.conflictBehavior":"replace"}}`))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := o.http.Do(req)
		if err != nil {
			return err
		}
		defer drain(res)
		if res.StatusCode/100 != 2 {
			return respErr("onedrive create upload session", res)
		}
		return json.NewDecoder(res.Body).Decode(&session)
	})
	if err != nil {
		return err
	}
	if session.UploadURL == "" {
		return fmt.Errorf("onedrive: empty upload session URL")
	}

	// PUT chunks. The upload URL is pre-authenticated (no bearer token).
	buf := make([]byte, oneDriveChunk)
	var offset int64
	for offset < size {
		if _, err := r.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		n, err := io.ReadFull(r, buf)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return err
		}
		if n == 0 {
			break
		}
		chunk := buf[:n]
		start, end := offset, offset+int64(n)-1
		contentRange := fmt.Sprintf("bytes %d-%d/%d", start, end, size)

		err = retry(ctx, func() error {
			req, err := http.NewRequestWithContext(ctx, "PUT", session.UploadURL, bytes.NewReader(chunk))
			if err != nil {
				return err
			}
			req.ContentLength = int64(n)
			req.Header.Set("Content-Range", contentRange)
			res, err := o.http.Do(req)
			if err != nil {
				return err
			}
			defer drain(res)
			// 202 Accepted between chunks; 200/201 Created on the last.
			if res.StatusCode != 202 && res.StatusCode/100 != 2 {
				return respErr("onedrive upload chunk", res)
			}
			return nil
		})
		if err != nil {
			o.cancelSession(ctx, session.UploadURL)
			return err
		}
		offset = end + 1
	}
	return nil
}

func (o *oneDrive) cancelSession(ctx context.Context, uploadURL string) {
	req, err := http.NewRequestWithContext(ctx, "DELETE", uploadURL, nil)
	if err != nil {
		return
	}
	if res, err := o.http.Do(req); err == nil {
		drain(res)
	}
}

func (o *oneDrive) Get(key string) (io.ReadCloser, error) {
	ctx, cancel := o.opCtx()
	url := o.base + o.itemPath(key) + "/content"
	req, err := o.authReq(ctx, "GET", url, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	res, err := o.http.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if res.StatusCode == http.StatusNotFound {
		drain(res)
		cancel()
		return nil, ErrNotFound
	}
	if res.StatusCode/100 != 2 {
		err := httpErr("onedrive download", res)
		drain(res)
		cancel()
		return nil, err
	}
	// Cancel the op context when the body is closed.
	return &ctxReadCloser{ReadCloser: res.Body, cancel: cancel}, nil
}

func (o *oneDrive) Has(key string) (bool, error) {
	ctx, cancel := o.opCtx()
	defer cancel()
	url := o.base + o.itemPath(key)
	req, err := o.authReq(ctx, "GET", url, nil)
	if err != nil {
		return false, err
	}
	res, err := o.http.Do(req)
	if err != nil {
		return false, err
	}
	defer drain(res)
	if res.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if res.StatusCode/100 != 2 {
		return false, httpErr("onedrive stat", res)
	}
	return true, nil
}

func (o *oneDrive) Delete(key string) error {
	ctx, cancel := o.opCtx()
	defer cancel()
	url := o.base + o.itemPath(key)
	return retry(ctx, func() error {
		req, err := o.authReq(ctx, "DELETE", url, nil)
		if err != nil {
			return err
		}
		res, err := o.http.Do(req)
		if err != nil {
			return err
		}
		defer drain(res)
		if res.StatusCode == http.StatusNotFound {
			return nil // deleting a missing object is not an error
		}
		if res.StatusCode/100 != 2 {
			return respErr("onedrive delete", res)
		}
		return nil
	})
}
