package pmtiles

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
)

// maxDecodedBlock bounds the decompressed size of a directory or the metadata (a malformed or
// hostile file must not be able to exhaust memory). Real ones are at most a few hundred KB.
const maxDecodedBlock = 64 << 20

// entry is one directory entry. RunLength > 0: the tile ids TileID..TileID+RunLength-1 all
// point at the same bytes. RunLength == 0: the entry points at a leaf directory.
type entry struct {
	TileID    uint64
	Offset    uint64
	Length    uint32
	RunLength uint32
}

// directory is a decoded list of entries sorted by tile id.
type directory []entry

var errBadDirectory = errors.New("pmtiles: malformed directory")

// decompress returns the plain bytes of a block stored with the given compression.
func decompress(b []byte, c Compression) ([]byte, error) {
	switch c {
	case CompressionNone, CompressionUnknown:
		return b, nil
	case CompressionGzip:
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		defer func() { _ = zr.Close() }()
		out, err := io.ReadAll(io.LimitReader(zr, maxDecodedBlock+1))
		if err != nil {
			return nil, err
		}
		if len(out) > maxDecodedBlock {
			return nil, fmt.Errorf("pmtiles: decoded block exceeds %d bytes", maxDecodedBlock)
		}
		return out, nil
	}
	return nil, fmt.Errorf("pmtiles: unsupported compression %s", c)
}

// parseDirectory decodes a (decompressed) directory: a varint entry count, then the columns
// tile ids (delta coded), run lengths, lengths and offsets (0 = directly after the previous entry).
func parseDirectory(b []byte) (directory, error) {
	r := bytes.NewReader(b)
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errBadDirectory, err)
	}
	// Every entry needs at least four bytes (one per column); this also bounds the allocation.
	if n > uint64(len(b)) {
		return nil, fmt.Errorf("%w: %d entries in %d bytes", errBadDirectory, n, len(b))
	}
	d := make(directory, n)
	var last uint64
	for i := range d {
		delta, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, fmt.Errorf("%w: tile id: %v", errBadDirectory, err)
		}
		last += delta
		d[i].TileID = last
	}
	for i := range d {
		v, err := binary.ReadUvarint(r)
		if err != nil || v > 0xFFFFFFFF {
			return nil, fmt.Errorf("%w: run length", errBadDirectory)
		}
		d[i].RunLength = uint32(v)
	}
	for i := range d {
		v, err := binary.ReadUvarint(r)
		if err != nil || v > 0xFFFFFFFF {
			return nil, fmt.Errorf("%w: length", errBadDirectory)
		}
		d[i].Length = uint32(v)
	}
	for i := range d {
		v, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, fmt.Errorf("%w: offset", errBadDirectory)
		}
		switch {
		case v == 0 && i > 0:
			d[i].Offset = d[i-1].Offset + uint64(d[i-1].Length)
		case v == 0:
			return nil, fmt.Errorf("%w: first entry has offset 0 (contiguous) with no predecessor", errBadDirectory)
		default:
			d[i].Offset = v - 1
		}
	}
	return d, nil
}

// find returns the entry that covers id: the last entry with TileID <= id. For a tile entry
// (RunLength > 0) id must lie inside its run; a leaf pointer (RunLength == 0) is returned as is.
func (d directory) find(id uint64) (entry, bool) {
	i := sort.Search(len(d), func(i int) bool { return d[i].TileID > id })
	if i == 0 {
		return entry{}, false
	}
	e := d[i-1]
	if e.RunLength == 0 {
		return e, true
	}
	if id < e.TileID+uint64(e.RunLength) {
		return e, true
	}
	return entry{}, false
}

// encodeDirectory is the inverse of parseDirectory; entries must be sorted by TileID. Used by
// the test fixture writer.
func encodeDirectory(d directory) []byte {
	var buf bytes.Buffer
	put := func(v uint64) {
		var tmp [binary.MaxVarintLen64]byte
		buf.Write(tmp[:binary.PutUvarint(tmp[:], v)])
	}
	put(uint64(len(d)))
	var last uint64
	for _, e := range d {
		put(e.TileID - last)
		last = e.TileID
	}
	for _, e := range d {
		put(uint64(e.RunLength))
	}
	for _, e := range d {
		put(uint64(e.Length))
	}
	for i, e := range d {
		if i > 0 && e.Offset == d[i-1].Offset+uint64(d[i-1].Length) {
			put(0)
		} else {
			put(e.Offset + 1)
		}
	}
	return buf.Bytes()
}
