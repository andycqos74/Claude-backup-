package storage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Shared helpers for the HTTP-based cloud backends (OneDrive, Google Drive,
// Box).

// encodePath percent-encodes each slash-separated segment of a storage key
// while preserving the separators, for use in provider path-addressed URLs.
func encodePath(p string) string {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for i, s := range segs {
		segs[i] = pathEscapeSegment(s)
	}
	return strings.Join(segs, "/")
}

// pathEscapeSegment escapes a single path segment. Our keys are hex + '.',
// so this is mostly defensive, but it keeps the backends correct for any
// folder name an admin might choose.
func pathEscapeSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '~':
			b.WriteRune(r)
		default:
			for _, c := range []byte(string(r)) {
				fmt.Fprintf(&b, "%%%02X", c)
			}
		}
	}
	return b.String()
}

// drain fully reads and closes a response body so the connection can be
// reused.
func drain(res *http.Response) {
	if res != nil && res.Body != nil {
		io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
		res.Body.Close()
	}
}

// httpErr builds an error from a non-2xx response, including a short body
// excerpt for diagnostics.
func httpErr(op string, res *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return fmt.Errorf("%s: %s: %s", op, res.Status, msg)
}

// retryableStatus reports whether an HTTP status warrants a retry.
func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests ||
		code == http.StatusInternalServerError ||
		code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable ||
		code == http.StatusGatewayTimeout
}

// respErr converts a non-2xx response into an error, marking transient
// statuses as retryable (so retry() will back off and try again) and
// everything else as permanent.
func respErr(op string, res *http.Response) error {
	if retryableStatus(res.StatusCode) {
		return retryAfter(op, res)
	}
	return httpErr(op, res)
}

// retryHTTPError lets retry() honour a provider's Retry-After without the
// caller having to parse it: an operation returns this to signal "retry
// after D".
type retryHTTPError struct {
	after time.Duration
	err   error
}

func (e retryHTTPError) Error() string { return e.err.Error() }
func (e retryHTTPError) Unwrap() error { return e.err }

// retryAfter builds a retryHTTPError from a response's Retry-After header.
func retryAfter(op string, res *http.Response) error {
	d := 2 * time.Second
	if v := res.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
			d = time.Duration(secs) * time.Second
		}
	}
	return retryHTTPError{after: d, err: httpErr(op, res)}
}

// retry runs fn up to a few times with backoff, retrying transient
// failures. fn should return a retryHTTPError (via retryAfter) or an error
// wrapping a retryable condition to be retried; other errors are returned
// immediately.
func retry(ctx context.Context, fn func() error) error {
	const maxAttempts = 4
	backoff := time.Second
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err = fn()
		if err == nil {
			return nil
		}
		var rerr retryHTTPError
		wait := backoff
		if asRetry(err, &rerr) {
			if rerr.after > wait {
				wait = rerr.after
			}
		} else {
			return err // not a retryable error
		}
		if attempt == maxAttempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		backoff *= 2
	}
	return err
}

func asRetry(err error, target *retryHTTPError) bool {
	for err != nil {
		if re, ok := err.(retryHTTPError); ok {
			*target = re
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// ctxReadCloser cancels an operation context when the body is closed, so a
// streamed download releases its context.
type ctxReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *ctxReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}
