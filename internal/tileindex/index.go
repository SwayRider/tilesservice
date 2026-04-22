// Package tileindex provides tile lookup functionality with hierarchical file organization.
//
// # Tile Storage Structure
//
// The tile index manages access to MBTiles files organized in a hierarchical structure
// based on zoom levels:
//
//   - L0: Single file (L0.mbtiles) covering the world (zoom 0-6)
//   - L1: Regional files per 10° grid (zoom 7-10) - large roads
//   - L2: Regional files per 10° grid (zoom 11-16) - all roads (unsimplified)
//
// # File Naming Convention
//
// Regional files (L1, L2) follow the naming pattern: {lat}_{lon}.mbtiles
// where the name represents the SOUTHWEST CORNER of the 10°×10° grid cell:
//
//   - Latitude: N (north) or S (south) followed by degrees (e.g., N50, S30)
//   - Longitude: E (east) or W (west) followed by degrees (e.g., E000, W010)
//
// The latitude value is the SOUTHERN edge of the bounding box.
// The longitude value is the WESTERN edge of the bounding box.
//
// Examples:
//
//	N50_E000.mbtiles → covers 50°N to 60°N, 0°E to 10°E (SW corner: 50°N, 0°E)
//	N40_W010.mbtiles → covers 40°N to 50°N, 10°W to 0°  (SW corner: 40°N, 10°W)
//	S30_E020.mbtiles → covers 30°S to 20°S, 20°E to 30°E (SW corner: 30°S, 20°E)
//
// # Coordinate Systems
//
// This package works with two coordinate systems:
//
// 1. XYZ Tile Coordinates (used by web maps):
//   - z: zoom level (0 = world, higher = more detail)
//   - x: column, increases eastward (left to right)
//   - y: row, increases SOUTHWARD (top to bottom, y=0 at north pole)
//
// 2. Geographic Coordinates (latitude/longitude):
//   - Latitude: degrees north (positive) or south (negative) of equator
//   - Longitude: degrees east (positive) or west (negative) of prime meridian
//
// # Tile-to-File Mapping
//
// To determine which MBTiles file contains a given tile:
//
//  1. Convert tile (z, x, y) to the tile's SOUTHWEST corner in lat/lon
//  2. Floor the coordinates to the nearest 10° grid
//  3. Format as filename (e.g., N50_E000.mbtiles)
//
// Using the southwest corner ensures consistency with the file naming convention,
// which is based on the southwest corner of each 10° grid cell.
package tileindex

import (
	"fmt"
	"math"
	"path/filepath"
	"sync"

	"github.com/swayrider/tilesservice/internal/mbtiles"
)

// Layer represents a zoom level range for tile storage.
type Layer int

const (
	// LayerL0 is the world backdrop layer (zoom 0-6).
	LayerL0 Layer = iota
	// LayerL1 is the continental layer with large roads (zoom 7-10).
	LayerL1
	// LayerL2 is the regional+local layer with all roads (zoom 11-16).
	LayerL2
)

// TileIndex manages access to hierarchically organized MBTiles files.
type TileIndex struct {
	basePath string
	cache    map[string]*mbtiles.Reader
	mu       sync.RWMutex
}

// New creates a new TileIndex with the given base path.
// The base path should point to the directory containing L0.mbtiles
// and the L1, L2 subdirectories.
func New(basePath string) *TileIndex {
	return &TileIndex{
		basePath: basePath,
		cache:    make(map[string]*mbtiles.Reader),
	}
}

// GetTile retrieves a tile for the given zoom level and coordinates.
//
// Parameters:
//   - z: zoom level (0-16)
//   - x: tile column
//   - y: tile row (XYZ scheme)
//
// Returns the tile data or an error if the tile cannot be found.
// If the tile spans multiple grid cells, data from all relevant files
// is fetched and merged.
func (idx *TileIndex) GetTile(z, x, y uint32) ([]byte, error) {
	layer := zoomToLayer(z)

	// L0 is a single file, no merging needed
	if layer == LayerL0 {
		path := filepath.Join(idx.basePath, "L0.mbtiles")
		reader, err := idx.getReader(path)
		if err != nil {
			return nil, err
		}
		return reader.GetTile(z, x, y)
	}

	// For L1-L2, check if tile spans multiple grid cells
	paths := idx.getOverlappingFilePaths(z, x, y)

	if len(paths) == 1 {
		// Single file, no merging needed
		reader, err := idx.getReader(paths[0])
		if err != nil {
			return nil, err
		}
		return reader.GetTile(z, x, y)
	}

	// Multiple files - fetch from all and merge
	var tiles [][]byte
	for _, path := range paths {
		reader, err := idx.getReader(path)
		if err != nil {
			// File doesn't exist - skip it
			continue
		}
		tileData, err := reader.GetTile(z, x, y)
		if err == mbtiles.ErrTileNotFound {
			continue
		}
		if err != nil {
			return nil, err
		}
		tiles = append(tiles, tileData)
	}

	if len(tiles) == 0 {
		return nil, mbtiles.ErrTileNotFound
	}

	return mbtiles.MergeTiles(tiles)
}

