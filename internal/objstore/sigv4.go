package objstore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

// emptyPayloadHash is the hex SHA-256 of an empty body (all requests here are GET/HEAD).
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// sign adds AWS Signature Version 4 headers (x-amz-date, x-amz-content-sha256, Authorization)
// to a body-less request. The signed headers are host, range (when set), x-amz-content-sha256
// and x-amz-date. The request URL must already carry its final path and no query.
func sign(req *http.Request, accessKey, secretKey, region string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	date := amzDate[:8]
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", emptyPayloadHash)

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headers := map[string]string{
		"host":                 host,
		"x-amz-content-sha256": emptyPayloadHash,
		"x-amz-date":           amzDate,
	}
	if rng := req.Header.Get("Range"); rng != "" {
		headers["range"] = rng
	}
	names := make([]string, 0, len(headers))
	for n := range headers {
		names = append(names, n)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n + ":" + strings.TrimSpace(headers[n]) + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	canonical := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL.EscapedPath()),
		"", // no query string
		canonHeaders.String(),
		signedHeaders,
		emptyPayloadHash,
	}, "\n")

	scope := date + "/" + region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hexSHA256([]byte(canonical))

	key := hmacSHA256([]byte("AWS4"+secretKey), date)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+
		",SignedHeaders="+signedHeaders+",Signature="+signature)
}

// canonicalURI returns the already-escaped request path as SigV4 wants it for S3 (each
// segment escaped once, never double-escaped); an empty path is "/".
func canonicalURI(escaped string) string {
	if escaped == "" {
		return "/"
	}
	return escaped
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
