//go:build !linux

package logs

import (
	"context"
	"log/slog"
	"sync"

	"github.com/0intro/dd-micro-agent/internal/config"
)

// startJournald is a stub on non-Linux systems, where journalctl does not exist.
// The config loader accepts journald sources on every OS for portability, so this
// keeps cross-compilation green and reports the gap once at startup, the same
// pattern as host_other.go and process collect_other.go.
func startJournald(_ context.Context, src config.LogSource, _ chan<- Message, _ *Registry, _ *sync.WaitGroup, log *slog.Logger) {
	log.Warn("journald log sources require Linux, skipping", "service", src.Service)
}
