# tilesservice

Vector tile serving service for the SwayRider platform. Serves Mapbox Vector Tiles (MVT) for map rendering at multiple zoom levels with hierarchical tile organization.

## Architecture

The tilesservice exposes two server interfaces:

| Interface | Port | Purpose |
| --------- | ---- | ------- |
| REST/HTTP | 8080 | HTTP API via gRPC-gateway |
| gRPC | 8081 | Internal service-to-service communication |

### Tile Storage

The service reads tiles from MBTiles files organized in a hierarchical structure based on zoom levels and geographic regions.

## Configuration

Configuration is provided via environment variables or CLI flags.

### Server Configuration

| Environment Variable | CLI Flag | Default | Description |
| -------------------- | -------- | ------- | ----------- |
| `HTTP_PORT` | `-http-port` | 8080 | REST API port |
| `GRPC_PORT` | `-grpc-port` | 8081 | gRPC port |

### Tiles Configuration

| Environment Variable | CLI Flag | Default | Description |
| -------------------- | -------- | ------- | ----------- |
| `TILES_PATH` | `-tiles-path` | | Path to the tiles storage directory |

## API Reference

The API is defined in the Protocol Buffer files at `backend/protos/tiles/v1/`.

All endpoints are public and require no authentication.

---

### Ping

Simple health check that returns HTTP 200.

- **Endpoint:** `GET /v1/tiles/ping`
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
# Generate protobuf code (run from repo root)
make proto

# Build the service
cd backend
go build ./services/tilesservice/cmd/tilesservice

# Run the service
go run ./services/tilesservice/cmd/tilesservice
```

## Docker

```bash
# Build container (from repo root)
make services-tilesservice-container
```

## Development

For local development:

1. Create the tiles directory structure
2. Copy or generate MBTiles files into the appropriate locations
3. Set `TILES_PATH` in your `.env` file
4. Run the service

```bash
cd backend/services/tilesservice
source .env
go run ./cmd/tilesservice
```

Development ports:
- REST API: 8080
- gRPC: 8081
