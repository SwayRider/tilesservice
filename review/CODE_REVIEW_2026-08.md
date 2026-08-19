# Code Review — `tilesservice`

**Date:** 2026-08
**Scope:** Full review of `tilesservice/` — the Mapbox Vector Tile (MVT) serving service (MBTiles hierarchy, tile merging, two-tier compression cache, HTTP tile/style serving, JWT scope enforcement).
**Reviewed:** `cmd/tilesservice/main.go`, `internal/server/*` (http_tile, http_style), `internal/tileindex/*` (index), `internal/mbtiles/*` (reader, merge), `internal/tilecache/*` (tile_cache, disk_cache, two_tier_cache), all test files, `tileviewer/`, `CACHE_IMPLEMENTATION.md`, `Dockerfile`, `Makefile`, `env.example`, `local.env`, `.github/workflows/ci.yml`, and the shared `swlib` app/logger/jwt machinery.
**Verification performed:** `go build ./...`, `go vet ./...`, `go test ./... -count=1 -race`, and per-package coverage all run clean. One suspected cache bug was confirmed empirically with a throwaway reproduction test (removed afterwards; the repo is unmodified).

---

## Summary

This is a solid, well-tested service: coverage is strong across the board (`internal/tileindex` 92.7%, `internal/mbtiles` 86.3%, `internal/tilecache` 85.6%, `internal/server` 83.2%) and the suite is race-clean. The layering is clear — `tileindex` (tile→file mapping), `mbtiles` (read + merge), `tilecache` (memory/disk/two-tier), `server` (HTTP) — and the details are mostly right: MBTiles opened read-only (`mode=ro`), atomic disk writes (temp file + rename), path-traversal protection on style names, graceful shutdown that drains the write queue, and correct service-token scope enforcement. Unlike the other services, CGO is genuinely required here (SQLite), so the Dockerfile's cross-compiler block is justified.

There is one dominant, dangerous problem and a cluster of cache/correctness issues:

