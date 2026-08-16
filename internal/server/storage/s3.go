package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// S3-compatible object storage: Backblaze B2, Wasabi, Cloudflare R2, MinIO,
// Storj, AWS S3 itself. Configuration is an endpoint, a bucket and a key
// pair — no OAuth app registration, no consent screen, no redirect URI and
// no token refresh, which makes it by far the simplest backend to set up
// and the only one that works unattended on a headless host.
//
// Requests are signed with AWS Signature V4 directly rather than through
// the AWS SDK: the four operations this interface needs are a small part of
// S3, and hand-signing avoids a very large dependency tree.

const (
	s3Algorithm = "AWS4-HMAC-SHA256"
	s3Service   = "s3"
	// maxSinglePut is the largest object S3 accepts in one PUT. Anything
	// bigger needs multipart upload, which is not implemented — a single
	// file over 5 GiB will fail with a clear error rather than silently
	// truncating.
	maxSinglePut = 5 << 30
)

type S3 struct {
	client    *http.Client
	endpoint  *url.URL
	region    string
	bucket    string
	prefix    string
	accessKey string
	secretKey string
}

// NewS3 builds an S3 backend from the admin's configuration.
func NewS3(cfg Config) (*S3, error) {
	if cfg.S3Endpoint == "" {
		return nil, fmt.Errorf("endpoint is required (e.g. https://s3.us-west-002.backblazeb2.com)")
	}
	if cfg.S3Bucket == "" {
		return nil, fmt.Errorf("bucket is required")
	}
	if cfg.S3AccessKey == "" || cfg.S3SecretKey == "" {
		return nil, fmt.Errorf("access key and secret key are required")
	}
	raw := cfg.S3Endpoint
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("endpoint is not a valid URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("endpoint must be http or https, got %q", u.Scheme)
	}
	region := cfg.S3Region
	if region == "" {
		// Most S3-compatible services ignore the region in the signature but
		// still require *a* value; us-east-1 is the conventional default.
		region = "us-east-1"
	}
	return &S3{
		client:    &http.Client{Timeout: 0}, // per-request deadlines via context
		endpoint:  u,
		region:    region,
		bucket:    cfg.S3Bucket,
		prefix:    strings.Trim(cfg.Folder, "/"),
		accessKey: cfg.S3AccessKey,
		secretKey: cfg.S3SecretKey,
	}, nil
}

// objectURL builds the path-style URL for a key. Path style
// (endpoint/bucket/key) is used rather than virtual-host style because it
// works with self-hosted MinIO and with bucket names that aren't
// DNS-compatible, and every major provider accepts it.
func (s *S3) objectURL(key string) string {
	p := s.bucket + "/" + s.objectKey(key)
	u := *s.endpoint
	u.Path = strings.TrimRight(u.Path, "/") + "/" + p
	return u.String()
}

func (s *S3) objectKey(key string) string {
	key = strings.TrimLeft(key, "/")
	if s.prefix == "" {
		return key
	}
	return s.prefix + "/" + key
}

func (s *S3) Put(key string, r io.Reader) (int64, error) {
	// S3 needs the length and payload hash up front, so the object is
	// spooled to a temp file and hashed in the same pass. Streaming
	// signatures would avoid this but are considerably more machinery for
	// no practical gain at blob sizes.
	tmp, err := os.CreateTemp("", "cb-s3-*")
	if err != nil {
		return 0, err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		return 0, fmt.Errorf("buffer object: %w", err)
	}
	if n > maxSinglePut {
		return 0, fmt.Errorf("object is %d bytes; objects larger than 5 GiB need multipart upload, which is not supported", n)
	}
	payloadHash := hex.EncodeToString(h.Sum(nil))

	err = retry(context.Background(), func() error {
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return err
		}
		req, err := http.NewRequest("PUT", s.objectURL(key), tmp)
		if err != nil {
			return err
		}
		req.ContentLength = n
		s.sign(req, payloadHash)

		res, err := s.client.Do(req)
		if err != nil {
			return err
		}
		defer drain(res)
		if res.StatusCode/100 != 2 {
			return respErr("s3 put", res)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (s *S3) Get(key string) (io.ReadCloser, error) {
	req, err := http.NewRequest("GET", s.objectURL(key), nil)
	if err != nil {
		return nil, err
	}
	s.sign(req, emptyPayloadHash)

	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode == http.StatusNotFound {
		drain(res)
		return nil, ErrNotFound
	}
	if res.StatusCode/100 != 2 {
		defer drain(res)
		return nil, respErr("s3 get", res)
	}
	return res.Body, nil
}

func (s *S3) Has(key string) (bool, error) {
	req, err := http.NewRequest("HEAD", s.objectURL(key), nil)
	if err != nil {
		return false, err
	}
	s.sign(req, emptyPayloadHash)

	res, err := s.client.Do(req)
	if err != nil {
		return false, err
	}
	defer drain(res)
	switch {
	case res.StatusCode == http.StatusNotFound:
		return false, nil
	case res.StatusCode/100 == 2:
		return true, nil
	default:
		return false, respErr("s3 head", res)
	}
}

func (s *S3) Delete(key string) error {
	req, err := http.NewRequest("DELETE", s.objectURL(key), nil)
	if err != nil {
		return err
	}
	s.sign(req, emptyPayloadHash)

	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer drain(res)
	// S3 reports deleting a missing object as success (204), which matches
	// the Backend contract.
	if res.StatusCode/100 != 2 && res.StatusCode != http.StatusNotFound {
		return respErr("s3 delete", res)
	}
	return nil
}

// emptyPayloadHash is SHA-256 of the empty string, used for bodiless
// requests.
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// sign applies AWS Signature Version 4 to req.
//
// The signature covers the method, the canonical URI and query, a chosen
// set of headers, and a hash of the payload — so a request cannot be
// replayed against a different object or with different content.
func (s *S3) sign(req *http.Request, payloadHash string) {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if req.Host == "" {
		req.Host = req.URL.Host
	}

	// Canonical headers: lowercase name, trimmed value, sorted by name.
	headers := map[string]string{
		"host":                 req.URL.Host,
		"x-amz-date":           amzDate,
		"x-amz-content-sha256": payloadHash,
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)

	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k)
		canonHeaders.WriteString(":")
		canonHeaders.WriteString(strings.TrimSpace(headers[k]))
		canonHeaders.WriteString("\n")
	}
	signedHeaders := strings.Join(names, ";")

	canonicalRequest := strings.Join([]string{
		req.Method,
		s3CanonicalURI(req.URL.Path),
		req.URL.RawQuery,
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, s.region, s3Service, "aws4_request"}, "/")
	crHash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		s3Algorithm,
		amzDate,
		scope,
		hex.EncodeToString(crHash[:]),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+s.secretKey), dateStamp)
	key = hmacSHA256(key, s.region)
	key = hmacSHA256(key, s3Service)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s3Algorithm, s.accessKey, scope, signedHeaders, signature))
}

// s3CanonicalURI percent-encodes each path segment as SigV4 requires,
// leaving the separators intact. Go's URL.Path is already decoded, so this
// re-encodes it consistently with what the server will canonicalise.
func s3CanonicalURI(p string) string {
	if p == "" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = s3EscapeSegment(s)
	}
	return strings.Join(segs, "/")
}

// s3EscapeSegment implements RFC 3986 unreserved-character escaping, which
// is stricter than url.PathEscape (notably it escapes '+' and '=').
func s3EscapeSegment(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}
