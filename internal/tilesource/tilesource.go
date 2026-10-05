// Package tilesource opens a PMTiles archive from a location string: a local path, a file:// URL
// or an s3://bucket/key object in an S3-compatible store.
package tilesource

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/swayrider/tilesservice/internal/objstore"
	"github.com/swayrider/tilesservice/internal/pmtiles"
)

// S3 holds the settings used for s3:// locations.
type S3 struct {
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
}

// Open returns a Reader for the archive at location. Close the Reader when done.
func Open(ctx context.Context, location string, s3 S3) (*pmtiles.Reader, error) {
	switch {
	case strings.HasPrefix(location, "s3://"):
		u, err := url.Parse(location)
		bucket, key := "", ""
		if err == nil {
			bucket, key = u.Host, strings.TrimPrefix(u.Path, "/")
		}
		if err != nil || bucket == "" || key == "" {
			return nil, fmt.Errorf("tilesource: %q is not a valid s3://bucket/key location", location)
		}
		if s3.Endpoint == "" {
			return nil, fmt.Errorf("tilesource: %q needs S3_ENDPOINT", location)
		}
		client, err := objstore.NewClient(objstore.Config{Endpoint: s3.Endpoint, Region: s3.Region, AccessKey: s3.AccessKey, SecretKey: s3.SecretKey})
		if err != nil {
			return nil, err
		}
		obj, err := client.Open(ctx, bucket, key)
		if err != nil {
			return nil, err
		}
		return pmtiles.Open(obj, obj.Size(), pmtiles.WithCloser(obj))
	default:
		path := strings.TrimPrefix(location, "file://")
		if strings.HasPrefix(location, "file://") && !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("tilesource: %q must be file:///absolute/path", location)
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("tilesource: %w", err)
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("tilesource: %w", err)
		}
		return pmtiles.Open(f, st.Size(), pmtiles.WithCloser(f))
	}
}
