// Package pmtiles reads PMTiles v3 archives (https://github.com/protomaps/PMTiles) through an
// io.ReaderAt, so the same code serves a local file and an object in an S3-compatible store
// (every ReadAt is one ranged GET there). Only what tile serving needs is implemented: header,
// directories, tile lookup and the metadata JSON.
package pmtiles

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// HeaderSize is the fixed size of a PMTiles v3 header.
const HeaderSize = 127

// Compression is the PMTiles compression enum (header bytes 97 and 98).
type Compression uint8

const (
	CompressionUnknown Compression = 0
	CompressionNone    Compression = 1
	CompressionGzip    Compression = 2
	CompressionBrotli  Compression = 3
	CompressionZstd    Compression = 4
)

func (c Compression) String() string {
	switch c {
	case CompressionNone:
		return "none"
	case CompressionGzip:
		return "gzip"
	case CompressionBrotli:
		return "brotli"
	case CompressionZstd:
		return "zstd"
	}
	return "unknown"
}

// TileType is the PMTiles tile type enum (header byte 99).
type TileType uint8

const (
	TileTypeUnknown TileType = 0
	TileTypeMVT     TileType = 1
	TileTypePNG     TileType = 2
	TileTypeJPEG    TileType = 3
	TileTypeWebP    TileType = 4
	TileTypeAVIF    TileType = 5
)

// Header is the decoded 127-byte PMTiles v3 header.
type Header struct {
	RootOffset, RootLength                       uint64
	MetadataOffset, MetadataLength               uint64
	LeafDirectoriesOffset, LeafDirectoriesLength uint64
	TileDataOffset, TileDataLength               uint64
	AddressedTiles, TileEntries                  uint64
	TileContents                                 uint64
	Clustered                                    bool
	InternalCompression                          Compression
	TileCompression                              Compression
	TileType                                     TileType
	MinZoom, MaxZoom                             uint8
	MinLonE7, MinLatE7                           int32
	MaxLonE7, MaxLatE7                           int32
	CenterZoom                                   uint8
	CenterLonE7, CenterLatE7                     int32
}

var (
	// ErrBadMagic is returned when the file does not start with "PMTiles".
	ErrBadMagic = errors.New("pmtiles: not a PMTiles file")
	// ErrUnsupportedVersion is returned for any version other than 3.
	ErrUnsupportedVersion = errors.New("pmtiles: unsupported version (only v3)")
)

// ParseHeader decodes the first HeaderSize bytes of a PMTiles file.
func ParseHeader(b []byte) (Header, error) {
	var h Header
	if len(b) < HeaderSize {
		return h, fmt.Errorf("pmtiles: header needs %d bytes, got %d", HeaderSize, len(b))
	}
	if string(b[0:7]) != "PMTiles" {
		return h, ErrBadMagic
	}
	if b[7] != 3 {
		return h, fmt.Errorf("%w: %d", ErrUnsupportedVersion, b[7])
	}
	le := binary.LittleEndian
	h.RootOffset, h.RootLength = le.Uint64(b[8:]), le.Uint64(b[16:])
	h.MetadataOffset, h.MetadataLength = le.Uint64(b[24:]), le.Uint64(b[32:])
	h.LeafDirectoriesOffset, h.LeafDirectoriesLength = le.Uint64(b[40:]), le.Uint64(b[48:])
	h.TileDataOffset, h.TileDataLength = le.Uint64(b[56:]), le.Uint64(b[64:])
	h.AddressedTiles, h.TileEntries, h.TileContents = le.Uint64(b[72:]), le.Uint64(b[80:]), le.Uint64(b[88:])
	h.Clustered = b[96] == 1
	h.InternalCompression, h.TileCompression = Compression(b[97]), Compression(b[98])
	h.TileType = TileType(b[99])
	h.MinZoom, h.MaxZoom = b[100], b[101]
	h.MinLonE7, h.MinLatE7 = int32(le.Uint32(b[102:])), int32(le.Uint32(b[106:]))
	h.MaxLonE7, h.MaxLatE7 = int32(le.Uint32(b[110:])), int32(le.Uint32(b[114:]))
	h.CenterZoom = b[118]
	h.CenterLonE7, h.CenterLatE7 = int32(le.Uint32(b[119:])), int32(le.Uint32(b[123:]))
	return h, nil
}

// Bytes encodes the header (used by the test fixture writer).
func (h Header) Bytes() []byte {
	b := make([]byte, HeaderSize)
	copy(b, "PMTiles")
	b[7] = 3
	le := binary.LittleEndian
	le.PutUint64(b[8:], h.RootOffset)
	le.PutUint64(b[16:], h.RootLength)
	le.PutUint64(b[24:], h.MetadataOffset)
	le.PutUint64(b[32:], h.MetadataLength)
	le.PutUint64(b[40:], h.LeafDirectoriesOffset)
	le.PutUint64(b[48:], h.LeafDirectoriesLength)
	le.PutUint64(b[56:], h.TileDataOffset)
	le.PutUint64(b[64:], h.TileDataLength)
	le.PutUint64(b[72:], h.AddressedTiles)
	le.PutUint64(b[80:], h.TileEntries)
	le.PutUint64(b[88:], h.TileContents)
	if h.Clustered {
		b[96] = 1
	}
	b[97], b[98], b[99] = byte(h.InternalCompression), byte(h.TileCompression), byte(h.TileType)
	b[100], b[101] = h.MinZoom, h.MaxZoom
	le.PutUint32(b[102:], uint32(h.MinLonE7))
	le.PutUint32(b[106:], uint32(h.MinLatE7))
	le.PutUint32(b[110:], uint32(h.MaxLonE7))
	le.PutUint32(b[114:], uint32(h.MaxLatE7))
	b[118] = h.CenterZoom
	le.PutUint32(b[119:], uint32(h.CenterLonE7))
	le.PutUint32(b[123:], uint32(h.CenterLatE7))
	return b
}
