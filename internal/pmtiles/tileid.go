package pmtiles

import "fmt"

// MaxZoom is the highest zoom level a PMTiles tile id can address (ids are uint64).
const MaxZoom = 31

// ZXYToID returns the PMTiles tile id of a tile: tiles are numbered zoom level by zoom level,
// inside a level along a Hilbert curve.
func ZXYToID(z uint8, x, y uint32) (uint64, error) {
	if z > MaxZoom {
		return 0, fmt.Errorf("pmtiles: zoom %d exceeds %d", z, MaxZoom)
	}
	n := int64(1) << z
	if int64(x) >= n || int64(y) >= n {
		return 0, fmt.Errorf("pmtiles: tile %d/%d/%d out of range", z, x, y)
	}
	var acc uint64
	for tz := uint8(0); tz < z; tz++ {
		acc += uint64(1) << (2 * tz)
	}
	tx, ty := int64(x), int64(y)
	var d int64
	for s := n / 2; s > 0; s /= 2 {
		var rx, ry int64
		if tx&s > 0 {
			rx = 1
		}
		if ty&s > 0 {
			ry = 1
		}
		d += s * s * ((3 * rx) ^ ry)
		tx, ty = rotate(n, tx, ty, rx, ry)
	}
	return acc + uint64(d), nil
}

// IDToZXY is the inverse of ZXYToID.
func IDToZXY(id uint64) (z uint8, x, y uint32, err error) {
	var acc uint64
	for tz := uint8(0); tz <= MaxZoom; tz++ {
		numTiles := uint64(1) << (2 * tz)
		if acc+numTiles > id {
			n := int64(1) << tz
			t := int64(id - acc)
			var tx, ty int64
			for s := int64(1); s < n; s *= 2 {
				rx := 1 & (t / 2)
				ry := 1 & (t ^ rx)
				tx, ty = rotate(s, tx, ty, rx, ry)
				tx += s * rx
				ty += s * ry
				t /= 4
			}
			return tz, uint32(tx), uint32(ty), nil
		}
		acc += numTiles
	}
	return 0, 0, 0, fmt.Errorf("pmtiles: tile id %d out of range", id)
}

func rotate(n, x, y, rx, ry int64) (int64, int64) {
	if ry == 0 {
		if rx == 1 {
			x, y = n-1-x, n-1-y
		}
		x, y = y, x
	}
	return x, y
}
