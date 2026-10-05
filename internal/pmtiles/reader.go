package pmtiles

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
)

const (
	// maxTileSize rejects directory entries that claim an absurd tile size.
	maxTileSize = 32 << 20
	// maxLeafDepth is the deepest directory nesting a lookup follows (the format allows 3 levels).
	maxLeafDepth = 3
	// defaultCacheEntries is the directory cache budget in entries (~32 bytes each, ~64 MB).
	defaultCacheEntries = 2_000_000
)

// ErrTileNotFound is returned when the archive has no tile for the requested coordinates.
var ErrTileNotFound = errors.New("pmtiles: tile not found")

// Reader answers tile lookups on one PMTiles v3 archive. It is safe for concurrent use.
type Reader struct {
	ra     io.ReaderAt
	size   int64
	closer io.Closer
	header Header
	id     string
	root   directory

	cache *dirCache

	metaOnce sync.Once
	meta     []byte
	metaErr  error
}

// Option configures Open.
type Option func(*Reader, *int)

// WithCloser makes Reader.Close close c (for example the underlying file).
func WithCloser(c io.Closer) Option { return func(r *Reader, _ *int) { r.closer = c } }

// WithDirectoryCacheEntries sets the leaf directory cache budget in entries.
func WithDirectoryCacheEntries(n int) Option { return func(_ *Reader, e *int) { *e = n } }

// Open reads and validates the header and root directory of an archive of the given size.
// It refuses what tile serving cannot pass through: other tile types than MVT and tile
// compressions other than none/gzip, and files whose sections reach past the end (a
// truncated upload).
func Open(ra io.ReaderAt, size int64, opts ...Option) (*Reader, error) {
	r := &Reader{ra: ra, size: size}
	entries := defaultCacheEntries
	for _, o := range opts {
		o(r, &entries)
	}
	fail := func(err error) (*Reader, error) {
		if r.closer != nil {
			_ = r.closer.Close()
		}
		return nil, err
	}

	hb, err := readFull(ra, 0, HeaderSize)
	if err != nil {
		return fail(fmt.Errorf("pmtiles: reading header: %w", err))
	}
	h, err := ParseHeader(hb)
	if err != nil {
		return fail(err)
	}
	if h.TileType != TileTypeMVT {
		return fail(fmt.Errorf("pmtiles: tile type %d is not MVT", h.TileType))
	}
	if h.TileCompression != CompressionNone && h.TileCompression != CompressionGzip {
		return fail(fmt.Errorf("pmtiles: tile compression %s is not supported (none or gzip)", h.TileCompression))
	}
	if h.InternalCompression != CompressionNone && h.InternalCompression != CompressionGzip {
		return fail(fmt.Errorf("pmtiles: internal compression %s is not supported (none or gzip)", h.InternalCompression))
	}
	for name, sec := range map[string][2]uint64{
		"root directory": {h.RootOffset, h.RootLength}, "metadata": {h.MetadataOffset, h.MetadataLength},
		"leaf directories": {h.LeafDirectoriesOffset, h.LeafDirectoriesLength}, "tile data": {h.TileDataOffset, h.TileDataLength},
	} {
		if sec[0] > uint64(size) || sec[1] > uint64(size)-sec[0] {
			return fail(fmt.Errorf("pmtiles: %s (offset %d, length %d) lies outside the %d byte file (truncated?)", name, sec[0], sec[1], size))
		}
	}
	if h.RootLength == 0 || h.RootLength > maxDecodedBlock {
		return fail(fmt.Errorf("pmtiles: invalid root directory length %d", h.RootLength))
	}

	rb, err := readFull(ra, int64(h.RootOffset), int(h.RootLength))
	if err != nil {
		return fail(fmt.Errorf("pmtiles: reading root directory: %w", err))
	}
	plain, err := decompress(rb, h.InternalCompression)
	if err != nil {
		return fail(fmt.Errorf("pmtiles: root directory: %w", err))
	}
	if r.root, err = parseDirectory(plain); err != nil {
		return fail(err)
	}

	sum := sha256.New()
	sum.Write(hb)
	sum.Write(rb)
	r.id = hex.EncodeToString(sum.Sum(nil))[:16]
	r.header = h
	r.cache = newDirCache(entries)
	return r, nil
}

// Header returns the decoded archive header.
func (r *Reader) Header() Header { return r.header }

// ID identifies this archive's content for cache validators: a hash over the header and root
// directory, so any different build gets a different id.
func (r *Reader) ID() string { return r.id }

// Close releases the underlying source if a closer was given.
func (r *Reader) Close() error {
	if r.closer != nil {
		return r.closer.Close()
	}
	return nil
}

