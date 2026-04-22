package tilecache

import (
	"testing"
	"time"
)

// TestTwoTierCache_MemoryHit tests the fast path from memory cache.
func TestTwoTierCache_MemoryHit(t *testing.T) {
	tmpDir := t.TempDir()

	memCache := NewCompressedTileCache(10, testLogger())
	diskCache, err := NewDiskTileCache(tmpDir, 100, testLogger())
	if err != nil {
		t.Fatalf("failed to create disk cache: %v", err)
	}

	twoTier := NewTwoTierCache(memCache, diskCache, testLogger())
	defer twoTier.Close()

	// Write tile
	testData := []byte("test data")
	twoTier.Set(7, 68, 34, testData)

	// Read immediately (should hit memory)
	data, ok := twoTier.Get(7, 68, 34)
	if !ok {
		t.Fatal("expected cache hit")
	}

	if string(data) != string(testData) {
		t.Errorf("wrong data: got %q, want %q", data, testData)
	}
}

// TestTwoTierCache_DiskHitPromotion tests that disk hits are promoted to memory.
func TestTwoTierCache_DiskHitPromotion(t *testing.T) {
	tmpDir := t.TempDir()

	memCache := NewCompressedTileCache(2, testLogger())
	diskCache, err := NewDiskTileCache(tmpDir, 100, testLogger())
	if err != nil {
		t.Fatalf("failed to create disk cache: %v", err)
	}

	twoTier := NewTwoTierCache(memCache, diskCache, testLogger())
	defer twoTier.Close()

	// Write tile
	testData := []byte("test data")
	twoTier.Set(7, 68, 34, testData)

	// Wait for async disk write
	time.Sleep(50 * time.Millisecond)

	// Clear memory cache manually (simulate eviction)
	memCache.mu.Lock()
	memCache.cache = make(map[string][]byte)
	memCache.lru = []string{}
	memCache.mu.Unlock()

	// Get tile (should hit disk and promote)
	data, ok := twoTier.Get(7, 68, 34)
	if !ok {
		t.Fatal("expected disk cache hit")
	}

	if string(data) != string(testData) {
		t.Errorf("wrong data: got %q, want %q", data, testData)
	}

	// Verify promotion to memory
	if _, ok := memCache.Get(7, 68, 34); !ok {
		t.Error("tile should be promoted to memory")
	}
}

// TestTwoTierCache_CacheMiss tests that both layers miss.
func TestTwoTierCache_CacheMiss(t *testing.T) {
	tmpDir := t.TempDir()

	memCache := NewCompressedTileCache(10, testLogger())
	diskCache, err := NewDiskTileCache(tmpDir, 100, testLogger())
	if err != nil {
		t.Fatalf("failed to create disk cache: %v", err)
	}

	twoTier := NewTwoTierCache(memCache, diskCache, testLogger())
	defer twoTier.Close()

	// Try to get non-existent tile
	_, ok := twoTier.Get(7, 68, 34)
	if ok {
		t.Error("expected cache miss")
	}
}

// TestTwoTierCache_MemoryEvictionWritesToDisk tests eviction callback.
func TestTwoTierCache_MemoryEvictionWritesToDisk(t *testing.T) {
	tmpDir := t.TempDir()

	// Small memory cache to trigger eviction
	memCache := NewCompressedTileCache(2, testLogger())
	diskCache, err := NewDiskTileCache(tmpDir, 100, testLogger())
	if err != nil {
		t.Fatalf("failed to create disk cache: %v", err)
	}

	twoTier := NewTwoTierCache(memCache, diskCache, testLogger())
	defer twoTier.Close()

	// Write 3 tiles (exceeds memory limit of 2)
	twoTier.Set(7, 68, 0, []byte("tile0"))
	twoTier.Set(7, 68, 1, []byte("tile1"))
	twoTier.Set(7, 68, 2, []byte("tile2"))

	// Wait for eviction worker to run
	time.Sleep(2 * time.Second)

	// Wait for async disk writes from eviction
	time.Sleep(100 * time.Millisecond)

	// All tiles should be on disk (from both Set and eviction)
	for i := 0; i < 3; i++ {
		if _, ok := diskCache.Get(7, 68, uint32(i)); !ok {
			t.Errorf("tile %d should be on disk", i)
		}
	}

	// Memory should have only ~2 tiles
	memCache.mu.RLock()
	memCount := len(memCache.cache)
	memCache.mu.RUnlock()

	if memCount > 2 {
		t.Errorf("memory cache should have <= 2 tiles, got %d", memCount)
	}
}

