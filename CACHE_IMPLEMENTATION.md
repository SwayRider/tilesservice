# Two-Tier Disk Cache Implementation

## Overview

The tilesservice now includes a two-tier caching system with memory (L1) and disk (L2) layers for compressed vector tiles.

## Architecture

```
┌─────────────────────────────────────────┐
│         HTTP Handler                     │
│              ↓                           │
│       TileCache Interface                 │
└─────────────────────────────────────────┘
                ↓
    ┌───────────┴──────────────┐
    │   TwoTierCache            │
    │   (Coordinator)           │
    └───────────┬──────────────┘
                ↓
    ┌───────────┴──────────────┐
    │                           │
┌───▼────┐              ┌──────▼────┐
│ Memory │◄─Promotion───│   Disk    │
│ Cache  │              │   Cache   │
│ (LRU)  │──Eviction───►│   (LRU)   │
└────────┘              └───────────┘
```

## Components

### 1. TileCache Interface
**File**: `backend/services/tilesservice/internal/server/tile_cache.go`

- Defines common interface for cache implementations
- Methods: `Get()`, `Set()`, `Close()`
- Allows transparent switching between single-tier and two-tier caching

### 2. CompressedTileCache (Memory Cache - L1)
**File**: `backend/services/tilesservice/internal/server/tile_cache.go`

- **Storage**: In-memory map
- **Capacity**: Configurable (default: 1000 tiles)
- **Eviction**: Background LRU worker (runs every 1 second)
- **Latency**: ~10 μs
- **Features**:
  - Non-blocking Set() operations
  - Eviction callback support for cascading to L2
  - Soft limit enforcement (allows temporary overage)

### 3. DiskTileCache (Disk Cache - L2)
**File**: `backend/services/tilesservice/internal/server/disk_cache.go`

- **Storage**: Hierarchical directory structure (`z{z}/{x}/{y}.mvt`)
- **Metadata**: SQLite database for LRU tracking
- **Capacity**: Configurable (default: 100,000 files)
- **Eviction**: Background worker (runs every 5 seconds)
- **Latency**: ~100-500 μs (depends on disk type)
- **Features**:
  - Async write queue (1000-tile buffer)
  - Atomic file writes (temp file + rename)
  - Automatic directory creation
  - Access time tracking for LRU
  - Graceful shutdown with pending write completion

### 4. TwoTierCache (Coordinator)
**File**: `backend/services/tilesservice/internal/server/two_tier_cache.go`

- **Role**: Coordinates between memory and disk layers
- **Features**:
  - Automatic promotion of disk hits to memory
  - Eviction cascade: L1 eviction → L2 write
  - Write-through to both layers on Set()
  - Graceful degradation if disk unavailable

## Cache Flow

### Read Path
```
1. Request arrives for tile z/x/y
2. Check memory cache (L1)
   ├─ Hit  → Return tile (fast path)
   └─ Miss → Continue to step 3
3. Check disk cache (L2)
   ├─ Hit  → Promote to memory + Return tile
   └─ Miss → Continue to step 4
4. Compress from MBTiles
5. Cache in both L1 and L2
6. Return tile
```

### Write Path
```
Set(z, x, y, data):
1. Write to memory (L1) - synchronous
2. Queue disk write (L2) - asynchronous
```

### Eviction Path
```
Memory Eviction (L1 → L2):
1. Background worker checks size every 1 second
2. If over limit, evict oldest tiles
3. Eviction callback writes tile to L2
4. Tile preserved in disk cache

Disk Eviction (L2 → Gone):
1. Background worker checks size every 5 seconds
2. If over limit, evict oldest files
3. Delete file and metadata
4. Tile must be re-compressed on next request
```

## Configuration

### Environment Variables

```bash
# Memory Cache (existing)
COMPRESSION_ENABLED=true          # Enable compression (default: true)
COMPRESSION_CACHE_SIZE=1000       # Max tiles in memory (default: 1000)

# Disk Cache (NEW)
DISK_CACHE_ENABLED=false          # Enable disk cache (default: false)
DISK_CACHE_PATH=""                # Path to cache directory (required if enabled)
DISK_CACHE_MAX_FILES=100000       # Max files on disk (default: 100,000)
```

### CLI Flags

```bash
-compression-enabled              # Enable compression
-compression-cache-size int       # Memory cache size
-disk-cache-enabled              # Enable disk cache
-disk-cache-path string          # Disk cache directory
-disk-cache-max-files int        # Max cached files
```

