package tilecache

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

// TestCompressedTileCache_GetSetBasic tests basic get/set operations.
func TestCompressedTileCache_GetSetBasic(t *testing.T) {
	cache := NewCompressedTileCache(10, testLogger())
	data := []byte("test tile data")

	// Test cache miss
	if _, ok := cache.Get(7, 68, 34); ok {
		t.Error("expected cache miss for non-existent tile")
	}

	// Set tile
	cache.Set(7, 68, 34, data)

	// Test cache hit
	retrieved, ok := cache.Get(7, 68, 34)
	if !ok {
		t.Error("expected cache hit for existing tile")
	}

	if !bytes.Equal(retrieved, data) {
		t.Error("retrieved data doesn't match original")
	}
}

// TestCompressedTileCache_MultipleTiles tests storing multiple different tiles.
func TestCompressedTileCache_MultipleTiles(t *testing.T) {
	cache := NewCompressedTileCache(10, testLogger())

	tiles := []struct {
		z, x, y uint32
		data    []byte
	}{
		{7, 68, 34, []byte("tile 1")},
		{8, 100, 50, []byte("tile 2")},
		{9, 200, 100, []byte("tile 3")},
	}

	// Store all tiles
	for _, tile := range tiles {
		cache.Set(tile.z, tile.x, tile.y, tile.data)
	}

	// Verify all tiles can be retrieved
	for _, tile := range tiles {
		retrieved, ok := cache.Get(tile.z, tile.x, tile.y)
		if !ok {
			t.Errorf("expected cache hit for tile z=%d x=%d y=%d",
				tile.z, tile.x, tile.y)
		}

		if !bytes.Equal(retrieved, tile.data) {
			t.Errorf("retrieved data doesn't match for tile z=%d x=%d y=%d",
				tile.z, tile.x, tile.y)
		}
	}
}

// TestCompressedTileCache_LRU tests LRU eviction policy.
func TestCompressedTileCache_LRU(t *testing.T) {
	// Create small cache that can hold only 3 tiles
	cache := NewCompressedTileCache(3, testLogger())
	defer func() { _ = cache.Close() }()

	// Fill cache with 3 tiles
	cache.Set(1, 0, 0, []byte("tile 1"))
	cache.Set(2, 0, 0, []byte("tile 2"))
	cache.Set(3, 0, 0, []byte("tile 3"))

	// Verify all 3 tiles are in cache
	if _, ok := cache.Get(1, 0, 0); !ok {
		t.Error("tile 1 should be in cache")
	}
	if _, ok := cache.Get(2, 0, 0); !ok {
		t.Error("tile 2 should be in cache")
	}
	if _, ok := cache.Get(3, 0, 0); !ok {
		t.Error("tile 3 should be in cache")
	}

	// Add 4th tile - eviction is now asynchronous
	cache.Set(4, 0, 0, []byte("tile 4"))

	// Wait for background eviction worker to run (runs every 1 second)
	time.Sleep(1500 * time.Millisecond)

	// Verify tile 1 was evicted
	if _, ok := cache.Get(1, 0, 0); ok {
		t.Error("tile 1 should have been evicted")
	}

	// Verify other tiles still exist
	if _, ok := cache.Get(2, 0, 0); !ok {
		t.Error("tile 2 should still be in cache")
	}
	if _, ok := cache.Get(3, 0, 0); !ok {
		t.Error("tile 3 should still be in cache")
	}
	if _, ok := cache.Get(4, 0, 0); !ok {
		t.Error("tile 4 should be in cache")
	}
}

