// Package mbtiles provides functionality for reading and merging tiles from MBTiles files.

package mbtiles

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"

	"github.com/paulmach/orb/encoding/mvt"
	"github.com/paulmach/orb/geojson"
)

// MergeTiles combines multiple MVT tiles into a single tile.
// Each input tile may be gzip compressed. The output is gzip compressed
// if any of the inputs were compressed.
//
// The merge combines features from layers with the same name across all tiles.
// If multiple tiles have the same layer, their features are concatenated.
func MergeTiles(tiles [][]byte) ([]byte, error) {
	if len(tiles) == 0 {
		return nil, ErrTileNotFound
	}
	if len(tiles) == 1 {
		return tiles[0], nil
	}

	// Track if any input was gzipped
	anyGzipped := false

	// Decompress and parse all tiles
	var allLayers mvt.Layers
	layerMap := make(map[string]*mvt.Layer)

	for _, tileData := range tiles {
		if len(tileData) == 0 {
			continue
		}

		// Check for gzip and decompress if needed
		data := tileData
		if len(tileData) >= 2 && tileData[0] == 0x1f && tileData[1] == 0x8b {
			anyGzipped = true
			reader, err := gzip.NewReader(bytes.NewReader(tileData))
			if err != nil {
				return nil, err
			}
			data, err = io.ReadAll(reader)
			if cerr := reader.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return nil, err
			}
		}

		// Parse MVT
		layers, err := mvt.Unmarshal(data)
		if err != nil {
			return nil, err
		}

		// Merge layers
		for _, layer := range layers {
			if existing, ok := layerMap[layer.Name]; ok {
				// Layers with the same name must agree on Version and Extent:
				// concatenating features from mismatched tiles yields corrupted
				// coordinates (different scales) or an invalid encoding.
				if existing.Version != layer.Version || existing.Extent != layer.Extent {
					return nil, fmt.Errorf(
						"cannot merge layer %q: version/extent mismatch (%d/%d vs %d/%d)",
						layer.Name, existing.Version, existing.Extent, layer.Version, layer.Extent)
				}
				// Append features to existing layer
				existing.Features = append(existing.Features, layer.Features...)
			} else {
				// New layer - clone it
				newLayer := &mvt.Layer{
					Name:     layer.Name,
					Version:  layer.Version,
					Extent:   layer.Extent,
					Features: make([]*geojson.Feature, len(layer.Features)),
				}
				copy(newLayer.Features, layer.Features)
				layerMap[layer.Name] = newLayer
				allLayers = append(allLayers, newLayer)
			}
		}
	}

	if len(allLayers) == 0 {
		return nil, ErrTileNotFound
	}

	// Encode merged layers
	mergedData, err := mvt.Marshal(allLayers)
	if err != nil {
		return nil, err
	}

	// Compress if any input was gzipped
	if anyGzipped {
		var buf bytes.Buffer
		writer := gzip.NewWriter(&buf)
		if _, err := writer.Write(mergedData); err != nil {
			_ = writer.Close()
			return nil, err
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}

	return mergedData, nil
}
