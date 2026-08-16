package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// googleDrive implements Backend over the Google Drive API. Unlike OneDrive,
// Drive is not path-addressable: files live in folders and are referenced by
// ID, so each object is stored as a flat file whose name is the full storage
// key (e.g. "blobs/ab/<hash>.zst") inside a single destination folder, and
// looked up by a name+parent query (which Drive indexes).
type googleDrive struct {
	apiBase    string // https://www.googleapis.com/drive/v3
	uploadBase string // https://www.googleapis.com/upload/drive/v3
	folderName string
	token      TokenSourceFunc
	http       *http.Client

	folderMu sync.Mutex
	folderID string // resolved lazily
}

const (
	gdriveAPIBase    = "https://www.googleapis.com/drive/v3"
	gdriveUploadBase = "https://www.googleapis.com/upload/drive/v3"
	// Resumable-upload chunk size; Google requires a multiple of 256 KiB.
	gdriveChunk = 8 << 20
)

func newGoogleDrive(cfg Config, token TokenSourceFunc, hc *http.Client) *googleDrive {
	folder := strings.Trim(cfg.Folder, "/")
	if folder == "" {
		folder = "central-backup"
	}
	return &googleDrive{
		apiBase:    gdriveAPIBase,
		uploadBase: gdriveUploadBase,
		folderName: folder,
		token:      token,
		http:       hc,
	}
}

func (g *googleDrive) opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 6*time.Hour)
}

