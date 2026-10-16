// http_style.go implements an HTTP handler for serving map styles.
//
// This handler serves MapLibre GL JS style JSON files from a configured
// directory. It also provides a listing endpoint that advertises only the
// styles that actually exist on disk.

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"text/template"
	"time"

	log "github.com/swayrider/swlib/logger"
)

// validStyleName matches style names that are safe to use as file names.
// Only alphanumeric characters, dashes, and underscores are allowed,
// preventing path traversal attacks.
var validStyleName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// validStyleVersion matches style versions in URLs ("3", "2026.10", "v2"); "." and ".." are
// rejected separately.
var validStyleVersion = regexp.MustCompile(`^[0-9A-Za-z._-]+$`)

// StyleInfo holds the metadata for a single available map style.
type StyleInfo struct {
	Name string `json:"name"`
}

// styleTemplateData holds the data passed to style JSON templates.
type styleTemplateData struct {
	TilesBaseURL string // the public (gateway) URL of this service including the prefix
	Tileset      string // tileset the style's tile source points at: the release's tileset, or "base" for legacy files
}

// StyleHTTPHandler serves map style JSON files and style listings over HTTP.
// It handles:
//   - GET /v1/tiles/styles                          - list all available styles
//   - GET /v1/tiles/styles/{name}                   - a legacy style file, else a release alias
//     ("light", "dark") or style id
//   - GET /v1/tiles/styles/{id}/{variant}           - newest version of a release style
//   - GET /v1/tiles/styles/{id}/{version}/{variant} - a fixed version of a release style
type StyleHTTPHandler struct {
	stylesDir    string
	tilesBaseURL string
	l            *log.Logger

	releases ReleaseSource // styles of the active release; nil = legacy files only
	rendered *renderCache  // rendered release styles, per release

	// tmplCache caches parsed style templates keyed by style name, along with
	// the file mtime they were parsed from, so edits are picked up without a
	// restart and files are not re-read and re-parsed on every request.
	tmplMu    sync.Mutex
	tmplCache map[string]styleTemplate
}

// styleTemplate pairs a parsed template with the file modification time it
// was parsed from, for mtime-based cache invalidation.
type styleTemplate struct {
	tmpl    *template.Template
	modTime time.Time
}

// NewStyleHTTPHandler creates a new handler for serving map styles.
// stylesDir is the directory containing style JSON files. If empty,
// the listing endpoint returns an empty list and individual style
// requests return 404.
// tilesBaseURL is substituted for {{.TilesBaseURL}} in style templates.
func NewStyleHTTPHandler(stylesDir string, tilesBaseURL string, l *log.Logger) *StyleHTTPHandler {
	return &StyleHTTPHandler{
		stylesDir:    stylesDir,
		tilesBaseURL: tilesBaseURL,
		l:            l.Derive(log.WithComponent("StyleHTTPHandler")),
		tmplCache:    make(map[string]styleTemplate),
	}
}

// ServeHTTP routes style requests to the appropriate sub-handler.
//
// Expected URL formats:
//   - GET /v1/tiles/styles                          → list all available styles
//   - GET /v1/tiles/styles/{name}                   → serve a style by name
//   - GET /v1/tiles/styles/{id}/{variant}           → newest version of a release style
//   - GET /v1/tiles/styles/{id}/{version}/{variant} → a fixed version
func (h *StyleHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	// Determine sub-path: strip /v1/tiles/styles prefix
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/tiles/styles"), "/")
	var segs []string
	if path != "" {
		segs = strings.Split(path, "/")
	}

	switch len(segs) {
	case 0:
		h.handleList(w, r)
	case 1:
		h.handleGetStyle(w, r, segs[0])
	case 2:
		h.serveReleaseVariant(w, r, segs[0], "", segs[1])
	case 3:
		h.serveReleaseVariant(w, r, segs[0], segs[1], segs[2])
	default:
		http.Error(w, "Style not found", http.StatusNotFound)
	}
}

