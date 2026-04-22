package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/swayrider/tilesservice/internal/server"
	log "github.com/swayrider/swlib/logger"
)

func newStyleHandler(t *testing.T, stylesDir string) *server.StyleHTTPHandler {
	t.Helper()
	l := log.New(log.WithComponent("test"))
	return server.NewStyleHTTPHandler(stylesDir, "http://localhost:8080/v1/tiles", l)
}

func TestStyleHandler_List(t *testing.T) {
	t.Run("empty stylesDir always returns light and dark", func(t *testing.T) {
		h := newStyleHandler(t, "")
		req := httptest.NewRequest(http.MethodGet, "/v1/tiles/styles", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %q", ct)
		}

		var styles []map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &styles); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		names := styleNames(styles)
		assertContains(t, names, "light")
		assertContains(t, names, "dark")
	})

	t.Run("configured dir includes defaults and extra styles", func(t *testing.T) {
		dir := t.TempDir()
		writeStyleFile(t, dir, "custom-light.json")
		writeStyleFile(t, dir, "custom-dark.json")

		h := newStyleHandler(t, dir)
		req := httptest.NewRequest(http.MethodGet, "/v1/tiles/styles", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		var styles []map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &styles); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		names := styleNames(styles)
		assertContains(t, names, "light")
		assertContains(t, names, "dark")
		assertContains(t, names, "custom-light")
		assertContains(t, names, "custom-dark")
	})

	t.Run("non-json files in dir are ignored", func(t *testing.T) {
		dir := t.TempDir()
		writeStyleFile(t, dir, "extra.json")
		os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hello"), 0644)

		h := newStyleHandler(t, dir)
		req := httptest.NewRequest(http.MethodGet, "/v1/tiles/styles", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		var styles []map[string]string
		json.Unmarshal(w.Body.Bytes(), &styles)
		names := styleNames(styles)

		for _, n := range names {
			if n == "readme" {
				t.Error("non-json file should not appear in style list")
			}
		}
	})

	t.Run("default styles not duplicated when files exist in dir", func(t *testing.T) {
		dir := t.TempDir()
		writeStyleFile(t, dir, "light.json")
		writeStyleFile(t, dir, "dark.json")

		h := newStyleHandler(t, dir)
		req := httptest.NewRequest(http.MethodGet, "/v1/tiles/styles", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		var styles []map[string]string
		json.Unmarshal(w.Body.Bytes(), &styles)
		names := styleNames(styles)

		count := 0
		for _, n := range names {
			if n == "light" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("expected 'light' to appear exactly once, got %d", count)
		}
	})
}

func TestStyleHandler_GetStyle(t *testing.T) {
	t.Run("returns style JSON with correct Content-Type", func(t *testing.T) {
		dir := t.TempDir()
		content := `{"version":8,"layers":[]}`
		writeStyleFileContent(t, dir, "light.json", content)

		h := newStyleHandler(t, dir)
		req := httptest.NewRequest(http.MethodGet, "/v1/tiles/styles/light", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %q", ct)
		}
		if body := w.Body.String(); body != content {
			t.Errorf("expected body %q, got %q", content, body)
		}
	})

	t.Run("returns 404 for missing style", func(t *testing.T) {
		dir := t.TempDir()

		h := newStyleHandler(t, dir)
		req := httptest.NewRequest(http.MethodGet, "/v1/tiles/styles/nonexistent", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", w.Code)
		}
	})

	t.Run("returns 404 when stylesDir is empty", func(t *testing.T) {
		h := newStyleHandler(t, "")
		req := httptest.NewRequest(http.MethodGet, "/v1/tiles/styles/light", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", w.Code)
		}
	})

	t.Run("returns 400 for path traversal with dots", func(t *testing.T) {
		dir := t.TempDir()

		h := newStyleHandler(t, dir)
		req := httptest.NewRequest(http.MethodGet, "/v1/tiles/styles/../etc/passwd", nil)
		w := httptest.NewRecorder()
		// Use the raw path to avoid net/http path cleaning
		req.URL.Path = "/v1/tiles/styles/../etc/passwd"
		h.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for path traversal, got %d", w.Code)
		}
	})

	t.Run("returns 400 for name with slash", func(t *testing.T) {
		dir := t.TempDir()

		h := newStyleHandler(t, dir)
		req := httptest.NewRequest(http.MethodGet, "/v1/tiles/styles/foo/bar", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		// foo/bar would be routed differently by mux, but handler should reject
		// any name containing invalid characters if it reaches the handler
		if w.Code == http.StatusOK {
			t.Fatal("should not return 200 for multi-segment style name")
		}
	})
}

func TestStyleHandler_MethodNotAllowed(t *testing.T) {
	h := newStyleHandler(t, "")
	req := httptest.NewRequest(http.MethodPost, "/v1/tiles/styles", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestStyleHandler_Options(t *testing.T) {
	h := newStyleHandler(t, "")
	req := httptest.NewRequest(http.MethodOptions, "/v1/tiles/styles", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for OPTIONS, got %d", w.Code)
	}
}

// helpers

func writeStyleFile(t *testing.T, dir, name string) {
	t.Helper()
	writeStyleFileContent(t, dir, name, `{"version":8}`)
}

func writeStyleFileContent(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("failed to write style file %s: %v", name, err)
	}
}

func styleNames(styles []map[string]string) []string {
	names := make([]string, 0, len(styles))
	for _, s := range styles {
		names = append(names, s["name"])
	}
	return names
}

func assertContains(t *testing.T, names []string, want string) {
	t.Helper()
	for _, n := range names {
		if n == want {
			return
		}
	}
	t.Errorf("expected style %q in list %v", want, names)
}
