package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/swayrider/tilesservice/internal/objstore"
	"github.com/swayrider/tilesservice/internal/pmtiles"
	"github.com/swayrider/tilesservice/internal/tilesource"
)

// MaxFileSize bounds a single small file (manifest, style, glyph range, sprite) read into memory.
const MaxFileSize = 32 << 20

// PointerFile is the object that names the active release in an object store.
const PointerFile = "current.json"

// pointerSchema is the schema of PointerFile this code reads.
const pointerSchema = 1

// Location is one release inside a store. Paths are relative to the release and are validated by
// the caller (ValidRel).
type Location interface {
	// ReadFile returns a small file of the release.
	ReadFile(ctx context.Context, rel string) ([]byte, error)
	// OpenArchive opens a PMTiles archive of the release.
	OpenArchive(ctx context.Context, rel string) (*pmtiles.Reader, error)
}

// Store finds the active release.
type Store interface {
	// Current resolves the active release. The key identifies it: it changes when the pointer
	// moves, and equal keys mean the same release.
	Current(ctx context.Context) (key string, loc Location, err error)
}

// OpenStore opens a store from a location: a directory (path or file:///path) holding a `current`
// symlink and the releases it points to, or s3://bucket[/prefix] holding current.json.
func OpenStore(location string, s3 tilesource.S3) (Store, error) {
	if strings.HasPrefix(location, "s3://") {
		u, err := url.Parse(location)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("release: %q is not a valid s3://bucket[/prefix] location", location)
		}
		if s3.Endpoint == "" {
			return nil, fmt.Errorf("release: %q needs S3_ENDPOINT", location)
		}
		client, err := objstore.NewClient(objstore.Config{Endpoint: s3.Endpoint, Region: s3.Region, AccessKey: s3.AccessKey, SecretKey: s3.SecretKey})
		if err != nil {
			return nil, err
		}
		base := strings.Trim(u.Path, "/")
		if base != "" {
			base += "/"
		}
		return &s3Store{client: client, bucket: u.Host, base: base}, nil
	}
	root := strings.TrimPrefix(location, "file://")
	if strings.HasPrefix(location, "file://") && !strings.HasPrefix(root, "/") {
		return nil, fmt.Errorf("release: %q must be file:///absolute/path", location)
	}
	if root == "" {
		return nil, errors.New("release: empty store location")
	}
	return &fileStore{root: root}, nil
}

// --- file backend -------------------------------------------------------------------------

type fileStore struct{ root string }

// Current follows <root>/current (a symlink to the release directory, or a plain directory).
func (s *fileStore) Current(context.Context) (string, Location, error) {
	dir, err := filepath.EvalSymlinks(filepath.Join(s.root, "current"))
	if err != nil {
		return "", nil, fmt.Errorf("release: cannot resolve %s: %w", filepath.Join(s.root, "current"), err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		return "", nil, fmt.Errorf("release: %w", err)
	}
	if !st.IsDir() {
		return "", nil, fmt.Errorf("release: %s is not a directory", dir)
	}
	return dir, fileLoc{dir: dir}, nil
}

type fileLoc struct{ dir string }

func (l fileLoc) ReadFile(_ context.Context, rel string) ([]byte, error) {
	if err := ValidRel(rel); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(l.dir, filepath.FromSlash(rel)))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readLimited(f, MaxFileSize)
}

func (l fileLoc) OpenArchive(_ context.Context, rel string) (*pmtiles.Reader, error) {
	if err := ValidRel(rel); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(l.dir, filepath.FromSlash(rel)))
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	r, err := pmtiles.Open(f, st.Size(), pmtiles.WithCloser(f))
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return r, nil
}

// --- object-store backend -----------------------------------------------------------------

type s3Store struct {
	client *objstore.Client
	bucket string
	base   string // key prefix inside the bucket, "" or "x/y/"
}

// pointer is the content of current.json.
type pointer struct {
	Schema  int    `json:"schema"`
	Release string `json:"release"`
	Prefix  string `json:"prefix"`
}

// Current reads current.json; its prefix is the key prefix of the active release.
func (s *s3Store) Current(ctx context.Context) (string, Location, error) {
	data, err := readObject(ctx, s.client, s.bucket, s.base+PointerFile)
	if err != nil {
		return "", nil, err
	}
	var p pointer
	if err := json.Unmarshal(data, &p); err != nil {
		return "", nil, fmt.Errorf("release: %s is not valid JSON: %w", PointerFile, err)
	}
	if p.Schema != pointerSchema {
		return "", nil, fmt.Errorf("release: %s schema %d is not supported (want %d)", PointerFile, p.Schema, pointerSchema)
	}
	prefix := strings.Trim(p.Prefix, "/")
	if prefix == "" || ValidRel(prefix) != nil {
		return "", nil, fmt.Errorf("release: %s has an invalid prefix %q", PointerFile, p.Prefix)
	}
	full := s.base + prefix + "/"
	return "s3://" + s.bucket + "/" + full, &s3Loc{client: s.client, bucket: s.bucket, prefix: full}, nil
}

type s3Loc struct {
	client *objstore.Client
	bucket string
	prefix string
}

func (l *s3Loc) ReadFile(ctx context.Context, rel string) ([]byte, error) {
	if err := ValidRel(rel); err != nil {
		return nil, err
	}
	return readObject(ctx, l.client, l.bucket, l.prefix+rel)
}

func (l *s3Loc) OpenArchive(ctx context.Context, rel string) (*pmtiles.Reader, error) {
	if err := ValidRel(rel); err != nil {
		return nil, err
	}
	obj, err := l.client.Open(ctx, l.bucket, l.prefix+rel)
	if err != nil {
		return nil, err
	}
	return pmtiles.Open(obj, obj.Size(), pmtiles.WithCloser(obj))
}

// readObject reads a whole (small) object.
func readObject(ctx context.Context, c *objstore.Client, bucket, key string) ([]byte, error) {
	obj, err := c.Open(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	if obj.Size() > MaxFileSize {
		return nil, fmt.Errorf("release: s3://%s/%s is %s bytes, larger than the %d byte limit", bucket, key, itoa(obj.Size()), MaxFileSize)
	}
	buf := make([]byte, obj.Size())
	n, err := obj.ReadAt(buf, 0)
	if err != nil && !(errors.Is(err, io.EOF) && n == len(buf)) {
		return nil, fmt.Errorf("release: reading s3://%s/%s: %w", bucket, key, err)
	}
	return buf, nil
}

// readLimited reads at most limit bytes and fails when the source is longer.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("release: file is larger than the %d byte limit", limit)
	}
	return data, nil
}
