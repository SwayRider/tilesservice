package tileindex

import (
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/mvt"
	"github.com/paulmach/orb/geojson"
)

func TestZoomToLayer(t *testing.T) {
	tests := []struct {
		zoom     uint32
		expected Layer
	}{
		{0, LayerL0},
		{1, LayerL0},
		{6, LayerL0},
		{7, LayerL1},
		{10, LayerL1},
		{11, LayerL2},
		{13, LayerL2},
		{14, LayerL2},
		{16, LayerL2},
		{17, LayerL2}, // Beyond max, still L2
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			got := zoomToLayer(tt.zoom)
			if got != tt.expected {
				t.Errorf("zoomToLayer(%d) = %d, want %d", tt.zoom, got, tt.expected)
			}
		})
	}
}

func TestTileToLatLon(t *testing.T) {
	// tileToLatLon returns the SOUTHWEST corner of the tile
	// In XYZ coordinates: southwest = (x, y+1) since y increases southward
	tests := []struct {
		name       string
		z, x, y    uint32
		expectLat  float64
		expectLon  float64
		tolerance  float64
	}{
		{
			name:      "z0 world tile southwest corner",
			z:         0,
			x:         0,
			y:         0,
			expectLat: -85.05, // South edge of mercator projection (y+1=1)
			expectLon: -180,   // West edge of world
			tolerance: 1,
		},
		{
			name:      "z1 northwest quadrant southwest corner",
			z:         1,
			x:         0,
			y:         0,
			expectLat: 0,    // Equator (y+1=1, south edge of north tiles)
			expectLon: -180, // West edge
			tolerance: 1,
		},
		{
			name:      "z1 southeast quadrant southwest corner",
			z:         1,
			x:         1,
			y:         1,
			expectLat: -85.05, // South pole (y+1=2, bottom of world)
			expectLon: 0,      // Prime meridian
			tolerance: 1,
		},
		{
			name:      "z2 tile over Europe southwest corner",
			z:         2,
			x:         2,
			y:         1,
			expectLat: 0,  // Equator (y+1=2)
			expectLon: 0,  // Prime meridian
			tolerance: 1,
		},
		{
			name:      "z8 Amsterdam tile southwest corner",
			z:         8,
			x:         131,
			y:         84,
			expectLat: 51.6, // South edge of tile (y+1=85)
			expectLon: 4.2,  // West edge of tile
			tolerance: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lat, lon := tileToLatLon(tt.z, tt.x, tt.y)

			if math.Abs(lat-tt.expectLat) > tt.tolerance {
				t.Errorf("tileToLatLon lat = %f, want ~%f (±%f)", lat, tt.expectLat, tt.tolerance)
			}
			if math.Abs(lon-tt.expectLon) > tt.tolerance {
				t.Errorf("tileToLatLon lon = %f, want ~%f (±%f)", lon, tt.expectLon, tt.tolerance)
			}
		})
	}
}

func TestSnapToGrid(t *testing.T) {
	tests := []struct {
		lat, lon           float64
		expectLat, expectLon int
	}{
		{0, 0, 0, 0},
		{5, 5, 0, 0},
		{9.9, 9.9, 0, 0},
		{10, 10, 10, 10},
		{15, 15, 10, 10},
		{-5, -5, -10, -10},
		{-15, -15, -20, -20},
		{50.5, -8.3, 50, -10},
		{-33.9, 18.4, -40, 10},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			gotLat, gotLon := snapToGrid(tt.lat, tt.lon)
			if gotLat != tt.expectLat || gotLon != tt.expectLon {
				t.Errorf("snapToGrid(%f, %f) = (%d, %d), want (%d, %d)",
					tt.lat, tt.lon, gotLat, gotLon, tt.expectLat, tt.expectLon)
			}
		})
	}
}

func TestFormatGridFilename(t *testing.T) {
	tests := []struct {
		lat, lon int
		expected string
	}{
		{0, 0, "N00_E000.mbtiles"},
		{10, 10, "N10_E010.mbtiles"},
		{50, -10, "N50_W010.mbtiles"},
		{-30, 20, "S30_E020.mbtiles"},
		{-40, -120, "S40_W120.mbtiles"},
		{20, 0, "N20_E000.mbtiles"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			got := formatGridFilename(tt.lat, tt.lon)
			if got != tt.expected {
				t.Errorf("formatGridFilename(%d, %d) = %q, want %q",
					tt.lat, tt.lon, got, tt.expected)
			}
		})
	}
}

