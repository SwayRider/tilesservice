// Package main implements the tilesservice binary.
//
// The tilesservice provides vector tile serving capabilities for the SwayRider
// platform. It serves Mapbox Vector Tiles (MVT) for map rendering.
//
// # Endpoints
//
// The ping endpoint is public. All other endpoints require a service client
// JWT carrying the "tiles:serve" scope; user JWTs are rejected (client
// requests go through swayrider-api, which injects its own service token):
//   - GET /v1/tiles/ping - Health check endpoint (public)
//   - GET /v1/tiles/{tileset}/{z}/{x}/{y} - Retrieve a vector tile (requires tiles:serve scope)
//   - GET /v1/tiles/styles - List map styles (requires tiles:serve scope)
//   - GET /v1/tiles/styles/{name} - Retrieve a map style (requires tiles:serve scope)
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/cors"
	"github.com/swayrider/grpcclients"
	"github.com/swayrider/grpcclients/authclient"
	"github.com/swayrider/swlib/app"
	"github.com/swayrider/swlib/jwt"
	"github.com/swayrider/swlib/jwtkeys"
	log "github.com/swayrider/swlib/logger"
	"github.com/swayrider/tilesservice/internal/server"
	"github.com/swayrider/tilesservice/internal/tilecache"
	"github.com/swayrider/tilesservice/internal/tileindex"
)

/*
Flags:

	-http-port				(default: 8080)
	-log-level				(default: info)
	-tiles-path				(default: "")
	-styles-path				(default: "")
	-compression-enabled		(default: true)
	-compression-cache-size		(default: 1000)
	-disk-cache-enabled		(default: false)
	-disk-cache-path		(default: "")
	-disk-cache-max-files		(default: 100000)
	-service-host			(default: "")
	-service-port			(default: "")
	-service-prefix			(default: "")
	-jwt-keys-refresh-interval-secs	(default: 300)
	-jwt-keys-fetch-timeout-secs	(default: 15)

Environment variables:

	HTTP_PORT
	LOG_LEVEL
	TILES_PATH
	STYLES_PATH
	COMPRESSION_ENABLED
	COMPRESSION_CACHE_SIZE
	DISK_CACHE_ENABLED
	DISK_CACHE_PATH
	DISK_CACHE_MAX_FILES
	SERVICE_HOST
	SERVICE_PORT
	SERVICE_PREFIX
	JWT_KEYS_REFRESH_INTERVAL_SECS
	JWT_KEYS_FETCH_TIMEOUT_SECS
*/

// Configuration field constants for the tiles and styles paths.
const (
	FldTilesPath = "tiles-path" // CLI flag name for tiles storage path
	EnvTilesPath = "TILES_PATH" // Environment variable name for tiles storage path
	DefTilesPath = ""           // Default tiles path (empty)

	FldStylesPath = "styles-path" // CLI flag name for styles directory path
	EnvStylesPath = "STYLES_PATH" // Environment variable name for styles directory path
	DefStylesPath = ""            // Default styles path (empty)

	FldCompressionEnabled   = "compression-enabled"    // CLI flag name for compression toggle
	EnvCompressionEnabled   = "COMPRESSION_ENABLED"    // Environment variable name for compression toggle
	DefCompressionEnabled   = true                     // Default compression enabled
	FldCompressionCacheSize = "compression-cache-size" // CLI flag name for cache size
	EnvCompressionCacheSize = "COMPRESSION_CACHE_SIZE" // Environment variable name for cache size
	DefCompressionCacheSize = 1000                     // Default cache size (tiles)

	FldDiskCacheEnabled  = "disk-cache-enabled"   // CLI flag name for disk cache toggle
	EnvDiskCacheEnabled  = "DISK_CACHE_ENABLED"   // Environment variable name for disk cache toggle
	DefDiskCacheEnabled  = false                  // Default disk cache disabled
	FldDiskCachePath     = "disk-cache-path"      // CLI flag name for disk cache path
	EnvDiskCachePath     = "DISK_CACHE_PATH"      // Environment variable name for disk cache path
	DefDiskCachePath     = ""                     // Default disk cache path (empty)
	FldDiskCacheMaxFiles = "disk-cache-max-files" // CLI flag name for disk cache max files
	EnvDiskCacheMaxFiles = "DISK_CACHE_MAX_FILES" // Environment variable name for disk cache max files
	DefDiskCacheMaxFiles = 100000                 // Default max files (100k)

	FldServiceHost   = "service-host"   // CLI flag name for the public service host
	EnvServiceHost   = "SERVICE_HOST"   // Environment variable name for the public service host
	DefServiceHost   = ""               // Default service host (empty)
	FldServicePort   = "service-port"   // CLI flag name for the public service port
	EnvServicePort   = "SERVICE_PORT"   // Environment variable name for the public service port
	DefServicePort   = ""               // Default service port (empty, omit for standard ports)
	FldServicePrefix = "service-prefix" // CLI flag name for the URL prefix appended after host:port
	EnvServicePrefix = "SERVICE_PREFIX" // Environment variable name for the URL prefix
	DefServicePrefix = ""               // Default service prefix (empty)
)

