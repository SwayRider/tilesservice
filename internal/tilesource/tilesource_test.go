package tilesource_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swayrider/tilesservice/internal/pmtiles/pmtilestest"
	"github.com/swayrider/tilesservice/internal/tilesource"
)

func archive() []byte {
	return pmtilestest.Build([]pmtilestest.Tile{{Z: 1, X: 1, Y: 0, Data: []byte("hello")}}, pmtilestest.Options{})
}

func TestOpenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.pmtiles")
	if err := os.WriteFile(path, archive(), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, loc := range []string{path, "file://" + path} {
		r, err := tilesource.Open(context.Background(), loc, tilesource.S3{})
		if err != nil {
			t.Fatalf("%s: %v", loc, err)
		}
		if got, err := r.Tile(1, 1, 0); err != nil || string(got) != "hello" {
			t.Errorf("%s: tile %q, %v", loc, got, err)
		}
		_ = r.Close()
	}
}

func TestOpenS3(t *testing.T) {
	data := archive()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/swayrider-tiles/releases/r-1/tiles.pmtiles" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			return
		}
		var s, e int
		_, _ = fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &s, &e)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, e, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[s : e+1])
	}))
	defer srv.Close()
	r, err := tilesource.Open(context.Background(), "s3://swayrider-tiles/releases/r-1/tiles.pmtiles",
		tilesource.S3{Endpoint: srv.URL, AccessKey: "AK", SecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if got, err := r.Tile(1, 1, 0); err != nil || !bytes.Equal(got, []byte("hello")) {
		t.Errorf("tile %q, %v", got, err)
	}
}

func TestOpenErrors(t *testing.T) {
	for loc, want := range map[string]string{
		"s3://bucket":             "valid s3://bucket/key",
		"s3:///key":               "valid s3://bucket/key",
		"s3://bucket/key":         "S3_ENDPOINT",
		"file://relative/path":    "absolute",
		"/does/not/exist.pmtiles": "no such file",
	} {
		_, err := tilesource.Open(context.Background(), loc, tilesource.S3{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want it to mention %q", loc, err, want)
		}
	}
}
