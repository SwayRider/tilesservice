# tilesservice

Vector tile serving service for the SwayRider platform. Serves Mapbox Vector Tiles (MVT) for map rendering at multiple zoom levels with hierarchical tile organization.

## Architecture

The tilesservice exposes an HTTP API for tile serving:

| Interface | Port | Purpose |
| --------- | ---- | ------- |
| HTTP | 8080 | REST API for tiles and styles |

### Tile Storage

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
| `GET /v1/tiles/styles` | Service client token with `tiles:serve` scope |
| `GET /v1/tiles/styles/{name}` | Service client token with `tiles:serve` scope |
| `GET /v1/tiles/{tileset}/{z}/{x}/{y}` | Service client token with `tiles:serve` scope |

AUTHSERVICE_HOST and AUTHSERVICE_PORT must be configured so the service can fetch JWT public keys for token validation.

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

## API Reference

All endpoints are public and require no authentication.

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

### Get Style

Retrieves style definition for a named style.

- **Endpoint:** `GET /v1/tiles/styles/{name}`
- **Access:** Public

**Parameters:**

| Parameter | Type | Description |
| --------- | ---- | ----------- |
| `name` | string | Style name (e.g., "light", "dark") |

**Response:**

Returns the map style JSON definition.

---

### List Styles

Lists available style names.

- **Endpoint:** `GET /v1/tiles/styles`
- **Access:** Public

---

### Get Tile

Retrieves a vector tile for the specified tileset and coordinates.

- **Endpoint:** `GET /v1/tiles/{tileset}/{z}/{x}/{y}`
- **Access:** Public

**Parameters:**

| Parameter | Type | Description |
| --------- | ---- | ----------- |
| `tileset` | string | Tileset name (e.g., "base", "roads") |
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
├── L2/                           # Regional tiles (zoom 11-13)
│   ├── N20_E000.mbtiles
│   ├── N20_E010.mbtiles
│   └── ...
└── L3/                           # Local tiles (zoom 14-16)
    ├── N20_E000.mbtiles
    ├── N20_E010.mbtiles
    └── ...
```

### Tile Layers

| Layer | Zoom Levels | Coverage | Content |
| ----- | ----------- | -------- | ------- |
| L0 | 0-6 | World | World backdrop, country boundaries, major features |
| L1 | 7-10 | 10° × 10° grid | Large roads (motorways, trunk roads) |
| L2 | 11-13 | 10° × 10° grid | Regional roads (primary, secondary) |
| L3 | 14-16 | 10° × 10° grid | Local roads (tertiary, residential) |

### File Naming Convention

Files in L1, L2, and L3 follow the naming pattern: `{lat}_{lon}.mbtiles`

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

### FORCE_DEV_LATEST

By default, a release build on a version-tagged commit (e.g., `v1.2.3`) pushes two tags: the version tag and `latest`. Set `FORCE_DEV_LATEST=1` to additionally push the `dev-latest` floating tag:

```bash
FORCE_DEV_LATEST=1 make container-build
```

Use this when a release should also advance environments that track `dev-latest`.

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
