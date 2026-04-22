package mbtiles

import (
	"bytes"
	"compress/gzip"
	"testing"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/mvt"
	"github.com/paulmach/orb/geojson"
)

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

// createMultiLayerMVTTile creates a test MVT tile with multiple layers.
func createMultiLayerMVTTile(layers map[string]int) []byte {
	var mvtLayers mvt.Layers
	for name, count := range layers {
		layer := &mvt.Layer{
			Name:     name,
			Version:  2,
			Extent:   4096,
			Features: make([]*geojson.Feature, count),
		}
		for i := 0; i < count; i++ {
			layer.Features[i] = geojson.NewFeature(orb.Point{float64(i * 100), float64(i * 100)})
			layer.Features[i].Properties = map[string]interface{}{
				"id":    i,
				"layer": name,
			}
		}
		mvtLayers = append(mvtLayers, layer)
	}
	data, err := mvt.Marshal(mvtLayers)
	if err != nil {
		panic(err)
	}
	return data
}

// gzipData compresses the input data using gzip.
func gzipData(data []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

// isGzipped checks if data starts with gzip magic bytes.
func isGzipped(data []byte) bool {
	return len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b
}

func TestMergeTiles_EmptyInput(t *testing.T) {
	_, err := MergeTiles(nil)
	if err != ErrTileNotFound {
		t.Errorf("MergeTiles(nil) error = %v, want ErrTileNotFound", err)
	}

	_, err = MergeTiles([][]byte{})
	if err != ErrTileNotFound {
		t.Errorf("MergeTiles([]) error = %v, want ErrTileNotFound", err)
	}
}

func TestMergeTiles_SingleTile(t *testing.T) {
	original := createTestMVTTile("roads", 5)

	merged, err := MergeTiles([][]byte{original})
	if err != nil {
		t.Fatalf("MergeTiles() error = %v", err)
	}

	// Single tile should be returned unchanged
	if !bytes.Equal(merged, original) {
		t.Error("MergeTiles() single tile should be returned unchanged")
	}
}

func TestMergeTiles_DifferentLayers(t *testing.T) {
	tile1 := createTestMVTTile("roads", 3)
	tile2 := createTestMVTTile("water", 2)

	merged, err := MergeTiles([][]byte{tile1, tile2})
	if err != nil {
		t.Fatalf("MergeTiles() error = %v", err)
	}

	// Parse merged tile to verify structure
	layers, err := mvt.Unmarshal(merged)
	if err != nil {
		t.Fatalf("failed to unmarshal merged tile: %v", err)
	}

	if len(layers) != 2 {
		t.Errorf("merged tile has %d layers, want 2", len(layers))
	}

	// Check that both layers exist with correct feature counts
	layerMap := make(map[string]int)
	for _, layer := range layers {
		layerMap[layer.Name] = len(layer.Features)
	}

	if layerMap["roads"] != 3 {
		t.Errorf("roads layer has %d features, want 3", layerMap["roads"])
	}
	if layerMap["water"] != 2 {
		t.Errorf("water layer has %d features, want 2", layerMap["water"])
	}
}

func TestMergeTiles_SameLayerName(t *testing.T) {
	tile1 := createTestMVTTile("roads", 3)
	tile2 := createTestMVTTile("roads", 4)

	merged, err := MergeTiles([][]byte{tile1, tile2})
	if err != nil {
		t.Fatalf("MergeTiles() error = %v", err)
	}

	// Parse merged tile
	layers, err := mvt.Unmarshal(merged)
	if err != nil {
		t.Fatalf("failed to unmarshal merged tile: %v", err)
	}

	if len(layers) != 1 {
		t.Errorf("merged tile has %d layers, want 1", len(layers))
	}

	// Features should be concatenated
	if len(layers[0].Features) != 7 {
		t.Errorf("roads layer has %d features, want 7 (3+4)", len(layers[0].Features))
	}
}

func TestMergeTiles_GzipCompressed(t *testing.T) {
	tile1 := gzipData(createTestMVTTile("roads", 3))
	tile2 := gzipData(createTestMVTTile("water", 2))

	if !isGzipped(tile1) || !isGzipped(tile2) {
		t.Fatal("test tiles should be gzip compressed")
	}

	merged, err := MergeTiles([][]byte{tile1, tile2})
	if err != nil {
		t.Fatalf("MergeTiles() error = %v", err)
	}

	// Output should be gzip compressed
	if !isGzipped(merged) {
		t.Error("merged tile should be gzip compressed when inputs are compressed")
	}

	// Decompress and verify structure
	reader, err := gzip.NewReader(bytes.NewReader(merged))
	if err != nil {
		t.Fatalf("failed to create gzip reader: %v", err)
	}
	var buf bytes.Buffer
	buf.ReadFrom(reader)
	reader.Close()

	layers, err := mvt.Unmarshal(buf.Bytes())
	if err != nil {
		t.Fatalf("failed to unmarshal merged tile: %v", err)
	}

	if len(layers) != 2 {
		t.Errorf("merged tile has %d layers, want 2", len(layers))
	}
}

func TestMergeTiles_MixedCompression(t *testing.T) {
	tile1 := createTestMVTTile("roads", 3)                // uncompressed
	tile2 := gzipData(createTestMVTTile("water", 2))      // compressed

	merged, err := MergeTiles([][]byte{tile1, tile2})
	if err != nil {
		t.Fatalf("MergeTiles() error = %v", err)
	}

	// Output should be gzip compressed if any input was compressed
	if !isGzipped(merged) {
		t.Error("merged tile should be gzip compressed when any input is compressed")
	}

	// Decompress and verify structure
	reader, err := gzip.NewReader(bytes.NewReader(merged))
	if err != nil {
		t.Fatalf("failed to create gzip reader: %v", err)
	}
	var buf bytes.Buffer
	buf.ReadFrom(reader)
	reader.Close()

	layers, err := mvt.Unmarshal(buf.Bytes())
	if err != nil {
		t.Fatalf("failed to unmarshal merged tile: %v", err)
	}

	if len(layers) != 2 {
		t.Errorf("merged tile has %d layers, want 2", len(layers))
	}
}

func TestMergeTiles_StructuralValidity(t *testing.T) {
	// Create tiles with multiple layers each
	tile1 := createMultiLayerMVTTile(map[string]int{"roads": 5, "buildings": 3})
	tile2 := createMultiLayerMVTTile(map[string]int{"water": 4, "roads": 2})
	tile3 := createMultiLayerMVTTile(map[string]int{"forest": 6})

	merged, err := MergeTiles([][]byte{tile1, tile2, tile3})
	if err != nil {
		t.Fatalf("MergeTiles() error = %v", err)
	}

	// Parse merged tile
	layers, err := mvt.Unmarshal(merged)
	if err != nil {
		t.Fatalf("failed to unmarshal merged tile: %v", err)
	}

	// Should have 4 unique layers: roads, buildings, water, forest
	if len(layers) != 4 {
		t.Errorf("merged tile has %d layers, want 4", len(layers))
	}

	// Build layer map
	layerMap := make(map[string]int)
	for _, layer := range layers {
		layerMap[layer.Name] = len(layer.Features)
	}

	// roads: 5 + 2 = 7
	if layerMap["roads"] != 7 {
		t.Errorf("roads layer has %d features, want 7", layerMap["roads"])
	}
	// buildings: 3
	if layerMap["buildings"] != 3 {
		t.Errorf("buildings layer has %d features, want 3", layerMap["buildings"])
	}
	// water: 4
	if layerMap["water"] != 4 {
		t.Errorf("water layer has %d features, want 4", layerMap["water"])
	}
	// forest: 6
	if layerMap["forest"] != 6 {
		t.Errorf("forest layer has %d features, want 6", layerMap["forest"])
	}

	// Verify each feature has valid properties
	for _, layer := range layers {
		for i, feature := range layer.Features {
			if feature.Properties == nil {
				t.Errorf("layer %s feature %d has nil properties", layer.Name, i)
			}
		}
	}
}

func TestMergeTiles_EmptyTilesSkipped(t *testing.T) {
	tile1 := createTestMVTTile("roads", 3)
	emptyTile := []byte{}
	tile2 := createTestMVTTile("water", 2)

	merged, err := MergeTiles([][]byte{tile1, emptyTile, tile2})
	if err != nil {
		t.Fatalf("MergeTiles() error = %v", err)
	}

	layers, err := mvt.Unmarshal(merged)
	if err != nil {
		t.Fatalf("failed to unmarshal merged tile: %v", err)
	}

	// Empty tiles should be skipped, result should have 2 layers
	if len(layers) != 2 {
		t.Errorf("merged tile has %d layers, want 2", len(layers))
	}
}

func TestMergeTiles_AllEmptyTiles(t *testing.T) {
	_, err := MergeTiles([][]byte{{}, {}, {}})
	if err != ErrTileNotFound {
		t.Errorf("MergeTiles() with all empty tiles error = %v, want ErrTileNotFound", err)
	}
}
