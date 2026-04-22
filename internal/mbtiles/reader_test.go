package mbtiles

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// createTestMBTiles creates a temporary MBTiles file with test data.
// Returns the file path and a cleanup function.
func createTestMBTiles(t *testing.T, tiles map[string][]byte) (string, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "mbtiles-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	dbPath := filepath.Join(tmpDir, "test.mbtiles")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("failed to create test db: %v", err)
	}

	// Create the tiles table
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
		db.Close()
		os.RemoveAll(tmpDir)
		t.Fatalf("failed to create tiles table: %v", err)
	}

	// Insert test tiles
	stmt, err := db.Prepare("INSERT INTO tiles (zoom_level, tile_column, tile_row, tile_data) VALUES (?, ?, ?, ?)")
	if err != nil {
		db.Close()
		os.RemoveAll(tmpDir)
		t.Fatalf("failed to prepare insert: %v", err)
	}

	for key, data := range tiles {
		var z, x, y int
		_, err := parseKey(key, &z, &x, &y)
		if err != nil {
			stmt.Close()
			db.Close()
			os.RemoveAll(tmpDir)
			t.Fatalf("invalid tile key %q: %v", key, err)
		}
		if _, err := stmt.Exec(z, x, y, data); err != nil {
			stmt.Close()
			db.Close()
			os.RemoveAll(tmpDir)
			t.Fatalf("failed to insert tile: %v", err)
		}
	}

	stmt.Close()
	db.Close()

	return dbPath, func() {
		os.RemoveAll(tmpDir)
	}
}

// parseKey parses a "z/x/y" key into integers. Returns the number of parsed values.
func parseKey(key string, z, x, y *int) (int, error) {
	n, err := sscanf(key, "%d/%d/%d", z, x, y)
	return n, err
}

// sscanf is a simple scanf-like parser for "z/x/y" format.
func sscanf(s, format string, args ...interface{}) (int, error) {
	var z, x, y int
	n, err := func() (int, error) {
		_, err := parseInts(s, &z, &x, &y)
		if err != nil {
			return 0, err
		}
		return 3, nil
	}()
	if err != nil {
		return 0, err
	}
	if len(args) >= 1 {
		*args[0].(*int) = z
	}
	if len(args) >= 2 {
		*args[1].(*int) = x
	}
	if len(args) >= 3 {
		*args[2].(*int) = y
	}
	return n, nil
}

// parseInts parses "z/x/y" into three integers.
func parseInts(s string, z, x, y *int) (int, error) {
	var count int
	var current int
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '/' {
			switch count {
			case 0:
				*z = current
			case 1:
				*x = current
			case 2:
				*y = current
			}
			count++
			current = 0
		} else if s[i] >= '0' && s[i] <= '9' {
			current = current*10 + int(s[i]-'0')
		}
	}
	return count, nil
}

func TestOpen(t *testing.T) {
	t.Run("opens valid mbtiles file", func(t *testing.T) {
		dbPath, cleanup := createTestMBTiles(t, map[string][]byte{
			"0/0/0": []byte("tile-data"),
		})
		defer cleanup()

		reader, err := Open(dbPath)
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		defer reader.Close()

		if reader.Path() != dbPath {
			t.Errorf("Path() = %v, want %v", reader.Path(), dbPath)
		}
	})

	t.Run("returns error for non-existent file", func(t *testing.T) {
		_, err := Open("/nonexistent/path/test.mbtiles")
		if err == nil {
			t.Error("Open() expected error for non-existent file")
		}
	})

	t.Run("opens valid mbtiles file with tiles view (deduplicated format)", func(t *testing.T) {
		tmpDir, err := os.MkdirTemp("", "mbtiles-test-*")
		if err != nil {
			t.Fatalf("failed to create temp dir: %v", err)
		}
		defer os.RemoveAll(tmpDir)

		dbPath := filepath.Join(tmpDir, "dedup.mbtiles")
		db, err := sql.Open("sqlite3", dbPath)
		if err != nil {
			t.Fatalf("failed to create db: %v", err)
		}
		db.Exec(`CREATE TABLE map (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_id TEXT)`)
		db.Exec(`CREATE TABLE images (tile_id TEXT PRIMARY KEY, tile_data BLOB)`)
		db.Exec(`CREATE VIEW tiles AS SELECT map.zoom_level, map.tile_column, map.tile_row, images.tile_data FROM map JOIN images ON map.tile_id = images.tile_id`)
		db.Exec(`INSERT INTO images VALUES ('abc', 'tile-data')`)
		db.Exec(`INSERT INTO map VALUES (0, 0, 0, 'abc')`)
		db.Close()

		reader, err := Open(dbPath)
		if err != nil {
			t.Fatalf("Open() error = %v, want nil for view-based mbtiles", err)
		}
		defer reader.Close()
	})

	t.Run("returns error for invalid mbtiles (no tiles table)", func(t *testing.T) {
		tmpDir, err := os.MkdirTemp("", "mbtiles-test-*")
		if err != nil {
			t.Fatalf("failed to create temp dir: %v", err)
		}
		defer os.RemoveAll(tmpDir)

		dbPath := filepath.Join(tmpDir, "invalid.mbtiles")
		db, err := sql.Open("sqlite3", dbPath)
		if err != nil {
			t.Fatalf("failed to create db: %v", err)
		}
		db.Exec("CREATE TABLE metadata (name TEXT, value TEXT)")
		db.Close()

		_, err = Open(dbPath)
		if err == nil {
			t.Error("Open() expected error for invalid mbtiles")
		}
	})
}

