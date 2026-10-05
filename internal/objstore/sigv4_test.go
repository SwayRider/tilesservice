package objstore

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The GET Object example of the AWS documentation ("Signature Calculations for the Authorization
// Header: Transferring Payload in a Single Chunk"): a ranged GET of test.txt, signed with the
// documented example credentials. The expected signature is the one AWS publishes.
func TestSignMatchesAWSDocumentationExample(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-9")
	now := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	sign(req, "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1", now)

	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request," +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date," +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization =\n %s\nwant\n %s", got, want)
	}
	if req.Header.Get("x-amz-date") != "20130524T000000Z" || req.Header.Get("x-amz-content-sha256") != emptyPayloadHash {
		t.Errorf("x-amz headers: %v", req.Header)
	}
}

func TestSignWithoutRangeSignsThreeHeaders(t *testing.T) {
	req, _ := http.NewRequest(http.MethodHead, "http://garage:3900/bucket/key", nil)
	sign(req, "AK", "SK", "garage", time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	if got := req.Header.Get("Authorization"); !strings.Contains(got, "SignedHeaders=host;x-amz-content-sha256;x-amz-date,") {
		t.Errorf("Authorization = %s", got)
	}
}

func TestEscapeSegment(t *testing.T) {
	tests := map[string]string{
		"releases":      "releases",
		"r 1":           "r%201",
		"a+b":           "a%2Bb",
		"tiles.pmtiles": "tiles.pmtiles",
		"é":             "%C3%A9",
		"a~b-c_d":       "a~b-c_d",
	}
	for in, want := range tests {
		if got := escapeSegment(in); got != want {
			t.Errorf("escapeSegment(%q) = %q, want %q", in, got, want)
		}
	}
	if got := escapePath("releases/r 1/tiles+x.pmtiles"); got != "releases/r%201/tiles%2Bx.pmtiles" {
		t.Errorf("escapePath = %q", got)
	}
}
