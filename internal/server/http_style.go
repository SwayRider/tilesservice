// http_style.go implements an HTTP handler for serving map styles.
//
// This handler serves MapLibre GL JS style JSON files from a configured
// directory. It also provides a listing endpoint to discover available styles.
// The "light" and "dark" styles are always advertised as available.

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

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
}

// NewStyleHTTPHandler creates a new handler for serving map styles.
// stylesDir is the directory containing style JSON files. If empty,
// the listing endpoint returns only the default styles and individual
// style requests return 404.
// tilesBaseURL is substituted for {{.TilesBaseURL}} in style templates.
func NewStyleHTTPHandler(stylesDir string, tilesBaseURL string, l *log.Logger) *StyleHTTPHandler {
	return &StyleHTTPHandler{
		stylesDir:    stylesDir,
		tilesBaseURL: tilesBaseURL,
		l:            l.Derive(log.WithComponent("StyleHTTPHandler")),
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
// The "light" and "dark" default styles are always included.
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

	data, err := os.ReadFile(filePath)
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

	tmpl, err := template.New(name).Parse(string(data))
	if err != nil {
		h.l.Errorf("failed to parse style template %s: %v", name, err)
		http.Error(w, "Failed to parse style", http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, styleTemplateData{TilesBaseURL: h.tilesBaseURL}); err != nil {
		h.l.Errorf("failed to execute style template %s: %v", name, err)
		http.Error(w, "Failed to render style", http.StatusInternalServerError)
		return
	}

	h.l.Debugf("serving style: %s (%d bytes)", name, buf.Len())

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(buf.Bytes())
}

// listStyles scans the styles directory and returns all available styles.
// The "light" and "dark" defaults are always present in the result,
// even if the directory is empty or not configured.
func (h *StyleHTTPHandler) listStyles() []StyleInfo {
	defaults := []string{"light", "dark"}
	seen := make(map[string]bool)
	var styles []StyleInfo

	// Add defaults first
	for _, name := range defaults {
		seen[name] = true
		styles = append(styles, StyleInfo{Name: name})
	}

	// Scan directory for additional styles
	if h.stylesDir != "" {
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
			name := strings.TrimSuffix(fname, ".json")
			if !seen[name] {
				seen[name] = true
				styles = append(styles, StyleInfo{Name: name})
			}
		}
	}

	return styles
}
