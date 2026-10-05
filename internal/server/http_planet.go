// http_planet.go serves tiles of the PMTiles tileset ("planet"); the legacy MBTiles tileset is
// handled in http_tile.go.

package server

import (
	"compress/gzip"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/swayrider/swlib/http/compression"
	"github.com/swayrider/tilesservice/internal/pmtiles"
)

// PlanetTileset is the tileset name served from a PMTiles archive. Every other tileset name
// still goes to the legacy MBTiles index.
const PlanetTileset = "planet"

// TileArchive is the PMTiles archive behind the planet tileset (implemented by *pmtiles.Reader).
type TileArchive interface {
	Header() pmtiles.Header
	// ID identifies the archive's content (used in ETags).
	ID() string
	// Tile returns the stored tile bytes or pmtiles.ErrTileNotFound.
	Tile(z uint8, x, y uint32) ([]byte, error)
}

// WithArchive enables the planet tileset on this handler; a nil archive leaves it disabled.
func (h *TileHTTPHandler) WithArchive(a TileArchive) *TileHTTPHandler {
	h.planet = a
	return h
}

// servePlanet answers GET /v1/tiles/planet/{z}/{x}/{y}.
func (h *TileHTTPHandler) servePlanet(w http.ResponseWriter, r *http.Request, z, x, y uint64) {
	if h.planet == nil {
		http.Error(w, "Tileset not available", http.StatusServiceUnavailable)
		return
	}
	if z > pmtiles.MaxZoom {
		http.Error(w, "Invalid zoom level", http.StatusBadRequest)
		return
	}
	if max := uint64(1) << z; x >= max || y >= max {
		http.Error(w, "Tile coordinate out of range for zoom level", http.StatusBadRequest)
		return
	}
	hdr := h.planet.Header()
	w.Header().Set("Vary", "Accept-Encoding")
	if z < uint64(hdr.MinZoom) || z > uint64(hdr.MaxZoom) {
		w.WriteHeader(http.StatusNoContent) // outside the archive's zoom range: clients over-zoom
		return
	}

	data, err := h.planet.Tile(uint8(z), uint32(x), uint32(y))
	if err != nil {
		if errors.Is(err, pmtiles.ErrTileNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.l.Errorf("failed to read planet tile z=%d x=%d y=%d: %v", z, x, y, err)
		http.Error(w, "Failed to retrieve tile", http.StatusInternalServerError)
		return
	}

	// Weak validator: the gzip and identity representations of one tile differ byte-wise.
	etag := fmt.Sprintf(`W/"%s-%d-%d-%d"`, h.planet.ID(), z, x, y)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if ifNoneMatch(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.writeTile(w, r, data, hdr.TileCompression == pmtiles.CompressionGzip)
}

// writeTile sends one tile in the representation the client accepts:
//
//	stored gzip,     client accepts gzip -> stored bytes, Content-Encoding: gzip
//	stored gzip,     client does not     -> decoded
//	stored identity, client accepts gzip -> gzip on demand
//	stored identity, client does not     -> stored bytes
func (h *TileHTTPHandler) writeTile(w http.ResponseWriter, r *http.Request, data []byte, storedGzip bool) {
	wantGzip := acceptsGzip(r)
	switch {
	case storedGzip && !wantGzip:
		plain, err := gunzip(data)
		if err != nil {
			h.l.Errorf("failed to decode stored tile: %v", err)
			http.Error(w, "Failed to decode tile", http.StatusInternalServerError)
			return
		}
		data = plain
	case !storedGzip && wantGzip:
		if packed, err := compression.CompressGzip(data, gzip.BestSpeed); err == nil {
			data = packed
			w.Header().Set("Content-Encoding", "gzip")
		}
	case storedGzip && wantGzip:
		w.Header().Set("Content-Encoding", "gzip")
	}
	w.Header().Set("Content-Type", ContentTypeMVT)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		h.l.Debugf("failed to write tile: %v", err)
	}
}

// ifNoneMatch reports whether an If-None-Match header matches the ETag (weak comparison).
func ifNoneMatch(header, etag string) bool {
	if header == "" {
		return false
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "*" || strings.TrimPrefix(part, "W/") == want {
			return true
		}
	}
	return false
}