// Metadata returns the archive's metadata JSON (decompressed); it is read once.
func (r *Reader) Metadata() ([]byte, error) {
	r.metaOnce.Do(func() {
		if r.header.MetadataLength == 0 {
			return
		}
		b, err := readFull(r.ra, int64(r.header.MetadataOffset), int(r.header.MetadataLength))
		if err != nil {
			r.metaErr = fmt.Errorf("pmtiles: reading metadata: %w", err)
			return
		}
		r.meta, r.metaErr = decompress(b, r.header.InternalCompression)
	})
	return r.meta, r.metaErr
}

// Tile returns the stored bytes of a tile (still compressed as Header().TileCompression says),
// or ErrTileNotFound.
func (r *Reader) Tile(z uint8, x, y uint32) ([]byte, error) {
	id, err := ZXYToID(z, x, y)
	if err != nil {
		return nil, ErrTileNotFound
	}
	dir := r.root
	for depth := 0; depth <= maxLeafDepth; depth++ {
		e, ok := dir.find(id)
		if !ok {
			return nil, ErrTileNotFound
		}
		if e.RunLength > 0 {
			return r.readTile(e)
		}
		if dir, err = r.leaf(e); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("pmtiles: leaf directories nested deeper than %d", maxLeafDepth)
}

func (r *Reader) readTile(e entry) ([]byte, error) {
	if e.Length == 0 || e.Length > maxTileSize || e.Offset > r.header.TileDataLength || uint64(e.Length) > r.header.TileDataLength-e.Offset {
		return nil, fmt.Errorf("pmtiles: tile entry (offset %d, length %d) outside the tile data section", e.Offset, e.Length)
	}
	b, err := readFull(r.ra, int64(r.header.TileDataOffset+e.Offset), int(e.Length))
	if err != nil {
		return nil, fmt.Errorf("pmtiles: reading tile: %w", err)
	}
	return b, nil
}

// leaf returns the decoded leaf directory an entry points at, from the cache or the source;
// concurrent requests for the same leaf share one read.
func (r *Reader) leaf(e entry) (directory, error) {
	if e.Length == 0 || e.Length > maxDecodedBlock || e.Offset > r.header.LeafDirectoriesLength || uint64(e.Length) > r.header.LeafDirectoriesLength-e.Offset {
		return nil, fmt.Errorf("pmtiles: leaf directory (offset %d, length %d) outside the leaf section", e.Offset, e.Length)
	}
	return r.cache.get(e.Offset, func() (directory, error) {
		b, err := readFull(r.ra, int64(r.header.LeafDirectoriesOffset+e.Offset), int(e.Length))
		if err != nil {
			return nil, fmt.Errorf("pmtiles: reading leaf directory: %w", err)
		}
		plain, err := decompress(b, r.header.InternalCompression)
		if err != nil {
			return nil, fmt.Errorf("pmtiles: leaf directory: %w", err)
		}
		return parseDirectory(plain)
	})
}

// readFull reads exactly n bytes at off (io.ReaderAt may return io.EOF together with a full read).
func readFull(ra io.ReaderAt, off int64, n int) ([]byte, error) {
	buf := make([]byte, n)
	got, err := ra.ReadAt(buf, off)
	if got == n {
		return buf, nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return nil, err
}

// dirCache is an LRU of decoded leaf directories bounded by the total number of entries, with
// per-key single flight.
type dirCache struct {
	mu       sync.Mutex
	budget   int
	used     int
	order    *list.List // front = most recently used; values are *cacheItem
	items    map[uint64]*list.Element
	inflight map[uint64]*flight
}

type cacheItem struct {
	key uint64
	dir directory
}

type flight struct {
	wg  sync.WaitGroup
	dir directory
	err error
}

func newDirCache(budget int) *dirCache {
	return &dirCache{budget: budget, order: list.New(), items: map[uint64]*list.Element{}, inflight: map[uint64]*flight{}}
}

func (c *dirCache) get(key uint64, load func() (directory, error)) (directory, error) {
	c.mu.Lock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		d := el.Value.(*cacheItem).dir
		c.mu.Unlock()
		return d, nil
	}
	if f, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		f.wg.Wait()
		return f.dir, f.err
	}
	f := &flight{}
	f.wg.Add(1)
	c.inflight[key] = f
	c.mu.Unlock()

	f.dir, f.err = load()

	c.mu.Lock()
	delete(c.inflight, key)
	if f.err == nil {
		c.items[key] = c.order.PushFront(&cacheItem{key: key, dir: f.dir})
		c.used += len(f.dir)
		for c.used > c.budget && c.order.Len() > 1 {
			oldest := c.order.Back()
			it := oldest.Value.(*cacheItem)
			c.order.Remove(oldest)
			delete(c.items, it.key)
			c.used -= len(it.dir)
		}
	}
	c.mu.Unlock()
	f.wg.Done()
	return f.dir, f.err
}
