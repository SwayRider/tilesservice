package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	log "github.com/swayrider/swlib/logger"
	"github.com/swayrider/tilesservice/internal/pmtiles/pmtilestest"
	"github.com/swayrider/tilesservice/internal/release"
	"github.com/swayrider/tilesservice/internal/server"
	"github.com/swayrider/tilesservice/internal/tilesource"
)

const baseURL = "https://api.example.com/v1/tiles"

// styleTemplate is a minimal style that uses both template variables; the first %s is a marker
// that identifies the file, the second the sprite flavor.
const styleTemplate = `{"version":8,"name":"%s","sources":{"p":{"type":"vector","tiles":["{{.TilesBaseURL}}/{{.Tileset}}/{z}/{x}/{y}"],"maxzoom":15}},` +
	`"glyphs":"{{.TilesBaseURL}}/fonts/{fontstack}/{range}.pbf","sprite":"{{.TilesBaseURL}}/sprites/%s","layers":[]}`

type styleSpec struct {
	id, label, version string
	def                bool
	variants           []string
}

var classic = styleSpec{id: "classic", label: "Classic", version: "3", def: true, variants: []string{"light", "dark"}}

// releaseDir writes a release with the given styles to root/releases/<id> and returns its path.
func releaseDir(t *testing.T, root, id string, styles ...styleSpec) string {
	t.Helper()
	dir := filepath.Join(root, "releases", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := pmtilestest.Build([]pmtilestest.Tile{{Z: 0, X: 0, Y: 0, Data: pmtilestest.Gzip([]byte("t"))}}, pmtilestest.Options{GzipDirectories: true})
	writeFile(t, dir, "tiles.pmtiles", string(archive))
	var entries []string
	for _, s := range styles {
		variants := map[string]string{}
		for _, v := range s.variants {
			rel := fmt.Sprintf("styles/%s/%s/%s.json", s.id, s.version, v)
			variants[v] = rel
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, filepath.FromSlash(rel))), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, dir, rel, fmt.Sprintf(styleTemplate, s.id+"-"+s.version+"-"+v, v))
		}
		vj, _ := json.Marshal(variants)
		entries = append(entries, fmt.Sprintf(`{"id":%q,"label":%q,"version":%q,"default":%v,"variants":%s}`, s.id, s.label, s.version, s.def, vj))
	}
	writeFile(t, dir, "manifest.json", fmt.Sprintf(
		`{"schema":1,"release":%q,"tileset":{"name":"planet","build":"b","date":"d","schema_version":"1","file":"tiles.pmtiles"},"styles":[%s]}`,
		id, strings.Join(entries, ",")))
	return dir
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func pointTo(t *testing.T, root, id string) {
	t.Helper()
	tmp := filepath.Join(root, ".current.tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink("releases/"+id, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
}

func newHolder(t *testing.T, root string) *release.Holder {
	t.Helper()
	store, err := release.OpenStore(root, tilesource.S3{})
	if err != nil {
		t.Fatal(err)
	}
	h := release.NewHolder(store, log.New(log.WithComponent("test")))
	h.SetValidator(server.ValidateStyles(baseURL))
	t.Cleanup(h.Close)
	return h
}

func releaseStyleHandler(t *testing.T, legacyDir string, h *release.Holder) *server.StyleHTTPHandler {
	t.Helper()
	return server.NewStyleHTTPHandler(legacyDir, baseURL, log.New(log.WithComponent("test"))).WithReleases(h)
}

// loaded returns a holder with a release containing the styles already loaded.
func loaded(t *testing.T, styles ...styleSpec) (*release.Holder, string) {
	t.Helper()
	root := t.TempDir()
	releaseDir(t, root, "r-1", styles...)
	pointTo(t, root, "r-1")
	h := newHolder(t, root)
	if _, err := h.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return h, root
}

// getRaw serves path without net/http's path cleaning, like the existing traversal test.
func getRaw(h http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.URL.Path = path
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func styleName(t *testing.T, body []byte) string {
	t.Helper()
	var s struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("not JSON: %q", body)
	}
	return s.Name
}

func TestReleaseStyles_List(t *testing.T) {
	h, _ := loaded(t, classic, styleSpec{id: "vivid", label: "Vivid", version: "1", variants: []string{"light"}})
	w := get(releaseStyleHandler(t, "", h), "/v1/tiles/styles", nil)
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") == "" {
		t.Fatalf("status %d headers %v", w.Code, w.Header())
	}
	var list []struct {
		Name     string   `json:"name"`
		ID       string   `json:"id"`
		Label    string   `json:"label"`
		Version  string   `json:"version"`
		Default  bool     `json:"default"`
		Variants []string `json:"variants"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name)
	}
	// the aliases of the default style, then the release styles with the default first
	if got := strings.Join(names, ","); got != "light,dark,classic,vivid" {
		t.Fatalf("names = %s", got)
	}
	if c := list[2]; c.ID != "classic" || c.Label != "Classic" || c.Version != "3" || !c.Default || strings.Join(c.Variants, ",") != "light,dark" {
		t.Fatalf("classic = %+v", c)
	}
	if v := list[3]; v.Default || strings.Join(v.Variants, ",") != "light" {
		t.Fatalf("vivid = %+v", v)
	}
}

func TestReleaseStyles_WithoutRelease(t *testing.T) {
	store, _ := release.OpenStore(t.TempDir(), tilesource.S3{})
	h := release.NewHolder(store, log.New(log.WithComponent("test")))
	t.Cleanup(h.Close)
	sh := releaseStyleHandler(t, "", h)

	if w := get(sh, "/v1/tiles/styles", nil); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("list without a release: %d %q", w.Code, w.Body.String())
	}
	if w := get(sh, "/v1/tiles/styles/light", nil); w.Code != http.StatusNotFound {
		t.Fatalf("alias without a release: %d", w.Code)
	}
	if w := get(sh, "/v1/tiles/styles/classic/light", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("variant without a release: %d", w.Code)
	}
}

func TestReleaseStyles_Serve(t *testing.T) {
	h, _ := loaded(t, classic, styleSpec{id: "vivid", label: "Vivid", version: "1", variants: []string{"light"}})
	sh := releaseStyleHandler(t, "", h)

	for path, want := range map[string]string{
		"/v1/tiles/styles/light":           "classic-3-light", // alias of the default style
		"/v1/tiles/styles/dark":            "classic-3-dark",
		"/v1/tiles/styles/classic":         "classic-3-light", // style id: its light variant
		"/v1/tiles/styles/vivid":           "vivid-1-light",
		"/v1/tiles/styles/classic/dark":    "classic-3-dark",  // newest version
		"/v1/tiles/styles/classic/3/light": "classic-3-light", // fixed version
		"/v1/tiles/styles/vivid/1/light":   "vivid-1-light",
	} {
		w := get(sh, path, nil)
		if w.Code != http.StatusOK {
			t.Errorf("%s: status %d", path, w.Code)
			continue
		}
		if got := styleName(t, w.Body.Bytes()); got != want {
			t.Errorf("%s: style %q, want %q", path, got, want)
		}
		if w.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: Content-Type %q", path, w.Header().Get("Content-Type"))
		}
	}

	// the template variables are filled in
	body := get(sh, "/v1/tiles/styles/classic/3/light", nil).Body.String()
	for _, want := range []string{
		`"tiles":["https://api.example.com/v1/tiles/planet/{z}/{x}/{y}"]`,
		`"glyphs":"https://api.example.com/v1/tiles/fonts/{fontstack}/{range}.pbf"`,
		`"sprite":"https://api.example.com/v1/tiles/sprites/light"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered style lacks %s:\n%s", want, body)
		}
	}

	for path, code := range map[string]int{
		"/v1/tiles/styles/nope":              http.StatusNotFound,
		"/v1/tiles/styles/nope/light":        http.StatusNotFound,
		"/v1/tiles/styles/classic/9/light":   http.StatusNotFound, // version not in the manifest
		"/v1/tiles/styles/classic/sepia":     http.StatusNotFound, // unknown variant
		"/v1/tiles/styles/vivid/dark":        http.StatusNotFound, // style without that variant
		"/v1/tiles/styles/vivid/1/dark":      http.StatusNotFound,
		"/v1/tiles/styles/classic/3/light/x": http.StatusNotFound, // too many segments
		"/v1/tiles/styles/../light":          http.StatusBadRequest,
		"/v1/tiles/styles/classic/../light":  http.StatusBadRequest,
		"/v1/tiles/styles/classic/./light":   http.StatusBadRequest,
		"/v1/tiles/styles/a b/light":         http.StatusBadRequest,
	} {
		if w := getRaw(sh, path); w.Code != code {
			t.Errorf("%s: status %d, want %d", path, w.Code, code)
		}
	}
}

