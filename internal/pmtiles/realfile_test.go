package pmtiles_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"os"
	"testing"

	"github.com/swayrider/tilesservice/internal/pmtiles"
)

// TestRealFile runs the reader against a real Protomaps extract. It is skipped unless
// PMTILES_TEST_FILE points at one, for example:
//
//	pmtiles extract https://build.protomaps.com/<build>.pmtiles brussels.pmtiles --bbox=4.2,50.75,4.55,50.95
//	PMTILES_TEST_FILE=$PWD/brussels.pmtiles go test ./internal/pmtiles -run TestRealFile -v
//
// With PMTILES_TEST_DUMP=1 it prints "z x y sha256(stored bytes)" lines for comparison with the
// official CLI (`pmtiles tile <file> z x y`).
func TestRealFile(t *testing.T) {
	path := os.Getenv("PMTILES_TEST_FILE")
	if path == "" {
		t.Skip("PMTILES_TEST_FILE not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	st, _ := f.Stat()
	r, err := pmtiles.Open(f, st.Size())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	h := r.Header()
	t.Logf("zoom %d-%d, %d addressed tiles, %d entries, tile compression %s, internal %s, id %s",
		h.MinZoom, h.MaxZoom, h.AddressedTiles, h.TileEntries, h.TileCompression, h.InternalCompression, r.ID())

	meta, err := r.Metadata()
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		VectorLayers []struct{ ID string } `json:"vector_layers"`
		Version      string                `json:"version"`
	}
	if err := json.Unmarshal(meta, &m); err != nil || len(m.VectorLayers) == 0 {
		t.Fatalf("metadata: %v (%d bytes, %d layers)", err, len(meta), len(m.VectorLayers))
	}
	t.Logf("metadata: schema version %s, %d vector layers", m.Version, len(m.VectorLayers))

	// The tile at the centre of the bounds exists on every zoom level of the extract.
	lon := (float64(h.MinLonE7) + float64(h.MaxLonE7)) / 2 / 1e7
	lat := (float64(h.MinLatE7) + float64(h.MaxLatE7)) / 2 / 1e7
	dump := os.Getenv("PMTILES_TEST_DUMP") != ""
	for z := h.MinZoom; z <= h.MaxZoom; z++ {
		n := math.Exp2(float64(z))
		x := uint32(math.Floor((lon + 180) / 360 * n))
		y := uint32(math.Floor((1 - math.Log(math.Tan(lat*math.Pi/180)+1/math.Cos(lat*math.Pi/180))/math.Pi) / 2 * n))
		b, err := r.Tile(z, x, y)
		if err != nil {
			t.Errorf("tile %d/%d/%d: %v", z, x, y, err)
			continue
		}
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Errorf("tile %d/%d/%d is not gzip: %v", z, x, y, err)
			continue
		}
		plain, err := io.ReadAll(zr)
		if err != nil || len(plain) == 0 {
			t.Errorf("tile %d/%d/%d does not decode: %v", z, x, y, err)
		}
		if dump {
			sum := sha256.Sum256(b)
			t.Logf("DUMP %d %d %d %s", z, x, y, hex.EncodeToString(sum[:]))
		}
	}
	// A tile far outside the extract is simply absent.
	if _, err := r.Tile(h.MaxZoom, 0, 0); err != pmtiles.ErrTileNotFound {
		t.Errorf("tile outside the extract: %v", err)
	}
}
