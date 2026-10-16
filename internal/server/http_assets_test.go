package server_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	log "github.com/swayrider/swlib/logger"
	"github.com/swayrider/tilesservice/internal/release"
	"github.com/swayrider/tilesservice/internal/server"
	"github.com/swayrider/tilesservice/internal/tilesource"
)

// assetRelease loads a release that has glyphs and sprites tagged with its id.
func assetRelease(t *testing.T, root, id string) string {
	t.Helper()
	dir := releaseDir(t, root, id, classic)
	for path, content := range map[string]string{
		"glyphs/Noto Sans Regular/0-255.pbf":      "regular-0-255-" + id,
		"glyphs/Noto Sans Regular/256-511.pbf":    "regular-256-511-" + id,
		"glyphs/Noto Sans Medium/0-255.pbf":       "medium-0-255-" + id,
		"glyphs/Noto Sans Medium/65280-65535.pbf": "medium-last-" + id,
		"sprites/light.json":                      `{"icon":{}}`,
		"sprites/light@2x.json":                   `{"icon":{"pixelRatio":2}}`,
		"sprites/light.png":                       "\x89PNG-light-" + id,
		"sprites/light@2x.png":                    "\x89PNG-light2x-" + id,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, filepath.FromSlash(path))), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, dir, path, content)
	}
	return dir
}

func assetHandler(t *testing.T) (*server.AssetHTTPHandler, *release.Holder, string) {
	t.Helper()
	root := t.TempDir()
	assetRelease(t, root, "r-1")
	pointTo(t, root, "r-1")
	h := newHolder(t, root)
	if _, err := h.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return server.NewAssetHTTPHandler(h, log.New(log.WithComponent("test"))), h, root
}