// getOverlappingFilePaths returns all MBTiles file paths that a tile overlaps.
// A tile can span up to 4 grid cells if it crosses both a latitude and longitude
// boundary.
func (idx *TileIndex) getOverlappingFilePaths(z, x, y uint32) []string {
	layer := zoomToLayer(z)
	layerDir := fmt.Sprintf("L%d", layer)

	// Calculate all four corners of the tile
	nwLat, nwLon := tileCornerToLatLon(z, x, y)     // Northwest (top-left)
	neLat, neLon := tileCornerToLatLon(z, x+1, y)   // Northeast (top-right)
	swLat, swLon := tileCornerToLatLon(z, x, y+1)   // Southwest (bottom-left)
	seLat, seLon := tileCornerToLatLon(z, x+1, y+1) // Southeast (bottom-right)

	corners := []struct{ lat, lon float64 }{
		{nwLat, nwLon},
		{neLat, neLon},
		{swLat, swLon},
		{seLat, seLon},
	}

	// Collect unique grid cells
	seen := make(map[string]bool)
	var paths []string

	for _, corner := range corners {
		gridLat, gridLon := snapToGrid(corner.lat, corner.lon)
		filename := formatGridFilename(gridLat, gridLon)

		if !seen[filename] {
			seen[filename] = true
			paths = append(paths, filepath.Join(idx.basePath, layerDir, filename))
		}
	}

	return paths
}

// tileCornerToLatLon converts a tile corner coordinate to lat/lon.
// Unlike tileToLatLon which returns the SW corner, this returns the exact
// corner at the given (z, x, y) position.
func tileCornerToLatLon(z, x, y uint32) (lat, lon float64) {
	n := float64(uint32(1) << z)

	// Longitude at x
	lon = float64(x)/n*360.0 - 180.0

	// Latitude at y (using Web Mercator projection)
	latRad := math.Atan(math.Sinh(math.Pi * (1 - 2*float64(y)/n)))
	lat = latRad * 180.0 / math.Pi

	return lat, lon
}

// getFilePath determines the MBTiles file path for the given tile coordinates.
//
// For L0 (zoom 0-6), returns the single world file.
// For L1-L2, calculates the tile's southwest corner, snaps to the 10° grid,
// and returns the corresponding regional file path.
//
// Deprecated: Use getOverlappingFilePaths for tiles that may span grid boundaries.
func (idx *TileIndex) getFilePath(z, x, y uint32) (string, error) {
	layer := zoomToLayer(z)

	if layer == LayerL0 {
		return filepath.Join(idx.basePath, "L0.mbtiles"), nil
	}

	// Calculate the southwest corner of the tile to determine which file to use
	lat, lon := tileToLatLon(z, x, y)

	// Snap to 10-degree grid
	gridLat, gridLon := snapToGrid(lat, lon)

	// Build filename
	filename := formatGridFilename(gridLat, gridLon)
	layerDir := fmt.Sprintf("L%d", layer)

	return filepath.Join(idx.basePath, layerDir, filename), nil
}

// getReader returns a cached reader for the given path, opening it if necessary.
func (idx *TileIndex) getReader(path string) (*mbtiles.Reader, error) {
	idx.mu.RLock()
	reader, ok := idx.cache[path]
	idx.mu.RUnlock()

	if ok {
		return reader, nil
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Double-check after acquiring write lock
	if reader, ok := idx.cache[path]; ok {
		return reader, nil
	}

	reader, err := mbtiles.Open(path)
	if err != nil {
		return nil, err
	}

	idx.cache[path] = reader
	return reader, nil
}

// Close closes all cached MBTiles readers.
func (idx *TileIndex) Close() error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var lastErr error
	for path, reader := range idx.cache {
		if err := reader.Close(); err != nil {
			lastErr = err
		}
		delete(idx.cache, path)
	}
	return lastErr
}