func TestGetTile(t *testing.T) {
	// Create test data with TMS coordinates (y increases northward)
	// For z=0, the world is 1x1 tiles, TMS y=0 is at the bottom
	// For z=1, the world is 2x2 tiles
	testTiles := map[string][]byte{
		"0/0/0": []byte("world-tile"),
		"1/0/0": []byte("z1-bottom-left"),
		"1/0/1": []byte("z1-top-left"),
		"1/1/0": []byte("z1-bottom-right"),
		"1/1/1": []byte("z1-top-right"),
		"5/10/15": []byte("z5-tile"),
	}

	dbPath, cleanup := createTestMBTiles(t, testTiles)
	defer cleanup()

	reader, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer reader.Close()

	t.Run("retrieves tile with XYZ to TMS conversion", func(t *testing.T) {
		// At z=0, there's only one tile (0,0)
		// XYZ y=0 -> TMS y = (2^0 - 1) - 0 = 0
		data, err := reader.GetTile(0, 0, 0)
		if err != nil {
			t.Fatalf("GetTile(0,0,0) error = %v", err)
		}
		if string(data) != "world-tile" {
			t.Errorf("GetTile(0,0,0) = %q, want %q", data, "world-tile")
		}
	})

	t.Run("converts XYZ to TMS correctly at z=1", func(t *testing.T) {
		// At z=1: TMS y = (2^1 - 1) - XYZ y = 1 - XYZ y
		// XYZ (1,0,0) -> TMS (1,0,1) = "z1-top-left"
		data, err := reader.GetTile(1, 0, 0)
		if err != nil {
			t.Fatalf("GetTile(1,0,0) error = %v", err)
		}
		if string(data) != "z1-top-left" {
			t.Errorf("GetTile(1,0,0) = %q, want %q", data, "z1-top-left")
		}

		// XYZ (1,0,1) -> TMS (1,0,0) = "z1-bottom-left"
		data, err = reader.GetTile(1, 0, 1)
		if err != nil {
			t.Fatalf("GetTile(1,0,1) error = %v", err)
		}
		if string(data) != "z1-bottom-left" {
			t.Errorf("GetTile(1,0,1) = %q, want %q", data, "z1-bottom-left")
		}
	})

	t.Run("returns ErrTileNotFound for missing tile", func(t *testing.T) {
		_, err := reader.GetTile(10, 999, 999)
		if err != ErrTileNotFound {
			t.Errorf("GetTile() error = %v, want ErrTileNotFound", err)
		}
	})
}

func TestClose(t *testing.T) {
	dbPath, cleanup := createTestMBTiles(t, map[string][]byte{
		"0/0/0": []byte("data"),
	})
	defer cleanup()

	reader, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	if err := reader.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}

	// Second close should not error
	if err := reader.Close(); err != nil {
		t.Errorf("second Close() error = %v", err)
	}
}
