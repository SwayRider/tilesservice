package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	jwt5 "github.com/golang-jwt/jwt/v5"
	"github.com/swayrider/swlib/jwt"
	"github.com/swayrider/swlib/jwtkeys"
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

// fakeKeyCache is a test double for KeyCache backed by the given public
// keys. Verify delegates to jwt.VerifyToken, matching the production path
// taken by *jwtkeys.Cache.
type fakeKeyCache struct {
	keys []string
}

func (f *fakeKeyCache) Keys() []string {
	return f.keys
}

func (f *fakeKeyCache) Verify(token string) (*jwt.Claims, error) {
	var (
		claims  *jwt.Claims
		lastErr error
	)
	for _, key := range f.keys {
		claims, lastErr = jwt.VerifyToken(token, key, jwt.VerifyDefault)
		if lastErr == nil {
			return claims, nil
		}
	}
	return nil, lastErr
}

// freePort reserves a TCP port and returns its number, releasing it so the
// server under test can bind it.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("failed to release port: %v", err)
	}
	return port
}

// httpGetWithRetry issues a GET request, retrying briefly while the server
// goroutine binds the listener. An optional Authorization header is attached
// so requests can reach past the Bearer-scheme check in the auth middleware.
func httpGetWithRetry(t *testing.T, port int, path, authHeader string) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	var resp *http.Response
	var err error
	for i := 0; i < 50; i++ {
		req, reqErr := http.NewRequest(http.MethodGet, url, nil)
		if reqErr != nil {
			t.Fatalf("failed to build request: %v", reqErr)
		}
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		resp, err = client.Do(req)
		if err == nil {
			return resp
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("GET %s failed: %v", path, err)
	return nil
}

// TestInitializeTileIndex_EmptyPath verifies that an unconfigured TILES_PATH
// (the default) leaves the tile index unset instead of panicking.
func TestInitializeTileIndex_EmptyPath(t *testing.T) {
	application := newApp()
	// Config is not parsed, so TILES_PATH resolves to its zero value.
	if err := initializeTileIndex(application); err != nil {
		t.Fatalf("initializeTileIndex() error = %v", err)
	}
	if application.AppData("TileIndex") != nil {
		t.Error("TileIndex should not be set when TILES_PATH is empty")
	}
}

// TestServerLifecycle drives the real startup/shutdown surface: config parse,
// tile index initialization, HTTP server start (public ping, auth-required
// endpoints returning 503 while no JWT keys are loaded), and graceful stop.
func TestServerLifecycle(t *testing.T) {
	tilesDir := t.TempDir()
	stylesDir := t.TempDir()
	diskCacheDir := t.TempDir()
	port := freePort(t)

	t.Setenv("HTTP_PORT", strconv.Itoa(port))
	t.Setenv("TILES_PATH", tilesDir)
	t.Setenv("STYLES_PATH", stylesDir)
	t.Setenv("COMPRESSION_ENABLED", "true")
	t.Setenv("COMPRESSION_CACHE_SIZE", "16")
	t.Setenv("DISK_CACHE_ENABLED", "true")
	t.Setenv("DISK_CACHE_PATH", diskCacheDir)
	t.Setenv("DISK_CACHE_MAX_FILES", "100")
	t.Setenv("SERVICE_HOST", "http://localhost")
	t.Setenv("SERVICE_PREFIX", "/v1/tiles")
	t.Setenv("LOG_LEVEL", "error")

	application := newApp()
	if err := application.Config().Parse(); err != nil {
		t.Fatalf("config parse: %v", err)
	}

	// startHTTPServer requires the JWT key cache app data (empty cache =>
	// authservice is effectively down; protected endpoints must return 503).
	application.SetAppData("JWTKeyCache", jwtkeys.New(application.Logger()))

	// stopHTTPServer with no running server must be a no-op.
	stopHTTPServer(application)

	// initializeTileIndex: TILES_PATH is set, so the index must be registered.
	if err := initializeTileIndex(application); err != nil {
		t.Fatalf("initializeTileIndex() error = %v", err)
	}
	if application.AppData("TileIndex") == nil {
		t.Fatal("TileIndex app data not set")
	}

	// Start the HTTP server (memory + disk cache path).
	if err := startHTTPServer(application); err != nil {
		t.Fatalf("startHTTPServer() error = %v", err)
	}
	// stopHTTPServer is idempotent, so a cleanup hook is safe even though the
	// test also stops the server explicitly below.
	t.Cleanup(func() { stopHTTPServer(application) })

	// Public ping endpoint is reachable.
	resp := httpGetWithRetry(t, port, "/v1/tiles/ping", "")
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read ping body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("ping status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "ok") {
		t.Errorf("ping body = %q, want status ok", body)
	}

	// Protected endpoints return 503 while no JWT keys are loaded
	// (the authservice-down-at-boot path through the real server). The
	// Bearer header is required to get past the scheme check to the empty-key
	// branch of the middleware.
	for _, path := range []string{"/v1/tiles/base/0/0/0", "/v1/tiles/styles", "/v1/tiles/styles/light"} {
		resp := httpGetWithRetry(t, port, path, "Bearer dummy-token")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("GET %s status = %d, want 503 (no JWT keys loaded)", path, resp.StatusCode)
		}
	}

	// Graceful shutdown stops the server.
	stopHTTPServer(application)
	if resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/tiles/ping", port)); err == nil {
		defer func() { _ = resp.Body.Close() }()
		t.Error("expected connection error after shutdown")
	}
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
			keyCache := &fakeKeyCache{keys: tt.keys}

			probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
			})
			handler := requireTilesAuth(keyCache, probe)

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
