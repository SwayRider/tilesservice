package pmtiles_test

import (
	"testing"

	"github.com/swayrider/tilesservice/internal/pmtiles"
)

func TestZXYToIDKnownValues(t *testing.T) {
	tests := []struct {
		z    uint8
		x, y uint32
		id   uint64
	}{
		{0, 0, 0, 0},
		{1, 0, 0, 1}, {1, 0, 1, 2}, {1, 1, 1, 3}, {1, 1, 0, 4},
		{2, 0, 0, 5},
	}
	for _, tt := range tests {
		got, err := pmtiles.ZXYToID(tt.z, tt.x, tt.y)
		if err != nil || got != tt.id {
			t.Errorf("ZXYToID(%d,%d,%d) = %d, %v; want %d", tt.z, tt.x, tt.y, got, err, tt.id)
		}
	}
}

// Every zoom level owns the contiguous id range [sum 4^i for i<z, + 4^z), the mapping is a
// bijection, and consecutive ids are edge-adjacent tiles (the Hilbert property).
func TestTileIDBijectionAndContinuity(t *testing.T) {
	var base uint64
	for z := uint8(0); z <= 7; z++ {
		n := uint64(1) << (2 * z)
		seen := make(map[uint64]bool, n)
		var px, py uint32
		for id := base; id < base+n; id++ {
			gz, x, y, err := pmtiles.IDToZXY(id)
			if err != nil || gz != z {
				t.Fatalf("IDToZXY(%d) = z%d, %v; want z%d", id, gz, err, z)
			}
			back, err := pmtiles.ZXYToID(z, x, y)
			if err != nil || back != id {
				t.Fatalf("round trip %d -> %d/%d/%d -> %d (%v)", id, z, x, y, back, err)
			}
			if seen[uint64(x)<<32|uint64(y)] {
				t.Fatalf("tile %d/%d/%d seen twice", z, x, y)
			}
			seen[uint64(x)<<32|uint64(y)] = true
			if id > base {
				dx, dy := absDiff(x, px), absDiff(y, py)
				if dx+dy != 1 {
					t.Fatalf("ids %d and %d are not adjacent: (%d,%d) -> (%d,%d)", id-1, id, px, py, x, y)
				}
			}
			px, py = x, y
		}
		base += n
	}
}

func TestZXYToIDRejectsOutOfRange(t *testing.T) {
	if _, err := pmtiles.ZXYToID(2, 4, 0); err == nil {
		t.Error("x = 4 at z2 must be rejected")
	}
	if _, err := pmtiles.ZXYToID(32, 0, 0); err == nil {
		t.Error("z32 must be rejected")
	}
	if _, _, _, err := pmtiles.IDToZXY(^uint64(0)); err == nil {
		t.Error("id beyond z31 must be rejected")
	}
}

func TestHighZoomRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		z    uint8
		x, y uint32
	}{{15, 16383, 16383}, {15, 12345, 6789}, {20, 1048575, 0}, {31, 2147483647, 1}} {
		id, err := pmtiles.ZXYToID(tt.z, tt.x, tt.y)
		if err != nil {
			t.Fatal(err)
		}
		z, x, y, err := pmtiles.IDToZXY(id)
		if err != nil || z != tt.z || x != tt.x || y != tt.y {
			t.Errorf("%d/%d/%d -> %d -> %d/%d/%d (%v)", tt.z, tt.x, tt.y, id, z, x, y, err)
		}
	}
}

func absDiff(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}
