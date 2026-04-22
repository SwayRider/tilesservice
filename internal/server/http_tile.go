// http_tile.go implements an HTTP handler for serving vector tiles.
//
// This handler returns raw MVT bytes directly, which is required for
// map clients like MapLibre GL JS that expect binary tile data.

package server

import (
	"compress/gzip"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/swayrider/tilesservice/internal/mbtiles"
	"github.com/swayrider/tilesservice/internal/tilecache"
	"github.com/swayrider/tilesservice/internal/tileindex"
	"github.com/swayrider/swlib/http/compression"
	log "github.com/swayrider/swlib/logger"
)

// ContentTypeMVT is the MIME type for Mapbox Vector Tiles.
const ContentTypeMVT = "application/vnd.mapbox-vector-tile"

// TileHTTPHandler serves raw MVT tiles over HTTP.
// It handles requests to /v1/tiles/{tileset}/{z}/{x}/{y} and returns
// raw binary tile data with appropriate content-type and CORS headers.
// Supports automatic gzip compression with caching for improved performance.
type TileHTTPHandler struct {
	idx   *tileindex.TileIndex
	cache tilecache.TileCache
	l     *log.Logger
}

// NewTileHTTPHandler creates a new HTTP handler for serving raw tiles.
// The cache parameter enables compression caching for improved performance.
func NewTileHTTPHandler(idx *tileindex.TileIndex, cache tilecache.TileCache, l *log.Logger) *TileHTTPHandler {
	return &TileHTTPHandler{
		idx:   idx,
		cache: cache,
		l:     l.Derive(log.WithComponent("TileHTTPHandler")),
	}
}

// ServeHTTP handles tile requests and returns raw MVT bytes.
//
// Expected URL format: /v1/tiles/{tileset}/{z}/{x}/{y}
// Response: Raw MVT bytes with Content-Type: application/vnd.mapbox-vector-tile
func (h *TileHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Set CORS headers for browser access
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	// Handle preflight requests
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse URL: /v1/tiles/{tileset}/{z}/{x}/{y}
	path := strings.TrimPrefix(r.URL.Path, "/v1/tiles/")
	parts := strings.Split(path, "/")

	if len(parts) != 4 {
		http.Error(w, "Invalid tile path", http.StatusBadRequest)
		return
	}

	// tileset := parts[0] // Currently unused, reserved for future multi-tileset support
	z, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		http.Error(w, "Invalid zoom level", http.StatusBadRequest)
		return
	}

	x, err := strconv.ParseUint(parts[2], 10, 32)
	if err != nil {
		http.Error(w, "Invalid x coordinate", http.StatusBadRequest)
		return
	}

	y, err := strconv.ParseUint(parts[3], 10, 32)
	if err != nil {
		http.Error(w, "Invalid y coordinate", http.StatusBadRequest)
		return
	}

	// Validate zoom level
	if z > 16 {
		http.Error(w, "Zoom level exceeds maximum of 16", http.StatusBadRequest)
		return
	}

	// Check if tile index is configured
	if h.idx == nil {
		h.l.Errorln("tile index not configured")
		http.Error(w, "Tile index not configured", http.StatusServiceUnavailable)
		return
	}

	// Fetch tile
	tileData, err := h.idx.GetTile(uint32(z), uint32(x), uint32(y))
	if err != nil {
		if err == mbtiles.ErrTileNotFound {
			// Debug log for missing tile
			h.l.Debugf("tile not found in MBTiles z=%d x=%d y=%d", z, x, y)
			// Return 204 No Content for missing tiles (standard practice)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.l.Errorf("failed to get tile z=%d x=%d y=%d: %v", z, x, y, err)
		http.Error(w, "Failed to retrieve tile", http.StatusInternalServerError)
		return
	}

	// Debug log for tile fetched from MBTiles
	h.l.Debugf("fetched tile from MBTiles z=%d x=%d y=%d size=%d", z, x, y, len(tileData))

	// Set common headers
	w.Header().Set("Content-Type", ContentTypeMVT)
	w.Header().Set("Cache-Control", "public, max-age=86400") // Cache for 24 hours

	// Check if tile is already compressed
	if compression.IsGzipped(tileData) {
		// Debug log for pre-compressed tile
		h.l.Debugf("serving pre-compressed tile z=%d x=%d y=%d", z, x, y)
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tileData)))
		w.WriteHeader(http.StatusOK)
		w.Write(tileData)
		return
	}

	// Check if client supports gzip
	if !compression.SupportsGzip(r) {
		// Debug log for uncompressed serve
		h.l.Debugf("serving uncompressed tile z=%d x=%d y=%d (client doesn't support gzip)", z, x, y)
		// Serve uncompressed (backward compatible)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tileData)))
		w.WriteHeader(http.StatusOK)
		w.Write(tileData)
		return
	}

	// Check cache for compressed version
	if compressed, ok := h.cache.Get(uint32(z), uint32(x), uint32(y)); ok {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(compressed)))
		w.Header().Set("Vary", "Accept-Encoding")
		w.WriteHeader(http.StatusOK)
		w.Write(compressed)
		// Note: Cache hit logging happens in the cache Get() method
		return
	}

	// Debug log before compression
	h.l.Debugf("compressing tile z=%d x=%d y=%d (cache miss)", z, x, y)

	// Compress and cache
	compressed, err := compression.CompressGzip(tileData, gzip.BestSpeed)
	if err != nil {
		h.l.Errorf("failed to compress tile z=%d x=%d y=%d: %v, serving uncompressed", z, x, y, err)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tileData)))
		w.WriteHeader(http.StatusOK)
		w.Write(tileData)
		return
	}

	// Store in cache
	h.cache.Set(uint32(z), uint32(x), uint32(y), compressed)

	// Serve compressed
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(compressed)))
	w.Header().Set("Vary", "Accept-Encoding")
	w.WriteHeader(http.StatusOK)
	w.Write(compressed)

	ratio := float64(len(tileData)) / float64(len(compressed))
	h.l.Debugf("compressed and cached tile z=%d x=%d y=%d original=%d compressed=%d ratio=%.1fx",
		z, x, y, len(tileData), len(compressed), ratio)
}
