package pmtiles_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/swayrider/tilesservice/internal/pmtiles"
	"github.com/swayrider/tilesservice/internal/pmtiles/pmtilestest"
)

func open(t *testing.T, file []byte, opts ...pmtiles.Option) *pmtiles.Reader {
	t.Helper()
	r, err := pmtiles.Open(bytes.NewReader(file), int64(len(file)), opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return r
}

// gridTiles returns every tile of zoom levels 0..maxZ, each with distinct content.
func gridTiles(maxZ uint8) []pmtilestest.Tile {
	var tiles []pmtilestest.Tile
	for z := uint8(0); z <= maxZ; z++ {
		for x := uint32(0); x < 1<<z; x++ {
			for y := uint32(0); y < 1<<z; y++ {
				tiles = append(tiles, pmtilestest.Tile{Z: z, X: x, Y: y, Data: []byte(fmt.Sprintf("tile-%d-%d-%d", z, x, y))})
			}
		}
	}
	return tiles
}

func checkAllTiles(t *testing.T, r *pmtiles.Reader, tiles []pmtilestest.Tile) {
	t.Helper()
	for _, tl := range tiles {
		got, err := r.Tile(tl.Z, tl.X, tl.Y)
		if err != nil || !bytes.Equal(got, tl.Data) {
			t.Fatalf("tile %d/%d/%d = %q, %v; want %q", tl.Z, tl.X, tl.Y, got, err, tl.Data)
		}
	}
}

func TestReaderLayouts(t *testing.T) {
	tiles := gridTiles(4) // 341 tiles
	tests := []struct {
		name string
		opts pmtilestest.Options
	}{
		{"root only, plain directories", pmtilestest.Options{}},
		{"root only, gzip directories", pmtilestest.Options{GzipDirectories: true}},
		{"one leaf level", pmtilestest.Options{LeafSize: 50, GzipDirectories: true}},
		{"three leaf levels", pmtilestest.Options{LeafSize: 5}},
		{"plain tiles, no compression", pmtilestest.Options{TileCompression: pmtiles.CompressionNone}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := open(t, pmtilestest.Build(tiles, tt.opts))
			checkAllTiles(t, r, tiles)
			h := r.Header()
			if h.MinZoom != 0 || h.MaxZoom != 4 || h.AddressedTiles != uint64(len(tiles)) {
				t.Errorf("header: %+v", h)
			}
		})
	}
}

func TestReaderRunLengths(t *testing.T) {
	same := []byte("ocean")
	var tiles []pmtilestest.Tile
	// z3: ids of the first eight tiles in curve order all share one payload (a run), the rest differ.
	for id := uint64(0); id < 64; id++ {
		z, x, y, _ := pmtiles.IDToZXY(21 + id) // ids 21..84 are the 64 tiles of z3
		data := same
		if id >= 8 {
			data = []byte(fmt.Sprintf("land-%d", id))
		}
		tiles = append(tiles, pmtilestest.Tile{Z: z, X: x, Y: y, Data: data})
	}
	file := pmtilestest.Build(tiles, pmtilestest.Options{RunLengths: true})
	r := open(t, file)
	h := r.Header()
	if h.TileEntries >= h.AddressedTiles || h.AddressedTiles != 64 {
		t.Errorf("expected run-length merging: %d entries for %d tiles", h.TileEntries, h.AddressedTiles)
	}
	checkAllTiles(t, r, tiles)
}

// The format allows the root plus three levels of leaf directories; a deeper (or cyclic) chain is an error.
func TestReaderRejectsTooDeepNesting(t *testing.T) {
	r := open(t, pmtilestest.Build(gridTiles(4), pmtilestest.Options{LeafSize: 4})) // 4 leaf levels
	if _, err := r.Tile(0, 0, 0); err == nil || errors.Is(err, pmtiles.ErrTileNotFound) {
		t.Errorf("err = %v, want a nesting error", err)
	}
}

func TestReaderMissingTiles(t *testing.T) {
	tiles := []pmtilestest.Tile{{Z: 3, X: 2, Y: 2, Data: []byte("a")}, {Z: 3, X: 5, Y: 1, Data: []byte("b")}}
	r := open(t, pmtilestest.Build(tiles, pmtilestest.Options{}))
	for _, c := range [][3]uint32{{3, 0, 0}, {3, 7, 7}, {2, 1, 1}, {10, 5, 5}, {3, 8, 0} /* out of range */} {
		if _, err := r.Tile(uint8(c[0]), c[1], c[2]); !errors.Is(err, pmtiles.ErrTileNotFound) {
			t.Errorf("tile %v: err = %v, want ErrTileNotFound", c, err)
		}
	}
}

func TestReaderMissingTileInsideLeaf(t *testing.T) {
	r := open(t, pmtilestest.Build(gridTiles(3), pmtilestest.Options{LeafSize: 8}))
	if _, err := r.Tile(4, 1, 1); !errors.Is(err, pmtiles.ErrTileNotFound) {
		t.Errorf("z4 does not exist: %v", err)
	}
}

func TestReaderMetadata(t *testing.T) {
	const meta = `{"name":"test","vector_layers":[{"id":"roads"}]}`
	for _, gz := range []bool{false, true} {
		r := open(t, pmtilestest.Build(gridTiles(1), pmtilestest.Options{Metadata: meta, GzipDirectories: gz}))
		got, err := r.Metadata()
		if err != nil || string(got) != meta {
			t.Errorf("gzip=%v: metadata %q, %v", gz, got, err)
		}
	}
	r := open(t, pmtilestest.Build(gridTiles(1), pmtilestest.Options{}))
	if got, err := r.Metadata(); err != nil || len(got) != 0 {
		t.Errorf("no metadata: %q, %v", got, err)
	}
}