### Example Configurations

**Development (Memory Only)**:
```bash
export COMPRESSION_ENABLED=true
export COMPRESSION_CACHE_SIZE=1000
export DISK_CACHE_ENABLED=false
```

**Production (Two-Tier)**:
```bash
export COMPRESSION_ENABLED=true
export COMPRESSION_CACHE_SIZE=5000          # ~50-60 MB in memory
export DISK_CACHE_ENABLED=true
export DISK_CACHE_PATH=/var/cache/tilesservice
export DISK_CACHE_MAX_FILES=500000          # ~5 GB on disk
```

## Performance Characteristics

### Latencies
- **Memory hit**: ~10 μs
- **Disk hit + promotion**: ~100-500 μs
- **Cache miss + compression**: ~5-50 ms
- **Set() operation**: ~1-5 μs (non-blocking)

### Storage Requirements
- **Memory**: ~10-15 MB per 1000 tiles
- **Disk**: ~10 KB average per tile
  - 100k files: ~1 GB
  - 500k files: ~5 GB
  - 1M files: ~10 GB

### Expected Cache Hit Rates
- **Hot tiles**: 95%+ memory hits
- **Warm tiles**: 80%+ disk hits
- **Cold tiles**: Compressed from source

## File Structure

### New Files
1. `backend/services/tilesservice/internal/server/disk_cache.go` - Disk cache implementation
2. `backend/services/tilesservice/internal/server/two_tier_cache.go` - Cache coordinator
3. `backend/services/tilesservice/internal/server/disk_cache_test.go` - Disk cache tests
4. `backend/services/tilesservice/internal/server/two_tier_cache_test.go` - Two-tier cache tests

### Modified Files
1. `backend/services/tilesservice/internal/server/tile_cache.go` - Added interface and background eviction
2. `backend/services/tilesservice/internal/server/http_tile.go` - Uses TileCache interface
3. `backend/services/tilesservice/cmd/tilesservice/main.go` - Configuration and initialization
4. `backend/go.mod` - Added SQLite dependency

## Testing

### Run All Cache Tests
```bash
go test ./services/tilesservice/internal/server/... -v
```

### Run Specific Test Suites
```bash
go test ./services/tilesservice/internal/server/... -v -run TestDisk
go test ./services/tilesservice/internal/server/... -v -run TestTwoTier
```

### Test Coverage
- ✅ Basic read/write operations
- ✅ Cache misses
- ✅ Directory creation
- ✅ Metadata consistency
- ✅ LRU eviction
- ✅ Concurrent access
- ✅ Memory hit fast path
- ✅ Disk hit promotion
- ✅ Eviction cascade (L1 → L2)
- ✅ Graceful degradation

## Deployment

### Backward Compatibility
- ✅ No breaking changes
- ✅ Disk cache disabled by default
- ✅ Existing deployments continue working
- ✅ No API or HTTP header changes

### Rollout Strategy
1. **Development**: Test with small cache sizes
2. **Staging**: Enable with conservative limits, monitor
3. **Production**: Gradually increase cache sizes based on metrics

## Monitoring

### Key Metrics to Track
- Memory cache size and hit rate
- Disk cache size and hit rate
- Eviction frequency
- Disk space usage
- Request latencies (p50, p95, p99)

### Log Messages
- `disk cache initialized` - Startup
- `disk cache enabled at {path}` - Configuration
- `promoted tile from disk to memory` - L2 → L1 promotion
- `evicted tile to disk` - L1 → L2 cascade
- `evicted N tiles` - Batch eviction stats
- `disk cache closed` - Shutdown

## Troubleshooting

### Disk Full
**Symptom**: Write errors in logs
**Solution**: Increase disk space or reduce `DISK_CACHE_MAX_FILES`

### SQLite Locked
**Symptom**: "database is locked" warnings
**Impact**: Minor, writes are retried
**Solution**: Normal under high concurrency

### Slow Tile Serving
**Symptom**: High latencies
**Check**: Cache hit rates, disk I/O
**Solution**: Increase cache sizes or switch to faster disk

## Future Enhancements

Potential improvements:
- [ ] Cache warming on startup
- [ ] Compression level tuning per zoom level
- [ ] Cache statistics endpoint
- [ ] Prometheus metrics export
- [ ] Periodic consistency checks
- [ ] TTL-based expiration
