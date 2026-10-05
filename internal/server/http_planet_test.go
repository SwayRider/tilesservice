package server_test

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	log "github.com/swayrider/swlib/logger"
	"github.com/swayrider/tilesservice/internal/pmtiles"
	"github.com/swayrider/tilesservice/internal/pmtiles/pmtilestest"
	"github.com/swayrider/tilesservice/internal/server"
)

func planetArchive(t *testing.T, tiles []pmtilestest.Tile, o pmtilestest.Options) *pmtiles.Reader {
	t.Helper()
	file := pmtilestest.Build(tiles, o)
	r, err := pmtiles.Open(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func planetHandler(t *testing.T, a server.TileArchive) *server.TileHTTPHandler {
	t.Helper()
	return server.NewTileHTTPHandler(nil, newMockCache(), log.New(log.WithComponent("test"))).WithArchive(a)
}

func get(h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

var plain = []byte("raw-mvt-tile-bytes")

func gzipTiles(t *testing.T) []pmtilestest.Tile {
	return []pmtilestest.Tile{
		{Z: 2, X: 1, Y: 1, Data: pmtilestest.Gzip(plain)},
		{Z: 3, X: 4, Y: 2, Data: pmtilestest.Gzip([]byte("other"))},
		{Z: 5, X: 7, Y: 9, Data: pmtilestest.Gzip(plain)},
	}
}

func TestPlanet_GzipPassThrough(t *testing.T) {
	h := planetHandler(t, planetArchive(t, gzipTiles(t), pmtilestest.Options{}))
	w := get(h, "/v1/tiles/planet/2/1/1", map[string]string{"Accept-Encoding": "gzip"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	hd := w.Header()
	if hd.Get("Content-Encoding") != "gzip" || hd.Get("Content-Type") != server.ContentTypeMVT ||
		hd.Get("Vary") != "Accept-Encoding" || hd.Get("Cache-Control") != "public, max-age=86400" || hd.Get("ETag") == "" {
		t.Errorf("headers: %v", hd)
	}
	if !bytes.Equal(w.Body.Bytes(), pmtilestest.Gzip(plain)) {
		t.Error("body must be the stored gzip bytes")
	}
	if hd.Get("Content-Length") != fmt.Sprint(w.Body.Len()) {
		t.Errorf("Content-Length %s, body %d", hd.Get("Content-Length"), w.Body.Len())
	}
}

func TestPlanet_DecodesForClientsWithoutGzip(t *testing.T) {
	h := planetHandler(t, planetArchive(t, gzipTiles(t), pmtilestest.Options{}))
	for _, accept := range []string{"", "identity", "gzip;q=0"} {
		w := get(h, "/v1/tiles/planet/2/1/1", map[string]string{"Accept-Encoding": accept})
		if w.Code != http.StatusOK || w.Header().Get("Content-Encoding") != "" || !bytes.Equal(w.Body.Bytes(), plain) {
			t.Errorf("Accept-Encoding %q: %d %q enc=%q", accept, w.Code, w.Body.Bytes(), w.Header().Get("Content-Encoding"))
		}
	}
}

func TestPlanet_UncompressedArchive(t *testing.T) {
	tiles := []pmtilestest.Tile{{Z: 1, X: 0, Y: 0, Data: plain}}
	h := planetHandler(t, planetArchive(t, tiles, pmtilestest.Options{TileCompression: pmtiles.CompressionNone}))
	w := get(h, "/v1/tiles/planet/1/0/0", nil)
	if w.Header().Get("Content-Encoding") != "" || !bytes.Equal(w.Body.Bytes(), plain) {
		t.Errorf("identity client: enc=%q body=%q", w.Header().Get("Content-Encoding"), w.Body.Bytes())
	}
	w = get(h, "/v1/tiles/planet/1/0/0", map[string]string{"Accept-Encoding": "gzip"})
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip client must get gzip on demand, enc=%q", w.Header().Get("Content-Encoding"))
	}
	decoded, err := gunzipForTest(w.Body.Bytes())
	if err != nil || !bytes.Equal(decoded, plain) {
		t.Errorf("on-demand gzip decodes to %q, %v", decoded, err)
	}
}

func TestPlanet_NoContent(t *testing.T) {
	h := planetHandler(t, planetArchive(t, gzipTiles(t), pmtilestest.Options{})) // zoom range 2..5
	for _, path := range []string{
		"/v1/tiles/planet/3/0/0",  // no such tile
		"/v1/tiles/planet/1/0/0",  // below the archive's min zoom
		"/v1/tiles/planet/6/0/0",  // above its max zoom (clients over-zoom)
		"/v1/tiles/planet/15/1/1", // far above
	} {
		w := get(h, path, nil)
		if w.Code != http.StatusNoContent || w.Body.Len() != 0 || w.Header().Get("Vary") != "Accept-Encoding" {
			t.Errorf("%s: %d body=%d vary=%q", path, w.Code, w.Body.Len(), w.Header().Get("Vary"))
		}
	}
}

func TestPlanet_BadCoordinates(t *testing.T) {
	h := planetHandler(t, planetArchive(t, gzipTiles(t), pmtilestest.Options{}))
	for _, path := range []string{
		"/v1/tiles/planet/3/8/0",                    // x out of range for z3
		"/v1/tiles/planet/3/0/8",                    // y out of range
		"/v1/tiles/planet/40/0/0",                   // zoom beyond what a tile id can hold
		"/v1/tiles/planet/a/0/0",                    // not a number
		"/v1/tiles/planet/3/0",                      // wrong shape
		"/v1/tiles/planet/3/0/0/0",                  // wrong shape
		"/v1/tiles/planet/3/-1/0",                   // negative
		"/v1/tiles/planet/3/0/99999999999999999999", // overflow
	} {
		if w := get(h, path, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", path, w.Code)
		}
	}
}

func TestPlanet_Disabled(t *testing.T) {
	h := planetHandler(t, nil)
	if w := get(h, "/v1/tiles/planet/0/0/0", nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("planet without archive: %d, want 503", w.Code)
	}
}

func TestPlanet_LegacyTilesetsUnchanged(t *testing.T) {
	compressed := gzipBytes(t, plain)
	idx, cleanup := createTileIndex(t, compressed)
	defer cleanup()
	arch := planetArchive(t, gzipTiles(t), pmtilestest.Options{})
	h := server.NewTileHTTPHandler(idx, newMockCache(), log.New(log.WithComponent("test"))).WithArchive(arch)
	for _, name := range []string{"default", "base"} {
		w := get(h, "/v1/tiles/"+name+"/0/0/0", map[string]string{"Accept-Encoding": "gzip"})
		if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), compressed) {
			t.Errorf("legacy tileset %q: %d", name, w.Code)
		}
	}
}

func TestPlanet_ETagAndNotModified(t *testing.T) {
	h := planetHandler(t, planetArchive(t, gzipTiles(t), pmtilestest.Options{}))
	first := get(h, "/v1/tiles/planet/2/1/1", map[string]string{"Accept-Encoding": "gzip"})
	etag := first.Header().Get("ETag")
	if etag == "" || etag[:2] != "W/" {
		t.Fatalf("ETag %q must be a weak validator", etag)
	}
	for _, inm := range []string{etag, etag[2:], "*", `"zzz", ` + etag} {
		w := get(h, "/v1/tiles/planet/2/1/1", map[string]string{"If-None-Match": inm, "Accept-Encoding": "gzip"})
		if w.Code != http.StatusNotModified || w.Body.Len() != 0 || w.Header().Get("ETag") != etag {
			t.Errorf("If-None-Match %q: %d body=%d", inm, w.Code, w.Body.Len())
		}
	}
	if w := get(h, "/v1/tiles/planet/2/1/1", map[string]string{"If-None-Match": `W/"other"`}); w.Code != http.StatusOK {
		t.Errorf("non-matching validator: %d", w.Code)
	}
	other := get(h, "/v1/tiles/planet/3/4/2", nil)
	if other.Header().Get("ETag") == etag {
		t.Error("different tiles must have different ETags")
	}
}

type failingArchive struct{ err error }

func (f failingArchive) Header() pmtiles.Header { return pmtiles.Header{MaxZoom: 15} }
func (f failingArchive) ID() string             { return "x" }
func (f failingArchive) Tile(uint8, uint32, uint32) ([]byte, error) {
	return nil, f.err
}

func TestPlanet_ReaderErrors(t *testing.T) {
	if w := get(planetHandler(t, failingArchive{errors.New("boom")}), "/v1/tiles/planet/1/0/0", nil); w.Code != http.StatusInternalServerError {
		t.Errorf("reader error: %d, want 500", w.Code)
	}
	if w := get(planetHandler(t, failingArchive{pmtiles.ErrTileNotFound}), "/v1/tiles/planet/1/0/0", nil); w.Code != http.StatusNoContent {
		t.Errorf("not found: %d, want 204", w.Code)
	}
}

func TestPlanet_CorruptStoredTile(t *testing.T) {
	tiles := []pmtilestest.Tile{{Z: 1, X: 0, Y: 0, Data: []byte("definitely not gzip")}} // archive says gzip
	h := planetHandler(t, planetArchive(t, tiles, pmtilestest.Options{}))
	if w := get(h, "/v1/tiles/planet/1/0/0", nil); w.Code != http.StatusInternalServerError {
		t.Errorf("corrupt gzip tile for a client without gzip: %d, want 500", w.Code)
	}
	// A client that accepts gzip gets the stored bytes as they are (it will fail to decode, as it would on the source).
	if w := get(h, "/v1/tiles/planet/1/0/0", map[string]string{"Accept-Encoding": "gzip"}); w.Code != http.StatusOK {
		t.Errorf("pass-through: %d", w.Code)
	}
}

func gunzipForTest(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	return io.ReadAll(zr)
}
