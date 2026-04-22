// Package tilecache implements tile caching strategies for improved performance.
package tilecache

import (
	"fmt"
	"sync"
	"time"

	log "github.com/swayrider/swlib/logger"
)

// TileCache defines the interface for tile caching implementations.
// This allows for flexible caching strategies (memory-only, two-tier, etc.).
type TileCache interface {
	Get(z, x, y uint32) ([]byte, bool)
	Set(z, x, y uint32, data []byte)
	Close() error
}

// CompressedTileCache is an LRU cache for compressed tile data.
// It stores compressed tiles in memory to avoid re-compressing the same tile
// on every request. The cache uses a simple LRU eviction policy with background
// eviction to avoid blocking during Set operations.
type CompressedTileCache struct {
	cache          map[string][]byte
	lru            []string // Simple LRU tracking
	mu             sync.RWMutex
	maxSize        int
	onEvict        func(key string, data []byte) // Callback when tile is evicted
	stopCh         chan struct{}                 // Signal to stop background worker
	evictionTicker *time.Ticker                  // Periodic eviction trigger
	l              *log.Logger                   // Logger for debug output
}

// NewCompressedTileCache creates a new compressed tile cache with the specified maximum size.
// If maxSize is 0, the cache is effectively disabled (all operations become no-ops).
// Background eviction worker is started to monitor and evict when cache exceeds maxSize.
func NewCompressedTileCache(maxSize int, logger *log.Logger) *CompressedTileCache {
	c := &CompressedTileCache{
		cache:   make(map[string][]byte),
		lru:     make([]string, 0, maxSize),
		maxSize: maxSize,
		stopCh:  make(chan struct{}),
		l:       logger.Derive(log.WithComponent("MemoryCache")),
	}

	// Start background eviction worker
	if maxSize > 0 {
		c.evictionTicker = time.NewTicker(1 * time.Second)
		go c.evictionWorker()
	}

	return c
}

// SetOnEvict sets the callback function to be called when tiles are evicted.
// This is used to cascade evictions to lower cache tiers (e.g., write to disk).
func (c *CompressedTileCache) SetOnEvict(callback func(string, []byte)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onEvict = callback
}

// evictionWorker runs in the background and periodically checks if the cache
// exceeds maxSize. When over limit, it evicts oldest entries.
// This prevents blocking during Set() operations.
func (c *CompressedTileCache) evictionWorker() {
	for {
		select {
		case <-c.evictionTicker.C:
			c.mu.Lock()

			// Track evictions for logging
			evictedCount := 0
			evictedKeys := []string{}
			beforeCount := len(c.cache)

			// Evict oldest entries while over limit
			for len(c.cache) > c.maxSize && len(c.lru) > 0 {
				oldest := c.lru[0]
				evictedData := c.cache[oldest]
				delete(c.cache, oldest)
				c.lru = c.lru[1:]

				// Track eviction
				evictedCount++
				evictedKeys = append(evictedKeys, oldest)

				// Call eviction callback to move tile to next tier (disk)
				if c.onEvict != nil {
					// Call callback without holding lock to avoid deadlock
					c.mu.Unlock()
					c.onEvict(oldest, evictedData)
					c.mu.Lock()
				}
			}

			afterCount := len(c.cache)
			c.mu.Unlock()

			// Log eviction summary
			if evictedCount > 0 && c.l != nil {
				c.l.Debugf("memory cache evicted %d tiles (was %d, now %d)",
					evictedCount, beforeCount, afterCount)

				// Log first few evicted tiles for debugging
				if len(evictedKeys) <= 3 {
					for _, key := range evictedKeys {
						c.l.Debugf("memory cache evicted tile %s", key)
					}
				} else {
					// Log sample if many tiles evicted
					c.l.Debugf("memory cache evicted tiles: %s, %s, ... (and %d more)",
						evictedKeys[0], evictedKeys[1], len(evictedKeys)-2)
				}
			}

		case <-c.stopCh:
			c.evictionTicker.Stop()
			return
		}
	}
}

// Get retrieves a compressed tile from the cache.
// Returns the compressed tile data and true if found, or nil and false if not in cache.
func (c *CompressedTileCache) Get(z, x, y uint32) ([]byte, bool) {
	if c.maxSize == 0 {
		return nil, false
	}

	key := fmt.Sprintf("%d/%d/%d", z, x, y)
	c.mu.RLock()
	defer c.mu.RUnlock()

	data, ok := c.cache[key]

	// Debug log for cache hits
	if ok && c.l != nil {
		c.l.Debugf("memory cache hit z=%d x=%d y=%d size=%d", z, x, y, len(data))
	}

	return data, ok
}

// Set stores a compressed tile in the cache.
// Eviction is handled asynchronously by the background worker.
func (c *CompressedTileCache) Set(z, x, y uint32, data []byte) {
	if c.maxSize == 0 {
		return
	}

	key := fmt.Sprintf("%d/%d/%d", z, x, y)
	c.mu.Lock()
	defer c.mu.Unlock()

	// Just add to cache (no synchronous eviction)
	// Background worker will handle eviction
	c.cache[key] = data
	c.lru = append(c.lru, key)
}

// Close stops the background eviction worker and cleans up resources.
func (c *CompressedTileCache) Close() error {
	if c.stopCh != nil {
		close(c.stopCh)
	}
	return nil
}