func TestReleaseStyles_CacheHeadersAndETag(t *testing.T) {
	h, _ := loaded(t, classic)
	sh := releaseStyleHandler(t, "", h)

	versioned := get(sh, "/v1/tiles/styles/classic/3/light", nil)
	if cc := versioned.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("versioned Cache-Control %q", cc)
	}
	for _, p := range []string{"/v1/tiles/styles/classic/light", "/v1/tiles/styles/light", "/v1/tiles/styles/classic"} {
		cc := get(sh, p, nil).Header().Get("Cache-Control")
		if cc == "" || strings.Contains(cc, "immutable") {
			t.Errorf("%s: Cache-Control %q should be short-lived", p, cc)
		}
	}

	etag := versioned.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	if e := get(sh, "/v1/tiles/styles/classic/3/light", nil).Header().Get("ETag"); e != etag {
		t.Fatalf("ETag not stable: %q vs %q", e, etag)
	}
	if e := get(sh, "/v1/tiles/styles/classic/3/dark", nil).Header().Get("ETag"); e == etag {
		t.Fatal("light and dark share an ETag")
	}
	for _, inm := range []string{etag, "W/" + etag, `"nope", ` + etag, "*"} {
		w := get(sh, "/v1/tiles/styles/classic/3/light", map[string]string{"If-None-Match": inm})
		if w.Code != http.StatusNotModified || w.Body.Len() != 0 {
			t.Errorf("If-None-Match %q: status %d, %d body bytes", inm, w.Code, w.Body.Len())
		}
	}
	if w := get(sh, "/v1/tiles/styles/classic/3/light", map[string]string{"If-None-Match": `"other"`}); w.Code != http.StatusOK {
		t.Errorf("non-matching If-None-Match: %d", w.Code)
	}
}

