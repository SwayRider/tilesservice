package server_test

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/swayrider/tilesservice/internal/server"
	"github.com/swayrider/tilesservice/internal/tileindex"
	log "github.com/swayrider/swlib/logger"
)

// mockCache is a simple in-memory TileCache for testing.
type mockCache struct {
	data map[string][]byte
}

func newMockCache() *mockCache {
	return &mockCache{data: make(map[string][]byte)}
}

func (m *mockCache) Get(z, x, y uint32) ([]byte, bool) {
	v, ok := m.data[fmt.Sprintf("%d/%d/%d", z, x, y)]
	return v, ok
}

func (m *mockCache) Set(z, x, y uint32, data []byte) {
	m.data[fmt.Sprintf("%d/%d/%d", z, x, y)] = data
}

func (m *mockCache) Close() error { return nil }

// gzipBytes compresses data and returns the result.
func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		t.Fatalf("failed to create gzip writer: %v", err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatalf("failed to write gzip data: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// createTileIndex builds a temporary tile directory with an L0.mbtiles containing
// a single tile at z=0, x=0, y=0 (XYZ), which maps to TMS y=0.
func createTileIndex(t *testing.T, tileData []byte) (*tileindex.TileIndex, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "tile-handler-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	dbPath := filepath.Join(tmpDir, "L0.mbtiles")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		t.Fatalf("failed to open db: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE tiles (
		zoom_level INTEGER,
		tile_column INTEGER,
		tile_row INTEGER,
		tile_data BLOB,
		PRIMARY KEY (zoom_level, tile_column, tile_row)
	)`)
	if err != nil {
		_ = db.Close()
		_ = os.RemoveAll(tmpDir)
		t.Fatalf("failed to create tiles table: %v", err)
	}
	// z=0, x=0, XYZ y=0 → TMS y = (2^0 - 1) - 0 = 0
	if _, err := db.Exec(`INSERT INTO tiles VALUES (0, 0, 0, ?)`, tileData); err != nil {
		_ = db.Close()
		_ = os.RemoveAll(tmpDir)
		t.Fatalf("failed to insert test tile: %v", err)
	}
	_ = db.Close()

	idx := tileindex.New(tmpDir)
	return idx, func() {
		_ = idx.Close()
		_ = os.RemoveAll(tmpDir)
	}
}

func newTileHandler(t *testing.T, idx *tileindex.TileIndex, cache *mockCache) *server.TileHTTPHandler {
	t.Helper()
	l := log.New(log.WithComponent("test"))
	return server.NewTileHTTPHandler(idx, cache, l)
}

func TestTileHandler_Options(t *testing.T) {
	h := newTileHandler(t, nil, newMockCache())
	req := httptest.NewRequest(http.MethodOptions, "/v1/tiles/default/0/0/0", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for OPTIONS, got %d", w.Code)
	}
}

func TestTileHandler_CORSHeaders(t *testing.T) {
	h := newTileHandler(t, nil, newMockCache())
	req := httptest.NewRequest(http.MethodOptions, "/v1/tiles/default/0/0/0", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if v := w.Header().Get("Access-Control-Allow-Origin"); v != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", v)
	}
	if v := w.Header().Get("Access-Control-Allow-Methods"); v == "" {
		t.Error("missing Access-Control-Allow-Methods header")
	}
}

func TestTileHandler_MethodNotAllowed(t *testing.T) {
	h := newTileHandler(t, nil, newMockCache())
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/v1/tiles/default/0/0/0", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: expected 405, got %d", method, w.Code)
		}
	}
}

func TestTileHandler_InvalidPath(t *testing.T) {
	h := newTileHandler(t, nil, newMockCache())
	// Missing y component — only 3 segments after the prefix
	req := httptest.NewRequest(http.MethodGet, "/v1/tiles/default/0/0", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for short path, got %d", w.Code)
	}
}

func TestTileHandler_InvalidCoords(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"non-numeric z", "/v1/tiles/default/abc/0/0"},
		{"non-numeric x", "/v1/tiles/default/0/abc/0"},
		{"non-numeric y", "/v1/tiles/default/0/0/abc"},
	}
	h := newTileHandler(t, nil, newMockCache())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("path %s: expected 400, got %d", tt.path, w.Code)
			}
		})
	}
}

func TestTileHandler_ZoomExceedsMax(t *testing.T) {
	h := newTileHandler(t, nil, newMockCache())
	req := httptest.NewRequest(http.MethodGet, "/v1/tiles/default/17/0/0", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for z=17, got %d", w.Code)
	}
}

func TestTileHandler_NilIndex(t *testing.T) {
	h := newTileHandler(t, nil, newMockCache())
	req := httptest.NewRequest(http.MethodGet, "/v1/tiles/default/0/0/0", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 for nil index, got %d", w.Code)
	}
}

func TestTileHandler_TileNotFound(t *testing.T) {
	// DB contains only (z=0, x=0, TMS y=0). Requesting x=1 returns ErrTileNotFound.
	idx, cleanup := createTileIndex(t, []byte("test-mvt"))
	defer cleanup()

	h := newTileHandler(t, idx, newMockCache())
	req := httptest.NewRequest(http.MethodGet, "/v1/tiles/default/0/1/0", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204 for missing tile, got %d", w.Code)
	}
}

func TestTileHandler_ServesUncompressed(t *testing.T) {
	tileData := []byte("raw-mvt-tile-bytes")
	idx, cleanup := createTileIndex(t, tileData)
	defer cleanup()

	h := newTileHandler(t, idx, newMockCache())
	// No Accept-Encoding header → handler serves raw bytes without compression
	req := httptest.NewRequest(http.MethodGet, "/v1/tiles/default/0/0/0", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != server.ContentTypeMVT {
		t.Errorf("Content-Type = %q, want %q", ct, server.ContentTypeMVT)
	}
	if enc := w.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q, want empty for uncompressed response", enc)
	}
	if !bytes.Equal(w.Body.Bytes(), tileData) {
		t.Error("response body does not match tile data")
	}
}

func TestTileHandler_CacheControl(t *testing.T) {
	idx, cleanup := createTileIndex(t, []byte("raw-mvt-tile-bytes"))
	defer cleanup()

	h := newTileHandler(t, idx, newMockCache())
	req := httptest.NewRequest(http.MethodGet, "/v1/tiles/default/0/0/0", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=86400" {
		t.Errorf("Cache-Control = %q, want %q", cc, "public, max-age=86400")
	}
}

func TestTileHandler_ServesPreCompressed(t *testing.T) {
	// Store a pre-gzip-compressed tile in the MBTiles file
	compressed := gzipBytes(t, []byte("raw-mvt-tile-bytes"))
	idx, cleanup := createTileIndex(t, compressed)
	defer cleanup()

	h := newTileHandler(t, idx, newMockCache())
	req := httptest.NewRequest(http.MethodGet, "/v1/tiles/default/0/0/0", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if enc := w.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip for pre-compressed tile", enc)
	}
	if !bytes.Equal(w.Body.Bytes(), compressed) {
		t.Error("response body does not match pre-compressed tile")
	}
}

func TestTileHandler_CompressesOnDemand(t *testing.T) {
	tileData := []byte("raw-mvt-tile-bytes")
	idx, cleanup := createTileIndex(t, tileData)
	defer cleanup()

	cache := newMockCache()
	h := newTileHandler(t, idx, cache)
	req := httptest.NewRequest(http.MethodGet, "/v1/tiles/default/0/0/0", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if enc := w.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", enc)
	}
	if v := w.Header().Get("Vary"); v != "Accept-Encoding" {
		t.Errorf("Vary = %q, want Accept-Encoding", v)
	}

	// Response body should be valid gzip wrapping the original tile data
	r, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("response body is not valid gzip: %v", err)
	}
	var decompressed bytes.Buffer
	_, _ = decompressed.ReadFrom(r)
	_ = r.Close()
	if !bytes.Equal(decompressed.Bytes(), tileData) {
		t.Error("decompressed response does not match original tile data")
	}

	// Compressed tile should be stored in cache for subsequent requests
	if _, ok := cache.Get(0, 0, 0); !ok {
		t.Error("expected cache to be populated after cache miss")
	}
}

func TestTileHandler_CacheHit(t *testing.T) {
	tileData := []byte("raw-mvt-tile-bytes")
	idx, cleanup := createTileIndex(t, tileData)
	defer cleanup()

	cache := newMockCache()
	// Pre-populate cache with distinct data to prove the cached version is served
	cachedData := gzipBytes(t, []byte("cached-tile-bytes"))
	cache.Set(0, 0, 0, cachedData)

	h := newTileHandler(t, idx, cache)
	req := httptest.NewRequest(http.MethodGet, "/v1/tiles/default/0/0/0", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !bytes.Equal(w.Body.Bytes(), cachedData) {
		t.Error("expected cached tile to be served verbatim")
	}
}
