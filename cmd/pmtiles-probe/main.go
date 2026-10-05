// Command pmtiles-probe opens a PMTiles archive the way tilesservice does (local file or an
// s3://bucket/key object, same S3_* environment variables) and prints what it finds. It is a
// check for new releases, object store credentials and endpoints that needs no gateway or JWT.
//
//	pmtiles-probe -url s3://swayrider-tiles/releases/r-test-1/tiles.pmtiles          # header + metadata summary
//	pmtiles-probe -url file:///data/brussels.pmtiles -tile 12/2097/1373 -out tile.mvt.gz   # one tile
//
// S3 settings: S3_ENDPOINT, S3_REGION (default garage), S3_ACCESS_KEY_ID, S3_SECRET_ACCESS_KEY.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/swayrider/tilesservice/internal/pmtiles"
	"github.com/swayrider/tilesservice/internal/tilesource"
)

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string, out io.Writer) error {
	fs := flag.NewFlagSet("pmtiles-probe", flag.ContinueOnError)
	url := fs.String("url", getenv("PMTILES_URL"), "archive location: file:///path, a path or s3://bucket/key (default $PMTILES_URL)")
	tile := fs.String("tile", "", "read one tile, as z/x/y")
	outFile := fs.String("out", "", "write the stored tile bytes to this file (with -tile)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *url == "" {
		return errors.New("no archive: pass -url or set PMTILES_URL")
	}
	region := getenv("S3_REGION")
	if region == "" {
		region = "garage"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	r, err := tilesource.Open(ctx, *url, tilesource.S3{
		Endpoint: getenv("S3_ENDPOINT"), Region: region,
		AccessKey: getenv("S3_ACCESS_KEY_ID"), SecretKey: getenv("S3_SECRET_ACCESS_KEY"),
	})
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()

	h := r.Header()
	_, _ = fmt.Fprintf(out, "opened %s in %s\n", *url, time.Since(start).Round(time.Millisecond))
	_, _ = fmt.Fprintf(out, "archive id:        %s\n", r.ID())
	_, _ = fmt.Fprintf(out, "zoom:              %d-%d\n", h.MinZoom, h.MaxZoom)
	_, _ = fmt.Fprintf(out, "bounds (lon,lat):  %.4f,%.4f to %.4f,%.4f\n", float64(h.MinLonE7)/1e7, float64(h.MinLatE7)/1e7, float64(h.MaxLonE7)/1e7, float64(h.MaxLatE7)/1e7)
	_, _ = fmt.Fprintf(out, "tiles addressed:   %d (%d entries, %d contents)\n", h.AddressedTiles, h.TileEntries, h.TileContents)
	_, _ = fmt.Fprintf(out, "tile compression:  %s, directories: %s\n", h.TileCompression, h.InternalCompression)
	_, _ = fmt.Fprintf(out, "tile data:         %d bytes\n", h.TileDataLength)
	if meta, err := r.Metadata(); err == nil && len(meta) > 0 {
		var m struct {
			Name         string `json:"name"`
			Version      string `json:"version"`
			VectorLayers []struct {
				ID string `json:"id"`
			} `json:"vector_layers"`
		}
		if json.Unmarshal(meta, &m) == nil {
			ids := make([]string, 0, len(m.VectorLayers))
			for _, l := range m.VectorLayers {
				ids = append(ids, l.ID)
			}
			_, _ = fmt.Fprintf(out, "metadata:          %s, version %s, layers: %s\n", m.Name, m.Version, strings.Join(ids, ", "))
		}
	} else if err != nil {
		return fmt.Errorf("metadata: %w", err)
	}

	if *tile == "" {
		return nil
	}
	parts := strings.Split(*tile, "/")
	if len(parts) != 3 {
		return fmt.Errorf("-tile must be z/x/y, got %q", *tile)
	}
	z, e1 := strconv.ParseUint(parts[0], 10, 8)
	x, e2 := strconv.ParseUint(parts[1], 10, 32)
	y, e3 := strconv.ParseUint(parts[2], 10, 32)
	if e1 != nil || e2 != nil || e3 != nil {
		return fmt.Errorf("-tile must be z/x/y numbers, got %q", *tile)
	}
	start = time.Now()
	data, err := r.Tile(uint8(z), uint32(x), uint32(y))
	if errors.Is(err, pmtiles.ErrTileNotFound) {
		_, _ = fmt.Fprintf(out, "tile %s: not in the archive\n", *tile)
		return nil
	}
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "tile %s: %d bytes stored (%s) in %s\n", *tile, len(data), h.TileCompression, time.Since(start).Round(time.Microsecond))
	if *outFile != "" {
		if err := os.WriteFile(*outFile, data, 0o600); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "written to %s\n", *outFile)
	}
	return nil
}