func TestReleaseStyles_NewestVersion(t *testing.T) {
	// versions compare as numbers: 10 is newer than 9 and 2
	h, _ := loaded(t,
		styleSpec{id: "classic", version: "9", variants: []string{"light"}},
		styleSpec{id: "classic", version: "10", def: true, variants: []string{"light"}},
		styleSpec{id: "classic", version: "2", variants: []string{"light"}})
	sh := releaseStyleHandler(t, "", h)

	if got := styleName(t, get(sh, "/v1/tiles/styles/classic/light", nil).Body.Bytes()); got != "classic-10-light" {
		t.Errorf("newest = %q", got)
	}
	// older versions stay reachable while they are in the manifest
	for _, v := range []string{"2", "9", "10"} {
		if got := styleName(t, get(sh, "/v1/tiles/styles/classic/"+v+"/light", nil).Body.Bytes()); got != "classic-"+v+"-light" {
			t.Errorf("version %s = %q", v, got)
		}
	}
	// one listing entry per style id, at its newest version
	var list []struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	}
	_ = json.Unmarshal(get(sh, "/v1/tiles/styles", nil).Body.Bytes(), &list)
	count := 0
	for _, e := range list {
		if e.ID == "classic" {
			count++
			if e.Version != "10" {
				t.Errorf("listed version %q", e.Version)
			}
		}
	}
	if count != 1 {
		t.Errorf("classic listed %d times", count)
	}
}

