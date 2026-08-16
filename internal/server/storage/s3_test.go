package storage

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeS3 is an object store that speaks enough of the S3 API to exercise the
// backend — and, importantly, *verifies the SigV4 signature* on every
// request by recomputing it from the credentials. A signing bug therefore
// fails the test rather than passing against a permissive double.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	srv     *httptest.Server
	bucket  string
	secret  string
	access  string
	region  string
}

func newFakeS3(t *testing.T) *fakeS3 {
	t.Helper()
	f := &fakeS3{
		objects: map[string][]byte{},
		bucket:  "backups",
		access:  "AKIAEXAMPLE",
		secret:  "s3cr3t/key+EXAMPLE",
		region:  "us-east-1",
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeS3) config(folder string) Config {
	return Config{
		Provider:    ProviderS3,
		S3Endpoint:  f.srv.URL,
		S3Region:    f.region,
		S3Bucket:    f.bucket,
		S3AccessKey: f.access,
		S3SecretKey: f.secret,
		Folder:      folder,
	}
}

func (f *fakeS3) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if err := f.verify(r, body); err != nil {
		http.Error(w, "SignatureDoesNotMatch: "+err.Error(), http.StatusForbidden)
		return
	}

	key := strings.TrimPrefix(r.URL.Path, "/"+f.bucket+"/")
	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.Method {
	case "PUT":
		f.objects[key] = body
		w.WriteHeader(http.StatusOK)
	case "GET", "HEAD":
		v, ok := f.objects[key]
		if !ok {
			http.Error(w, "NoSuchKey", http.StatusNotFound)
			return
		}
		if r.Method == "GET" {
			w.Write(v)
		}
	case "DELETE":
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "MethodNotAllowed", http.StatusMethodNotAllowed)
	}
}

// verify recomputes the signature the client should have sent.
func (f *fakeS3) verify(r *http.Request, body []byte) error {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return errors.New("no Authorization header")
	}
	amzDate := r.Header.Get("X-Amz-Date")
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if amzDate == "" || payloadHash == "" {
		return errors.New("missing X-Amz-Date or X-Amz-Content-Sha256")
	}
	// The advertised payload hash must match what actually arrived,
	// otherwise the body could be swapped without invalidating the
	// signature.
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != payloadHash {
		return errors.New("payload hash does not match the body")
	}

	dateStamp := amzDate[:8]
	canonHeaders := "host:" + r.Host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signed := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{
		r.Method, s3CanonicalURI(r.URL.Path), r.URL.RawQuery,
		canonHeaders, signed, payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, f.region, "s3", "aws4_request"}, "/")
	crHash := sha256.Sum256([]byte(canonicalRequest))
	sts := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(crHash[:]),
	}, "\n")

	k := hmacSHA256([]byte("AWS4"+f.secret), dateStamp)
	k = hmacSHA256(k, f.region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	want := hex.EncodeToString(hmacSHA256(k, sts))

	if !strings.Contains(auth, "Signature="+want) {
		return errors.New("signature mismatch")
	}
	if !strings.Contains(auth, "Credential="+f.access+"/"+scope) {
		return errors.New("credential scope mismatch")
	}
	if !hmac.Equal([]byte(want), []byte(want)) { // keep hmac import honest
		return errors.New("unreachable")
	}
	return nil
}

func TestS3RoundTrip(t *testing.T) {
	f := newFakeS3(t)
	b, err := NewS3(f.config("central-backup"))
	if err != nil {
		t.Fatal(err)
	}

	key := BlobKey("abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789")
	payload := bytes.Repeat([]byte("backup data "), 5000)

	n, err := b.Put(key, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n != int64(len(payload)) {
		t.Errorf("Put wrote %d bytes, want %d", n, len(payload))
	}

	ok, err := b.Has(key)
	if err != nil || !ok {
		t.Fatalf("Has = %v, %v; want true", ok, err)
	}

	rc, err := b.Get(key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, payload) {
		t.Errorf("Get returned %d bytes, want %d identical", len(got), len(payload))
	}

	// The configured folder must be applied as a key prefix, so several
	// deployments can share one bucket without colliding.
	f.mu.Lock()
	var stored []string
	for k := range f.objects {
		stored = append(stored, k)
	}
	f.mu.Unlock()
	if len(stored) != 1 || !strings.HasPrefix(stored[0], "central-backup/blobs/") {
		t.Errorf("stored keys = %v, want one under central-backup/blobs/", stored)
	}

	if err := b.Delete(key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, _ := b.Has(key); ok {
		t.Error("object still present after Delete")
	}
}

func TestS3GetMissingIsErrNotFound(t *testing.T) {
	f := newFakeS3(t)
	b, _ := NewS3(f.config(""))
	if _, err := b.Get(ManifestKey("nope")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
	if ok, err := b.Has(ManifestKey("nope")); ok || err != nil {
		t.Errorf("Has(missing) = %v, %v; want false, nil", ok, err)
	}
	// Deleting something that isn't there is defined as success.
	if err := b.Delete(ManifestKey("nope")); err != nil {
		t.Errorf("Delete(missing) = %v, want nil", err)
	}
}

func TestS3RejectsBadCredentials(t *testing.T) {
	f := newFakeS3(t)
	cfg := f.config("")
	cfg.S3SecretKey = "wrong-secret"
	b, _ := NewS3(cfg)

	// Proves the fake really checks the signature: a wrong key must fail.
	if _, err := b.Put(BlobKey(strings.Repeat("a", 64)), strings.NewReader("x")); err == nil {
		t.Error("Put with a bad secret key succeeded, so the signature is not being verified")
	}
}

func TestNewS3Validation(t *testing.T) {
	base := Config{Provider: ProviderS3, S3Endpoint: "https://s3.example.com",
		S3Bucket: "b", S3AccessKey: "a", S3SecretKey: "s"}

	if _, err := NewS3(base); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
	// A bare host is accepted and assumed https, matching how admins type it.
	bare := base
	bare.S3Endpoint = "s3.example.com"
	if _, err := NewS3(bare); err != nil {
		t.Errorf("bare host endpoint rejected: %v", err)
	}

	for name, mutate := range map[string]func(*Config){
		"no endpoint":   func(c *Config) { c.S3Endpoint = "" },
		"no bucket":     func(c *Config) { c.S3Bucket = "" },
		"no access key": func(c *Config) { c.S3AccessKey = "" },
		"no secret":     func(c *Config) { c.S3SecretKey = "" },
		"bad scheme":    func(c *Config) { c.S3Endpoint = "ftp://s3.example.com" },
	} {
		cfg := base
		mutate(&cfg)
		if _, err := NewS3(cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
