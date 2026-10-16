package server

import (
	"encoding/json"
	"net/http"
)

// ReadyFunc reports whether the service can serve what it is configured to serve, and a JSON
// body describing the state (loaded release, tileset build, style versions, last reload error).
type ReadyFunc func() (ready bool, body any)

// NewReadyHandler serves GET /v1/tiles/ready: 200 when ready, 503 when not. Like /ping it is
// public (liveness and readiness probes carry no token); it exposes release ids and versions
// only, never paths or credentials.
func NewReadyHandler(fn ReadyFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ready, body := fn()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if ready {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ready": ready, "planet": body})
	})
}
