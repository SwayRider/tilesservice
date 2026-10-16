package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/swayrider/tilesservice/internal/pmtiles/pmtilestest"
)

const releaseManifest = `{"schema":1,"release":%q,
 "tileset":{"name":"planet","build":%q,"date":"2026-10-04","schema_version":"4.15.2","file":"tiles.pmtiles"},
 "styles":[{"id":"classic","label":"Classic","version":3,"default":true,"variants":{"light":"styles/classic/3/light.json"}}]}`

// writeTilesRelease writes releases/<id>/ below root with a one-tile archive holding tag.
func writeTilesRelease(t *testing.T, root, id, build, tag string) {
	t.Helper()
	dir := filepath.Join(root, "releases", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := pmtilestest.Build([]pmtilestest.Tile{{Z: 0, X: 0, Y: 0, Data: pmtilestest.Gzip([]byte(tag))}},
		pmtilestest.Options{GzipDirectories: true})
	if err := os.WriteFile(filepath.Join(dir, "tiles.pmtiles"), archive, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(fmt.Sprintf(releaseManifest, id, build)), 0o644); err != nil {
		t.Fatal(err)
	}
	style := filepath.Join(dir, "styles", "classic", "3")
	if err := os.MkdirAll(style, 0o755); err != nil {
		t.Fatal(err)
	}
	// the style names its release and the tile URL it was rendered with
	doc := fmt.Sprintf(`{"version":8,"name":%q,"sources":{"p":{"type":"vector","tiles":["{{.TilesBaseURL}}/{{.Tileset}}/{z}/{x}/{y}"]}}}`, id)
	if err := os.WriteFile(filepath.Join(style, "light.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

func pointCurrent(t *testing.T, root, id string) {
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

type readyBody struct {
	Ready  bool `json:"ready"`
	Planet struct {
		Loaded       bool              `json:"loaded"`
		Release      string            `json:"release"`
		TilesetBuild string            `json:"tileset_build"`
		Styles       map[string]string `json:"styles"`
		LastError    string            `json:"last_error"`
		Mode         string            `json:"mode"`
	} `json:"planet"`
}

func getReady(t *testing.T, port int) (int, readyBody) {
	t.Helper()
	// public: no token
	resp, body := getPlanet(t, port, "/v1/tiles/ready", "", "")
	var rb readyBody
	if err := json.Unmarshal(body, &rb); err != nil {
		t.Fatalf("ready body %q: %v", body, err)
	}
	return resp.StatusCode, rb
}

func tileBody(t *testing.T, port int, bearer string) string {
	t.Helper()
	resp, body := getPlanet(t, port, "/v1/tiles/planet/0/0/0", bearer, "identity")
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return string(body)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The planet tileset comes from the release behind TILES_ROOT; moving the pointer switches
// releases without a restart, a broken release keeps serving the old one, and /ready tells.
func TestPlanetFromReleaseStore(t *testing.T) {
	root := t.TempDir()
	writeTilesRelease(t, root, "r-1", "20261001", "tile-one")
	writeTilesRelease(t, root, "r-2", "20261002", "tile-two")
	pointCurrent(t, root, "r-1")

	port, bearer := startPlanetApp(t, map[string]string{"TILES_ROOT": root, "RELOAD_POLL_SECONDS": "1"})

	if got := tileBody(t, port, bearer); got != "tile-one" {
		t.Fatalf("tile = %q, want tile-one", got)
	}
	status, rb := getReady(t, port)
	if status != http.StatusOK || !rb.Ready || rb.Planet.Release != "r-1" || rb.Planet.TilesetBuild != "20261001" || rb.Planet.Styles["classic"] != "3" {
		t.Fatalf("ready = %d %+v", status, rb)
	}

	// switch by moving the pointer (the poller picks it up)
	pointCurrent(t, root, "r-2")
	waitUntil(t, "tile-two", func() bool { return tileBody(t, port, bearer) == "tile-two" })
	if _, rb := getReady(t, port); rb.Planet.Release != "r-2" {
		t.Fatalf("ready after switch: %+v", rb)
	}

	// a broken release keeps r-2 serving and shows up in /ready
	writeTilesRelease(t, root, "r-bad", "20261009", "tile-bad")
	if err := os.WriteFile(filepath.Join(root, "releases", "r-bad", "tiles.pmtiles"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	pointCurrent(t, root, "r-bad")
	waitUntil(t, "the reload error in /ready", func() bool { _, rb := getReady(t, port); return rb.Planet.LastError != "" })
	if got := tileBody(t, port, bearer); got != "tile-two" {
		t.Fatalf("tile after a broken release = %q, want tile-two", got)
	}
	if status, rb := getReady(t, port); status != http.StatusOK || rb.Planet.Release != "r-2" {
		t.Fatalf("ready with a broken pointer = %d %+v", status, rb)
	}

	// rollback
	pointCurrent(t, root, "r-1")
	waitUntil(t, "rollback to tile-one", func() bool { return tileBody(t, port, bearer) == "tile-one" })

	// tiles stay behind auth, /ready does not
	if resp, _ := getPlanet(t, port, "/v1/tiles/planet/0/0/0", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("tile without token: %d, want 401", resp.StatusCode)
	}
}

// SIGHUP reloads at once, also when polling is off.
func TestPlanetReleaseReloadOnSIGHUP(t *testing.T) {
	root := t.TempDir()
	writeTilesRelease(t, root, "r-1", "b1", "tile-one")
	writeTilesRelease(t, root, "r-2", "b2", "tile-two")
	pointCurrent(t, root, "r-1")
	port, bearer := startPlanetApp(t, map[string]string{"TILES_ROOT": root, "RELOAD_POLL_SECONDS": "0"})

	pointCurrent(t, root, "r-2")
	time.Sleep(100 * time.Millisecond)
	if got := tileBody(t, port, bearer); got != "tile-one" {
		t.Fatalf("switched without a signal: %q", got)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "tile-two after SIGHUP", func() bool { return tileBody(t, port, bearer) == "tile-two" })
}

// Without a loadable release the service starts, planet answers 503, /ready says 503, and the
// first release that appears is picked up.
func TestPlanetReleaseAppearsLater(t *testing.T) {
	root := t.TempDir() // no current pointer yet
	port, bearer := startPlanetApp(t, map[string]string{"TILES_ROOT": root, "RELOAD_POLL_SECONDS": "1"})

	if resp, _ := getPlanet(t, port, "/v1/tiles/planet/0/0/0", bearer, ""); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("planet without a release: %d, want 503", resp.StatusCode)
	}
	if status, rb := getReady(t, port); status != http.StatusServiceUnavailable || rb.Ready || rb.Planet.LastError == "" {
		t.Fatalf("ready without a release = %d %+v", status, rb)
	}

	writeTilesRelease(t, root, "r-1", "b1", "tile-one")
	pointCurrent(t, root, "r-1")
	waitUntil(t, "the first release", func() bool { return tileBody(t, port, bearer) == "tile-one" })
	if status, rb := getReady(t, port); status != http.StatusOK || !rb.Ready {
		t.Fatalf("ready after the release appeared = %d %+v", status, rb)
	}
}

func TestReadyModes(t *testing.T) {
	// nothing configured: the legacy tilesets still work, so the service is ready
	port, _ := startPlanetApp(t, nil)
	if status, rb := getReady(t, port); status != http.StatusOK || !rb.Ready || rb.Planet.Mode != "disabled" {
		t.Fatalf("ready with nothing configured = %d %+v", status, rb)
	}
}

func TestReadyWithSingleArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "planet.pmtiles")
	if err := os.WriteFile(path, planetFile(), 0o600); err != nil {
		t.Fatal(err)
	}
	port, _ := startPlanetApp(t, map[string]string{"PMTILES_URL": "file://" + path})
	if status, rb := getReady(t, port); status != http.StatusOK || !rb.Ready || rb.Planet.Mode != "archive" {
		t.Fatalf("ready with PMTILES_URL = %d %+v", status, rb)
	}
}

// PMTILES_URL pointing at a file that cannot be opened leaves the service unready.
func TestReadyWithBrokenArchive(t *testing.T) {
	port, _ := startPlanetApp(t, map[string]string{"PMTILES_URL": "file://" + filepath.Join(t.TempDir(), "missing.pmtiles")})
	if status, rb := getReady(t, port); status != http.StatusServiceUnavailable || rb.Ready {
		t.Fatalf("ready with a broken archive = %d %+v", status, rb)
	}
}

// TILES_ROOT wins when PMTILES_URL is set as well.
func TestTilesRootTakesPrecedenceOverPMTilesURL(t *testing.T) {
	root := t.TempDir()
	writeTilesRelease(t, root, "r-1", "b1", "from-release")
	pointCurrent(t, root, "r-1")
	path := filepath.Join(t.TempDir(), "planet.pmtiles")
	if err := os.WriteFile(path, planetFile(), 0o600); err != nil {
		t.Fatal(err)
	}
	port, bearer := startPlanetApp(t, map[string]string{"TILES_ROOT": root, "PMTILES_URL": "file://" + path})
	if got := tileBody(t, port, bearer); got != "from-release" {
		t.Fatalf("tile = %q", got)
	}
}

// The style routes sit next to the tile wildcard: a literal "styles" must win over {tileset}, for
// every path shape, and all of them stay behind the service token.
func TestStyleRoutesFromRelease(t *testing.T) {
	root := t.TempDir()
	writeTilesRelease(t, root, "r-1", "b1", "tile-one")
	pointCurrent(t, root, "r-1")
	port, bearer := startPlanetApp(t, map[string]string{
		"TILES_ROOT": root, "SERVICE_HOST": "https://api.example.com", "SERVICE_PREFIX": "/v1/tiles",
	})

	for _, path := range []string{
		"/v1/tiles/styles/light",           // alias of the default style
		"/v1/tiles/styles/classic",         // style id
		"/v1/tiles/styles/classic/light",   // newest version
		"/v1/tiles/styles/classic/3/light", // fixed version, 4 segments like a tile URL
	} {
		resp, body := getPlanet(t, port, path, bearer, "identity")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: %d %s", path, resp.StatusCode, body)
			continue
		}
		var style struct {
			Name    string `json:"name"`
			Sources struct {
				P struct {
					Tiles []string `json:"tiles"`
				} `json:"p"`
			} `json:"sources"`
		}
		if err := json.Unmarshal(body, &style); err != nil || style.Name != "r-1" ||
			len(style.Sources.P.Tiles) != 1 || style.Sources.P.Tiles[0] != "https://api.example.com/v1/tiles/planet/{z}/{x}/{y}" {
			t.Errorf("%s: unexpected style %q (%v)", path, body, err)
		}
		if resp, _ := getPlanet(t, port, path, "", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a token: %d, want 401", path, resp.StatusCode)
		}
	}

	// the listing
	resp, body := getPlanet(t, port, "/v1/tiles/styles", bearer, "identity")
	var list []struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(body, &list); err != nil || resp.StatusCode != http.StatusOK || len(list) == 0 {
		t.Fatalf("list: %d %q (%v)", resp.StatusCode, body, err)
	}

	// tiles are still tiles
	if got := tileBody(t, port, bearer); got != "tile-one" {
		t.Errorf("tile = %q", got)
	}
	// and a tileset named like a style segment does not shadow them
	if resp, _ := getPlanet(t, port, "/v1/tiles/planet/0/0/0", bearer, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("planet tile: %d", resp.StatusCode)
	}
}