// httpServer holds the HTTP server instance for graceful shutdown.
var httpServer *http.Server

// KeyCache provides the JWT public keys used by requireTilesAuth. It is
// satisfied by *jwtkeys.Cache, which keeps the keys refreshed from
// authservice in the background (default 5-minute interval, retains the last
// known-good keys across failures).
type KeyCache interface {
	Keys() []string
	Verify(token string) (*jwt.Claims, error)
}

// requireTilesAuth is an HTTP middleware that validates a JWT and enforces the
// "tiles:serve" scope. Only service client tokens carrying tiles:serve are
// accepted; user JWTs are rejected (client requests go through swayrider-api,
// which injects its own service token).
func requireTilesAuth(keyCache KeyCache, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")

		if len(keyCache.Keys()) == 0 {
			http.Error(w, "service unavailable: no JWT keys loaded", http.StatusServiceUnavailable)
			return
		}

		claims, err := keyCache.Verify(token)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// Only service client tokens with the tiles:serve scope are accepted.
		// User JWTs must go through swayrider-api, which injects the service token.
		svcClaims, ok := claims.SwayRiderClaims.(*jwt.SwayRiderServiceClaims)
		if !ok || !hasTilesScope(svcClaims.Scopes) {
			http.Error(w, "forbidden: missing tiles:serve scope", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func hasTilesScope(scopes []string) bool {
	for _, s := range scopes {
		if s == "tiles:serve" {
			return true
		}
	}
	return false
}

// authServiceClientCtor creates a new auth service gRPC client used to fetch
// JWT public keys for token verification. The host/port are read from the
// authservice-host / authservice-port config (AUTHSERVICE_HOST / AUTHSERVICE_PORT).
func authServiceClientCtor(a app.App) grpcclients.Client {
	lg := a.Logger().Derive(log.WithFunction("authServiceClientCtor"))
	clnt, err := authclient.New(
		app.ServiceClientHostAndPort(a, "authservice"))
	if err != nil {
		lg.Fatalf("failed to create authservice client: %v", err)
	}
	return clnt
}

func main() {
	stdConfigFields :=
		app.BackendServiceFields |
			app.LoggerFields

	application := app.New("tilesservice")

	// Shared JWT public-key cache: refreshed from authservice by the
	// app.JWTKeysFetcher background routine (default 5-minute interval,
	// 15-second fetch timeout, retains last known-good keys on failure).
	jwtKeyCache := jwtkeys.New(application.Logger())

	application = application.
		WithDefaultConfigFields(stdConfigFields, app.FlagGroupOverrides{}).
		WithServiceClients(
			app.NewServiceClient("authservice", authServiceClientCtor),
		).
		WithConfigFields(
			app.NewStringConfigField(
				FldTilesPath, EnvTilesPath, "Path to the tiles storage directory", DefTilesPath),
			app.NewStringConfigField(
				FldStylesPath, EnvStylesPath, "Path to the map styles directory", DefStylesPath),
			app.NewBoolConfigField(
				FldCompressionEnabled, EnvCompressionEnabled, "Enable HTTP gzip compression", DefCompressionEnabled),
			app.NewIntConfigField(
				FldCompressionCacheSize, EnvCompressionCacheSize, "Maximum number of compressed tiles to cache", DefCompressionCacheSize),
			app.NewBoolConfigField(
				FldDiskCacheEnabled, EnvDiskCacheEnabled, "Enable disk cache layer", DefDiskCacheEnabled),
			app.NewStringConfigField(
				FldDiskCachePath, EnvDiskCachePath, "Path to disk cache directory", DefDiskCachePath),
			app.NewIntConfigField(
				FldDiskCacheMaxFiles, EnvDiskCacheMaxFiles, "Maximum number of cached files on disk", DefDiskCacheMaxFiles),
			app.NewStringConfigField(
				FldServiceHost, EnvServiceHost, "Public host of the tiles service (e.g. https://tiles.example.com)", DefServiceHost),
			app.NewStringConfigField(
				FldServicePort, EnvServicePort, "Public port of the tiles service (optional, omit for standard ports)", DefServicePort),
			app.NewStringConfigField(
				FldServicePrefix, EnvServicePrefix, "URL prefix appended after host:port (e.g. /v1/tiles)", DefServicePrefix),
		).
		WithConfigFields(app.JWTKeysConfigFields()...).
		WithAppData("JWTKeyCache", jwtKeyCache).
		WithInitializers(initializeTileIndex, app.JWTKeysInitializer(jwtKeyCache)).
		WithBackgroundRoutines(app.JWTKeysFetcher(jwtKeyCache)).
		WithHTTP(startHTTPServer, stopHTTPServer)

	application.Run()
}

// startHTTPServer creates and starts the HTTP server for tile serving.
func startHTTPServer(a app.App) error {
	lg := a.Logger().Derive(log.WithFunction("startHTTPServer"))
	httpPort := app.GetConfigField[int](a.Config(), app.KeyHttpPort)

	// Get tile index
	idx := app.GetAppData[*tileindex.TileIndex](a, "TileIndex")

	// Initialize compressed tile cache
	compressionEnabled := app.GetConfigField[bool](a.Config(), FldCompressionEnabled)
	compressionCacheSize := app.GetConfigField[int](a.Config(), FldCompressionCacheSize)

	// Create memory cache
	var memCache *tilecache.CompressedTileCache
	if compressionEnabled {
		memCache = tilecache.NewCompressedTileCache(compressionCacheSize, a.Logger())
		lg.Infof("compression enabled with cache size: %d tiles", compressionCacheSize)
	} else {
		memCache = tilecache.NewCompressedTileCache(0, a.Logger()) // Disabled cache
		lg.Infoln("compression disabled")
	}

	// Initialize tile cache (memory-only or two-tier)
	var tileCache tilecache.TileCache
	diskCacheEnabled := app.GetConfigField[bool](a.Config(), FldDiskCacheEnabled)

	if diskCacheEnabled {
		diskCachePath := app.GetConfigField[string](a.Config(), FldDiskCachePath)
		diskCacheMaxFiles := app.GetConfigField[int](a.Config(), FldDiskCacheMaxFiles)

		// Validate disk cache path
		if diskCachePath == "" {
			lg.Warnln("disk cache enabled but path not set, using memory only")
			tileCache = memCache
		} else {
			// Expand ~ to home directory
			if strings.HasPrefix(diskCachePath, "~/") {
				home, err := os.UserHomeDir()
				if err != nil {
					lg.Errorf("failed to get home directory: %v, using memory only", err)
					tileCache = memCache
				} else {
					diskCachePath = filepath.Join(home, diskCachePath[2:])
				}
			}

			if tileCache == nil {
				// Initialize disk cache
				diskCache, err := tilecache.NewDiskTileCache(diskCachePath, diskCacheMaxFiles, a.Logger())
				if err != nil {
					lg.Errorf("failed to initialize disk cache: %v, using memory only", err)
					tileCache = memCache
				} else {
					// Create two-tier cache
					tileCache = tilecache.NewTwoTierCache(memCache, diskCache, a.Logger())
					lg.Infof("disk cache enabled at %s with max %d files", diskCachePath, diskCacheMaxFiles)
					a.SetAppData("DiskCache", diskCache)
				}
			}
		}
	}

	if tileCache == nil {
		tileCache = memCache
		lg.Infoln("disk cache disabled, using memory cache only")
	}

	// Save memory cache for cleanup
	a.SetAppData("MemoryCache", memCache)

	// Resolve styles path (expand ~ to home directory)
	stylesPath := app.GetConfigField[string](a.Config(), FldStylesPath)
	if strings.HasPrefix(stylesPath, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			lg.Errorf("failed to get home directory for styles path: %v", err)
			stylesPath = ""
		} else {
			stylesPath = filepath.Join(home, stylesPath[2:])
		}
	}
	if stylesPath != "" {
		lg.Infof("styles directory: %s", stylesPath)
	} else {
		lg.Warnln("STYLES_PATH not configured, only default style names will be advertised")
	}

	// Construct tiles base URL for style templates
	serviceHost := app.GetConfigField[string](a.Config(), FldServiceHost)
	servicePort := app.GetConfigField[string](a.Config(), FldServicePort)
	servicePrefix := app.GetConfigField[string](a.Config(), FldServicePrefix)
	tilesBaseURL := serviceHost
	if servicePort != "" {
		tilesBaseURL += ":" + servicePort
	}
	tilesBaseURL += servicePrefix
	if tilesBaseURL != "" {
		lg.Infof("tiles base URL for styles: %s", tilesBaseURL)
	} else {
		lg.Warnln("SERVICE_HOST not configured, style tile URLs will be empty")
	}

	// JWT key cache — kept fresh by the app.JWTKeysFetcher background routine.
	keyCache := app.GetAppData[*jwtkeys.Cache](a, "JWTKeyCache")

	// Create HTTP handlers
	mux := http.NewServeMux()

	// Health check endpoint — public, no auth.
	mux.HandleFunc("GET /v1/tiles/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`{"status":"ok"}`)); err != nil {
			a.Logger().Debugf("failed to write ping response: %v", err)
		}
	})

	// Style endpoints — require a service JWT with the tiles:serve scope.
	// Registered before the tile wildcard route to ensure the static
	// /v1/tiles/styles prefix takes priority.
	styleHandler := server.NewStyleHTTPHandler(stylesPath, tilesBaseURL, a.Logger())
	mux.Handle("GET /v1/tiles/styles", requireTilesAuth(keyCache, styleHandler))
	mux.Handle("GET /v1/tiles/styles/{name}", requireTilesAuth(keyCache, styleHandler))

	// Tile endpoint — requires a service JWT with the tiles:serve scope.
	tileHandler := server.NewTileHTTPHandler(idx, tileCache, a.Logger())
	mux.Handle("GET /v1/tiles/{tileset}/{z}/{x}/{y}", requireTilesAuth(keyCache, tileHandler))

	// CORS middleware - allow all origins for development
	handler := cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "OPTIONS"},
		AllowedHeaders:   []string{"*"},
		AllowCredentials: false,
	}).Handler(mux)

	// Create HTTP server
	httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", httpPort),
		Handler: handler,
	}

	// Start server in goroutine
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			lg.Fatalf("HTTP server error: %v", err)
		}
	}()

	lg.Infof("HTTP server running on port: %d", httpPort)
	return nil
}