1. **`NewDiskTileCache` runs `os.RemoveAll(basePath)` on the configured cache path at every startup.** The "clear stale cache" logic recursively deletes *whatever directory is configured*. A single misconfigured `DISK_CACHE_PATH` (e.g. a typo pointing at a home directory, a mounted volume, or `/`) deletes that tree irreversibly. This is the most serious footgun in the service. **[Fixed — see #1]**
2. The rest: the memory "LRU" cache is not actually LRU (duplicate keys accumulate and `Get` never touches the recency order — verified) **[#2 fixed — see #2]**, the auth documentation contradicts the code in three places **[#3 fixed — see #3]**, disk-cache hits spawn an unbounded goroutine per request **[#4 fixed — see #4]**, some tile responses are missing `Vary: Accept-Encoding` (a shared-cache correctness bug) **[#5 fixed — see #5]**, tile x/y coordinates are never range-checked (a uint32 underflow in the TMS conversion) **[#6 fixed — see #6]**, and the JWT public-key cache refreshes only hourly with no retry on failure **[#7 fixed — see #7]**.

---

## High

### 1. `NewDiskTileCache` deletes its configured directory at startup — catastrophic data loss on misconfiguration
`internal/tilecache/disk_cache.go:50-57`

**Status: ✅ Fixed (2026-08).** `NewDiskTileCache` now guards startup clearing: it rejects obviously-wrong paths (empty, `/`, home dir), claims the cache directory with a hidden `.tilesservice_cache_owner` marker file, and clears only known cache artifacts (`z<zoom>/` subdirs, `metadata.db` + SQLite sidecars). A directory without the marker that contains unrelated files is refused — `main.go` falls back to memory-only and nothing is deleted; empty and cache-shaped pre-marker directories are adopted and claimed. Existing clear-on-startup tests pass unchanged; new tests cover marker ownership, refusal, adoption, and scoped clearing.

```go
if err := os.MkdirAll(basePath, 0755); err != nil { ... }
l.Infoln("clearing existing cache directory")
if err := os.RemoveAll(basePath); err != nil && !os.IsNotExist(err) { ... }
if err := os.MkdirAll(basePath, 0755); err != nil { ... }
```

The intent (clear stale tiles so regenerated MBTiles aren't shadowed by the cache) is reasonable, but the implementation recursively deletes **the entire configured path** on every startup. There is no guard, no marker file, and no check that the path actually looks like a cache directory. If `DISK_CACHE_PATH` is ever misconfigured (typo, a parent directory, a mount point, or `/`), the service silently `RemoveAll`s it — irrecoverable data loss at boot, with only an `Infoln("clearing existing cache directory")` to show for it. This is amplified by the fact that the code runs `MkdirAll` *first*, so even a non-existent-but-valid-looking path is created and then wiped.

The test suite (`TestDiskTileCache_ClearOnStartup`, `TestDiskTileCache_ClearPreventsStaleTiles`) actively codifies this behavior as correct, so the risk is invisible to the tests.

Fix direction: only remove known cache subdirectories (e.g. `z0`…`z16`), or track cache ownership with a marker file and refuse to `RemoveAll` a directory that isn't clearly the cache (e.g. requires the marker to be present). Add a path-sanity guard before deleting.

---

## Medium

### 2. The memory cache's "LRU" is not LRU — duplicate keys accumulate and recency is never tracked
`internal/tilecache/tile_cache.go:108-160`

**Status: ✅ Fixed (2026-08).** The memory cache now implements a true LRU with `container/list`: `lru` is the recency order (front = MRU, back = LRU) and a new `pos` map indexes key → list element. `Set` dedupes (existing key → update data + promote to MRU; no duplicate entries), `Get` touches on hit (promotes to MRU), and the eviction worker pops the LRU tail in O(1). A defensive guard prevents the `onEvict` callback from ever receiving nil data, so stale-duplicate evictions can no longer cascade empty tiles to the disk tier. Invariant `len(cache) == lru.Len() == len(pos)` is documented on the struct and asserted by `TestCompressedTileCache_LRUSync`; new tests also cover recency promotion (`TestCompressedTileCache_GetPromotesRecency`) and nil-free eviction (`TestCompressedTileCache_NoNilEviction`). The `memCache.lru` references in `two_tier_cache_test.go` were updated for the new type (resets now also clear `pos`).

Two defects:
- **`Set` does not remove an existing key before appending.** Setting the same tile twice produces two entries in `lru` for one entry in `cache`.
- **`Get` never updates `lru`**, so a hit does not promote the tile to most-recently-used.

```go
func (c *CompressedTileCache) Set(...) {
    ...
    c.cache[key] = data
    c.lru = append(c.lru, key)   // no dedup — repeated Set appends duplicates
}
```

**Confirmed empirically** (throwaway test): after `Set(k,v1); Set(k,v2); Set(k2,..); Set(k3,..)` the cache held 3 entries but `lru` held 4. Consequences:
- `lru` grows without bound with stale duplicate keys (memory growth unrelated to `maxSize`).
- The eviction worker later evicts a stale key whose `cache` entry is already gone, retrieving `nil` and passing `nil` to the `onEvict` callback — so the two-tier cache can write **nil** tile data to the disk cache.
- Eviction order is effectively FIFO-with-duplicates, not LRU, so frequently-hit tiles are not protected from eviction as the name/comments promise.

Fix direction: dedupe in `Set` (remove the key from `lru` before re-appending, or use a map + doubly-linked list / container/list), and touch the key in `Get`. Add a regression test that asserts `len(lru) == len(cache)` after repeated sets.

### 3. Auth documentation contradicts the code in three places
`cmd/tilesservice/main.go:8,132`, `tilesservice/README.md:82`

**Status: ✅ Fixed (2026-08).** All stale "public / no-auth" statements now match the enforcement: the `main.go` package doc (rewritten endpoint list), the `requireTilesAuth` middleware comment, the two route comments, and the README API Reference (intro + the three endpoint Access lines) now state that only service client tokens with `tiles:serve` are accepted and user JWTs are rejected with 403. Added `cmd/tilesservice/main_test.go` with `TestRequireTilesAuth` (8 cases: no header → 401, non-Bearer → 401, malformed token → 401, valid service token with scope → 200, wrong scope → 403, user JWT → 403, token signed with unknown key → 401, empty key cache → 503). `cmd/tilesservice` coverage went from 0% to 18.6%.

The enforcement is correct — the `requireTilesAuth` middleware requires a JWT that decodes to `*jwt.SwayRiderServiceClaims` and carries the `tiles:serve` scope, and the README's Authorization table says the same. But three other places were never updated and now claim the opposite:

- `main.go:8` package doc: *"All endpoints are public (no authentication required)"*.
- `main.go:132` `requireTilesAuth` comment: *"Regular user JWTs are accepted as-is"* — the code actually **rejects** user JWTs (the `svcClaims, ok := claims.SwayRiderClaims.(*jwt.SwayRiderServiceClaims)` type assertion fails for user claims → 403).
- `README.md:82` API Reference: *"All endpoints are public and require no authentication."*

Anyone reading these docs will misjudge the security posture. Fix direction: update the package doc, the middleware comment, and the README API Reference to match the actual `tiles:serve` service-token requirement, and add an auth-enforcement test (there is none — see coverage gaps).

### 4. Disk-cache `Get` spawns a goroutine per hit
`internal/tilecache/disk_cache.go:124`

**Status: ✅ Fixed (2026-08).** `Get` no longer spawns a goroutine per hit. A dedicated, `wg`-tracked `accessWorker` (coalescing-worker design) consumes hits from a bounded `accessQueue`, drains them into a `map[string]int64` batch — repeated hits to the same tile collapse to a single UPDATE — and commits each batch in one SQLite transaction via `applyAccessBatch`. Queue-full sends are dropped (best-effort, as before), shutdown drains through the existing `wg.Wait` in `Close` (no `Close` changes needed), and `updateAccessTime` was removed. New tests: `TestDiskTileCache_AccessBurstBound` (10k Gets keep the goroutine count bounded) and `TestDiskTileCache_AccessBatchUpdates` (all hit tiles' access times advance, duplicates coalesce).

```go
// Update access time asynchronously (best-effort)
go c.updateAccessTime(z, x, y)
```

Every disk-cache hit launches an unbounded, untracked goroutine that immediately contends on `c.mu.Lock()` to run a SQLite `UPDATE`. Under sustained load (disk cache enabled, high hit rate) this is a goroutine-explosion and lock-contention hazard, and the goroutines are not tied to `c.wg` so they can outlive `Close`. Fix direction: funnel access-time updates through a dedicated worker/queue (like the write path) or batch/debounce them.

### 5. Missing `Vary: Accept-Encoding` on some tile responses
`internal/server/http_tile.go:120-150`

**Status: ✅ Fixed (2026-08).** `ServeHTTP` now sets `Vary: Accept-Encoding` once in the common-headers block, which covers the pre-compressed, uncompressed, cache-hit, and compress-on-the-fly branches; the two per-branch sets were removed. The 204 no-content path also carries `Vary` so every tile response is uniform. Tests now assert `Vary` on the pre-compressed, uncompressed, and cache-hit branches.

`ServeHTTP` sets `Vary: Accept-Encoding` on the cache-hit and compress-on-the-fly branches (`:159`, `:189`) but **not** on the pre-compressed branch (`:130`) or the uncompressed branch (`:145`). All responses carry `Cache-Control: public, max-age=86400` (24h). Because the pre-compressed branch emits `Content-Encoding: gzip` without a `Vary`, a shared cache (CDN/browser) can store the gzip payload and serve it to a client that did not advertise gzip support, which cannot decode it — broken tiles. Fix direction: set `Vary: Accept-Encoding` on every tile response path.

### 6. No validation of tile x/y range — uint32 underflow in the TMS conversion
`internal/server/http_tile.go:95`, `internal/mbtiles/reader.go:73`

**Status: ✅ Fixed (2026-08).** The handler rejects `x`/`y` outside `[0, 2^z)` with 400 before any tile work, and `Reader.GetTile` defensively returns `ErrTileNotFound` for out-of-range coordinates before the XYZ→TMS flip (`z >= 32` is guarded too, because `1<<z` wraps to 0 there). New handler tests cover out-of-range x/y and the max valid coordinate (accepted); new reader tests cover x/y/z underflow; `TestTileHandler_TileNotFound` now uses an in-range-but-missing tile (its old `0/1/0` request would be a 400 under the new rule).

The handler validates `z ≤ 16` but never checks that `x, y < 2^z`. A request like `/v1/tiles/base/16/999999999/999999999` flows into `Reader.GetTile`:

```go
tmsY := (1 << z) - 1 - y   // uint32 arithmetic wraps for y > 2^z - 1
```

The subtraction underflows (uint32 wraparound), producing a garbage TMS row, and `TileIndex.GetTile` computes a garbage grid filename from the out-of-range coordinates (via `tileCornerToLatLon`/`snapToGrid`). The result is inconsistent behavior — a 500 "Failed to retrieve tile" when the (absurd) grid file doesn't exist, or a 204 — and wasted work on every such request. A malicious or buggy client can send arbitrary x/y at no cost. Fix direction: reject `x`/`y` outside `[0, 2^z)` in the handler (or in `GetTile`), and/or guard the TMS conversion.

### 7. JWT public keys refreshed only hourly, with no retry on failure
`cmd/tilesservice/main.go:338-343`

**Status: ✅ Fixed (2026-08).** The hand-rolled hourly ticker was replaced by the shared `swlib/jwtkeys.Cache` — the same cache every other service uses. The refresh interval is configurable (`JWT_KEYS_REFRESH_INTERVAL_SECS`, default 300s) with a per-fetch timeout (`JWT_KEYS_FETCH_TIMEOUT_SECS`, default 15s), the last known-good keys are retained across failures, the loop is panic-safe, and the first refresh runs before serving starts. The authservice client is wired via `WithServiceClients` and the refresh loop via `app.JWTKeysInitializer`/`app.JWTKeysFetcher`; `requireTilesAuth` verifies through a `KeyCache` interface (`Keys()`/`Verify()`) satisfied by `*jwtkeys.Cache`, and the middleware tests now use a `fakeKeyCache` double instead of poking the old global.

```go
refreshJWTKeys(authClt, lg)                 // once at startup
go func() {
    t := time.NewTicker(time.Hour)
    defer t.Stop()
    for range t.C { refreshJWTKeys(authClt, lg) }
}()
```

If authservice is unreachable at startup, the key cache stays empty and `requireTilesAuth` returns `503 Service Unavailable` for **every** protected request until the next hourly tick — the tile service is effectively hard-down for up to an hour, fully coupled to authservice availability at boot. There is no backoff, no shorter retry, and no attempt to re-fetch when the cache is found empty at request time. Fix direction: retry with backoff on failure, refresh more frequently, and/or fall back to fetching on-demand when `len(keys) == 0` at request time.

---

## Low

### 8. `tileToLatLon` is dead production code
`internal/tileindex/index.go:278`

**Status: ✅ Fixed (2026-08).** `tileToLatLon` removed entirely — it was referenced only from tests, and the production path computes the southwest corner as `tileCornerToLatLon(z, x, y+1)` in `getOverlappingFilePaths`. Its test was removed and the `tileCornerToLatLon` doc comment updated.

`tileToLatLon` (the "southwest corner" helper) is referenced only from tests; the production path uses `tileCornerToLatLon` in `getOverlappingFilePaths`. Either wire it in or remove it.

### 9. README documents a 4-layer structure the code doesn't implement
`tilesservice/README.md` (Tile Layers table + directory example)

**Status: ✅ Fixed (2026-08).** README aligned with the 3-layer code: the directory example and Tile Layers table no longer show `L3` (zoom 14–16) — `L2` now covers zoom 11–16 ("all roads, unsimplified") — and the File Naming section mentions only `L1` and `L2`.

The README shows `L0`–`L3` (with L3 covering zoom 14–16), but `index.go`'s `Layer` enum has only `LayerL0`/`LayerL1`/`LayerL2`, and `zoomToLayer` maps every zoom > 10 to `LayerL2`. Any `L3/` directory produced by the pipeline would be silently ignored (tiles at z14–16 are looked up in `L2/`). Align the README with the code, or add L3 support.

### 10. `MergeTiles` doesn't validate layer Version/Extent consistency
`internal/mbtiles/merge.go`

**Status: ✅ Fixed (2026-08).** `MergeTiles` now returns an error when same-named layers disagree on `Version` or `Extent` ("cannot merge layer …: version/extent mismatch") instead of concatenating features at inconsistent scales. New tests cover both an extent mismatch and a version mismatch.

When concatenating same-named layers from adjacent grid cells, the merge keeps the first layer's `Version`/`Extent` and blindly appends features from the others. If two files use different `Extent` values, the merged feature coordinates are in inconsistent scales and the tile is corrupted. Features crossing a grid boundary may also be duplicated (depends on the pipeline). Consider validating `Version`/`Extent` match (or reprojecting), and add a mismatched-extent test.

### 11. `listStyles` advertises `light`/`dark` that may not exist
`internal/server/http_style.go:165-175`

**Status: ✅ Fixed (2026-08).** `listStyles` now advertises only styles that actually exist on disk — an empty or unconfigured `STYLES_PATH` yields an empty list instead of names that 404. The `light` and `dark` styles ship in the image (`assets/map/styles/`, copied by the Dockerfile), so they remain the default always-present styles; the existing `dark.json` is a faithful 82-layer dark variant of `light.json` (kept as-is).

`listStyles` always returns `light` and `dark` as available, even when no such files exist — so a client that follows the listing gets a 404 on `GET /v1/tiles/styles/light`. Either only advertise files that exist (with the defaults conditional on presence) or document the discrepancy.

### 12. Style files re-read and re-parsed as templates on every request
`internal/server/http_style.go:135-150`

**Status: ✅ Fixed (2026-08).** Parsed style templates are cached per style name with file-mtime invalidation: a request stats the file and reuses the cached parsed template when it is unchanged, so a file is only re-read and re-parsed when it changes on disk. A test verifies an updated file (bumped mtime) is picked up on the next request.

`handleGetStyle` reads and `template.Parse`s the style JSON on each request. Styles are small and low-traffic, so this is minor, but a parsed-template cache (with an mtime check) would avoid the redundant work.

### 13. `CompressedTileCache.Close` is not idempotent
`internal/tilecache/tile_cache.go:172-176`

**Status: ✅ Fixed (2026-08).** `Close` is now guarded by `sync.Once`, so a second call is a no-op instead of panicking on the closed `stopCh` — matching `DiskTileCache.Close`. A double-close regression test was added.

`Close` does `close(c.stopCh)` with no guard; a second `Close` panics on a closed channel. `DiskTileCache.Close` correctly guards against double-close; the memory cache should too.

### 14. `tileset` path segment is parsed but ignored
`internal/server/http_tile.go:75`

**Status: ✅ Fixed (2026-08).** Documented in the README Get Tile section: the `{tileset}` segment is accepted but currently ignored (single tileset, reserved for future multi-tileset support).

`// tileset := parts[0] // Currently unused, reserved for future multi-tileset support` — the URL segment is accepted and discarded. Fine as a forward-compat placeholder, but worth a comment in the README (the docs already show a `{tileset}` path param, so this is consistent).

### 15. Dockerfile hardening (with one justified difference)
`Dockerfile`

**Status: ✅ Fixed (2026-08).** Builder pinned to `golang:1.26-bookworm` and the runtime stage pinned to the dated `debian:bookworm-20260805-slim` (both reproducible); a `.dockerignore` excludes `.git`, `.gitignore`, `.DS_Store`, `*.md`, `local.env`, `.github`, and `tileviewer/`; a curl-based healthcheck was added. The CGO/cross-gcc block is kept — justified by the go-sqlite3 dependency.

- `FROM golang:latest` and `FROM debian:bookworm-slim` — unpinned, mutable base tags; builds are not reproducible.
- `COPY . .` with **no `.dockerignore`** — the build context ships `.git/`, `local.env` (machine-specific paths), `.DS_Store`, and `tileviewer/`. Add a `.dockerignore`.
- Unlike the other services, the CGO/cross-gcc block is **legitimate** here (`github.com/mattn/go-sqlite3` requires cgo), so keep it — but pin the builder base image.

### 16. `CACHE_IMPLEMENTATION.md` references stale paths
`CACHE_IMPLEMENTATION.md`

**Status: ✅ Fixed (2026-08).** All path references and test commands updated from the old `backend/services/tilesservice/internal/server` layout to the real one (`internal/tilecache/`, `internal/server/`, `cmd/tilesservice/`, `go.mod`).

The doc points to `backend/services/tilesservice/internal/server/{tile_cache,disk_cache,two_tier_cache}.go`, but the code now lives under `internal/tilecache/`. Minor doc drift.

---

## Positive observations

- **Strong test coverage and race-clean** — tileindex 92.7%, mbtiles 86.3%, tilecache 85.6%, server 83.2%; `go test -race` green. The MBTiles merge, grid-boundary mapping, and cache-eviction paths are genuinely exercised.
- **Correct auth enforcement** — the middleware genuinely requires a service token with the `tiles:serve` scope (only the surrounding *documentation* is stale, see #3).
- **Clean layering** — `tileindex` (tile→file mapping), `mbtiles` (read/merge), `tilecache` (memory/disk/two-tier), `server` (HTTP) with small, readable files and clear interfaces (`TileCache`).
- **Safety-conscious details** — MBTiles opened `mode=ro`; atomic disk writes (temp + rename); `validStyleName` regex blocks path traversal; graceful shutdown drains the write queue before closing; CORS configured for tile serving.
- **Correct XYZ↔TMS conversion** in the reader, and a well-documented grid/file naming convention in `index.go`.
- **CGO justified** — the SQLite dependency means the Dockerfile's cross-compiler setup is necessary here (a contrast to `routerservice`/`searchservice`, where it's needless).
- **Build/vet/tests clean** — all green.

---

## Test-coverage gaps

Measured with `go test -cover`:

| Package | Coverage | Notes |
| ------- | -------- | ----- |
| `internal/tileindex` | 92.3% | Good — grid mapping and merge paths covered. |
| `internal/mbtiles` | 87.0% | Good. |
| `internal/tilecache` | 82.7% | Good. |
| `internal/server` | 81.5% | Tile/style handlers covered; auth middleware lives in `cmd/tilesservice`. |
| `cmd/tilesservice` | 76.6% | Auth middleware, key refresh, and the full startup/shutdown lifecycle tested; only `main()` (the `Run()` entry point) is untested. |

**Status: ✅ All gaps closed (2026-08).**

Specific gaps:

- ✅ **Auth-enforcement tests** — `TestRequireTilesAuth` (8 cases: no/malformed header → 401, wrong scope → 403, user JWT → 403, unknown key → 401, empty key cache → 503) added with fix #3.
- ✅ **Memory-cache duplicate-key test** — `TestCompressedTileCache_LRUSync` (plus recency-promotion and nil-free-eviction tests) added with fix #2.
- ✅ **Out-of-range x/y tests** — handler tests `TestTileHandler_OutOfRangeCoords` / `TestTileHandler_MaxValidCoordAccepted` and the reader out-of-range subtest added with fix #6.
- ✅ **`Vary` tests** — the pre-compressed, uncompressed, and cache-hit branches assert `Vary: Accept-Encoding` (fix #5).
- ✅ **Authservice-down-at-boot test** — the empty-key → 503 path is covered by `TestRequireTilesAuth` and end-to-end through the real server in `TestServerLifecycle`.
- ✅ **Startup/shutdown tests** — `TestServerLifecycle` drives config parse, `initializeTileIndex`, `startHTTPServer`/`stopHTTPServer` (start, graceful stop, no-op stop when not running), the public ping endpoint, and the 503-while-no-keys behavior (with the disk cache enabled); `TestInitializeTileIndex_EmptyPath` covers the unconfigured-TILES_PATH branch. `main.go` was refactored to expose `newApp()` so tests can build the app without running it. `cmd/tilesservice` coverage went from 0% to 76.6%.

---

## Recommended fix order

1. **#1 (high)** — stop `os.RemoveAll`-ing the configured path; scope the cache clear to known cache subdirectories and add a marker-file/path-sanity guard. This is the data-loss footgun. **DONE (2026-08).**
2. **#2 (medium)** — fix the memory cache LRU (dedupe on `Set`, touch on `Get`) and add a regression test asserting `lru`/`cache` stay in sync. **DONE (2026-08).**
3. **#3 (medium)** — correct the three stale "public/no-auth" statements (main.go doc, middleware comment, README) and add an auth-enforcement test. **DONE (2026-08).**
4. **#4 (medium)** — replace the goroutine-per-hit access-time update with a worker/queue. **DONE (2026-08).**
5. **#5 (medium)** — set `Vary: Accept-Encoding` on all tile responses (and test the pre-compressed/uncompressed branches). **DONE (2026-08).**
6. **#6 (medium)** — validate `x,y < 2^z` and guard the TMS conversion against underflow. **DONE (2026-08).**
7. **#7 (medium)** — add retry/backoff (or on-demand fetch) to JWT key refresh. **DONE (2026-08).**
8. **#8–#16 (low)** — remove dead code, align the README's 4-layer docs with the 3-layer implementation, add `.dockerignore`, pin base images, and fix the remaining nits. **DONE (2026-08).**

Items #1 and #2 are the priority: #1 is a catastrophic-data-loss footgun hidden in startup code with 0% coverage, and #2 is a correctness/memory-growth bug in the cache that sits on every tile request's hot path.
