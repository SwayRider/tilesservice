// http_assets.go serves the glyph ranges (fonts) and sprite sheets of the active release.
//
// MapLibre fetches them with its own requests, so they sit behind the same authentication as the
// tiles and styles. The files live in the release (glyphs/<fontstack>/<range>.pbf and
// sprites/<name>[@2x].{json,png}); every path is built from validated parts, never from the raw
// request.

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	log "github.com/swayrider/swlib/logger"
	"github.com/swayrider/tilesservice/internal/release"
)

// assetCacheLimit bounds the bytes of assets kept in memory per release. The vendored Noto Sans
// glyphs and the sprites are about 14 MB; what does not fit is read from the release each time.
const assetCacheLimit = 64 << 20

// cacheAsset: glyphs and sprites change only with a new release, and a client re-checks with the
// ETag after a day.
const cacheAsset = "public, max-age=86400"

var (
	// validFontName is one font name of a font stack: letters, digits, space and a few
	// separators; no slash, no leading dot.
	validFontName = regexp.MustCompile(`^[A-Za-z0-9_()+-][A-Za-z0-9 _.()+-]*$`)
	glyphFile     = regexp.MustCompile(`^[0-9]{1,6}-[0-9]{1,6}\.pbf$`)
	spriteFile    = regexp.MustCompile(`^([A-Za-z0-9_-]+)(@2x)?\.(json|png)$`)
)

// maxFontStack bounds the length of the fontstack path segment and the names tried in it.
const (
	maxFontStackLen   = 256
	maxFontStackNames = 8
)

// AssetHTTPHandler serves
//   - GET /v1/tiles/fonts/{fontstack}/{range}.pbf
//   - GET /v1/tiles/sprites/{name}[@2x].{json,png}
//
// from the active release. A fontstack that lists several fonts ("A,B") is served from the
// first font that has the range.
type AssetHTTPHandler struct {
	releases ReleaseSource
	l        *log.Logger

	mu    sync.Mutex
	key   string // release the cache belongs to
	bytes int
	cache map[string]asset
}

type asset struct {
	body        []byte
	contentType string
	etag        string
}

// NewAssetHTTPHandler creates the handler; without releases (or without a loaded release) every
// request answers 503.
func NewAssetHTTPHandler(releases ReleaseSource, l *log.Logger) *AssetHTTPHandler {
	return &AssetHTTPHandler{
		releases: releases,
		l:        l.Derive(log.WithComponent("AssetHTTPHandler")),
		cache:    map[string]asset{},
	}
}

// ServeHTTP routes by the first path segment.
func (h *AssetHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/tiles/"), "/")
	segs := strings.Split(path, "/")
	switch {
	case len(segs) == 3 && segs[0] == "fonts":
		h.serveGlyphs(w, r, segs[1], segs[2])
	case len(segs) == 2 && segs[0] == "sprites":
		h.serveSprite(w, r, segs[1])
	default:
		http.Error(w, "Not found", http.StatusNotFound)
	}
}

func (h *AssetHTTPHandler) serveGlyphs(w http.ResponseWriter, r *http.Request, fontstack, file string) {
	names, ok := parseFontStack(fontstack)
	if !ok || !glyphFile.MatchString(file) {
		http.Error(w, "Invalid font or glyph range", http.StatusBadRequest)
		return
	}
	rel, done := h.acquire(w)
	if rel == nil {
		return
	}
	defer done()

	for _, name := range names {
		rp := "glyphs/" + name + "/" + file
		a, err := h.load(r, rel, rp, "application/x-protobuf")
		if errors.Is(err, release.ErrNotFound) {
			continue // try the next font of the stack
		}
		if err != nil {
			h.l.Errorf("failed to read %s: %v", rp, err)
			http.Error(w, "Failed to read glyphs", http.StatusInternalServerError)
			return
		}
		h.write(w, r, a)
		return
	}
	http.Error(w, "Glyph range not found", http.StatusNotFound)
}

func (h *AssetHTTPHandler) serveSprite(w http.ResponseWriter, r *http.Request, file string) {
	m := spriteFile.FindStringSubmatch(file)
	if m == nil {
		http.Error(w, "Invalid sprite name", http.StatusBadRequest)
		return
	}
	contentType := "application/json"
	if m[3] == "png" {
		contentType = "image/png"
	}
	rel, done := h.acquire(w)
	if rel == nil {
		return
	}
	defer done()

	rp := "sprites/" + file
	a, err := h.load(r, rel, rp, contentType)
	if errors.Is(err, release.ErrNotFound) {
		http.Error(w, "Sprite not found", http.StatusNotFound)
		return
	}
	if err != nil {
		h.l.Errorf("failed to read %s: %v", rp, err)
		http.Error(w, "Failed to read sprite", http.StatusInternalServerError)
		return
	}
	h.write(w, r, a)
}

// parseFontStack splits a "A,B" font stack into validated font names.
func parseFontStack(stack string) ([]string, bool) {
	if stack == "" || len(stack) > maxFontStackLen {
		return nil, false
	}
	parts := strings.Split(stack, ",")
	if len(parts) > maxFontStackNames {
		return nil, false
	}
	names := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if !validFontName.MatchString(p) || strings.Contains(p, "..") {
			return nil, false
		}
		names = append(names, p)
	}
	return names, true
}

// acquire gets the active release or answers 503.
func (h *AssetHTTPHandler) acquire(w http.ResponseWriter) (*release.Release, func()) {
	if h.releases == nil {
		http.Error(w, "Assets not available", http.StatusServiceUnavailable)
		return nil, func() {}
	}
	rel, done := h.releases.Acquire()
	if rel == nil {
		done()
		http.Error(w, "Assets not available", http.StatusServiceUnavailable)
		return nil, func() {}
	}
	return rel, done
}

// load returns a release file through the per-release cache.
func (h *AssetHTTPHandler) load(r *http.Request, rel *release.Release, path, contentType string) (asset, error) {
	h.mu.Lock()
	if h.key != rel.Key {
		h.key, h.bytes, h.cache = rel.Key, 0, map[string]asset{}
	}
	if a, ok := h.cache[path]; ok {
		h.mu.Unlock()
		return a, nil
	}
	h.mu.Unlock()

	data, err := rel.ReadFile(r.Context(), path)
	if err != nil {
		return asset{}, err
	}
	sum := sha256.Sum256(data)
	a := asset{body: data, contentType: contentType, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}

	h.mu.Lock()
	if h.key == rel.Key && h.bytes+len(data) <= assetCacheLimit {
		if _, dup := h.cache[path]; !dup {
			h.cache[path] = a
			h.bytes += len(data)
		}
	}
	h.mu.Unlock()
	return a, nil
}

func (h *AssetHTTPHandler) write(w http.ResponseWriter, r *http.Request, a asset) {
	w.Header().Set("Content-Type", a.contentType)
	w.Header().Set("ETag", a.etag)
	w.Header().Set("Cache-Control", cacheAsset)
	if ifNoneMatch(r.Header.Get("If-None-Match"), a.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(a.body)))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(a.body); err != nil {
		h.l.Debugf("failed to write asset: %v", err)
	}
}
