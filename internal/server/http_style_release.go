// http_style_release.go serves the styles of the active release (manifest.json): named styles with
// a light and a dark variant, each with a version. The files are Go templates, rendered per
// request with the public URLs; the rendered result is cached per release.

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"text/template"

	"github.com/swayrider/tilesservice/internal/release"
)

// Cache lifetimes. A versioned style URL always returns the same content (the data-manager bumps
// the version when the content changes), so clients may keep it; the unversioned URLs follow
// the newest version and must be re-checked soon.
const (
	cacheVersionedStyle   = "public, max-age=31536000, immutable"
	cacheUnversionedStyle = "public, max-age=300"
	cacheStyleList        = "public, max-age=60"
)

// legacyTileset is the tileset the legacy style files (STYLES_PATH) point at.
const legacyTileset = "base"

var styleVariants = []string{"light", "dark"}

// ReleaseSource hands out the active release; the caller must call the returned func when done
// with it. It returns (nil, no-op) when no release is loaded (implemented by *release.Holder).
type ReleaseSource interface {
	Acquire() (*release.Release, func())
}

// WithReleases makes the handler serve the styles of the active release next to the legacy
// style files. Legacy files keep their names: a file STYLES_PATH/light.json wins over the
// release alias "light", so clients of the legacy tileset are unaffected until it is removed.
func (h *StyleHTTPHandler) WithReleases(src ReleaseSource) *StyleHTTPHandler {
	h.releases = src
	h.rendered = &renderCache{entries: map[string]renderedStyle{}}
	return h
}

// ValidateStyles returns a check for release.Holder.SetValidator: every style variant of the
// manifest must exist, parse as a template, render and produce valid JSON. A broken style then
// rejects the release at load time instead of failing at the first request.
func ValidateStyles(tilesBaseURL string) func(context.Context, *release.Release) error {
	return func(ctx context.Context, rel *release.Release) error {
		for _, s := range rel.Manifest.Styles {
			for _, variant := range styleVariants {
				p, ok := s.Variants[variant]
				if !ok {
					continue
				}
				data, err := rel.ReadFile(ctx, p)
				if err != nil {
					return fmt.Errorf("style %s/%s/%s: %w", s.ID, s.Version, variant, err)
				}
				if _, err := renderTemplate(s.ID+"/"+variant, data, tilesBaseURL, rel.Manifest.Tileset.Name); err != nil {
					return fmt.Errorf("style %s/%s/%s: %w", s.ID, s.Version, variant, err)
				}
			}
		}
		return nil
	}
}

// renderTemplate executes a style template and checks that the result is JSON.
func renderTemplate(name string, tmplSrc []byte, tilesBaseURL, tileset string) ([]byte, error) {
	tmpl, err := template.New(name).Parse(string(tmplSrc))
	if err != nil {
		return nil, fmt.Errorf("parsing template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, styleTemplateData{TilesBaseURL: tilesBaseURL, Tileset: tileset}); err != nil {
		return nil, fmt.Errorf("rendering template: %w", err)
	}
	if !json.Valid(buf.Bytes()) {
		return nil, fmt.Errorf("rendered style is not valid JSON")
	}
	return buf.Bytes(), nil
}

// renderedStyle is a rendered style with its validator.
type renderedStyle struct {
	body []byte
	etag string
}

// renderCache holds the rendered styles of one release; it is emptied when another release
// becomes active, so a switch never serves a style of the old release.
type renderCache struct {
	mu      sync.Mutex
	key     string
	entries map[string]renderedStyle
}

func (c *renderCache) get(releaseKey, path string) (renderedStyle, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key != releaseKey {
		return renderedStyle{}, false
	}
	e, ok := c.entries[path]
	return e, ok
}

func (c *renderCache) put(releaseKey, path string, e renderedStyle) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key != releaseKey {
		c.key = releaseKey
		c.entries = map[string]renderedStyle{}
	}
	c.entries[path] = e
}

// releaseStyleInfo is one style in the listing.
type releaseStyleInfo struct {
	Name     string   `json:"name"` // = id; GET /styles/{name} serves its light variant
	ID       string   `json:"id"`
	Label    string   `json:"label,omitempty"`
	Version  string   `json:"version"`
	Default  bool     `json:"default,omitempty"`
	Variants []string `json:"variants"`
}

// releaseStyles lists the newest version of every style of the release; the default style first.
func releaseStyles(m *release.Manifest) []releaseStyleInfo {
	def := defaultStyle(m)
	var out []releaseStyleInfo
	seen := map[string]bool{}
	for _, s := range m.Styles {
		if seen[s.ID] {
			continue
		}
		seen[s.ID] = true
		latest := latestStyle(m, s.ID)
		info := releaseStyleInfo{Name: latest.ID, ID: latest.ID, Label: latest.Label, Version: string(latest.Version),
			Default: def != nil && def.ID == latest.ID}
		for _, v := range styleVariants {
			if _, ok := latest.Variants[v]; ok {
				info.Variants = append(info.Variants, v)
			}
		}
		out = append(out, info)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Default && !out[j].Default })
	return out
}

