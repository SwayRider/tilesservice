package tilecache

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	log "github.com/swayrider/swlib/logger"
)

// markerFileName is the hidden file that marks a directory as owned by the
// disk tile cache. NewDiskTileCache refuses to clear a directory that does
// not carry this marker (and is not obviously a tile-cache layout), so a
// misconfigured DISK_CACHE_PATH can never silently wipe unrelated data.
const markerFileName = ".tilesservice_cache_owner"

// markerFileContent identifies the cache format that created the marker.
// Bump it if the on-disk layout ever changes.
const markerFileContent = "tilesservice disk cache v1\n"

// zDirRe matches the z<zoom> subdirectories the cache creates for tiles.
var zDirRe = regexp.MustCompile(`^z[0-9]+$`)

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
	wg             sync.WaitGroup   // Tracks background worker goroutines
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
//
// The cache claims basePath with an ownership marker file and, at startup,
// clears only the known cache artifacts inside it (z<zoom> subdirectories and
// the metadata database). It never removes basePath itself, and it refuses to
// clear a directory that does not look like a tile cache, so a misconfigured
// DISK_CACHE_PATH cannot cause data loss.
func NewDiskTileCache(basePath string, maxFiles int, logger *log.Logger) (*DiskTileCache, error) {
	l := logger.Derive(log.WithComponent("DiskTileCache"))

	// Reject obviously wrong paths before touching the filesystem.
	if err := validateCachePath(basePath); err != nil {
		return nil, err
	}

	// Claim the directory with an ownership marker and clear only the known
	// cache artifacts inside it (z<zoom> subdirectories and the metadata DB).
	// This replaces the old os.RemoveAll(basePath) startup clear, which would
	// recursively delete whatever directory was configured — catastrophic on
	// a misconfigured DISK_CACHE_PATH.
	if err := prepareCacheDir(basePath, l); err != nil {
		return nil, err
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
		_ = db.Close()
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
	c.wg.Add(2)
	go c.writeWorker()
	go c.evictionWorker()

	l.Infof("disk cache initialized at %s (max: %d files)", basePath, maxFiles)
	return c, nil
}

// validateCachePath rejects paths that can never be a safe cache directory
// (empty, filesystem root, home directory). These are the misconfigurations
// that would otherwise turn a startup cache-clear into data loss.
func validateCachePath(basePath string) error {
	if basePath == "" {
		return fmt.Errorf("refusing to use empty disk cache path")
	}
	if basePath == string(os.PathSeparator) {
		return fmt.Errorf("refusing to use filesystem root %q as disk cache path", basePath)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if basePath == home || basePath == home+string(os.PathSeparator) {
			return fmt.Errorf("refusing to use home directory %q as disk cache path", home)
		}
	}
	return nil
}

// prepareCacheDir makes sure basePath is a directory the disk cache owns, then
// clears any stale cache artifacts from a previous run.
//
// Ownership is tracked with a marker file: the cache only clears directories
// that carry the marker, are empty, or contain nothing but cache artifacts
// (an upgrade from a pre-marker layout). Any other directory is left untouched
// and produces an error, so a misconfigured DISK_CACHE_PATH fails fast instead
// of deleting data.
func prepareCacheDir(basePath string, l *log.Logger) error {
	markerPath := filepath.Join(basePath, markerFileName)

	// Fresh directory: create it and claim ownership.
	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		if err := os.MkdirAll(basePath, 0755); err != nil {
			return fmt.Errorf("failed to create cache directory: %w", err)
		}
		if err := writeMarker(markerPath); err != nil {
			return err
		}
		l.Debugf("cache directory created and claimed at %s", basePath)
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to inspect cache directory %s: %w", basePath, err)
	}

	// Existing directory with our marker: safe to clear stale artifacts.
	if _, err := os.Stat(markerPath); err == nil {
		l.Infoln("clearing existing cache directory (ownership marker present)")
		return clearCacheContents(basePath, l)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to inspect cache marker %s: %w", markerPath, err)
	}

	// No marker. Only adopt the directory if it is empty or consists solely of
	// cache artifacts (pre-marker layout); anything else is not ours to clear.
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return fmt.Errorf("failed to list cache directory %s: %w", basePath, err)
	}

	for _, e := range entries {
		name := e.Name()
		if zDirRe.MatchString(name) || strings.HasPrefix(name, "metadata.db") ||
			name == markerFileName || name == markerFileName+".tmp" {
			continue
		}
		l.Warnf("directory %s contains unexpected entry %q — not a tile cache directory", basePath, name)
		return fmt.Errorf("refusing to clear %s: it does not look like a tile cache directory (missing %s marker and contains unrelated files); configure a dedicated DISK_CACHE_PATH", basePath, markerFileName)
	}

	if len(entries) == 0 {
		l.Debugf("adopting empty cache directory %s", basePath)
	} else {
		l.Infoln("adopting existing cache directory (pre-marker layout); clearing stale content")
		if err := clearCacheContents(basePath, l); err != nil {
			return err
		}
	}
	return writeMarker(markerPath)
}

// clearCacheContents removes only the artifacts the disk cache itself creates:
// z<zoom> subdirectories and the metadata SQLite database (including any
// journal/WAL sidecar files). The configured directory and the ownership
// marker are never removed, and unexpected entries are left in place with a
// warning.
func clearCacheContents(basePath string, l *log.Logger) error {
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return fmt.Errorf("failed to list cache directory %s: %w", basePath, err)
	}

	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(basePath, name)
		switch {
		case zDirRe.MatchString(name):
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("failed to clear cache subdirectory %s: %w", path, err)
			}
		case strings.HasPrefix(name, "metadata.db"):
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("failed to remove cache metadata %s: %w", path, err)
			}
		case name == markerFileName:
			// Ownership marker survives the clear.
		case name == markerFileName+".tmp":
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("failed to remove stale marker temp file %s: %w", path, err)
			}
		default:
			l.Warnf("leaving unexpected entry %s in cache directory", path)
		}
	}
	return nil
}

// writeMarker claims a cache directory by writing the ownership marker file.
// Written atomically (temp file + rename), like tile writes.
func writeMarker(markerPath string) error {
	if err := os.MkdirAll(filepath.Dir(markerPath), 0755); err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}
	tmp := markerPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(markerFileContent), 0644); err != nil {
		return fmt.Errorf("failed to write cache marker: %w", err)
	}
	if err := os.Rename(tmp, markerPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("failed to write cache marker: %w", err)
	}
	return nil
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

	// Signal workers to stop; they drain the write queue before exiting
	close(c.stopCh)

	// Stop eviction ticker
	c.evictionTicker.Stop()

	// Wait for both workers to finish — writeWorker drains the queue
	// before returning, so all pending writes complete before we proceed.
	c.wg.Wait()

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
	defer c.wg.Done()
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
	defer c.wg.Done()
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
		_ = os.Remove(tmpPath)
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
	defer func() { _ = rows.Close() }()

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