// zoomToLayer maps a zoom level to its corresponding layer.
func zoomToLayer(z uint32) Layer {
	switch {
	case z <= 6:
		return LayerL0
	case z <= 10:
		return LayerL1
	default:
		return LayerL2  // Z11-16 uses L2 (consolidated regional+local)
	}
}

// tileToLatLon converts XYZ tile coordinates to the SOUTHWEST corner of the tile.
//
// In the XYZ tile coordinate system:
//   - x increases eastward (left to right)
//   - y increases SOUTHWARD (top to bottom, y=0 is at the north pole)
//
// A tile at (z, x, y) has four corners:
//   - Northwest (top-left):     x,   y
//   - Northeast (top-right):    x+1, y
//   - Southwest (bottom-left):  x,   y+1  ← this is what we return
//   - Southeast (bottom-right): x+1, y+1
//
// The southwest corner is at:
//   - Western edge:  longitude at x (not x+1)
//   - Southern edge: latitude at y+1 (not y, since y increases southward)
//
// We use the southwest corner because it matches the MBTiles file naming
// convention, where filenames like "N50_E000.mbtiles" represent the
// southwest corner of the 10° grid cell.
func tileToLatLon(z, x, y uint32) (lat, lon float64) {
	n := float64(uint32(1) << z)

	// Longitude: western edge of tile (x, not x+1)
	lon = float64(x)/n*360.0 - 180.0

	// Latitude: southern edge of tile (y+1, not y, because y increases southward)
	// Uses inverse Web Mercator projection
	latRad := math.Atan(math.Sinh(math.Pi * (1 - 2*float64(y+1)/n)))
	lat = latRad * 180.0 / math.Pi

	return lat, lon
}

// snapToGrid snaps geographic coordinates to the 10° grid used for file naming.
//
// Given any lat/lon coordinate, this returns the southwest corner of the
// 10°×10° grid cell containing that point. The result can be used directly
// with formatGridFilename to determine the MBTiles file.
//
// Examples:
//
//	snapToGrid(52.37, 4.90)  → (50, 0)   // Amsterdam → N50_E000
//	snapToGrid(38.72, -9.14) → (30, -10) // Lisbon → N30_W010
//	snapToGrid(-33.9, 18.4)  → (-40, 10) // Cape Town → S40_E010
func snapToGrid(lat, lon float64) (gridLat, gridLon int) {
	// Floor to 10-degree intervals to get the southwest corner of the grid cell
	gridLat = int(math.Floor(lat/10.0)) * 10
	gridLon = int(math.Floor(lon/10.0)) * 10

	return gridLat, gridLon
}

// formatGridFilename creates an MBTiles filename for the given grid coordinates.
//
// The filename format matches the data pipeline convention:
//
//	{lat_prefix}{lat_degrees}_{lon_prefix}{lon_degrees}.mbtiles
//
// Where:
//   - lat_prefix: "N" for north (≥0), "S" for south (<0)
//   - lat_degrees: 2-digit absolute value of latitude
//   - lon_prefix: "E" for east (≥0), "W" for west (<0)
//   - lon_degrees: 3-digit absolute value of longitude
//
// Examples:
//
//	formatGridFilename(50, 0)    → "N50_E000.mbtiles"
//	formatGridFilename(40, -10)  → "N40_W010.mbtiles"
//	formatGridFilename(-30, 20)  → "S30_E020.mbtiles"
//	formatGridFilename(-40, -120) → "S40_W120.mbtiles"
func formatGridFilename(gridLat, gridLon int) string {
	var latPrefix, lonPrefix string
	var latVal, lonVal int

	if gridLat >= 0 {
		latPrefix = "N"
		latVal = gridLat
	} else {
		latPrefix = "S"
		latVal = -gridLat
	}

	if gridLon >= 0 {
		lonPrefix = "E"
		lonVal = gridLon
	} else {
		lonPrefix = "W"
		lonVal = -gridLon
	}

	return fmt.Sprintf("%s%02d_%s%03d.mbtiles", latPrefix, latVal, lonPrefix, lonVal)
}