// TestCompressedTileCache_CloseIdempotent verifies that Close can be called
// multiple times without panicking on the closed stop channel.
func TestCompressedTileCache_CloseIdempotent(t *testing.T) {
	cache := NewCompressedTileCache(10, testLogger())
	if err := cache.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	// A second Close must not panic.
	if err := cache.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

// TestCompressedTileCache_DisabledCache tests cache with size 0 (disabled).
func TestCompressedTileCache_DisabledCache(t *testing.T) {
	cache := NewCompressedTileCache(0, testLogger())
	data := []byte("test data")

	// Try to set tile
	cache.Set(7, 68, 34, data)

	// Verify tile is not cached (disabled cache)
	if _, ok := cache.Get(7, 68, 34); ok {
		t.Error("disabled cache should not store tiles")
	}
}

// TestCompressedTileCache_Overwrite tests overwriting existing tile.
func TestCompressedTileCache_Overwrite(t *testing.T) {
	cache := NewCompressedTileCache(10, testLogger())

	originalData := []byte("original data")
	newData := []byte("new data")

	// Set original data
	cache.Set(7, 68, 34, originalData)

	// Verify original data
	retrieved, ok := cache.Get(7, 68, 34)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if !bytes.Equal(retrieved, originalData) {
		t.Error("original data doesn't match")
	}

	// Overwrite with new data
	cache.Set(7, 68, 34, newData)

	// Verify new data
	retrieved, ok = cache.Get(7, 68, 34)
	if !ok {
		t.Fatal("expected cache hit after overwrite")
	}
	if !bytes.Equal(retrieved, newData) {
		t.Error("new data doesn't match")
	}
}

// TestCompressedTileCache_LargeData tests caching large tile data.
func TestCompressedTileCache_LargeData(t *testing.T) {
	cache := NewCompressedTileCache(10, testLogger())

	// Simulate 10MB compressed tile
	largeData := make([]byte, 10*1024*1024)
	for i := range largeData {
		largeData[i] = byte(i % 256)
	}

	cache.Set(7, 68, 34, largeData)

	retrieved, ok := cache.Get(7, 68, 34)
	if !ok {
		t.Error("expected cache hit for large data")
	}

	if !bytes.Equal(retrieved, largeData) {
		t.Error("large data doesn't match")
	}
}

// TestCompressedTileCache_ConcurrentAccess tests basic concurrent safety.
func TestCompressedTileCache_ConcurrentAccess(t *testing.T) {
	cache := NewCompressedTileCache(100, testLogger())

	// Run concurrent operations
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func(id int) {
			for j := 0; j < 100; j++ {
				data := []byte("test data")
				cache.Set(uint32(id), uint32(j), 0, data)
				cache.Get(uint32(id), uint32(j), 0)
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	// Test passes if no race conditions occurred (run with -race flag)
}

// TestCompressedTileCache_LRUSync verifies that Set does not duplicate keys:
// the LRU list, the data map, and the position index always hold exactly one
// entry per key, even after repeated Set of the same key.
func TestCompressedTileCache_LRUSync(t *testing.T) {
	cache := NewCompressedTileCache(10, testLogger())

	// Repeatedly overwrite the same key, plus two distinct keys.
	cache.Set(7, 68, 34, []byte("v1"))
	cache.Set(7, 68, 34, []byte("v2"))
	cache.Set(8, 1, 1, []byte("other 1"))
	cache.Set(7, 68, 34, []byte("v3"))
	cache.Set(9, 2, 2, []byte("other 2"))

	cache.mu.RLock()
	cacheLen := len(cache.cache)
	lruLen := cache.lru.Len()
	posLen := len(cache.pos)
	unique := make(map[string]bool)
	for e := cache.lru.Front(); e != nil; e = e.Next() {
		unique[e.Value.(string)] = true
	}
	cache.mu.RUnlock()

	if cacheLen != 3 || lruLen != 3 || posLen != 3 {
		t.Errorf("cache/lru/pos out of sync after repeated Set: cache=%d lru=%d pos=%d, want 3 each",
			cacheLen, lruLen, posLen)
	}
	if len(unique) != 3 {
		t.Errorf("LRU list contains duplicate keys: %d unique, want 3", len(unique))
	}

	// Overwrite semantics: the latest value wins.
	data, ok := cache.Get(7, 68, 34)
	if !ok || string(data) != "v3" {
		t.Errorf("overwrite failed: got %q ok=%v, want %q", data, ok, "v3")
	}
}

// TestCompressedTileCache_GetPromotesRecency verifies that Get moves a tile to
// the most-recently-used position, protecting it from eviction.
func TestCompressedTileCache_GetPromotesRecency(t *testing.T) {
	cache := NewCompressedTileCache(3, testLogger())
	defer func() { _ = cache.Close() }()

	cache.Set(1, 0, 0, []byte("a"))
	cache.Set(2, 0, 0, []byte("b"))
	cache.Set(3, 0, 0, []byte("c"))

	// Touch the oldest tile (a); it should now be MRU and survive eviction.
	if _, ok := cache.Get(1, 0, 0); !ok {
		t.Fatal("tile a should be in cache")
	}

	// Add a 4th tile; eviction is asynchronous (worker runs every 1s).
	cache.Set(4, 0, 0, []byte("d"))
	time.Sleep(1500 * time.Millisecond)

	// b is now the least recently used and should be evicted; a was touched.
	for _, tt := range []struct {
		z, x, y uint32
		want    bool
	}{
		{1, 0, 0, true},  // a — touched, survives
		{3, 0, 0, true},  // c
		{4, 0, 0, true},  // d
		{2, 0, 0, false}, // b — untouched LRU, evicted
	} {
		_, ok := cache.Get(tt.z, tt.x, tt.y)
		if ok != tt.want {
			t.Errorf("tile z=%d x=%d y=%d: present=%v, want %v", tt.z, tt.x, tt.y, ok, tt.want)
		}
	}
}

// TestCompressedTileCache_NoNilEviction verifies that the eviction callback is
// never invoked with nil data. Before the LRU dedup fix, repeated Set of the
// same key left stale duplicate entries that could be evicted after the real
// entry was gone, handing nil to the callback (and letting the two-tier cache
// write empty tiles to disk).
func TestCompressedTileCache_NoNilEviction(t *testing.T) {
	cache := NewCompressedTileCache(2, testLogger())
	defer func() { _ = cache.Close() }()

	var mu sync.Mutex
	var evictions []struct {
		key  string
		data []byte
	}
	cache.SetOnEvict(func(key string, data []byte) {
		mu.Lock()
		defer mu.Unlock()
		evictions = append(evictions, struct {
			key  string
			data []byte
		}{key, data})
	})

	// Overwrite the same key several times (previously created duplicate LRU
	// entries), then add enough distinct tiles to force multiple eviction
	// rounds — the second round used to evict a stale duplicate with nil data.
	cache.Set(7, 68, 34, []byte("v1"))
	cache.Set(7, 68, 34, []byte("v2"))
	cache.Set(7, 68, 34, []byte("v3"))
	cache.Set(8, 0, 0, []byte("a"))
	cache.Set(9, 0, 0, []byte("b"))
	time.Sleep(1500 * time.Millisecond) // first eviction round
	cache.Set(10, 0, 0, []byte("c"))
	time.Sleep(1500 * time.Millisecond) // second eviction round

	mu.Lock()
	defer mu.Unlock()
	if len(evictions) == 0 {
		t.Fatal("expected at least one eviction")
	}
	for _, ev := range evictions {
		if ev.data == nil {
			t.Errorf("eviction callback received nil data for key %s", ev.key)
		}
	}
}
