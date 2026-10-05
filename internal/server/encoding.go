package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// maxDecodedTileSize bounds the gunzip of a stored tile (an MVT tile is far smaller).
const maxDecodedTileSize = 16 << 20

// acceptsGzip reports whether the request's Accept-Encoding allows a gzip
// response: a "gzip" (or "*") token whose q-value is not 0.
func acceptsGzip(r *http.Request) bool {
	var gzipSeen, gzipOK, starOK bool
	for _, header := range r.Header.Values("Accept-Encoding") {
		for _, part := range strings.Split(header, ",") {
			token, params, _ := strings.Cut(strings.TrimSpace(part), ";")
			switch strings.ToLower(strings.TrimSpace(token)) {
			case "gzip":
				gzipSeen, gzipOK = true, !qValueIsZero(params)
			case "*":
				starOK = !qValueIsZero(params)
			}
		}
	}
	if gzipSeen {
		return gzipOK // an explicit gzip entry beats the wildcard
	}
	return starOK
}

// qValueIsZero reports whether the parameters of an encoding token contain q=0.
func qValueIsZero(params string) bool {
	for _, p := range strings.Split(params, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "q") {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return err == nil && q == 0
	}
	return false
}

// gunzip decompresses a stored gzip tile, refusing output above maxDecodedTileSize.
func gunzip(data []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	out, err := io.ReadAll(io.LimitReader(zr, maxDecodedTileSize+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxDecodedTileSize {
		return nil, io.ErrUnexpectedEOF
	}
	return out, nil
}