// TestTwoTierCache_ConcurrentAccess tests thread safety across layers.
func TestTwoTierCache_ConcurrentAccess(t *testing.T) {
	tmpDir := t.TempDir()

	memCache := NewCompressedTileCache(50, testLogger())
	diskCache, err := NewDiskTileCache(tmpDir, 200, testLogger())
	if err != nil {
		t.Fatalf("failed to create disk cache: %v", err)
	}

	twoTier := NewTwoTierCache(memCache, diskCache, testLogger())
	defer twoTier.Close()

	// Concurrent writes and reads
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func(n int) {
			for j := 0; j < 10; j++ {
				// Write
				twoTier.Set(7, uint32(n), uint32(j), []byte("data"))
				// Read
				twoTier.Get(7, uint32(n), uint32(j))
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	// Wait for async writes
	time.Sleep(200 * time.Millisecond)

	// Verify no panics or errors (basic smoke test)
	// If we got here without panic, concurrent access is safe
}

// TestTwoTierCache_DiskUnavailable tests graceful degradation when disk fails.
func TestTwoTierCache_DiskUnavailable(t *testing.T) {
	// Create disk cache with invalid path (will fail on writes)
	tmpDir := t.TempDir()
	memCache := NewCompressedTileCache(10, testLogger())
	diskCache, err := NewDiskTileCache(tmpDir, 100, testLogger())
	if err != nil {
		t.Fatalf("failed to create disk cache: %v", err)
	}

	twoTier := NewTwoTierCache(memCache, diskCache, testLogger())
	defer twoTier.Close()

	// Close disk cache to simulate unavailability
	diskCache.Close()

	// Writes should still work (memory layer continues)
	testData := []byte("test data")
	twoTier.Set(7, 68, 34, testData)

	// Memory read should work
	data, ok := twoTier.Get(7, 68, 34)
	if !ok {
		t.Error("memory cache should still work when disk is unavailable")
	}

	if string(data) != string(testData) {
		t.Errorf("wrong data: got %q, want %q", data, testData)
	}
}

// TestTwoTierCache_PromotionUpdatesAccessPattern tests that promotion maintains LRU order.
func TestTwoTierCache_PromotionUpdatesAccessPattern(t *testing.T) {
	tmpDir := t.TempDir()

	memCache := NewCompressedTileCache(2, testLogger())
	diskCache, err := NewDiskTileCache(tmpDir, 100, testLogger())
	if err != nil {
		t.Fatalf("failed to create disk cache: %v", err)
	}

	twoTier := NewTwoTierCache(memCache, diskCache, testLogger())
	defer twoTier.Close()

	// Write tiles
	twoTier.Set(7, 68, 0, []byte("tile0"))
	twoTier.Set(7, 68, 1, []byte("tile1"))
	time.Sleep(50 * time.Millisecond) // Wait for disk writes

	// Clear memory
	memCache.mu.Lock()
	memCache.cache = make(map[string][]byte)
	memCache.lru = []string{}
	memCache.mu.Unlock()

	// Access tile0 from disk (should promote)
	twoTier.Get(7, 68, 0)

	// Verify tile0 is in memory
	memCache.mu.RLock()
	_, inMem := memCache.cache["7/68/0"]
	lruLen := len(memCache.lru)
	memCache.mu.RUnlock()

	if !inMem {
		t.Error("promoted tile should be in memory")
	}

	if lruLen == 0 {
		t.Error("LRU list should have promoted tile")
	}
}

// TestTwoTierCache_DiskWriteQueueFull tests behavior when disk write queue is full.
func TestTwoTierCache_DiskWriteQueueFull(t *testing.T) {
	tmpDir := t.TempDir()

	memCache := NewCompressedTileCache(50, testLogger())
	diskCache, err := NewDiskTileCache(tmpDir, 10000, testLogger())
	if err != nil {
		t.Fatalf("failed to create disk cache: %v", err)
	}

	twoTier := NewTwoTierCache(memCache, diskCache, testLogger())
	defer twoTier.Close()

	// Write many tiles rapidly to overflow disk queue
	for i := 0; i < 1100; i++ {
		twoTier.Set(7, 68, uint32(i), []byte("data"))
	}

	// Memory cache should still work even if disk queue is full
	if _, ok := twoTier.Get(7, 68, 0); !ok {
		t.Error("memory cache should still work when disk queue is full")
	}
}

// TestTwoTierCache_MemoryDisabled tests two-tier cache with memory cache disabled.
func TestTwoTierCache_MemoryDisabled(t *testing.T) {
	tmpDir := t.TempDir()

	// Memory cache with size 0 (disabled)
	memCache := NewCompressedTileCache(0, testLogger())
	diskCache, err := NewDiskTileCache(tmpDir, 100, testLogger())
	if err != nil {
		t.Fatalf("failed to create disk cache: %v", err)
	}

	twoTier := NewTwoTierCache(memCache, diskCache, testLogger())
	defer twoTier.Close()

	// Write tile
	twoTier.Set(7, 68, 34, []byte("test data"))
	time.Sleep(100 * time.Millisecond) // Wait for disk write

	// Should hit disk (not memory)
	data, ok := twoTier.Get(7, 68, 34)
	if !ok {
		t.Error("expected disk cache hit when memory is disabled")
	}

	if string(data) != "test data" {
		t.Errorf("wrong data: got %q, want %q", data, "test data")
	}
}

// TestTwoTierCache_EvictionCallbackWithInvalidKey tests eviction callback error handling.
func TestTwoTierCache_EvictionCallbackWithInvalidKey(t *testing.T) {
	tmpDir := t.TempDir()

	memCache := NewCompressedTileCache(2, testLogger())
	diskCache, err := NewDiskTileCache(tmpDir, 100, testLogger())
	if err != nil {
		t.Fatalf("failed to create disk cache: %v", err)
	}

	twoTier := NewTwoTierCache(memCache, diskCache, testLogger())
	defer twoTier.Close()

	// Manually trigger eviction callback with invalid key
	// This tests the error handling in the callback
	memCache.mu.Lock()
	if memCache.onEvict != nil {
		// Invalid key format (should log warning but not crash)
		memCache.onEvict("invalid-key", []byte("data"))
	}
	memCache.mu.Unlock()

	// Cache should still work normally
	twoTier.Set(7, 68, 34, []byte("test"))
	if _, ok := twoTier.Get(7, 68, 34); !ok {
		t.Error("cache should still work after invalid eviction callback")
	}
}

// TestTwoTierCache_ConcurrentReadsDuringEviction tests concurrent reads while eviction occurs.
func TestTwoTierCache_ConcurrentReadsDuringEviction(t *testing.T) {
	tmpDir := t.TempDir()

	memCache := NewCompressedTileCache(5, testLogger())
	diskCache, err := NewDiskTileCache(tmpDir, 100, testLogger())
	if err != nil {
		t.Fatalf("failed to create disk cache: %v", err)
	}

	twoTier := NewTwoTierCache(memCache, diskCache, testLogger())
	defer twoTier.Close()

	// Write initial tiles
	for i := 0; i < 3; i++ {
		twoTier.Set(7, 68, uint32(i), []byte("data"))
	}

	// Concurrent reads and writes
	done := make(chan bool)

	// Reader goroutines
	for i := 0; i < 5; i++ {
		go func() {
			for j := 0; j < 20; j++ {
				twoTier.Get(7, 68, uint32(j%3))
				time.Sleep(10 * time.Millisecond)
			}
			done <- true
		}()
	}

	// Writer goroutines (trigger eviction)
	for i := 0; i < 3; i++ {
		go func(n int) {
			for j := 0; j < 10; j++ {
				twoTier.Set(7, 68, uint32(j+n*10), []byte("data"))
				time.Sleep(10 * time.Millisecond)
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 8; i++ {
		<-done
	}

	// If we got here without deadlock or panic, test passes
}

// TestTwoTierCache_EmptyCache tests operations on empty cache.
func TestTwoTierCache_EmptyCache(t *testing.T) {
	tmpDir := t.TempDir()

	memCache := NewCompressedTileCache(10, testLogger())
	diskCache, err := NewDiskTileCache(tmpDir, 100, testLogger())
	if err != nil {
		t.Fatalf("failed to create disk cache: %v", err)
	}

	twoTier := NewTwoTierCache(memCache, diskCache, testLogger())
	defer twoTier.Close()

	// Get from empty cache
	if _, ok := twoTier.Get(7, 68, 34); ok {
		t.Error("expected cache miss on empty cache")
	}
}

// TestTwoTierCache_LargeDataPromotion tests promoting large tiles from disk to memory.
func TestTwoTierCache_LargeDataPromotion(t *testing.T) {
	tmpDir := t.TempDir()

	memCache := NewCompressedTileCache(5, testLogger())
	diskCache, err := NewDiskTileCache(tmpDir, 100, testLogger())
	if err != nil {
		t.Fatalf("failed to create disk cache: %v", err)
	}

	twoTier := NewTwoTierCache(memCache, diskCache, testLogger())
	defer twoTier.Close()

	// Large tile data (1MB)
	largeData := make([]byte, 1024*1024)
	for i := range largeData {
		largeData[i] = byte(i % 256)
	}

	// Write large tile
	twoTier.Set(7, 68, 34, largeData)
	time.Sleep(100 * time.Millisecond)

	// Clear memory
	memCache.mu.Lock()
	memCache.cache = make(map[string][]byte)
	memCache.lru = []string{}
	memCache.mu.Unlock()

	// Read from disk (should promote)
	data, ok := twoTier.Get(7, 68, 34)
	if !ok {
		t.Fatal("expected disk hit")
	}

	// Verify data integrity
	if len(data) != len(largeData) {
		t.Errorf("data size mismatch: got %d, want %d", len(data), len(largeData))
	}

	// Verify promotion to memory
	if _, ok := memCache.Get(7, 68, 34); !ok {
		t.Error("large tile should be promoted to memory")
	}
}
