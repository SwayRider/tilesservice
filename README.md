# tilesservice

Vector tile serving service for the SwayRider platform. Serves Mapbox Vector Tiles (MVT) for map rendering at multiple zoom levels with hierarchical tile organization.

## Architecture

The tilesservice exposes an HTTP API for tile serving:

| Interface | Port | Purpose |
| --------- | ---- | ------- |
| HTTP | 8080 | REST API for tiles and styles |

### Tile Storage

> **Migration in progress:** MBTiles storage (below) is being replaced by a single Protomaps planet PMTiles release read from the object store (or `TILES_ROOT/current` with the local-file backend; with styles, fonts and sprites), with `base` (MBTiles) and `planet` (PMTiles) tilesets served side-by-side during transition. The planet release is read from an S3-compatible object store (Garage) with ranged GETs; a local-file backend stays for tests and laptops. See [`Docs/MIGRATION-DATA-MANAGER.md`](../Docs/MIGRATION-DATA-MANAGER.md) and `data-manager/TILESSERVICE-PMTILES.md`. This README is rewritten with the code change.

### Planet tileset (PMTiles)

`GET /v1/tiles/planet/{z}/{x}/{y}` is served from a single [PMTiles v3](https://github.com/protomaps/PMTiles) archive (Protomaps basemap, gzip MVT, z0–15) found in one of two ways. Every other tileset name still uses the legacy MBTiles index (`TILES_PATH`).

- **A release store (`TILES_ROOT`, the normal setup):** a directory (`/data/tiles` or `file:///data/tiles`) holding a `current` symlink to the active release directory, or an S3-compatible bucket (`s3://bucket[/prefix]`, ranged GETs; `S3_ENDPOINT`, `S3_REGION`, `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY`, use the read-only key) holding `current.json`. A release is a `manifest.json` plus the archive (and, later, styles, glyphs and sprites); the layout is the contract in `data-manager/SERVICES.md`. The service switches to a new release by itself, see below.
- **A single archive (`PMTILES_URL`):** a local file (`file:///path/tiles.pmtiles`) or `s3://bucket/key`, opened once at startup (a new archive needs a restart). Ignored, with a warning, when `TILES_ROOT` is set.

#### Releases and reload

```text
<TILES_ROOT>/current -> releases/<id>          (file store: a relative symlink)
<bucket>/current.json  {"schema":1,"release":"<id>","prefix":"releases/<id>/","updated_at":"…"}   (S3 store)
```

- The service checks the pointer every `RELOAD_POLL_SECONDS` (default 10; `0` = only on `SIGHUP`) and on `SIGHUP`. When it names a release it has not loaded, the service reads `manifest.json`, validates it, opens the archive and then switches. Requests that already hold the old release finish on it, which is closed after its last request; new requests see the new one. No request fails during a switch.
- A release that fails to load (missing or invalid manifest, missing or corrupt archive, unreachable store) is **not** switched to: the service keeps serving the current release and reports the error in `/v1/tiles/ready` (`last_error`) and the log. Pointing `current` back (rollback) or fixing the release clears it.
- Without any loadable release at startup the service still starts: `planet` answers `503` and `/ready` answers `503` until a release loads; the legacy tilesets are unaffected.
- `manifest.json` (schema 1) is validated: tileset name `planet`, release id and style ids/versions from a safe character set, and every path in it stays inside the release (no absolute paths, `..` or backslashes).

- **Zoom range** comes from the archive header: tiles outside it, and missing tiles, answer `204`; invalid coordinates `400`; `503` when the planet archive is not configured or could not be opened (see the startup log).
- **Compression:** stored gzip tiles are sent as they are to clients that accept gzip, and decoded for those that do not; an archive with uncompressed tiles is gzipped on demand. Brotli/zstd archives and non-MVT archives are refused at startup.
- **Caching:** weak `ETag` (archive id + coordinates, `If-None-Match` answers `304`), `Cache-Control: public, max-age=86400`.
- **Reader:** `internal/pmtiles` reads through an `io.ReaderAt` (file, or `internal/objstore` for S3: ranged GETs signed with SigV4, no SDK dependency); leaf directories are cached in memory.
- Archives are validated at open (header, section bounds against the file size, so a truncated upload is refused). `tiles.json`, manifest-driven styles, glyphs and sprites follow in later PRs (see `data-manager/TILESSERVICE-PMTILES.md`).

**Check an archive or the object store credentials without the gateway:** `go run ./cmd/pmtiles-probe -url s3://swayrider-tiles/releases/r-test-1/tiles.pmtiles [-tile 12/2097/1373 -out tile.gz]` opens it with the same code and `S3_*` environment variables as the service and prints header, bounds, layers and (optionally) one tile.

A small test archive: `pmtiles extract https://build.protomaps.com/<build>.pmtiles brussels.pmtiles --bbox=4.2,50.75,4.55,50.95`.

The service reads tiles from MBTiles files organized in a hierarchical structure based on zoom levels and geographic regions.

### Caching

The service implements optional two-tier caching:

1. **Memory cache**: Compressed tiles cached in memory
2. **Disk cache**: Persistent file-based cache for tile data

## Authorization

tilesservice only accepts **service client tokens** with the `tiles:serve` scope. Direct user JWT access is not permitted — all client requests must go through swayrider-api, which injects its own service token.

| HTTP endpoint | Access |
|---|---|
| `GET /v1/tiles/ping` | Public — no token required |
| `GET /v1/tiles/ready` | Public — no token required |
| `GET /v1/tiles/styles` | Service client token with `tiles:serve` scope |
| `GET /v1/tiles/styles/{name}` | Service client token with `tiles:serve` scope |
| `GET /v1/tiles/styles/{id}/{variant}`, `GET /v1/tiles/styles/{id}/{version}/{variant}` | Service client token with `tiles:serve` scope |
| `GET /v1/tiles/{tileset}/{z}/{x}/{y}` | Service client token with `tiles:serve` scope |

AUTHSERVICE_HOST and AUTHSERVICE_PORT must be configured so the service can fetch JWT public keys for token validation. Keys are fetched once at startup and then refreshed in the background (default every 300s — see JWT Key Refresh below); a failed refresh never clears the last known-good keys, and each fetch is bounded by a timeout.

---

## Configuration

Configuration is provided via environment variables or CLI flags. See `env.example` for the full configuration.

### Server Configuration

| Environment Variable | CLI Flag | Default | Description |
| -------------------- | -------- | ------- | ----------- |
| `HTTP_PORT` | `-http-port` | 8080 | HTTP API port |
| `LOG_LEVEL` | `-log-level` | info | Logging level (debug, info, warn, error) |

### Tiles Configuration

| Environment Variable | CLI Flag | Default | Description |
| -------------------- | -------- | ------- | ----------- |
| `TILES_PATH` | `-tiles-path` | | Path to the tiles storage directory (required) |
| `STYLES_PATH` | `-styles-path` | | Path to the map styles directory |
| `TILES_ROOT` | `-tiles-root` | | Release store of the `planet` tileset: a directory with a `current` symlink (or `file:///dir`), or `s3://bucket[/prefix]` with `current.json`. Takes precedence over `PMTILES_URL` |
| `RELOAD_POLL_SECONDS` | `-reload-poll-seconds` | 10 | Seconds between checks of the release pointer (`0` = only on `SIGHUP`) |
| `PMTILES_URL` | `-pmtiles-url` | | A single planet archive (`file:///…` or `s3://bucket/key`), opened once at startup |
| `S3_ENDPOINT`, `S3_REGION`, `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY` | `-s3-…` | region `garage` | Object store for `s3://` locations (use the read-only key; set the secret via the environment) |

### Compression Settings

| Environment Variable | CLI Flag | Default | Description |
| -------------------- | -------- | ------- | ----------- |
| `COMPRESSION_ENABLED` | `-compression-enabled` | true | Enable HTTP gzip compression |
| `COMPRESSION_CACHE_SIZE` | `-compression-cache-size` | 1000 | Max compressed tiles in memory |

### Disk Cache Settings

| Environment Variable | CLI Flag | Default | Description |
| -------------------- | -------- | ------- | ----------- |
| `DISK_CACHE_ENABLED` | `-disk-cache-enabled` | false | Enable persistent disk cache |
| `DISK_CACHE_PATH` | `-disk-cache-path` | | Path to disk cache directory |
| `DISK_CACHE_MAX_FILES` | `-disk-cache-max-files` | 100000 | Max files in disk cache |

### Public Service URL

| Environment Variable | CLI Flag | Default | Description |
| -------------------- | -------- | ------- | ----------- |
| `SERVICE_HOST` | `-service-host` | | Public hostname (used in style tile URLs) |
| `SERVICE_PORT` | `-service-port` | | Public port (optional) |
| `SERVICE_PREFIX` | `-service-prefix` | | URL prefix (e.g. /v1/tiles) |

### JWT Key Refresh

| Environment Variable | CLI Flag | Default | Description |
| -------------------- | -------- | ------- | ----------- |
| `AUTHSERVICE_HOST` | `-authservice-host` | | Auth service host for JWT public key discovery |
| `AUTHSERVICE_PORT` | `-authservice-port` | 8081 | Auth service gRPC port |
| `JWT_KEYS_REFRESH_INTERVAL_SECS` | `-jwt-keys-refresh-interval-secs` | 300 | How often (seconds) to refresh JWT public keys from authservice |
| `JWT_KEYS_FETCH_TIMEOUT_SECS` | `-jwt-keys-fetch-timeout-secs` | 15 | Upper bound (seconds) on a single public-key fetch |

## API Reference

The `ping` endpoint is public. All other endpoints require a **service client token** with the `tiles:serve` scope (see Authorization above); direct user-JWT access is rejected with 403 — client requests go through swayrider-api, which injects its own service token.

---

### Ping

Health check endpoint that returns HTTP 200.

- **Endpoint:** `GET /v1/tiles/ping`
- **Access:** Public

**Response:**

```json
{"status":"ok"}
```

---

### Ready

Readiness: whether the planet tileset can be served. Like ping it needs no token (probes carry none); it exposes release ids and versions only.

- **Endpoint:** `GET /v1/tiles/ready`
- **Access:** Public
- **Status:** `200` when ready, `503` otherwise. With `TILES_ROOT`, ready means a release is loaded; with `PMTILES_URL`, that the archive opened; with neither the service is ready (only the legacy tilesets are served) and reports `"mode":"disabled"`.

**Response (release store):**

```json
{"ready":true,"planet":{"loaded":true,"release":"r-20261005-1","key":"/data/tiles/releases/r-20261005-1","tileset_build":"20261004","tileset_date":"2026-10-04","schema_version":"4.15.2","styles":{"classic":"3"},"loaded_at":"…","last_check":"…"}}
```

`last_error` appears while the last pointer check failed (the previous release keeps serving); `ready` stays true then.

### HTTP server timeouts

`ReadHeaderTimeout` 10 s, `ReadTimeout` 30 s, `WriteTimeout` 60 s, `IdleTimeout` 120 s.

---

### Styles

Styles are MapLibre style JSON files that are Go templates, rendered per request with the public URLs. Two sources:

- **The active release** (`TILES_ROOT`): named styles listed in `manifest.json`, each with a version and a `light` and a `dark` variant. Template variables: `{{.TilesBaseURL}}` (= `SERVICE_HOST[:SERVICE_PORT]SERVICE_PREFIX`, the gateway's public URL with `/v1/tiles`) and `{{.Tileset}}` (the release's tileset name, `planet`). A typical source is `"tiles": ["{{.TilesBaseURL}}/{{.Tileset}}/{z}/{x}/{y}"]` with `"maxzoom": 15`. MapLibre's single-brace `{z}/{x}/{y}` does not collide with the template syntax.
- **Legacy files** in `STYLES_PATH` (`<name>.json`, pointing at the `base` tileset; `{{.Tileset}}` renders `base`).

A release is rejected at load time (the previous one keeps serving, see `/ready`) when a style variant is missing, does not parse as a template, fails to render or does not produce valid JSON.

| Endpoint | Serves |
| -------- | ------ |
| `GET /v1/tiles/styles` | the listing |
| `GET /v1/tiles/styles/{name}` | a legacy file `{name}.json` if there is one; else, from the release, the alias `light`/`dark` (that variant of the default style) or a style id (its `light` variant, else `dark`) |
| `GET /v1/tiles/styles/{id}/{variant}` | the newest version of the style `id`; `variant` is `light` or `dark` |
| `GET /v1/tiles/styles/{id}/{version}/{variant}` | a fixed version; any version listed in the manifest stays reachable |

**A legacy file wins over a release alias of the same name**, so clients of the legacy tileset keep getting their style (`light`/`dark` → `base`) while a release is loaded. New clients use `styles/{id}/{variant}`. The unversioned `light`/`dark` aliases start pointing at the release once `STYLES_PATH` has no file of that name (when the legacy tileset is removed).

Status codes: `400` for an invalid id/version in the path (anything outside `[A-Za-z0-9_-]` for ids, `[0-9A-Za-z._-]` for versions, or `.`/`..`); `404` for an unknown style, version or variant; `503` for `styles/{id}/…` while no release is loaded.

**Caching:** release styles carry a strong `ETag` (`If-None-Match` answers `304`). Versioned URLs are `Cache-Control: public, max-age=31536000, immutable` (the data-manager bumps the version when the content changes); unversioned URLs and aliases `max-age=300`. Rendered styles are cached per release, so a release switch never serves the old release's style. Legacy files are served as before.

**Listing:** a JSON array. Every entry has a `name` that `GET /styles/{name}` serves, so clients that only read `name` keep working. Legacy files come first; with a release loaded the aliases `light`/`dark` (unless a legacy file has that name) and one entry per style id follow, at its newest version, the default first:

```json
[{"name":"light"},{"name":"dark"},
 {"name":"classic","id":"classic","label":"Classic","version":"3","default":true,"variants":["light","dark"]}]
```

---

### Get Tile

Retrieves a vector tile for the specified tileset and coordinates.

- **Endpoint:** `GET /v1/tiles/{tileset}/{z}/{x}/{y}`
- **Access:** Service client token with `tiles:serve` scope

**Parameters:**

| Parameter | Type | Description |
| --------- | ---- | ----------- |
| `tileset` | string | Tileset name (e.g., "base", "roads") — currently accepted but ignored; reserved for future multi-tileset support (only one tileset is served) |
| `z` | uint32 | Zoom level (0-16) |
| `x` | uint32 | Tile X coordinate |
| `y` | uint32 | Tile Y coordinate |

**Response:**

Returns the vector tile data in MVT (Mapbox Vector Tile) format.

```bash
curl --request GET \
  --url http://localhost:8080/v1/tiles/base/10/512/384
```

---

## Tiles Data Structure

The `TILES_PATH` directory must contain MBTiles files organized in a hierarchical structure:

```
tiles/
├── L0.mbtiles                    # World backdrop (zoom 0-6)
├── L1/                           # Continental tiles (zoom 7-10)
│   ├── N20_E000.mbtiles
│   ├── N20_E010.mbtiles
│   ├── N30_W010.mbtiles
│   └── ...
├── L2/                           # Regional+local tiles (zoom 11-16)
│   ├── N20_E000.mbtiles
│   ├── N20_E010.mbtiles
│   └── ...
```

### Tile Layers

| Layer | Zoom Levels | Coverage | Content |
| ----- | ----------- | -------- | ------- |
| L0 | 0-6 | World | World backdrop, country boundaries, major features |
| L1 | 7-10 | 10° × 10° grid | Large roads (motorways, trunk roads) |
| L2 | 11-16 | 10° × 10° grid | All roads (unsimplified) |

### File Naming Convention

Files in L1 and L2 follow the naming pattern: `{lat}_{lon}.mbtiles`

- `lat`: Latitude prefix (`N` for north, `S` for south) followed by degrees (e.g., `N20`, `S10`)
- `lon`: Longitude prefix (`E` for east, `W` for west) followed by degrees (e.g., `E000`, `W120`)

**Examples:**
- `N20_E000.mbtiles` - Covers 20°N to 30°N, 0°E to 10°E
- `N50_W010.mbtiles` - Covers 50°N to 60°N, 10°W to 0°
- `S30_E020.mbtiles` - Covers 30°S to 20°S, 20°E to 30°E

## Building

```bash
# Build the service
go build ./cmd/tilesservice

# Run the service
go run ./cmd/tilesservice
```

## Docker

```bash
# Build container (from tilesservice/ directory)
make container-build
```

### Tagging

Tags are derived from the git state of the checkout:

| Branch / state | Tags applied |
|----------------|--------------|
| Version-tagged commit (`v1.2.3`) | `v1.2.3`, `latest` |
| `main` (untagged) | `v{last}-{date}-dev-b{N}`, `dev-latest` |
| Other branch | `v{last}-{branch}-b{N}` |
| Detached HEAD | `v{last}-{sha}-b{N}` |

Non-release builds get an incrementing build number (`-b{N}`) so repeated builds of the same branch don't overwrite each other. The number comes from querying the registry for the highest existing `-b{N}` tag on the same base tag and adding 1; the build fails if the registry can't be reached. Release builds are immutable and never get a build number.

### FORCE_DEV_LATEST

Only `main` (untagged HEAD) pushes the `dev-latest` floating tag automatically. Set `FORCE_DEV_LATEST=1` to also push `dev-latest` from a release build (a version-tagged commit, e.g. `v1.2.3`) or from any other branch:

```bash
FORCE_DEV_LATEST=1 make container-build
```

Or, across all services at once, `tools/containerbuild.py --dev-latest`. Use this when a release — or a branch build — should also advance environments that track `dev-latest`.

## Development

Copy the example configuration and adjust as needed:

```bash
cp env.example .env
# Edit .env with your paths
```

Run the service:

```bash
source .env
go run ./cmd/tilesservice
```

Development port: HTTP API on 8080
