// Package mbtiles provides functionality for reading tiles from MBTiles files.
//
// MBTiles is a SQLite database format for storing map tiles. This package
// provides a Reader type that can open MBTiles files and retrieve individual
// tiles by their zoom level and x/y coordinates.
//
// The MBTiles format stores tiles using the TMS (Tile Map Service) y-coordinate
// scheme where y increases northward. This package automatically converts from
// the XYZ scheme (y increases southward, as used by web maps) to TMS when
// querying tiles.
package mbtiles

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"

	_ "github.com/mattn/go-sqlite3" // SQLite driver
)

// ErrTileNotFound is returned when a requested tile does not exist in the database.
var ErrTileNotFound = errors.New("tile not found")

// Reader provides read access to an MBTiles file.
type Reader struct {
	db   *sql.DB
	path string
	mu   sync.RWMutex
}

// Open opens an MBTiles file for reading.
// Returns an error if the file does not exist or cannot be opened.
func Open(path string) (*Reader, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf("mbtiles file not found: %s", path)
	}

	db, err := sql.Open("sqlite3", path+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("failed to open mbtiles: %w", err)
	}

	// Verify the database has a tiles table
	var name string
	err = db.QueryRow("SELECT name FROM sqlite_master WHERE type IN ('table', 'view') AND name='tiles'").Scan(&name)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("invalid mbtiles file (no tiles table): %w", err)
	}

	return &Reader{
		db:   db,
		path: path,
	}, nil
}

// GetTile retrieves a tile from the MBTiles file.
//
// Parameters:
//   - z: zoom level
//   - x: tile column (x coordinate in XYZ scheme)
//   - y: tile row (y coordinate in XYZ scheme, will be converted to TMS)
//
// Returns the tile data as bytes, or ErrTileNotFound if the tile does not exist.
func (r *Reader) GetTile(z, x, y uint32) ([]byte, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Convert from XYZ to TMS y-coordinate
	// TMS y = (2^z - 1) - XYZ y
	tmsY := (1 << z) - 1 - y

	var tileData []byte
	err := r.db.QueryRow(
		"SELECT tile_data FROM tiles WHERE zoom_level = ? AND tile_column = ? AND tile_row = ?",
		z, x, tmsY,
	).Scan(&tileData)

	if err == sql.ErrNoRows {
		return nil, ErrTileNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query tile: %w", err)
	}

	return tileData, nil
}

// Close closes the MBTiles file.
func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.db != nil {
		return r.db.Close()
	}
	return nil
}

// Path returns the file path of the MBTiles file.
func (r *Reader) Path() string {
	return r.path
}