func TestOpenRefusals(t *testing.T) {
	good := pmtilestest.Build(gridTiles(2), pmtilestest.Options{})
	t.Run("not pmtiles", func(t *testing.T) {
		junk := bytes.Repeat([]byte("not a pmtiles file "), 20)
		_, err := pmtiles.Open(bytes.NewReader(junk), int64(len(junk)))
		if !errors.Is(err, pmtiles.ErrBadMagic) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("too short", func(t *testing.T) {
		if _, err := pmtiles.Open(bytes.NewReader(good[:50]), 50); err == nil {
			t.Error("want error")
		}
	})
	t.Run("raster tiles", func(t *testing.T) {
		f := pmtilestest.Build(gridTiles(1), pmtilestest.Options{TileType: pmtiles.TileTypePNG})
		if _, err := pmtiles.Open(bytes.NewReader(f), int64(len(f))); err == nil {
			t.Error("png archive must be refused")
		}
	})
	for _, c := range []pmtiles.Compression{pmtiles.CompressionBrotli, pmtiles.CompressionZstd} {
		t.Run("tile compression "+c.String(), func(t *testing.T) {
			f := pmtilestest.Build(gridTiles(1), pmtilestest.Options{TileCompression: c})
			if _, err := pmtiles.Open(bytes.NewReader(f), int64(len(f))); err == nil {
				t.Error("want error")
			}
		})
	}
	t.Run("truncated upload", func(t *testing.T) {
		cut := good[:len(good)-5]
		if _, err := pmtiles.Open(bytes.NewReader(cut), int64(len(cut))); err == nil {
			t.Error("a file shorter than its tile data section must be refused at open")
		}
	})
	t.Run("closes the source on failure", func(t *testing.T) {
		c := &closeCounter{}
		_, _ = pmtiles.Open(bytes.NewReader([]byte("short")), 5, pmtiles.WithCloser(c))
		if c.n != 1 {
			t.Errorf("closer called %d times", c.n)
		}
	})
}

func TestReaderRejectsCorruptEntries(t *testing.T) {
	// A directory entry that points beyond the tile data section must error, not read garbage.
	file := pmtilestest.Build(gridTiles(1), pmtilestest.Options{})
	r := open(t, file)
	h := r.Header()
	corrupt := append([]byte(nil), file...)
	// Shrink the tile data length in the header so every entry is out of bounds.
	h.TileDataLength = 3
	copy(corrupt, h.Bytes())
	r2 := open(t, corrupt)
	if _, err := r2.Tile(1, 1, 1); err == nil || errors.Is(err, pmtiles.ErrTileNotFound) {
		t.Errorf("err = %v, want a bounds error", err)
	}
}

func TestReaderConcurrentAndCached(t *testing.T) {
	tiles := gridTiles(5)
	file := pmtilestest.Build(tiles, pmtilestest.Options{LeafSize: 64, GzipDirectories: true})
	src := &countingReaderAt{r: bytes.NewReader(file)}
	r, err := pmtiles.Open(src, int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, tl := range tiles {
				got, err := r.Tile(tl.Z, tl.X, tl.Y)
				if err != nil || !bytes.Equal(got, tl.Data) {
					t.Errorf("tile %d/%d/%d: %q, %v", tl.Z, tl.X, tl.Y, got, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	leaves := (len(tiles) + 63) / 64
	// header + root at open, then each leaf exactly once (cache + single flight) and each tile once per request.
	wantMax := int64(2 + leaves + 16*len(tiles))
	if n := atomic.LoadInt64(&src.reads); n > wantMax {
		t.Errorf("%d reads, expected at most %d: leaf directories are not cached", n, wantMax)
	}
	leafReads := atomic.LoadInt64(&src.reads) - int64(16*len(tiles)) - 2
	if leafReads != int64(leaves) {
		t.Errorf("leaf directory reads = %d, want %d (one per leaf)", leafReads, leaves)
	}
}

func TestReaderCacheEviction(t *testing.T) {
	tiles := gridTiles(4)
	file := pmtilestest.Build(tiles, pmtilestest.Options{LeafSize: 16})
	src := &countingReaderAt{r: bytes.NewReader(file)}
	r, err := pmtiles.Open(src, int64(len(file)), pmtiles.WithDirectoryCacheEntries(16)) // room for about one leaf
	if err != nil {
		t.Fatal(err)
	}
	checkAllTiles(t, r, tiles)
	checkAllTiles(t, r, tiles) // must still be correct after evictions
}

func TestIDChangesWithContent(t *testing.T) {
	a := open(t, pmtilestest.Build(gridTiles(2), pmtilestest.Options{}))
	b := open(t, pmtilestest.Build(gridTiles(3), pmtilestest.Options{}))
	a2 := open(t, pmtilestest.Build(gridTiles(2), pmtilestest.Options{}))
	if a.ID() == b.ID() || a.ID() != a2.ID() || len(a.ID()) != 16 {
		t.Errorf("ids: %s %s %s", a.ID(), b.ID(), a2.ID())
	}
}

type countingReaderAt struct {
	r     io.ReaderAt
	reads int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	atomic.AddInt64(&c.reads, 1)
	return c.r.ReadAt(p, off)
}

type closeCounter struct{ n int }

func (c *closeCounter) Close() error { c.n++; return nil }
