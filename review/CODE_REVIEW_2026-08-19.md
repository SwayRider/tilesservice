# Code Review — 2026-08-19

Follow-up security audit of the full current codebase (not diff-based), cross-checked against [`review/CODE_REVIEW_2026-08.md`](CODE_REVIEW_2026-08.md). See [`Docs/REVIEW.md`](../../Docs/REVIEW.md) for how findings in this file are tracked.

**Verification of prior review:** all 16 previously-tracked findings are marked Fixed; the security-relevant ones were re-verified against current code with no regressions — the `os.RemoveAll` data-loss footgun now has marker-file ownership + path-sanity guards (`internal/tilecache/disk_cache.go:73-267`), x/y range validation blocks the uint32-underflow path at both the handler and reader layers (`internal/server/http_tile.go:100-107`, `internal/mbtiles/reader.go:71-77`), and auth docs now correctly state service-token-only. Style-name path traversal is blocked (`validStyleName` regexp `^[a-zA-Z0-9_-]+$`) and tile-path construction never uses raw user strings. SQL queries in `mbtiles/reader.go` and `tilecache/disk_cache.go` are fully parameterized — no injection. No secrets in the repo (`.env`/`local.env` gitignored, only `env.example` tracked).

Authorization is sound at the code level: `requireTilesAuth` (`cmd/tilesservice/main.go:128-158`) requires RS256-signed tokens (algorithm confusion blocked in shared `swlib/jwt`), rejects user JWTs via a type assertion on `SwayRiderServiceClaims`, and checks the `tiles:serve` scope explicitly. Whether the service is actually unreachable except through the gateway is an infra/network question, not something the code enforces. CORS is wide open (`AllowedOrigins: ["*"]`) but `AllowCredentials: false` and auth is via `Authorization: Bearer` header (not cookies), so this doesn't create a CSRF/credential-leak vector.

### 1. Missing HTTP server timeouts (Slowloris / slow-client DoS)

`cmd/tilesservice/main.go:373-376`:

```go
httpServer = &http.Server{
    Addr:    fmt.Sprintf(":%d", httpPort),
    Handler: handler,
}
```

No `ReadTimeout`, `ReadHeaderTimeout`, `WriteTimeout`, or `IdleTimeout` is set. A client that opens a connection and sends headers/body slowly (or not at all) can hold a goroutine and file descriptor indefinitely; with enough such connections the service exhausts its listener capacity. Since this is instantiated directly with `net/http` defaults (no swlib wrapper applies timeouts here), it's uncontained. Severity tempered by the service being internal-only (gated by `tiles:serve` service-token auth, reachable only through `swayrider-api`), so practical exposure depends on network segmentation being correctly enforced in `infra/dev` compose layers — still worth hardening as defense-in-depth. Fix direction: set `ReadHeaderTimeout` at minimum (mitigates Slowloris), plus `ReadTimeout`/`WriteTimeout`/`IdleTimeout` sized to tile-serving latency. Severity: Medium/Low.