func TestAssets_Glyphs(t *testing.T) {
	ah, _, _ := assetHandler(t)
	w := getA(ah, "/v1/tiles/fonts/Noto Sans Regular/0-255.pbf", nil)
	if w.Code != http.StatusOK || w.Body.String() != "regular-0-255-r-1" {
		t.Fatalf("status %d body %q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/x-protobuf" {
		t.Errorf("Content-Type %q", ct)
	}
	if w.Header().Get("Cache-Control") == "" || w.Header().Get("ETag") == "" || w.Header().Get("Content-Length") == "" {
		t.Errorf("headers %v", w.Header())
	}
	if w := getA(ah, "/v1/tiles/fonts/Noto Sans Medium/65280-65535.pbf", nil); w.Body.String() != "medium-last-r-1" {
		t.Errorf("last range: %d %q", w.Code, w.Body.String())
	}
}

func TestAssets_FontStackFallback(t *testing.T) {
	ah, _, _ := assetHandler(t)
	// the first font that has the range is served
	for path, want := range map[string]string{
		"/v1/tiles/fonts/Noto Sans Regular,Noto Sans Medium/0-255.pbf":       "regular-0-255-r-1",
		"/v1/tiles/fonts/Noto Sans Regular,Noto Sans Medium/256-511.pbf":     "regular-256-511-r-1",
		"/v1/tiles/fonts/Noto Sans Regular,Noto Sans Medium/65280-65535.pbf": "medium-last-r-1", // Regular lacks it
		"/v1/tiles/fonts/Unknown Font,Noto Sans Medium/0-255.pbf":            "medium-0-255-r-1",
	} {
		if w := getA(ah, path, nil); w.Code != http.StatusOK || w.Body.String() != want {
			t.Errorf("%s: %d %q, want %q", path, w.Code, w.Body.String(), want)
		}
	}
	if w := getA(ah, "/v1/tiles/fonts/Unknown Font/0-255.pbf", nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown font: %d", w.Code)
	}
	if w := getA(ah, "/v1/tiles/fonts/Noto Sans Regular/9999-10000.pbf", nil); w.Code != http.StatusNotFound {
		t.Errorf("missing range: %d", w.Code)
	}
}

func TestAssets_Sprites(t *testing.T) {
	ah, _, _ := assetHandler(t)
	for path, want := range map[string]struct{ ct, body string }{
		"/v1/tiles/sprites/light.json":    {"application/json", `{"icon":{}}`},
		"/v1/tiles/sprites/light@2x.json": {"application/json", `{"icon":{"pixelRatio":2}}`},
		"/v1/tiles/sprites/light.png":     {"image/png", "\x89PNG-light-r-1"},
		"/v1/tiles/sprites/light@2x.png":  {"image/png", "\x89PNG-light2x-r-1"},
	} {
		w := getA(ah, path, nil)
		if w.Code != http.StatusOK || w.Body.String() != want.body || w.Header().Get("Content-Type") != want.ct {
			t.Errorf("%s: %d %q %q", path, w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
	}
	if w := getA(ah, "/v1/tiles/sprites/dark.json", nil); w.Code != http.StatusNotFound {
		t.Errorf("missing sprite: %d", w.Code)
	}
}

func TestAssets_ETagAndNotModified(t *testing.T) {
	ah, _, _ := assetHandler(t)
	for _, path := range []string{"/v1/tiles/fonts/Noto Sans Regular/0-255.pbf", "/v1/tiles/sprites/light.png"} {
		etag := getA(ah, path, nil).Header().Get("ETag")
		if etag == "" {
			t.Fatalf("%s: no ETag", path)
		}
		for _, inm := range []string{etag, "W/" + etag, "*"} {
			if w := getA(ah, path, map[string]string{"If-None-Match": inm}); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
				t.Errorf("%s If-None-Match %q: %d, %d bytes", path, inm, w.Code, w.Body.Len())
			}
		}
		if w := getA(ah, path, map[string]string{"If-None-Match": `"other"`}); w.Code != http.StatusOK {
			t.Errorf("%s non-matching If-None-Match: %d", path, w.Code)
		}
	}
	if getA(ah, "/v1/tiles/fonts/Noto Sans Regular/0-255.pbf", nil).Header().Get("ETag") ==
		getA(ah, "/v1/tiles/fonts/Noto Sans Regular/256-511.pbf", nil).Header().Get("ETag") {
		t.Error("different files share an ETag")
	}
}

// Nothing a client sends may select a file outside glyphs/ and sprites/ of the release.
func TestAssets_RejectsBadPaths(t *testing.T) {
	ah, _, root := assetHandler(t)
	// files that traversal would reach: next to the release, and at the release root
	if err := os.WriteFile(filepath.Join(root, "secret.pbf"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "releases", "r-1", "manifest.pbf"), []byte("manifest"), 0o644); err != nil {
		t.Fatal(err)
	}

	// paths are set on the request directly (no net/http cleaning), as in the style traversal test
	for path, code := range map[string]int{
		"/v1/tiles/fonts/../0-255.pbf":                               http.StatusBadRequest,
		"/v1/tiles/fonts/../../secret.pbf":                           http.StatusNotFound,   // wrong segment count
		"/v1/tiles/fonts/..%2F..%2Fglyphs/0-255.pbf":                 http.StatusBadRequest, // a literal %, not in the name set
		"/v1/tiles/fonts/Noto Sans Regular/0-255.pbf/x":              http.StatusNotFound,
		"/v1/tiles/fonts/Noto Sans Regular/0-255.txt":                http.StatusBadRequest,
		"/v1/tiles/fonts/Noto Sans Regular/a-b.pbf":                  http.StatusBadRequest,
		"/v1/tiles/fonts/Noto Sans Regular/manifest.pbf":             http.StatusBadRequest,
		"/v1/tiles/fonts/Noto Sans Regular/../manifest.pbf":          http.StatusNotFound, // wrong segment count
		"/v1/tiles/fonts/.hidden/0-255.pbf":                          http.StatusBadRequest,
		"/v1/tiles/fonts/Noto Sans Regular,../0-255.pbf":             http.StatusBadRequest,
		"/v1/tiles/fonts/Noto Sans Regular,,/0-255.pbf":              http.StatusBadRequest,
		"/v1/tiles/fonts/Noto\\Sans/0-255.pbf":                       http.StatusBadRequest,
		"/v1/tiles/fonts/Noto\x00Sans/0-255.pbf":                     http.StatusBadRequest,
		"/v1/tiles/fonts/" + strings.Repeat("a", 300) + "/0-255.pbf": http.StatusBadRequest,
		"/v1/tiles/fonts/a,b,c,d,e,f,g,h,i/0-255.pbf":                http.StatusBadRequest,
		"/v1/tiles/sprites/../manifest.json":                         http.StatusNotFound, // wrong segment count
		"/v1/tiles/sprites/light.svg":                                http.StatusBadRequest,
		"/v1/tiles/sprites/light@3x.png":                             http.StatusBadRequest,
		"/v1/tiles/sprites/.json":                                    http.StatusBadRequest,
		"/v1/tiles/sprites/a/b.json":                                 http.StatusNotFound,
		"/v1/tiles/other/x":                                          http.StatusNotFound,
	} {
		w := getRaw(ah, path)
		if w.Code != code {
			t.Errorf("%q: status %d, want %d", path, w.Code, code)
		}
		if bytes.Contains(w.Body.Bytes(), []byte("secret")) || strings.Contains(w.Body.String(), "manifest") && w.Code == http.StatusOK {
			t.Errorf("%q: leaked a file outside glyphs/ and sprites/", path)
		}
	}
}

func TestAssets_WithoutRelease(t *testing.T) {
	l := log.New(log.WithComponent("test"))
	if w := getA(server.NewAssetHTTPHandler(nil, l), "/v1/tiles/fonts/Noto Sans Regular/0-255.pbf", nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("no source: %d", w.Code)
	}
	store, _ := release.OpenStore(t.TempDir(), tilesourceS3())
	h := release.NewHolder(store, l)
	t.Cleanup(h.Close)
	ah := server.NewAssetHTTPHandler(h, l)
	for _, p := range []string{"/v1/tiles/fonts/Noto Sans Regular/0-255.pbf", "/v1/tiles/sprites/light.json"} {
		if w := getA(ah, p, nil); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s with no release loaded: %d", p, w.Code)
		}
	}
}

func TestAssets_MethodsAndCORS(t *testing.T) {
	ah, _, _ := assetHandler(t)
	req := newRequest(http.MethodPost, "/v1/tiles/sprites/light.json")
	if w := serveReq(ah, req); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
	req = newRequest(http.MethodOptions, "/v1/tiles/sprites/light.json")
	if w := serveReq(ah, req); w.Code != http.StatusOK || w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("OPTIONS: %d %v", w.Code, w.Header())
	}
}

// After a release switch the next request serves the new release's file, not a cached copy.
func TestAssets_SwitchServesTheNewRelease(t *testing.T) {
	ah, h, root := assetHandler(t)
	path := "/v1/tiles/fonts/Noto Sans Regular/0-255.pbf"
	first := getA(ah, path, nil)
	if first.Body.String() != "regular-0-255-r-1" {
		t.Fatal("r-1")
	}
	getA(ah, path, nil) // fill the cache

	assetRelease(t, root, "r-2")
	pointTo(t, root, "r-2")
	if _, err := h.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := getA(ah, path, nil)
	if second.Body.String() != "regular-0-255-r-2" || second.Header().Get("ETag") == first.Header().Get("ETag") {
		t.Fatalf("after the switch: %q (cache not reset)", second.Body.String())
	}
}

// Files are read once and then served from memory.
func TestAssets_CachesPerRelease(t *testing.T) {
	ah, _, root := assetHandler(t)
	path := "/v1/tiles/sprites/light.json"
	if w := getA(ah, path, nil); w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	// remove the file from the release: a cached copy still answers
	if err := os.Remove(filepath.Join(root, "releases", "r-1", "sprites", "light.json")); err != nil {
		t.Fatal(err)
	}
	if w := getA(ah, path, nil); w.Code != http.StatusOK || w.Body.String() != `{"icon":{}}` {
		t.Fatalf("expected the cached copy: %d %q", w.Code, w.Body.String())
	}
	// a file that was never requested is gone for good
	if err := os.Remove(filepath.Join(root, "releases", "r-1", "sprites", "light@2x.json")); err != nil {
		t.Fatal(err)
	}
	if w := getA(ah, "/v1/tiles/sprites/light@2x.json", nil); w.Code != http.StatusNotFound {
		t.Fatalf("removed file: %d", w.Code)
	}
}

func tilesourceS3() tilesource.S3 { return tilesource.S3{} }

func newRequest(method, path string) *http.Request {
	return httptest.NewRequest(method, path, nil)
}

func serveReq(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// getA is get with spaces in the path escaped (httptest.NewRequest refuses raw spaces).
func getA(h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	return get(h, strings.ReplaceAll(path, " ", "%20"), hdr)
}