// createTestTileDir creates a temporary directory structure with test mbtiles files.
// Test data covers Western Europe: Netherlands, Belgium, Germany, Luxembourg, France, Spain, Portugal.
func createTestTileDir(t *testing.T) (string, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "tileindex-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	// Create L0.mbtiles with world tile and a z=5 tile over Western Europe
	// z=5 tile 15/10 covers roughly Western Europe
	// XYZ y=10 -> TMS y = (2^5 - 1) - 10 = 21
	createTestMBTiles(t, filepath.Join(tmpDir, "L0.mbtiles"), map[tileKey][]byte{
		{0, 0, 0}:   []byte("L0-world"),
		{5, 15, 21}: []byte("L0-z5-europe"), // TMS coords for Western Europe
	})

	// Create L1 directory with N50_E000.mbtiles (Netherlands, Belgium, Luxembourg, western Germany)
	// Amsterdam is roughly at 52.37°N, 4.90°E
	// At z=8: x=131, y=84 (XYZ) -> TMS y = 255 - 84 = 171
	l1Dir := filepath.Join(tmpDir, "L1")
	if err := os.MkdirAll(l1Dir, 0755); err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("failed to create L1 dir: %v", err)
	}
	createTestMBTiles(t, filepath.Join(l1Dir, "N50_E000.mbtiles"), map[tileKey][]byte{
		{8, 131, 171}: []byte("L1-amsterdam"), // TMS coords
	})

	// Create L2 directory with N50_E000.mbtiles
	// Brussels is roughly at 50.85°N, 4.35°E
	// At z=12: x=2100, y=1362 (XYZ) -> TMS y = 4095 - 1362 = 2733
	l2Dir := filepath.Join(tmpDir, "L2")
	if err := os.MkdirAll(l2Dir, 0755); err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("failed to create L2 dir: %v", err)
	}
	createTestMBTiles(t, filepath.Join(l2Dir, "N50_E000.mbtiles"), map[tileKey][]byte{
		{12, 2100, 2733}: []byte("L2-brussels"), // TMS coords
	})

	// Add z=14 tile to L2 directory (Portugal)
	// Lisbon is roughly at 38.72°N, -9.14°W
	// At z=14: x=7895, y=6178 (XYZ) -> TMS y = 16383 - 6178 = 10205
	// The tile's top-left corner is ~40.5°N, -6.4°W which snaps to N40_W010
	createTestMBTiles(t, filepath.Join(l2Dir, "N40_W010.mbtiles"), map[tileKey][]byte{
		{14, 7895, 10205}: []byte("L2-lisbon"), // TMS coords
	})

	return tmpDir, func() {
		os.RemoveAll(tmpDir)
	}
}

type tileKey struct {
	z, x, y uint32
}

