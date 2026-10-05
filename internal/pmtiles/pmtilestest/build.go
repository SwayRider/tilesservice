// Package pmtilestest builds small PMTiles v3 archives for tests. Its directory encoder is
// written independently of the reader's decoder on purpose, so the tests do not check the
// reader against itself.
package pmtilestest

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"sort"

	"github.com/swayrider/tilesservice/internal/pmtiles"
)

// Tile is one tile to store.
type Tile struct {
	Z    uint8
	X, Y uint32
	Data []byte
}

// Options controls the archive layout.
type Options struct {
	GzipDirectories bool                // internal compression: gzip instead of none
	TileCompression pmtiles.Compression // zero value = gzip (the Protomaps default)
	TileType        pmtiles.TileType    // zero value = MVT
	LeafSize        int                 // 0: everything in the root; n: directories hold at most n entries (nested as needed)
	RunLengths      bool                // merge consecutive tile ids with identical data into one entry
	Metadata        string              // metadata JSON, stored with the internal compression
}

type entry struct {
	id, offset  uint64
	length, run uint32
}

func uvarint(buf *bytes.Buffer, v uint64) {
	var tmp [binary.MaxVarintLen64]byte
	buf.Write(tmp[:binary.PutUvarint(tmp[:], v)])
}

func encode(es []entry) []byte {
	var b bytes.Buffer
	uvarint(&b, uint64(len(es)))
	var last uint64
	for _, e := range es {
		uvarint(&b, e.id-last)
		last = e.id
	}
	for _, e := range es {
		uvarint(&b, uint64(e.run))
	}
	for _, e := range es {
		uvarint(&b, uint64(e.length))
	}
	for i, e := range es {
		if i > 0 && e.offset == es[i-1].offset+uint64(es[i-1].length) {
			uvarint(&b, 0)
		} else {
			uvarint(&b, e.offset+1)
		}
	}
	return b.Bytes()
}

func gz(b []byte) []byte {
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	_, _ = w.Write(b)
	_ = w.Close()
	return out.Bytes()
}

// Gzip returns b gzip-compressed (handy for tile payloads).
func Gzip(b []byte) []byte { return gz(b) }

// Build returns the bytes of a PMTiles v3 archive holding the tiles. The file layout is
// header, root directory, metadata, leaf directories, tile data.
func Build(tiles []Tile, o Options) []byte {
	type item struct {
		id   uint64
		data []byte
	}
	items := make([]item, 0, len(tiles))
	minZ, maxZ := uint8(255), uint8(0)
	for _, t := range tiles {
		id, err := pmtiles.ZXYToID(t.Z, t.X, t.Y)
		if err != nil {
			panic(err)
		}
		items = append(items, item{id, t.Data})
		if t.Z < minZ {
			minZ = t.Z
		}
		if t.Z > maxZ {
			maxZ = t.Z
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].id < items[j].id })

	var tileData bytes.Buffer
	var es []entry
	var contents uint64
	for _, it := range items {
		if o.RunLengths && len(es) > 0 {
			last := &es[len(es)-1]
			if last.id+uint64(last.run) == it.id && bytes.Equal(it.data, tileData.Bytes()[last.offset:last.offset+uint64(last.length)]) {
				last.run++
				continue
			}
		}
		es = append(es, entry{id: it.id, offset: uint64(tileData.Len()), length: uint32(len(it.data)), run: 1})
		tileData.Write(it.data)
		contents++
	}
	tileEntries := uint64(len(es))

	pack := func(b []byte) []byte {
		if o.GzipDirectories {
			return gz(b)
		}
		return b
	}

	var leaves bytes.Buffer
	level := es
	if o.LeafSize > 0 {
		for len(level) > o.LeafSize {
			var next []entry
			for i := 0; i < len(level); i += o.LeafSize {
				end := i + o.LeafSize
				if end > len(level) {
					end = len(level)
				}
				enc := pack(encode(level[i:end]))
				next = append(next, entry{id: level[i].id, offset: uint64(leaves.Len()), length: uint32(len(enc)), run: 0})
				leaves.Write(enc)
			}
			level = next
		}
	}
	root := pack(encode(level))
	var meta []byte
	if o.Metadata != "" {
		meta = pack([]byte(o.Metadata))
	}

	tc := o.TileCompression
	if tc == pmtiles.CompressionUnknown {
		tc = pmtiles.CompressionGzip
	}
	tt := o.TileType
	if tt == pmtiles.TileTypeUnknown {
		tt = pmtiles.TileTypeMVT
	}
	ic := pmtiles.CompressionNone
	if o.GzipDirectories {
		ic = pmtiles.CompressionGzip
	}
	var addressed uint64
	for _, e := range es {
		addressed += uint64(e.run)
	}
	if len(tiles) == 0 {
		minZ = 0
	}
	h := pmtiles.Header{
		RootOffset: pmtiles.HeaderSize, RootLength: uint64(len(root)),
		MetadataOffset: pmtiles.HeaderSize + uint64(len(root)), MetadataLength: uint64(len(meta)),
		LeafDirectoriesOffset: pmtiles.HeaderSize + uint64(len(root)) + uint64(len(meta)), LeafDirectoriesLength: uint64(leaves.Len()),
		TileDataOffset: pmtiles.HeaderSize + uint64(len(root)) + uint64(len(meta)) + uint64(leaves.Len()), TileDataLength: uint64(tileData.Len()),
		AddressedTiles: addressed, TileEntries: tileEntries, TileContents: contents,
		Clustered: true, InternalCompression: ic, TileCompression: tc, TileType: tt,
		MinZoom: minZ, MaxZoom: maxZ,
		MinLonE7: -1800000000, MinLatE7: -850000000, MaxLonE7: 1800000000, MaxLatE7: 850000000,
		CenterZoom: minZ,
	}
	var out bytes.Buffer
	out.Write(h.Bytes())
	out.Write(root)
	out.Write(meta)
	out.Write(leaves.Bytes())
	out.Write(tileData.Bytes())
	return out.Bytes()
}
