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

// StyleInfo holds the metadata for a single available map style.
type StyleInfo struct {
	Name string `json:"name"`
}

// styleTemplateData holds the data passed to style JSON templates.
type styleTemplateData struct {
	TilesBaseURL string
}

// StyleHTTPHandler serves map style JSON files and style listings over HTTP.
// It handles:
//   - GET /v1/tiles/styles        - list all available styles
//   - GET /v1/tiles/styles/{name} - serve a specific style JSON file
type StyleHTTPHandler struct {
	stylesDir    string
	tilesBaseURL string
	l            *log.Logger

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
//   - GET /v1/tiles/styles          → list all available styles
//   - GET /v1/tiles/styles/{name}   → serve style JSON file
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
	path := strings.TrimPrefix(r.URL.Path, "/v1/tiles/styles")
	path = strings.TrimPrefix(path, "/")

	if path == "" {
		h.handleList(w, r)
	} else {
		h.handleGetStyle(w, r, path)
	}
}

// handleList returns all available map styles as a JSON array.
// Only styles that exist on disk are included.
func (h *StyleHTTPHandler) handleList(w http.ResponseWriter, r *http.Request) {
	styles := h.listStyles()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(styles); err != nil {
		h.l.Errorf("failed to encode style list: %v", err)
	}
}

// handleGetStyle serves the JSON content of a named style file.
func (h *StyleHTTPHandler) handleGetStyle(w http.ResponseWriter, r *http.Request, name string) {
	// Validate name to prevent path traversal
	if !validStyleName.MatchString(name) {
		http.Error(w, "Invalid style name", http.StatusBadRequest)
		return
	}

	if h.stylesDir == "" {
		h.l.Warnln("styles directory not configured")
		http.Error(w, "Styles not configured", http.StatusNotFound)
		return
	}

	filePath := filepath.Join(h.stylesDir, name+".json")

	// Stat first: a missing file is a 404, and the mtime drives the template
	// cache below.
	info, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			h.l.Debugf("style not found: %s", name)
			http.Error(w, "Style not found", http.StatusNotFound)
			return
		}
		h.l.Errorf("failed to read style %s: %v", name, err)
		http.Error(w, "Failed to read style", http.StatusInternalServerError)
		return
	}

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
	if err := tmpl.Execute(&buf, styleTemplateData{TilesBaseURL: h.tilesBaseURL}); err != nil {
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
