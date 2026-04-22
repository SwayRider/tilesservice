package tilecache

import (
	log "github.com/swayrider/swlib/logger"
)

// testLogger creates a logger for testing.
func testLogger() *log.Logger {
	return log.New(log.WithComponent("test"))
}
