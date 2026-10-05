package pmtiles_test

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/swayrider/tilesservice/internal/pmtiles"
)

// goldenHeader assembles a header byte by byte from the PMTiles v3 spec, independently of Header.Bytes.
func goldenHeader() []byte {
	b := make([]byte, 127)
	copy(b, "PMTiles")
	b[7] = 3
	le := binary.LittleEndian
	for i, v := range []uint64{127, 20, 147, 30, 177, 40, 217, 1000, 7, 6, 5} { // offsets 8..88
		le.PutUint64(b[8+8*i:], v)
	}
	b[96], b[97], b[98], b[99], b[100], b[101] = 1, 2, 2, 1, 0, 15
	minLon := int32(-1800000000) // -180 degrees in e7
	le.PutUint32(b[102:], uint32(minLon))
	le.PutUint32(b[106:], 500000000) // +50 degrees
	le.PutUint32(b[110:], 1800000000)
	le.PutUint32(b[114:], 600000000)
	b[118] = 7
	le.PutUint32(b[119:], 40000000)
	le.PutUint32(b[123:], 505000000)
	return b
}

func TestParseHeaderGolden(t *testing.T) {
	h, err := pmtiles.ParseHeader(goldenHeader())
	if err != nil {
		t.Fatal(err)
	}
	if h.RootOffset != 127 || h.RootLength != 20 || h.MetadataOffset != 147 || h.MetadataLength != 30 ||
		h.LeafDirectoriesOffset != 177 || h.LeafDirectoriesLength != 40 || h.TileDataOffset != 217 || h.TileDataLength != 1000 ||
		h.AddressedTiles != 7 || h.TileEntries != 6 || h.TileContents != 5 {
		t.Errorf("section fields wrong: %+v", h)
	}
	if !h.Clustered || h.InternalCompression != pmtiles.CompressionGzip || h.TileCompression != pmtiles.CompressionGzip ||
		h.TileType != pmtiles.TileTypeMVT || h.MinZoom != 0 || h.MaxZoom != 15 {
		t.Errorf("flag fields wrong: %+v", h)
	}
	if h.MinLonE7 != -1800000000 || h.MinLatE7 != 500000000 || h.MaxLonE7 != 1800000000 || h.MaxLatE7 != 600000000 ||
		h.CenterZoom != 7 || h.CenterLonE7 != 40000000 || h.CenterLatE7 != 505000000 {
		t.Errorf("bounds wrong: %+v", h)
	}
	if got := h.Bytes(); string(got) != string(goldenHeader()) {
		t.Error("Header.Bytes does not reproduce the golden header")
	}
}

func TestParseHeaderErrors(t *testing.T) {
	if _, err := pmtiles.ParseHeader(goldenHeader()[:100]); err == nil {
		t.Error("short header must fail")
	}
	bad := goldenHeader()
	copy(bad, "NotPMTi")
	if _, err := pmtiles.ParseHeader(bad); !errors.Is(err, pmtiles.ErrBadMagic) {
		t.Errorf("bad magic: %v", err)
	}
	v2 := goldenHeader()
	v2[7] = 2
	if _, err := pmtiles.ParseHeader(v2); !errors.Is(err, pmtiles.ErrUnsupportedVersion) {
		t.Errorf("v2: %v", err)
	}
}
