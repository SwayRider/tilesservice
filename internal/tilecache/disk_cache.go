package tilecache

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	log "github.com/swayrider/swlib/logger"
)

// DiskTileCache implements a persistent disk-based tile cache with LRU eviction.
// It uses a hierarchical directory structure for file storage and SQLite for
// metadata tracking and LRU ordering.
type DiskTileCache struct {
	basePath       string           // Root cache directory
	mu             sync.RWMutex     // Protects metadata
	maxFiles       int              // Maximum cached files (soft limit)
	fileCount      int              // Current file count
	lruDB          *sql.DB          // SQLite for LRU tracking
	writeQueue     chan writeJob    // Async write queue
	stopCh         chan struct{}    // Shutdown signal
	evictionTicker *time.Ticker     // Background eviction ticker
	l              *log.Logger
}

// writeJob represents an async write operation.
type writeJob struct {
	z, x, y uint32
	data    []byte
}

// NewDiskTileCache creates a new disk-based tile cache.
// basePath: root directory for cache files
// maxFiles: maximum number of cached files (soft limit)
// logger: logger instance
func NewDiskTileCache(basePath string, maxFiles int, logger *log.Logger) (*DiskTileCache, error) {
	l := logger.Derive(log.WithComponent("DiskTileCache"))

	// Create cache directory if not exists
	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Clear existing cache directory to prevent serving stale tiles
	// after source MBTiles have been regenerated
	l.Infoln("clearing existing cache directory")
	if err := os.RemoveAll(basePath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to clear cache directory: %w", err)
	}

	// Recreate cache directory
	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Open/create SQLite database
	dbPath := filepath.Join(basePath, "metadata.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Initialize schema
	schema := `
	CREATE TABLE IF NOT EXISTS tile_cache (
		tile_key TEXT PRIMARY KEY,
		z INTEGER NOT NULL,
		x INTEGER NOT NULL,
		y INTEGER NOT NULL,
		access_time INTEGER NOT NULL,
		size INTEGER NOT NULL,
		created_time INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_access_time ON tile_cache(access_time);
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	c := &DiskTileCache{
		basePath:       basePath,
		maxFiles:       maxFiles,
		fileCount:      0, // Always start fresh after cache clear
		lruDB:          db,
		writeQueue:     make(chan writeJob, 1000), // Buffer for async writes
		stopCh:         make(chan struct{}),
		evictionTicker: time.NewTicker(5 * time.Second),
		l:              l,
	}

	// Start background workers
	go c.writeWorker()
	go c.evictionWorker()

	l.Infof("disk cache initialized at %s (max: %d files)", basePath, maxFiles)
	return c, nil
}

// Get retrieves a tile from disk cache.
// Returns tile data and true if found, or nil and false if not found.
func (c *DiskTileCache) Get(z, x, y uint32) ([]byte, bool) {
	path := c.tilePath(z, x, y)

	// Read file
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			c.l.Warnf("failed to read tile z=%d x=%d y=%d: %v", z, x, y, err)
		}
		return nil, false
	}

	// Debug log for successful read
	c.l.Debugf("disk cache hit z=%d x=%d y=%d size=%d", z, x, y, len(data))

	// Update access time asynchronously (best-effort)
	go c.updateAccessTime(z, x, y)

	return data, true
}

// SetAsync queues a tile for asynchronous write to disk.
// Returns error if write queue is full.
func (c *DiskTileCache) SetAsync(z, x, y uint32, data []byte) error {
	select {
	case c.writeQueue <- writeJob{z, x, y, data}:
		return nil
	default:
		return fmt.Errorf("write queue full")
	}
}

// Close gracefully shuts down the disk cache.
// Waits for pending writes to complete and closes the database.
func (c *DiskTileCache) Close() error {
	// Check if already closed
	select {
	case <-c.stopCh:
		// Already closed
		return nil
	default:
	}

	c.l.Infoln("closing disk cache")

	// Signal workers to stop
	close(c.stopCh)

	// Stop eviction ticker
	c.evictionTicker.Stop()

	// Wait for write queue to drain (with timeout)
	timeout := time.After(10 * time.Second)
	for len(c.writeQueue) > 0 {
		select {
		case <-timeout:
			c.l.Warnf("timeout waiting for write queue to drain (%d pending)", len(c.writeQueue))
			goto cleanup
		case <-time.After(100 * time.Millisecond):
			// Continue waiting
		}
	}

cleanup:
	// Close database
	if err := c.lruDB.Close(); err != nil {
		c.l.Errorf("failed to close database: %v", err)
		return err
	}

	c.mu.RLock()
	finalCount := c.fileCount
	c.mu.RUnlock()

	c.l.Infof("disk cache closed (%d files)", finalCount)
	return nil
}

// writeWorker processes async write jobs from the queue.
func (c *DiskTileCache) writeWorker() {
	for {
		select {
		case job := <-c.writeQueue:
			if err := c.writeFile(job.z, job.x, job.y, job.data); err != nil {
				c.l.Errorf("failed to write tile z=%d x=%d y=%d: %v", job.z, job.x, job.y, err)
			}
		case <-c.stopCh:
			// Drain remaining writes before exiting
			c.l.Debugf("write worker shutting down, draining %d pending writes", len(c.writeQueue))
			for {
				select {
				case job := <-c.writeQueue:
					if err := c.writeFile(job.z, job.x, job.y, job.data); err != nil {
						c.l.Errorf("failed to write tile during shutdown z=%d x=%d y=%d: %v", job.z, job.x, job.y, err)
					}
				default:
					c.l.Debugln("write worker finished draining queue")
					return
				}
			}
		}
	}
}

// evictionWorker monitors cache size and evicts oldest files when over limit.
func (c *DiskTileCache) evictionWorker() {
	for {
		select {
		case <-c.evictionTicker.C:
			// Check if shutting down before eviction
			select {
			case <-c.stopCh:
				return
			default:
			}

			c.mu.RLock()
			overLimit := c.fileCount > c.maxFiles
			currentCount := c.fileCount
			c.mu.RUnlock()

			if overLimit {
				// Evict down to 90% of max (10% headroom)
				target := int(float64(c.maxFiles) * 0.9)
				toEvict := currentCount - target
				if toEvict > 0 {
					evicted, err := c.evictBatch(toEvict)
					if err != nil {
						c.l.Errorf("eviction batch failed: %v", err)
					} else if evicted > 0 {
						c.l.Infof("evicted %d tiles (was %d, now %d, target %d)",
							evicted, currentCount, c.fileCount, target)
					}
				}
			}

		case <-c.stopCh:
			return
		}
	}
}

// writeFile writes a tile to disk and updates metadata.
func (c *DiskTileCache) writeFile(z, x, y uint32, data []byte) error {
	path := c.tilePath(z, x, y)
	key := fmt.Sprintf("%d/%d/%d", z, x, y)

	c.mu.Lock()
	defer c.mu.Unlock()

	// Create directory hierarchy
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Atomic write: write to temp file, then rename
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath) // Clean up temp file
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	// Update metadata
	now := time.Now().Unix()
	_, err := c.lruDB.Exec(`
		INSERT OR REPLACE INTO tile_cache (tile_key, z, x, y, access_time, size, created_time)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, key, z, x, y, now, len(data), now)
	if err != nil {
		// File written but metadata failed - not critical
		c.l.Debugf("failed to update metadata for z=%d x=%d y=%d: %v", z, x, y, err)
		// If metadata update failed but file was written, still increment count
		// This keeps file count accurate even if DB is closed
	}

	// Increment file count
	c.fileCount++

	// Debug log for successful write
	c.l.Debugf("disk cache wrote tile z=%d x=%d y=%d size=%d", z, x, y, len(data))

	return nil
}

