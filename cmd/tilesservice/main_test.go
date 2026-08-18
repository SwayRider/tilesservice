package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	jwt5 "github.com/golang-jwt/jwt/v5"
	"github.com/swayrider/swlib/jwt"
)

// TestMain pins the JWT issuer/audience the middleware verifies against
// (matching the package defaults used by the running service).
func TestMain(m *testing.M) {
	jwt.Configure("SwayRider", "SwayRider")
	os.Exit(m.Run())
}

// testKeyPair generates an RSA key pair and returns the private key PEM
// (for signing) and public key PEM (for verification).
func testKeyPair(t *testing.T) (privPEM, pubPEM string) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	privPEM = string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(priv),
	}))

	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("failed to marshal public key: %v", err)
	}
	pubPEM = string(pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubDER,
	}))

	return privPEM, pubPEM
}

// signToken signs a JWT with the given SwayRider claims.
func signToken(t *testing.T, privPEM string, claims jwt.SwayRiderClaims) string {
	t.Helper()

	_, token, _, err := jwt.GenerateToken("test-subject", nil, claims, privPEM, 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}
	return string(token)
}

// setJWTKeys replaces the cached JWT public keys for the duration of a test.
func setJWTKeys(t *testing.T, keys ...string) {
	t.Helper()

	jwtKeyCache.mu.Lock()
	old := jwtKeyCache.keys
	jwtKeyCache.keys = keys
	jwtKeyCache.mu.Unlock()

	t.Cleanup(func() {
		jwtKeyCache.mu.Lock()
		jwtKeyCache.keys = old
		jwtKeyCache.mu.Unlock()
	})
}

// TestRequireTilesAuth exercises the auth middleware: malformed requests are
// rejected, only service client tokens with the tiles:serve scope pass, and a
// missing key cache yields 503.
func TestRequireTilesAuth(t *testing.T) {
	privPEM, pubPEM := testKeyPair(t)
	otherPrivPEM, _ := testKeyPair(t)

	svcOK := signToken(t, privPEM, jwt.NewSwayRiderServiceClaims(jwt5.ClaimStrings{"tiles:serve"}))
	svcWrongScope := signToken(t, privPEM, jwt.NewSwayRiderServiceClaims(jwt5.ClaimStrings{"mail:send"}))
	userToken := signToken(t, privPEM, jwt.NewSwayRiderUserClaims(false, "standard"))
	wrongKeyToken := signToken(t, otherPrivPEM, jwt.NewSwayRiderServiceClaims(jwt5.ClaimStrings{"tiles:serve"}))

	tests := []struct {
		name       string
		keys       []string
		authHeader string
		wantStatus int
	}{
		{"no authorization header", []string{pubPEM}, "", http.StatusUnauthorized},
		{"non-bearer scheme", []string{pubPEM}, "Basic dXNlcjpwYXNz", http.StatusUnauthorized},
		{"malformed bearer token", []string{pubPEM}, "Bearer not.a.jwt", http.StatusUnauthorized},
		{"service token with tiles:serve scope", []string{pubPEM}, "Bearer " + svcOK, http.StatusOK},
		{"service token without tiles:serve scope", []string{pubPEM}, "Bearer " + svcWrongScope, http.StatusForbidden},
		{"user jwt rejected", []string{pubPEM}, "Bearer " + userToken, http.StatusForbidden},
		{"token signed with unknown key", []string{pubPEM}, "Bearer " + wrongKeyToken, http.StatusUnauthorized},
		{"no keys loaded", nil, "Bearer " + svcOK, http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setJWTKeys(t, tt.keys...)

			probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
			})
			handler := requireTilesAuth(probe)

			req := httptest.NewRequest(http.MethodGet, "/v1/tiles/base/10/512/384", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body: %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}