// handleList returns all available map styles as a JSON array. Every entry has a "name" that
// GET /styles/{name} serves. Legacy files on disk come first; with a release loaded, the
// aliases "light" and "dark" (unless a legacy file has that name) and the release's styles
// (id, label, version, variants) follow.
func (h *StyleHTTPHandler) handleList(w http.ResponseWriter, r *http.Request) {
	legacy := h.listStyles()
	styles := make([]any, 0, len(legacy))
	have := map[string]bool{}
	for _, s := range legacy {
		styles = append(styles, s)
		have[s.Name] = true
	}
	if h.releases != nil {
		rel, done := h.releases.Acquire()
		if rel != nil {
			if def := defaultStyle(rel.Manifest); def != nil {
				for _, v := range styleVariants {
					if _, ok := def.Variants[v]; ok && !have[v] {
						styles = append(styles, StyleInfo{Name: v})
					}
				}
			}
			for _, rs := range releaseStyles(rel.Manifest) {
				if !have[rs.Name] {
					styles = append(styles, rs)
				}
			}
		}
		done()
		w.Header().Set("Cache-Control", cacheStyleList)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(styles); err != nil {
		h.l.Errorf("failed to encode style list: %v", err)
	}
}

// handleGetStyle serves a style by name: a legacy style file in the styles directory wins, so
// clients of the legacy tileset keep their style; otherwise the name is looked up in the active
// release (alias "light"/"dark" or a style id).
func (h *StyleHTTPHandler) handleGetStyle(w http.ResponseWriter, r *http.Request, name string) {
	// Validate name to prevent path traversal
	if !validStyleName.MatchString(name) {
		http.Error(w, "Invalid style name", http.StatusBadRequest)
		return
	}

	if h.stylesDir != "" {
		filePath := filepath.Join(h.stylesDir, name+".json")

		// Stat first: a missing file is not an error here (the release may have the name), and
		// the mtime drives the template cache below.
		info, err := os.Stat(filePath)
		switch {
		case err == nil:
			h.serveLegacyStyle(w, name, filePath, info)
			return
		case !os.IsNotExist(err):
			h.l.Errorf("failed to read style %s: %v", name, err)
			http.Error(w, "Failed to read style", http.StatusInternalServerError)
			return
		}
	} else if h.releases == nil {
		h.l.Warnln("styles directory not configured")
	}

	if h.serveReleaseName(w, r, name) {
		return
	}
	h.l.Debugf("style not found: %s", name)
	http.Error(w, "Style not found", http.StatusNotFound)
}

// serveLegacyStyle renders a style file from the styles directory.
func (h *StyleHTTPHandler) serveLegacyStyle(w http.ResponseWriter, name, filePath string, info os.FileInfo) {
	// Serve the cached parsed template when the file is unchanged.
	h.tmplMu.Lock()
	cached, ok := h.tmplCache[name]
	h.tmplMu.Unlock()
	if ok && cached.modTime.Equal(info.ModTime()) {
		h.renderStyle(w, name, cached.tmpl)
		return
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		h.l.Errorf("failed to read style %s: %v", name, err)
		http.Error(w, "Failed to read style", http.StatusInternalServerError)
		return
	}

	tmpl, err := template.New(name).Parse(string(data))
	if err != nil {
		h.l.Errorf("failed to parse style template %s: %v", name, err)
		http.Error(w, "Failed to parse style", http.StatusInternalServerError)
		return
	}

	h.tmplMu.Lock()
	h.tmplCache[name] = styleTemplate{tmpl: tmpl, modTime: info.ModTime()}
	h.tmplMu.Unlock()

	h.renderStyle(w, name, tmpl)
}

// renderStyle executes a parsed style template and writes the response.
// Parsed templates are safe for concurrent execution.
func (h *StyleHTTPHandler) renderStyle(w http.ResponseWriter, name string, tmpl *template.Template) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, styleTemplateData{TilesBaseURL: h.tilesBaseURL, Tileset: legacyTileset}); err != nil {
		h.l.Errorf("failed to execute style template %s: %v", name, err)
		http.Error(w, "Failed to render style", http.StatusInternalServerError)
		return
	}

	h.l.Debugf("serving style: %s (%d bytes)", name, buf.Len())

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(buf.Bytes()); err != nil {
		h.l.Debugf("failed to write style response: %v", err)
	}
}

// listStyles scans the styles directory and returns the styles that are
// actually available on disk. Only existing .json files are advertised, so a
// client never follows the listing into a 404. An empty or unreadable
// directory yields an empty list.
func (h *StyleHTTPHandler) listStyles() []StyleInfo {
	var styles []StyleInfo

	if h.stylesDir == "" {
		return styles
	}

	entries, err := os.ReadDir(h.stylesDir)
	if err != nil {
		h.l.Warnf("failed to read styles directory %s: %v", h.stylesDir, err)
		return styles
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fname := entry.Name()
		if !strings.HasSuffix(fname, ".json") {
			continue
		}
		styles = append(styles, StyleInfo{Name: strings.TrimSuffix(fname, ".json")})
	}

	return styles
}
