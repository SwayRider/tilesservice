package server_test

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/swayrider/tilesservice/internal/pmtiles/pmtilestest"
	"github.com/swayrider/tilesservice/internal/server"
)

func TestReadyHandler(t *testing.T) {
	for _, tc := range []struct {
		ready bool
		code  int
	}{{true, http.StatusOK}, {false, http.StatusServiceUnavailable}} {
		w := get(server.NewReadyHandler(func() (bool, any) { return tc.ready, map[string]string{"release": "r-1"} }), "/v1/tiles/ready", nil)
		if w.Code != tc.code {
			t.Errorf("ready=%v: status %d, want %d", tc.ready, w.Code, tc.code)
		}
		if w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("headers %v", w.Header())
		}
		var body struct {
			Ready  bool              `json:"ready"`
			Planet map[string]string `json:"planet"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Ready != tc.ready || body.Planet["release"] != "r-1" {
			t.Errorf("body %q: %v", w.Body.String(), err)
		}
	}
}

type countingSource struct {
	archive  server.TileArchive
	acquired atomic.Int32
	released atomic.Int32
}

func (c *countingSource) Archive() (server.TileArchive, func()) {
	c.acquired.Add(1)
	return c.archive, func() { c.released.Add(1) }
}

// The handler takes the archive from the source for every request and releases it afterwards,
// also on error paths; without an archive it answers 503.
func TestPlanet_ArchiveSource(t *testing.T) {
	src := &countingSource{archive: planetArchive(t, gzipTiles(t), pmtilestest.Options{})}
	h := planetHandler(t, nil).WithArchiveSource(src)

	if w := get(h, "/v1/tiles/planet/2/1/1", nil); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if w := get(h, "/v1/tiles/planet/2/9/9", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("out of range: status %d", w.Code)
	}
	if w := get(h, "/v1/tiles/planet/4/0/0", nil); w.Code != http.StatusNoContent {
		t.Fatalf("missing tile: status %d", w.Code)
	}
	if src.acquired.Load() != 3 || src.released.Load() != 3 {
		t.Fatalf("acquired %d, released %d; want 3 and 3", src.acquired.Load(), src.released.Load())
	}

	src.archive = nil
	if w := get(h, "/v1/tiles/planet/2/1/1", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no archive: status %d, want 503", w.Code)
	}
	if src.acquired.Load() != src.released.Load() {
		t.Fatal("an acquired archive was not released on the 503 path")
	}
}
