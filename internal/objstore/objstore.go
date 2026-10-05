// Package objstore reads objects from an S3-compatible store (Garage, MinIO, AWS) with ranged
// GETs, exposing them as an io.ReaderAt. It is deliberately tiny and dependency-free: it signs
// GET and HEAD requests with AWS Signature V4 using only the standard library and uses path-style
// addressing (endpoint/bucket/key), which every S3-compatible store accepts.
package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config describes the store. With an empty AccessKey requests are sent unsigned (public buckets, tests).
type Config struct {
	Endpoint  string // e.g. http://garage:3900 or https://s3.example.com
	Region    string // signing region; Garage's default is "garage"
	AccessKey string
	SecretKey string

	HTTPClient *http.Client  // optional
	Timeout    time.Duration // per request, default 15s
	Retries    int           // extra attempts on network errors and 5xx, default 2
}

// Client talks to one store.
type Client struct {
	endpoint *url.URL
	cfg      Config
	hc       *http.Client
	now      func() time.Time
}

// NewClient validates the configuration.
func NewClient(cfg Config) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("objstore: endpoint %q must be an http(s) URL", cfg.Endpoint)
	}
	if (cfg.AccessKey == "") != (cfg.SecretKey == "") {
		return nil, errors.New("objstore: access key and secret key must be set together")
	}
	if cfg.Region == "" {
		cfg.Region = "garage"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.Retries < 0 {
		cfg.Retries = 0
	} else if cfg.Retries == 0 {
		cfg.Retries = 2
	}
	hc := cfg.HTTPClient
	if hc == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.MaxIdleConnsPerHost = 32  // tile requests read concurrently
		tr.DisableCompression = true // never let the transport decode a stored gzip object
		hc = &http.Client{Transport: tr}
	}
	return &Client{endpoint: u, cfg: cfg, hc: hc, now: time.Now}, nil
}

// Object is one object in a bucket; it implements io.ReaderAt with one ranged GET per call.
type Object struct {
	c      *Client
	bucket string
	key    string
	size   int64
	etag   string
}

// Open looks the object up (HEAD) and returns a handle.
func (c *Client) Open(ctx context.Context, bucket, key string) (*Object, error) {
	resp, err := c.do(ctx, http.MethodHead, bucket, key, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("objstore: s3://%s/%s not found", bucket, key)
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("objstore: access to s3://%s/%s denied (HTTP %d): check the key and bucket permissions", bucket, key, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("objstore: HEAD s3://%s/%s: HTTP %d", bucket, key, resp.StatusCode)
	}
	size, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil || size < 0 {
		return nil, fmt.Errorf("objstore: HEAD s3://%s/%s: no usable Content-Length", bucket, key)
	}
	return &Object{c: c, bucket: bucket, key: key, size: size, etag: resp.Header.Get("ETag")}, nil
}

// Size is the object size in bytes.
func (o *Object) Size() int64 { return o.size }

// ETag is the ETag the store reported when the object was opened.
func (o *Object) ETag() string { return o.etag }

// Close is a no-op (requests are independent); it exists so an Object can be used as an io.Closer.
func (o *Object) Close() error { return nil }

// ReadAt implements io.ReaderAt with a single ranged GET. Reads past the end are clamped and
// return io.EOF with the bytes that exist.
func (o *Object) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("objstore: negative offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= o.size {
		return 0, io.EOF
	}
	end := off + int64(len(p)) - 1
	if end >= o.size {
		end = o.size - 1
	}
	want := int(end - off + 1)

	resp, err := o.c.do(context.Background(), http.MethodGet, o.bucket, o.key, fmt.Sprintf("bytes=%d-%d", off, end))
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		if cr := resp.Header.Get("Content-Range"); !strings.HasPrefix(cr, fmt.Sprintf("bytes %d-%d/", off, end)) {
			return 0, fmt.Errorf("objstore: unexpected Content-Range %q for bytes=%d-%d", cr, off, end)
		}
	case http.StatusOK:
		// A store that ignores Range sends the whole object; that is only usable when we asked for all of it.
		if off != 0 || end != o.size-1 {
			return 0, errors.New("objstore: the store ignored the Range header")
		}
	default:
		return 0, fmt.Errorf("objstore: GET s3://%s/%s bytes=%d-%d: HTTP %d", o.bucket, o.key, off, end, resp.StatusCode)
	}
	n, err := io.ReadFull(resp.Body, p[:want])
	if err != nil {
		return n, fmt.Errorf("objstore: reading s3://%s/%s bytes=%d-%d: %w", o.bucket, o.key, off, end, err)
	}
	if want < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// do sends one signed request with retries on network errors and 5xx answers. The caller
// closes the returned body.
func (c *Client) do(parent context.Context, method, bucket, key, rng string) (*http.Response, error) {
	u := *c.endpoint
	u.Path = strings.TrimRight(u.Path, "/") + "/" + bucket + "/" + key
	u.RawPath = strings.TrimRight(c.endpoint.EscapedPath(), "/") + "/" + escapeSegment(bucket) + "/" + escapePath(key)

	var lastErr error
	for attempt := 0; attempt <= c.cfg.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-parent.Done():
				return nil, parent.Err()
			case <-time.After(time.Duration(attempt) * 150 * time.Millisecond):
			}
		}
		ctx, cancel := context.WithTimeout(parent, c.cfg.Timeout)
		req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
		if err != nil {
			cancel()
			return nil, err
		}
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		if c.cfg.AccessKey != "" {
			sign(req, c.cfg.AccessKey, c.cfg.SecretKey, c.cfg.Region, c.now())
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			cancel()
			lastErr = fmt.Errorf("objstore: %s %s: %w", method, u.Redacted(), err)
			continue
		}
		if resp.StatusCode >= 500 {
			_ = resp.Body.Close()
			cancel()
			lastErr = fmt.Errorf("objstore: %s %s: HTTP %d", method, u.Redacted(), resp.StatusCode)
			continue
		}
		resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
		return resp, nil
	}
	return nil, lastErr
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// escapePath escapes every segment of an object key, keeping the slashes.
func escapePath(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = escapeSegment(p)
	}
	return strings.Join(parts, "/")
}

// escapeSegment percent-encodes everything except the SigV4 unreserved set A-Z a-z 0-9 - _ . ~
func escapeSegment(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.' || ch == '~' {
			b.WriteByte(ch)
		} else {
			b.WriteByte('%')
			b.WriteByte(hex[ch>>4])
			b.WriteByte(hex[ch&15])
		}
	}
	return b.String()
}
