package tilecache

import (
	"strconv"
	"strings"

	log "github.com/swayrider/swlib/logger"
)

// TwoTierCache implements a two-tier caching system with memory (L1) and disk (L2) layers.
// It coordinates between the fast memory cache and the persistent disk cache,
// automatically promoting disk hits to memory and cascading memory evictions to disk.
type TwoTierCache struct {
	memory *CompressedTileCache
	disk   *DiskTileCache
	l      *log.Logger
}

// NewTwoTierCache creates a new two-tier cache with the given memory and disk caches.
// It wires the memory eviction callback to write evicted tiles to disk.
func NewTwoTierCache(memory *CompressedTileCache, disk *DiskTileCache, logger *log.Logger) *TwoTierCache {
	l := logger.Derive(log.WithComponent("TwoTierCache"))

	t := &TwoTierCache{
		memory: memory,
		disk:   disk,
		l:      l,
	}

	// Wire memory eviction callback to disk writes
	// This implements the eviction cascade: L1 eviction -> L2 write
	memory.SetOnEvict(func(key string, data []byte) {
		// Parse key "z/x/y"
		parts := strings.Split(key, "/")
		if len(parts) != 3 {
			l.Warnf("invalid key format in eviction callback: %s", key)
			return
		}

		z, err := strconv.ParseUint(parts[0], 10, 32)
		if err != nil {
			l.Warnf("invalid z in key %s: %v", key, err)
			return
		}

		x, err := strconv.ParseUint(parts[1], 10, 32)
		if err != nil {
			l.Warnf("invalid x in key %s: %v", key, err)
			return
		}

		y, err := strconv.ParseUint(parts[2], 10, 32)
		if err != nil {
			l.Warnf("invalid y in key %s: %v", key, err)
			return
		}

		// Write evicted tile to disk (async, non-blocking)
		if err := disk.SetAsync(uint32(z), uint32(x), uint32(y), data); err != nil {
			l.Warnf("failed to write evicted tile to disk z=%d x=%d y=%d: %v", z, x, y, err)
		} else {
			l.Debugf("evicted tile to disk z=%d x=%d y=%d", z, x, y)
		}
	})

	return t
}

// Get retrieves a tile from the cache hierarchy.
// Checks memory first (L1), then disk (L2). Disk hits are promoted to memory.
func (t *TwoTierCache) Get(z, x, y uint32) ([]byte, bool) {
	// L1: Check memory cache
	if data, ok := t.memory.Get(z, x, y); ok {
		return data, true
	}

	// L2: Check disk cache
	data, ok := t.disk.Get(z, x, y)
	if !ok {
		return nil, false
	}

	// Promote to memory
	t.memory.Set(z, x, y, data)
	t.l.Debugf("promoted tile from disk to memory z=%d x=%d y=%d", z, x, y)

	return data, true
}

// Set stores a tile in both cache layers.
// Memory write is synchronous, disk write is asynchronous (best-effort).
func (t *TwoTierCache) Set(z, x, y uint32, data []byte) {
	// Always write to memory (fast, synchronous)
	t.memory.Set(z, x, y, data)

	// Queue disk write (async, best-effort)
	// Note: Memory evictions will also write to disk via callback
	if err := t.disk.SetAsync(z, x, y, data); err != nil {
		t.l.Debugf("disk write queue full z=%d x=%d y=%d: %v", z, x, y, err)
	}
}

// Close gracefully shuts down both cache layers.
func (t *TwoTierCache) Close() error {
	t.l.Infoln("closing two-tier cache")

	// Close memory cache
	if err := t.memory.Close(); err != nil {
		t.l.Errorf("failed to close memory cache: %v", err)
	}

	// Close disk cache
	if err := t.disk.Close(); err != nil {
		t.l.Errorf("failed to close disk cache: %v", err)
		return err
	}

	return nil
}
