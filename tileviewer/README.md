# tileviewer

A simple browser-based viewer for debugging and visualizing vector tiles from the SwayRider tilesservice.

## Usage

### 1. Start the tile server

```bash
cd backend
source services/tilesservice/.env
go run ./services/tilesservice/cmd/tilesservice
```

### 2. Open the viewer

**Option A: Direct file access**

Open `index.html` directly in your browser. Note: Some browsers may block local file requests due to CORS.

**Option B: Local HTTP server (recommended)**

```bash
cd backend/apps/tileviewer
python3 -m http.server 8000
```

Then open http://localhost:8000 in your browser.

### 3. Configure tile URL (optional)

The default tile URL is `http://localhost:8080/v1/tiles/base/{z}/{x}/{y}`.

You can change this in two ways:

- **UI**: Edit the URL in the top-right config panel and click "Reload"
- **URL parameter**: Add `?tiles=URL` to the viewer URL

Example:
```
http://localhost:8000/?tiles=http://localhost:8080/v1/tiles/roads/{z}/{x}/{y}
```

## Features

- Interactive pan/zoom map (MapLibre GL JS)
- Centered on Western Europe (Amsterdam area) by default
- Real-time display of current zoom level and tile coordinates
- Configurable tile server URL
- Optional tile grid overlay for debugging

## Expected Tile Layers

The viewer includes default styling for these OpenMapTiles-compatible layers:

| Layer | Description |
|-------|-------------|
| `water` | Water bodies (lakes, seas) |
| `waterway` | Rivers, canals, streams |
| `landuse` | Land use areas (forest, grass, etc.) |
| `transportation` | Roads by class (motorway, trunk, primary, secondary, tertiary, minor) |

If your tiles use different layer names, you'll need to modify the style in `index.html`.

## Troubleshooting

**"Failed to load tiles" error**

- Verify the tile server is running: `curl http://localhost:8080/v1/tiles/ping`
- Check that `TILES_PATH` is configured and contains valid mbtiles files
- Check browser console for CORS errors

**No tiles visible**

- Ensure you're viewing an area where tile data exists
- Check the zoom level (different layers appear at different zoom ranges)
- Verify the layer names in your mbtiles match those in the viewer style

**CORS errors**

If running the tile server and viewer on different origins, you may need to configure CORS headers on the tile server.