func (g *googleDrive) authReq(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	tok, err := g.token(ctx)
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

// folder resolves (creating if necessary) the destination folder ID.
func (g *googleDrive) folder(ctx context.Context) (string, error) {
	g.folderMu.Lock()
	defer g.folderMu.Unlock()
	if g.folderID != "" {
		return g.folderID, nil
	}
	// Look for an existing folder of this name in the drive root.
	q := fmt.Sprintf("name = %s and mimeType = 'application/vnd.google-apps.folder' and 'root' in parents and trashed = false",
		gdriveQuote(g.folderName))
	id, err := g.queryOneID(ctx, q)
	if err != nil {
		return "", err
	}
	if id != "" {
		g.folderID = id
		return id, nil
	}
	// Create it.
	meta := map[string]any{
		"name":     g.folderName,
		"mimeType": "application/vnd.google-apps.folder",
		"parents":  []string{"root"},
	}
	body, _ := json.Marshal(meta)
	var created struct {
		ID string `json:"id"`
	}
	err = retry(ctx, func() error {
		req, err := g.authReq(ctx, "POST", g.apiBase+"/files?fields=id", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := g.http.Do(req)
		if err != nil {
			return err
		}
		defer drain(res)
		if res.StatusCode/100 != 2 {
			return respErr("gdrive create folder", res)
		}
		return json.NewDecoder(res.Body).Decode(&created)
	})
	if err != nil {
		return "", err
	}
	g.folderID = created.ID
	return created.ID, nil
}

// queryOneID runs a files.list query and returns the first matching file ID
// (or "" if none).
func (g *googleDrive) queryOneID(ctx context.Context, q string) (string, error) {
	var result struct {
		Files []struct {
			ID string `json:"id"`
		} `json:"files"`
	}
	u := g.apiBase + "/files?spaces=drive&fields=files(id)&pageSize=1&q=" + url.QueryEscape(q)
	err := retry(ctx, func() error {
		req, err := g.authReq(ctx, "GET", u, nil)
		if err != nil {
			return err
		}
		res, err := g.http.Do(req)
		if err != nil {
			return err
		}
		defer drain(res)
		if res.StatusCode/100 != 2 {
			return respErr("gdrive query", res)
		}
		return json.NewDecoder(res.Body).Decode(&result)
	})
	if err != nil {
		return "", err
	}
	if len(result.Files) == 0 {
		return "", nil
	}
	return result.Files[0].ID, nil
}

// fileID resolves the Drive file ID for a storage key.
func (g *googleDrive) fileID(ctx context.Context, key string) (string, error) {
	folderID, err := g.folder(ctx)
	if err != nil {
		return "", err
	}
	q := fmt.Sprintf("name = %s and %s in parents and trashed = false",
		gdriveQuote(strings.TrimLeft(key, "/")), gdriveQuote(folderID))
	return g.queryOneID(ctx, q)
}

func (g *googleDrive) Put(key string, r io.Reader) (int64, error) {
	ctx, cancel := g.opCtx()
	defer cancel()

	folderID, err := g.folder(ctx)
	if err != nil {
		return 0, err
	}

	tmp, err := os.CreateTemp("", "gd-up-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	size, err := io.Copy(tmp, r)
	if err != nil {
		return 0, err
	}

	// Start a resumable upload session.
	meta := map[string]any{"name": strings.TrimLeft(key, "/"), "parents": []string{folderID}}
	metaBody, _ := json.Marshal(meta)
	var sessionURL string
	err = retry(ctx, func() error {
		req, err := g.authReq(ctx, "POST",
			g.uploadBase+"/files?uploadType=resumable&fields=id", bytes.NewReader(metaBody))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json; charset=UTF-8")
		req.Header.Set("X-Upload-Content-Length", fmt.Sprint(size))
		res, err := g.http.Do(req)
		if err != nil {
			return err
		}
		defer drain(res)
		if res.StatusCode/100 != 2 {
			return respErr("gdrive start upload", res)
		}
		sessionURL = res.Header.Get("Location")
		return nil
	})
	if err != nil {
		return 0, err
	}
	if sessionURL == "" {
		return 0, fmt.Errorf("gdrive: no resumable session URL returned")
	}

	if err := g.uploadChunks(ctx, sessionURL, tmp, size); err != nil {
		return 0, err
	}
	return size, nil
}

func (g *googleDrive) uploadChunks(ctx context.Context, sessionURL string, r io.ReadSeeker, size int64) error {
	if size == 0 {
		// Zero-length: a single empty PUT finalises the session.
		return retry(ctx, func() error {
			req, err := http.NewRequestWithContext(ctx, "PUT", sessionURL, nil)
			if err != nil {
				return err
			}
			req.Header.Set("Content-Range", "bytes */0")
			res, err := g.http.Do(req)
			if err != nil {
				return err
			}
			defer drain(res)
			if res.StatusCode/100 != 2 {
				return respErr("gdrive upload empty", res)
			}
			return nil
		})
	}
	buf := make([]byte, gdriveChunk)
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
		last := end+1 >= size
		err = retry(ctx, func() error {
			req, err := http.NewRequestWithContext(ctx, "PUT", sessionURL, bytes.NewReader(chunk))
			if err != nil {
				return err
			}
			req.ContentLength = int64(n)
			req.Header.Set("Content-Range", contentRange)
			res, err := g.http.Do(req)
			if err != nil {
				return err
			}
			defer drain(res)
			// 308 Resume Incomplete between chunks; 200/201 on completion.
			if !last && res.StatusCode == 308 {
				return nil
			}
			if last && res.StatusCode/100 == 2 {
				return nil
			}
			return respErr("gdrive upload chunk", res)
		})
		if err != nil {
			return err
		}
		offset = end + 1
	}
	return nil
}

func (g *googleDrive) Get(key string) (io.ReadCloser, error) {
	ctx, cancel := g.opCtx()
	id, err := g.fileID(ctx, key)
	if err != nil {
		cancel()
		return nil, err
	}
	if id == "" {
		cancel()
		return nil, ErrNotFound
	}
	req, err := g.authReq(ctx, "GET", g.apiBase+"/files/"+id+"?alt=media", nil)
	if err != nil {
		cancel()
		return nil, err
	}
	res, err := g.http.Do(req)
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
		e := httpErr("gdrive download", res)
		drain(res)
		cancel()
		return nil, e
	}
	return &ctxReadCloser{ReadCloser: res.Body, cancel: cancel}, nil
}

func (g *googleDrive) Has(key string) (bool, error) {
	ctx, cancel := g.opCtx()
	defer cancel()
	id, err := g.fileID(ctx, key)
	if err != nil {
		return false, err
	}
	return id != "", nil
}

func (g *googleDrive) Delete(key string) error {
	ctx, cancel := g.opCtx()
	defer cancel()
	id, err := g.fileID(ctx, key)
	if err != nil {
		return err
	}
	if id == "" {
		return nil // already gone
	}
	return retry(ctx, func() error {
		req, err := g.authReq(ctx, "DELETE", g.apiBase+"/files/"+id, nil)
		if err != nil {
			return err
		}
		res, err := g.http.Do(req)
		if err != nil {
			return err
		}
		defer drain(res)
		if res.StatusCode == http.StatusNotFound {
			return nil
		}
		if res.StatusCode/100 != 2 {
			return respErr("gdrive delete", res)
		}
		return nil
	})
}

// gdriveQuote wraps a value in single quotes for a Drive query, escaping
// backslashes and single quotes per the API's rules.
func gdriveQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}
