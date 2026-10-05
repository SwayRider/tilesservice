package objstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeS3 serves one object path-style with Range support and records what it saw.
type fakeS3 struct {
	bucket    string
	key       string
	data      []byte
	ignoreRng bool
	failFirst int32 // answer 500 to this many first requests
	mu        sync.Mutex
	rawPaths  []string
	auths     []string
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.rawPaths = append(f.rawPaths, r.URL.EscapedPath())
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	f.mu.Unlock()
	if atomic.AddInt32(&f.failFirst, -1) >= 0 {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	if r.URL.Path != "/"+f.bucket+"/"+f.key {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("ETag", `"abc"`)
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.Itoa(len(f.data)))
		return
	}
	rng := r.Header.Get("Range")
	if rng == "" || f.ignoreRng {
		_, _ = w.Write(f.data)
		return
	}
	var start, end int
	if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); err != nil || start > end || end >= len(f.data) {
		http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(f.data)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(f.data[start : end+1])
}

func newStore(t *testing.T, f *fakeS3, cfg Config) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg.Endpoint = srv.URL
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func testData() []byte {
	b := make([]byte, 10_000)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

func TestReadAtRanges(t *testing.T) {
	data := testData()
	f := &fakeS3{bucket: "swayrider-tiles", key: "releases/r 1/tiles.pmtiles", data: data}
	c := newStore(t, f, Config{AccessKey: "AK", SecretKey: "SK"})
	obj, err := c.Open(context.Background(), "swayrider-tiles", "releases/r 1/tiles.pmtiles")
	if err != nil {
		t.Fatal(err)
	}
	if obj.Size() != int64(len(data)) || obj.ETag() != `"abc"` {
		t.Errorf("size %d etag %s", obj.Size(), obj.ETag())
	}
	for _, r := range [][2]int{{0, 127}, {5000, 100}, {9900, 100}, {1, 1}} {
		p := make([]byte, r[1])
		n, err := obj.ReadAt(p, int64(r[0]))
		if err != nil || n != r[1] || !bytes.Equal(p, data[r[0]:r[0]+r[1]]) {
			t.Errorf("ReadAt(%d,%d) = %d, %v", r[0], r[1], n, err)
		}
	}
	for _, p := range f.rawPaths {
		if p != "/swayrider-tiles/releases/r%201/tiles.pmtiles" {
			t.Errorf("request path = %q", p)
		}
	}
	for _, a := range f.auths {
		if !strings.HasPrefix(a, "AWS4-HMAC-SHA256 Credential=AK/") {
			t.Errorf("request not signed: %q", a)
		}
	}
}

func TestReadAtPastEnd(t *testing.T) {
	data := testData()
	c := newStore(t, &fakeS3{bucket: "b", key: "k", data: data}, Config{})
	obj, err := c.Open(context.Background(), "b", "k")
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 100)
	n, err := obj.ReadAt(p, int64(len(data))-40) // only 40 bytes exist
	if n != 40 || err != io.EOF || !bytes.Equal(p[:40], data[len(data)-40:]) {
		t.Errorf("clamped read = %d, %v", n, err)
	}
	if n, err := obj.ReadAt(p, int64(len(data))); n != 0 || err != io.EOF {
		t.Errorf("read at end = %d, %v", n, err)
	}
	if _, err := obj.ReadAt(p, -1); err == nil {
		t.Error("negative offset must fail")
	}
}

func TestReadAtConcurrent(t *testing.T) {
	data := testData()
	f := &fakeS3{bucket: "b", key: "k", data: data}
	c := newStore(t, f, Config{})
	obj, _ := c.Open(context.Background(), "b", "k")
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := make([]byte, 64)
			off := int64(i * 100)
			if n, err := obj.ReadAt(p, off); err != nil || n != 64 || !bytes.Equal(p, data[off:off+64]) {
				t.Errorf("concurrent read %d: %d, %v", i, n, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestRetriesOnServerError(t *testing.T) {
	f := &fakeS3{bucket: "b", key: "k", data: testData(), failFirst: 2}
	c := newStore(t, f, Config{Retries: 3})
	if _, err := c.Open(context.Background(), "b", "k"); err != nil {
		t.Fatalf("Open must survive two 500s: %v", err)
	}
	f2 := &fakeS3{bucket: "b", key: "k", data: testData(), failFirst: 100}
	c2 := newStore(t, f2, Config{Retries: 1})
	if _, err := c2.Open(context.Background(), "b", "k"); err == nil {
		t.Error("a persistent 500 must fail")
	}
}

func TestOpenErrors(t *testing.T) {
	c := newStore(t, &fakeS3{bucket: "b", key: "k", data: testData()}, Config{})
	if _, err := c.Open(context.Background(), "b", "missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing object: %v", err)
	}
}

func TestStoreIgnoringRangeIsAnError(t *testing.T) {
	data := testData()
	c := newStore(t, &fakeS3{bucket: "b", key: "k", data: data, ignoreRng: true}, Config{})
	obj, _ := c.Open(context.Background(), "b", "k")
	if _, err := obj.ReadAt(make([]byte, 10), 500); err == nil {
		t.Error("a 200 answer to a partial range must not be accepted")
	}
	whole := make([]byte, len(data))
	if n, err := obj.ReadAt(whole, 0); err != nil || n != len(data) {
		t.Errorf("a full-object read may be served with 200: %d, %v", n, err)
	}
}

func TestNewClientValidation(t *testing.T) {
	for _, cfg := range []Config{{Endpoint: ""}, {Endpoint: "garage:3900"}, {Endpoint: "ftp://x"}, {Endpoint: "http://x", AccessKey: "a"}} {
		if _, err := NewClient(cfg); err == nil {
			t.Errorf("config %+v must be rejected", cfg)
		}
	}
	if _, err := NewClient(Config{Endpoint: "http://garage:3900/", AccessKey: "a", SecretKey: "b"}); err != nil {
		t.Error(err)
	}
}
