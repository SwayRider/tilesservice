package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	jwt5 "github.com/golang-jwt/jwt/v5"
	"github.com/swayrider/swlib/jwt"
	"github.com/swayrider/swlib/jwtkeys"
	"github.com/swayrider/tilesservice/internal/pmtiles/pmtilestest"
)

type staticKeys struct{ keys []string }

func (s staticKeys) PublicKeys() ([]string, error) { return s.keys, nil }

var planetTilePlain = []byte("planet-mvt-bytes")

func planetFile() []byte {
	return pmtilestest.Build([]pmtilestest.Tile{
		{Z: 0, X: 0, Y: 0, Data: pmtilestest.Gzip(planetTilePlain)},
		{Z: 1, X: 1, Y: 0, Data: pmtilestest.Gzip([]byte("tile-1-1-0"))},
	}, pmtilestest.Options{GzipDirectories: true})
}

// startPlanetApp starts the real server (config parse, initializers, HTTP) with a working JWT key
// cache and returns the port and a service token with the tiles:serve scope.
func startPlanetApp(t *testing.T, env map[string]string) (port int, bearer string) {
	t.Helper()
	port = freePort(t)
	t.Setenv("HTTP_PORT", strconv.Itoa(port))
	t.Setenv("LOG_LEVEL", "error")
	for k, v := range env {
		t.Setenv(k, v)
	}
	privPEM, pubPEM := testKeyPair(t)
	bearer = "Bearer " + signToken(t, privPEM, jwt.NewSwayRiderServiceClaims(jwt5.ClaimStrings{"tiles:serve"}))

	// Config.Parse registers its flags on the global flag set, which allows one parse per process:
	// start from a fresh set so every test can parse its own environment.
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	application := newApp()
	if err := application.Config().Parse(); err != nil {
		t.Fatalf("config parse: %v", err)
	}
	cache := jwtkeys.New(application.Logger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cache.Start(ctx, staticKeys{keys: []string{pubPEM}})
	application.SetAppData("JWTKeyCache", cache)

	if err := initializeTileIndex(application); err != nil {
		t.Fatal(err)
	}
	if err := initializePMTiles(application); err != nil {
		t.Fatal(err)
	}
	if err := startHTTPServer(application); err != nil {
		t.Fatalf("startHTTPServer: %v", err)
	}
	t.Cleanup(func() { stopHTTPServer(application) })
	return port, bearer
}

func getPlanet(t *testing.T, port int, path, bearer, acceptEncoding string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	req.Header.Set("Authorization", bearer)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	// Do not let the client decode gzip itself: the test inspects the wire format.
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	var resp *http.Response
	var err error
	for i := 0; i < 50; i++ {
		if resp, err = client.Do(req); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

// A planet-only setup (no TILES_PATH) must start and stop without the legacy tile index, and serve
// the planet tileset from a local file through the real auth middleware.
func TestPlanetFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "planet.pmtiles")
	if err := os.WriteFile(path, planetFile(), 0o600); err != nil {
		t.Fatal(err)
	}
	port, bearer := startPlanetApp(t, map[string]string{"PMTILES_URL": "file://" + path})

	resp, body := getPlanet(t, port, "/v1/tiles/planet/0/0/0", bearer, "gzip")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "gzip" || !bytes.Equal(body, pmtilestest.Gzip(planetTilePlain)) {
		t.Errorf("gzip client: %d enc=%q body=%d bytes", resp.StatusCode, resp.Header.Get("Content-Encoding"), len(body))
	}
	resp, body = getPlanet(t, port, "/v1/tiles/planet/0/0/0", bearer, "identity")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" || !bytes.Equal(body, planetTilePlain) {
		t.Errorf("identity client: %d enc=%q body=%q", resp.StatusCode, resp.Header.Get("Content-Encoding"), body)
	}
	if resp, _ := getPlanet(t, port, "/v1/tiles/planet/1/0/0", bearer, ""); resp.StatusCode != http.StatusNoContent {
		t.Errorf("missing tile: %d, want 204", resp.StatusCode)
	}
	if resp, _ := getPlanet(t, port, "/v1/tiles/planet/0/0/0", "Bearer nonsense", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad token: %d, want 401", resp.StatusCode)
	}
	// No legacy index configured: legacy tilesets answer 503, they do not crash the service.
	if resp, _ := getPlanet(t, port, "/v1/tiles/base/0/0/0", bearer, ""); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("legacy tileset without TILES_PATH: %d, want 503", resp.StatusCode)
	}
}

// The same archive read from an S3-compatible store through ranged GETs.
func TestPlanetFromS3(t *testing.T) {
	data := planetFile()
	var ranges int
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/swayrider-tiles/releases/r-test-1/tiles.pmtiles" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			return
		}
		var s, e int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &s, &e); err != nil || r.Header.Get("Authorization") == "" {
			http.Error(w, "need a signed range request", http.StatusBadRequest)
			return
		}
		ranges++
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, e, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[s : e+1])
	}))
	defer store.Close()
	port, bearer := startPlanetApp(t, map[string]string{
		"PMTILES_URL":          "s3://swayrider-tiles/releases/r-test-1/tiles.pmtiles",
		"S3_ENDPOINT":          store.URL,
		"S3_ACCESS_KEY_ID":     "GKtest",
		"S3_SECRET_ACCESS_KEY": "secret",
	})
	resp, body := getPlanet(t, port, "/v1/tiles/planet/1/1/0", bearer, "gzip")
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, pmtilestest.Gzip([]byte("tile-1-1-0"))) {
		t.Errorf("tile via S3: %d %d bytes", resp.StatusCode, len(body))
	}
	if ranges == 0 {
		t.Error("no ranged GET reached the store")
	}
}

// A broken PMTILES_URL must not take the service down: the planet tileset answers 503.
func TestPlanetUnavailableDoesNotStopService(t *testing.T) {
	port, bearer := startPlanetApp(t, map[string]string{"PMTILES_URL": "file:///does/not/exist.pmtiles"})
	if resp, _ := getPlanet(t, port, "/v1/tiles/planet/0/0/0", bearer, ""); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("planet with a failing source: %d, want 503", resp.StatusCode)
	}
	if resp, _ := getPlanet(t, port, "/v1/tiles/ping", "", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("ping: %d", resp.StatusCode)
	}
}

func TestInitializePMTilesWithoutConfig(t *testing.T) {
	application := newApp()
	if err := initializePMTiles(application); err != nil {
		t.Fatal(err)
	}
	if application.AppData("PMTiles") != nil {
		t.Error("no archive may be registered without PMTILES_URL")
	}
}