func createTestMBTiles(t *testing.T, path string, tiles map[tileKey][]byte) {
	t.Helper()

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("failed to create db: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE tiles (
			zoom_level INTEGER,
			tile_column INTEGER,
			tile_row INTEGER,
			tile_data BLOB,
			PRIMARY KEY (zoom_level, tile_column, tile_row)
		)
	`)
	if err != nil {
		t.Fatalf("failed to create table: %v", err)
	}

	for key, data := range tiles {
		_, err := db.Exec(
			"INSERT INTO tiles (zoom_level, tile_column, tile_row, tile_data) VALUES (?, ?, ?, ?)",
			key.z, key.x, key.y, data,
		)
		if err != nil {
			t.Fatalf("failed to insert tile: %v", err)
		}
	}
}

func TestTileIndex_GetTile(t *testing.T) {
	tmpDir, cleanup := createTestTileDir(t)
	defer cleanup()

	idx := New(tmpDir)
	defer idx.Close()

	t.Run("retrieves L0 world tile at z=0", func(t *testing.T) {
		data, err := idx.GetTile(0, 0, 0)
		if err != nil {
			t.Fatalf("GetTile error = %v", err)
		}
		if string(data) != "L0-world" {
			t.Errorf("GetTile = %q, want %q", data, "L0-world")
		}
	})

	t.Run("retrieves L0 tile at z=5 over Western Europe", func(t *testing.T) {
		// z=5 tile 15/10 (XYZ) covers Western Europe
		// XYZ y=10 -> TMS y = (2^5 - 1) - 10 = 21
		data, err := idx.GetTile(5, 15, 10)
		if err != nil {
			t.Fatalf("GetTile error = %v", err)
		}
		if string(data) != "L0-z5-europe" {
			t.Errorf("GetTile = %q, want %q", data, "L0-z5-europe")
		}
	})

	t.Run("retrieves L1 tile at z=8 for Amsterdam area", func(t *testing.T) {
		// z=8 is L1, Amsterdam area: x=131, y=84 (XYZ)
		// XYZ y=84 -> TMS y = 255 - 84 = 171
		// Maps to N50_E000.mbtiles
		data, err := idx.GetTile(8, 131, 84)
		if err != nil {
			t.Fatalf("GetTile error = %v", err)
		}
		if string(data) != "L1-amsterdam" {
			t.Errorf("GetTile = %q, want %q", data, "L1-amsterdam")
		}
	})

	t.Run("retrieves L2 tile at z=12 for Brussels area", func(t *testing.T) {
		// z=12 is L2, Brussels area: x=2100, y=1362 (XYZ)
		// XYZ y=1362 -> TMS y = 4095 - 1362 = 2733
		// Maps to N50_E000.mbtiles
		data, err := idx.GetTile(12, 2100, 1362)
		if err != nil {
			t.Fatalf("GetTile error = %v", err)
		}
		if string(data) != "L2-brussels" {
			t.Errorf("GetTile = %q, want %q", data, "L2-brussels")
		}
	})

	t.Run("retrieves L2 tile at z=14 for Lisbon area", func(t *testing.T) {
		// z=14 is L2, Lisbon area: x=7895, y=6178 (XYZ)
		// XYZ y=6178 -> TMS y = 16383 - 6178 = 10205
		// Maps to N40_W010.mbtiles
		data, err := idx.GetTile(14, 7895, 6178)
		if err != nil {
			t.Fatalf("GetTile error = %v", err)
		}
		if string(data) != "L2-lisbon" {
			t.Errorf("GetTile = %q, want %q", data, "L2-lisbon")
		}
	})

	t.Run("returns error for tile outside covered area", func(t *testing.T) {
		// Request tile in Asia (outside our Western Europe test data)
		// z=8, somewhere in China
		_, err := idx.GetTile(8, 210, 100)
		if err == nil {
			t.Error("expected error for tile outside covered area")
		}
	})
}

func TestTileIndex_Close(t *testing.T) {
	tmpDir, cleanup := createTestTileDir(t)
	defer cleanup()

	idx := New(tmpDir)

	// Access a tile to populate the cache
	_, _ = idx.GetTile(0, 0, 0)

	// Close should not error
	if err := idx.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}
}

func TestTileCornerToLatLon(t *testing.T) {
	tests := []struct {
		name      string
		z, x, y   uint32
		expectLat float64
		expectLon float64
		tolerance float64
	}{
		{
			name:      "z0 origin (northwest corner of world)",
			z:         0,
			x:         0,
			y:         0,
			expectLat: 85.05, // North edge of mercator
			expectLon: -180,
			tolerance: 1,
		},
		{
			name:      "z8 tile 135/86 northwest corner",
			z:         8,
			x:         135,
			y:         86,
			expectLat: 50.74,
			expectLon: 9.84,
			tolerance: 0.1,
		},
		{
			name:      "z8 tile 135/86 southeast corner (136/87)",
			z:         8,
			x:         136,
			y:         87,
			expectLat: 49.84,
			expectLon: 11.25,
			tolerance: 0.1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lat, lon := tileCornerToLatLon(tt.z, tt.x, tt.y)

			if math.Abs(lat-tt.expectLat) > tt.tolerance {
				t.Errorf("tileCornerToLatLon lat = %f, want ~%f (±%f)", lat, tt.expectLat, tt.tolerance)
			}
			if math.Abs(lon-tt.expectLon) > tt.tolerance {
				t.Errorf("tileCornerToLatLon lon = %f, want ~%f (±%f)", lon, tt.expectLon, tt.tolerance)
			}
		})
	}
}

func TestGetOverlappingFilePaths(t *testing.T) {
	idx := New("/test/path")

	t.Run("tile entirely within one grid cell", func(t *testing.T) {
		// Tile 8/131/84 is entirely within N50_E000 (Amsterdam area)
		paths := idx.getOverlappingFilePaths(8, 131, 84)
		if len(paths) != 1 {
			t.Errorf("expected 1 file, got %d: %v", len(paths), paths)
		}
		if len(paths) > 0 && filepath.Base(paths[0]) != "N50_E000.mbtiles" {
			t.Errorf("expected N50_E000.mbtiles, got %s", filepath.Base(paths[0]))
		}
	})

	t.Run("tile spanning 4 grid cells at 50N/10E boundary", func(t *testing.T) {
		// Tile 8/135/86 spans 4 grid cells at the 50°N/10°E intersection
		paths := idx.getOverlappingFilePaths(8, 135, 86)
		if len(paths) != 4 {
			t.Errorf("expected 4 files, got %d: %v", len(paths), paths)
		}

		// Check all expected files are present
		expected := map[string]bool{
			"N40_E000.mbtiles": false,
			"N40_E010.mbtiles": false,
			"N50_E000.mbtiles": false,
			"N50_E010.mbtiles": false,
		}
		for _, p := range paths {
			base := filepath.Base(p)
			if _, ok := expected[base]; ok {
				expected[base] = true
			}
		}
		for name, found := range expected {
			if !found {
				t.Errorf("expected file %s not found in paths", name)
			}
		}
	})

	t.Run("tile spanning 2 grid cells (latitude boundary only)", func(t *testing.T) {
		// Find a tile that crosses 50°N but stays within E000 longitude range
		// Tile 8/132/86 should cross 50°N but stay within 0-10°E
		paths := idx.getOverlappingFilePaths(8, 132, 86)
		if len(paths) != 2 {
			t.Errorf("expected 2 files, got %d: %v", len(paths), paths)
		}
	})
}

// createTestMVTTile creates a test MVT tile with the specified layer name and feature count.
func createTestMVTTile(layerName string, featureCount int) []byte {
	layer := &mvt.Layer{
		Name:     layerName,
		Version:  2,
		Extent:   4096,
		Features: make([]*geojson.Feature, featureCount),
	}
	for i := 0; i < featureCount; i++ {
		layer.Features[i] = geojson.NewFeature(orb.Point{float64(i * 100), float64(i * 100)})
		layer.Features[i].Properties = map[string]interface{}{
			"id":    i,
			"layer": layerName,
		}
	}
	data, err := mvt.Marshal(mvt.Layers{layer})
	if err != nil {
		panic(err)
	}
	return data
}

// createGridBoundaryTestDir creates a test directory with 4 mbtiles files
// at the 50°N/10°E grid boundary intersection.
func createGridBoundaryTestDir(t *testing.T) (string, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "tileindex-boundary-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	// Create L0.mbtiles (required for the index)
	createTestMBTiles(t, filepath.Join(tmpDir, "L0.mbtiles"), map[tileKey][]byte{
		{0, 0, 0}: []byte("L0-world"),
	})

	// Create L1 directory with 4 files at the 50°N/10°E intersection
	l1Dir := filepath.Join(tmpDir, "L1")
	if err := os.MkdirAll(l1Dir, 0755); err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("failed to create L1 dir: %v", err)
	}

	// Tile 8/135/86 spans all 4 grid cells
	// XYZ y=86 -> TMS y = 255 - 86 = 169
	tmsY := uint32(169)

	// Create each file with a distinct layer and feature count
	// SW quadrant: N40_E000
	createTestMBTiles(t, filepath.Join(l1Dir, "N40_E000.mbtiles"), map[tileKey][]byte{
		{8, 135, tmsY}: createTestMVTTile("sw_layer", 1),
	})

	// SE quadrant: N40_E010
	createTestMBTiles(t, filepath.Join(l1Dir, "N40_E010.mbtiles"), map[tileKey][]byte{
		{8, 135, tmsY}: createTestMVTTile("se_layer", 2),
	})

	// NW quadrant: N50_E000
	createTestMBTiles(t, filepath.Join(l1Dir, "N50_E000.mbtiles"), map[tileKey][]byte{
		{8, 135, tmsY}: createTestMVTTile("nw_layer", 3),
	})

	// NE quadrant: N50_E010
	createTestMBTiles(t, filepath.Join(l1Dir, "N50_E010.mbtiles"), map[tileKey][]byte{
		{8, 135, tmsY}: createTestMVTTile("ne_layer", 4),
	})

	return tmpDir, func() {
		os.RemoveAll(tmpDir)
	}
}

func TestTileIndex_GetTile_GridBoundary(t *testing.T) {
	tmpDir, cleanup := createGridBoundaryTestDir(t)
	defer cleanup()

	idx := New(tmpDir)
	defer idx.Close()

	t.Run("merges tiles from 4 grid cells", func(t *testing.T) {
		// Request tile 8/135/86 which spans all 4 grid cells
		data, err := idx.GetTile(8, 135, 86)
		if err != nil {
			t.Fatalf("GetTile error = %v", err)
		}

		// Parse the merged tile
		layers, err := mvt.Unmarshal(data)
		if err != nil {
			t.Fatalf("failed to unmarshal merged tile: %v", err)
		}

		// Should have 4 layers from 4 different files
		if len(layers) != 4 {
			t.Errorf("merged tile has %d layers, want 4", len(layers))
		}

		// Build layer map
		layerMap := make(map[string]int)
		for _, layer := range layers {
			layerMap[layer.Name] = len(layer.Features)
		}

		// Verify each layer has the expected feature count
		expected := map[string]int{
			"sw_layer": 1,
			"se_layer": 2,
			"nw_layer": 3,
			"ne_layer": 4,
		}
		for name, count := range expected {
			if layerMap[name] != count {
				t.Errorf("layer %s has %d features, want %d", name, layerMap[name], count)
			}
		}

		// Total features should be 1+2+3+4 = 10
		totalFeatures := 0
		for _, layer := range layers {
			totalFeatures += len(layer.Features)
		}
		if totalFeatures != 10 {
			t.Errorf("total features = %d, want 10", totalFeatures)
		}
	})
}