// A legacy style file keeps its name: clients of the legacy tileset must not get a planet style
// just because a release is loaded.
func TestReleaseStyles_LegacyFileWinsOverAlias(t *testing.T) {
	h, _ := loaded(t, classic)
	legacy := t.TempDir()
	writeStyleFileContent(t, legacy, "light.json", `{"version":8,"name":"legacy-light","tile":"{{.TilesBaseURL}}/base/{z}/{x}/{y}"}`)
	sh := releaseStyleHandler(t, legacy, h)

	if got := styleName(t, get(sh, "/v1/tiles/styles/light", nil).Body.Bytes()); got != "legacy-light" {
		t.Errorf("light = %q, want the legacy file", got)
	}
	if got := styleName(t, get(sh, "/v1/tiles/styles/dark", nil).Body.Bytes()); got != "classic-3-dark" {
		t.Errorf("dark = %q, want the release alias (there is no legacy dark.json)", got)
	}
	if got := styleName(t, get(sh, "/v1/tiles/styles/classic/3/light", nil).Body.Bytes()); got != "classic-3-light" {
		t.Errorf("explicit release URL = %q", got)
	}

	var list []map[string]any
	_ = json.Unmarshal(get(sh, "/v1/tiles/styles", nil).Body.Bytes(), &list)
	var names []string
	for _, e := range list {
		names = append(names, e["name"].(string))
	}
	// the legacy file first; the shadowed alias is not listed twice
	if got := strings.Join(names, ","); got != "light,dark,classic" {
		t.Errorf("names = %s", got)
	}
}

func TestReleaseStyles_SwitchServesTheNewRelease(t *testing.T) {
	root := t.TempDir()
	releaseDir(t, root, "r-1", styleSpec{id: "classic", version: "3", def: true, variants: []string{"light"}})
	releaseDir(t, root, "r-2", styleSpec{id: "classic", version: "4", def: true, variants: []string{"light"}})
	pointTo(t, root, "r-1")
	h := newHolder(t, root)
	ctx := context.Background()
	if _, err := h.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	sh := releaseStyleHandler(t, "", h)

	first := get(sh, "/v1/tiles/styles/classic/light", nil)
	if styleName(t, first.Body.Bytes()) != "classic-3-light" {
		t.Fatal("r-1 style")
	}
	pointTo(t, root, "r-2")
	if _, err := h.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	second := get(sh, "/v1/tiles/styles/classic/light", nil)
	if styleName(t, second.Body.Bytes()) != "classic-4-light" || second.Header().Get("ETag") == first.Header().Get("ETag") {
		t.Fatal("the render cache served the old release's style after a switch")
	}
	if w := get(sh, "/v1/tiles/styles/classic/3/light", nil); w.Code != http.StatusNotFound {
		t.Fatalf("a version that left the manifest: %d, want 404", w.Code)
	}
}

// A release with a style that does not parse, render or produce JSON is not switched to.
func TestReleaseStyles_BrokenStyleRejectsTheRelease(t *testing.T) {
	root := t.TempDir()
	releaseDir(t, root, "r-1", classic)
	pointTo(t, root, "r-1")
	h := newHolder(t, root)
	ctx := context.Background()
	if _, err := h.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	breakers := map[string]func(dir string){
		"template syntax": func(dir string) { writeFile(t, dir, "styles/classic/3/light.json", `{"a":"{{.TilesBaseURL"}`) },
		"unknown field":   func(dir string) { writeFile(t, dir, "styles/classic/3/light.json", `{"a":"{{.Nope}}"}`) },
		"not json":        func(dir string) { writeFile(t, dir, "styles/classic/3/light.json", `<html>`) },
		"missing file":    func(dir string) { _ = os.Remove(filepath.Join(dir, "styles/classic/3/dark.json")) },
	}
	for name, breakIt := range breakers {
		t.Run(name, func(t *testing.T) {
			_ = os.RemoveAll(filepath.Join(root, "releases", "r-bad"))
			breakIt(releaseDir(t, root, "r-bad", classic))
			pointTo(t, root, "r-bad")
			if _, err := h.Reload(ctx); err == nil {
				t.Fatal("a release with a broken style was accepted")
			}
			if st := h.Status(); st.Release != "r-1" || st.LastError == "" {
				t.Fatalf("status %+v", st)
			}
		})
	}
}