// stopHTTPServer gracefully shuts down the HTTP server.
func stopHTTPServer(a app.App) {
	lg := a.Logger().Derive(log.WithFunction("stopHTTPServer"))

	if httpServer == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		lg.Errorf("HTTP server shutdown error: %v", err)
	} else {
		lg.Infoln("HTTP server stopped")
	}

	// Close memory cache
	if memCache := app.GetAppData[*tilecache.CompressedTileCache](a, "MemoryCache"); memCache != nil {
		if err := memCache.Close(); err != nil {
			lg.Errorf("failed to close memory cache: %v", err)
		}
	}

	// Close disk cache
	if diskCache := app.GetAppData[*tilecache.DiskTileCache](a, "DiskCache"); diskCache != nil {
		if err := diskCache.Close(); err != nil {
			lg.Errorf("failed to close disk cache: %v", err)
		} else {
			lg.Infoln("disk cache closed")
		}
	}

	// Close tile index
	idx := app.GetAppData[*tileindex.TileIndex](a, "TileIndex")
	if idx != nil {
		if err := idx.Close(); err != nil {
			lg.Errorf("failed to close tile index: %v", err)
		}
	}
}

// initializeTileIndex creates and stores the tile index in the application.
// It expands the tiles path (including ~ for home directory) and creates
// a TileIndex instance for serving tiles.
func initializeTileIndex(a app.App) error {
	lg := a.Logger().Derive(log.WithFunction("initializeTileIndex"))

	tilesPath := app.GetConfigField[string](a.Config(), FldTilesPath)
	if tilesPath == "" {
		lg.Warnln("TILES_PATH not configured, tile serving will fail")
		return nil
	}

	// Expand ~ to home directory
	if strings.HasPrefix(tilesPath, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			lg.Fatalf("failed to get home directory: %v", err)
		}
		tilesPath = filepath.Join(home, tilesPath[2:])
	}

	lg.Infof("Initializing tile index at: %s", tilesPath)
	idx := tileindex.New(tilesPath)
	a.SetAppData("TileIndex", idx)

	return nil
}