// defaultStyle is the style marked default, else the first one; nil without styles.
func defaultStyle(m *release.Manifest) *release.Style {
	for i := range m.Styles {
		if m.Styles[i].Default {
			return latestStyle(m, m.Styles[i].ID)
		}
	}
	if len(m.Styles) > 0 {
		return latestStyle(m, m.Styles[0].ID)
	}
	return nil
}

// latestStyle returns the newest version of the style id (numeric versions compare as numbers,
// others as strings); nil if the id is not in the manifest.
func latestStyle(m *release.Manifest, id string) *release.Style {
	var best *release.Style
	for i := range m.Styles {
		s := &m.Styles[i]
		if s.ID != id {
			continue
		}
		if best == nil || versionLess(string(best.Version), string(s.Version)) {
			best = s
		}
	}
	return best
}

func versionLess(a, b string) bool {
	an, aerr := strconv.ParseUint(a, 10, 64)
	bn, berr := strconv.ParseUint(b, 10, 64)
	if aerr == nil && berr == nil {
		return an < bn
	}
	return a < b
}

// findStyle returns the style id at an exact version.
func findStyle(m *release.Manifest, id, version string) *release.Style {
	for i := range m.Styles {
		if m.Styles[i].ID == id && string(m.Styles[i].Version) == version {
			return &m.Styles[i]
		}
	}
	return nil
}

// serveReleaseName answers GET /styles/{name} from the release: the aliases "light" and "dark"
// (that variant of the default style) and a style id (its light variant, else its dark one).
// It reports false when the release has no such name.
func (h *StyleHTTPHandler) serveReleaseName(w http.ResponseWriter, r *http.Request, name string) bool {
	if h.releases == nil {
		return false
	}
	rel, done := h.releases.Acquire()
	defer done()
	if rel == nil {
		return false
	}
	var s *release.Style
	variant := ""
	switch name {
	case "light", "dark":
		s, variant = defaultStyle(rel.Manifest), name
	default:
		if s = latestStyle(rel.Manifest, name); s != nil {
			for _, v := range styleVariants {
				if _, ok := s.Variants[v]; ok {
					variant = v
					break
				}
			}
		}
	}
	if s == nil {
		return false
	}
	if _, ok := s.Variants[variant]; !ok {
		return false
	}
	h.writeReleaseStyle(w, r, rel, s, variant, cacheUnversionedStyle)
	return true
}

// serveReleaseVariant answers GET /styles/{id}/{variant} (newest version) and
// GET /styles/{id}/{version}/{variant} (fixed version; version is "" for the first form).
func (h *StyleHTTPHandler) serveReleaseVariant(w http.ResponseWriter, r *http.Request, id, version, variant string) {
	if !validStyleName.MatchString(id) || (version != "" && !validStyleVersion.MatchString(version)) || version == "." || version == ".." {
		http.Error(w, "Invalid style name", http.StatusBadRequest)
		return
	}
	if variant != "light" && variant != "dark" {
		http.Error(w, "Style variant not found", http.StatusNotFound)
		return
	}
	if h.releases == nil {
		http.Error(w, "Style not found", http.StatusNotFound)
		return
	}
	rel, done := h.releases.Acquire()
	defer done()
	if rel == nil {
		http.Error(w, "Styles not available", http.StatusServiceUnavailable)
		return
	}
	var s *release.Style
	cacheControl := cacheVersionedStyle
	if version == "" {
		s, cacheControl = latestStyle(rel.Manifest, id), cacheUnversionedStyle
	} else {
		s = findStyle(rel.Manifest, id, version)
	}
	if s == nil {
		http.Error(w, "Style not found", http.StatusNotFound)
		return
	}
	if _, ok := s.Variants[variant]; !ok {
		http.Error(w, "Style variant not found", http.StatusNotFound)
		return
	}
	h.writeReleaseStyle(w, r, rel, s, variant, cacheControl)
}

// writeReleaseStyle renders one variant (cached per release) and writes it with ETag and
// Cache-Control; a matching If-None-Match answers 304.
func (h *StyleHTTPHandler) writeReleaseStyle(w http.ResponseWriter, r *http.Request, rel *release.Release, s *release.Style, variant, cacheControl string) {
	path := s.Variants[variant]
	out, ok := h.rendered.get(rel.Key, path)
	if !ok {
		data, err := rel.ReadFile(r.Context(), path)
		if err != nil {
			h.l.Errorf("failed to read style %s/%s/%s: %v", s.ID, s.Version, variant, err)
			http.Error(w, "Failed to read style", http.StatusInternalServerError)
			return
		}
		body, err := renderTemplate(s.ID+"/"+variant, data, h.tilesBaseURL, rel.Manifest.Tileset.Name)
		if err != nil {
			h.l.Errorf("failed to render style %s/%s/%s: %v", s.ID, s.Version, variant, err)
			http.Error(w, "Failed to render style", http.StatusInternalServerError)
			return
		}
		sum := sha256.Sum256(body)
		out = renderedStyle{body: body, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		h.rendered.put(rel.Key, path, out)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", out.etag)
	w.Header().Set("Cache-Control", cacheControl)
	if ifNoneMatch(r.Header.Get("If-None-Match"), out.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(out.body); err != nil {
		h.l.Debugf("failed to write style response: %v", err)
	}
}