// updateAccessTime updates the access timestamp for a tile (best-effort).
func (c *DiskTileCache) updateAccessTime(z, x, y uint32) {
	// Check if cache is being shut down
	select {
	case <-c.stopCh:
		return
	default:
	}

	key := fmt.Sprintf("%d/%d/%d", z, x, y)
	now := time.Now().Unix()

	c.mu.Lock()
	defer c.mu.Unlock()

	_, err := c.lruDB.Exec("UPDATE tile_cache SET access_time = ? WHERE tile_key = ?", now, key)
	if err != nil {
		// Silently ignore errors during shutdown
		select {
		case <-c.stopCh:
			return
		default:
			c.l.Debugf("failed to update access time for z=%d x=%d y=%d: %v", z, x, y, err)
		}
	}
}

// evictBatch evicts up to n oldest tiles from cache.
// Returns number of tiles actually evicted.
func (c *DiskTileCache) evictBatch(n int) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Query oldest tiles
	rows, err := c.lruDB.Query(`
		SELECT tile_key, z, x, y FROM tile_cache
		ORDER BY access_time ASC
		LIMIT ?
	`, n)
	if err != nil {
		return 0, fmt.Errorf("failed to query oldest tiles: %w", err)
	}
	defer rows.Close()

	evicted := 0
	for rows.Next() {
		var key string
		var z, x, y uint32

		if err := rows.Scan(&key, &z, &x, &y); err != nil {
			c.l.Warnf("failed to scan tile row: %v", err)
			continue
		}

		// Delete file
		path := c.tilePath(z, x, y)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			c.l.Warnf("failed to delete tile file %s: %v", path, err)
		}

		// Delete metadata
		if _, err := c.lruDB.Exec("DELETE FROM tile_cache WHERE tile_key = ?", key); err != nil {
			c.l.Warnf("failed to delete tile metadata %s: %v", key, err)
		}

		c.fileCount--
		evicted++
	}

	return evicted, nil
}

// tilePath returns the file path for a tile.
// Format: {basePath}/z{z}/{x}/{y}.mvt
func (c *DiskTileCache) tilePath(z, x, y uint32) string {
	return filepath.Join(c.basePath, fmt.Sprintf("z%d", z), fmt.Sprintf("%d", x), fmt.Sprintf("%d.mvt", y))
}
