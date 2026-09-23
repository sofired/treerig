// Package progress is a Go port of src/progress.ts: one small utility for
// logging a long phase and an elapsed-time heartbeat while it runs. It is
// its own package — rather than living beside one of its callers — so that
// both internal/commands (a mutation's own phases) and internal/hooks (one
// hook run) can use it without either importing the other.
package progress

import (
	"fmt"
	"time"

	"github.com/sofired/berth/internal/logger"
)

// Heartbeat is how often a long phase says it is still going.
var Heartbeat = 10 * time.Second

// WithProgress logs phase, then an elapsed-time heartbeat every Heartbeat
// while action runs.
func WithProgress(log logger.Logger, phase string, action func() error) error {
	log.Info(phase + ".")
	started := time.Now()
	done := make(chan struct{})
	ticker := time.NewTicker(Heartbeat)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				log.Info(fmt.Sprintf("%s (%s elapsed).", phase, elapsed(started)))
			}
		}
	}()
	defer func() {
		ticker.Stop()
		close(done)
	}()
	return action()
}

func elapsed(started time.Time) string {
	total := max(1, int(time.Since(started)/time.Second))
	if total < 60 {
		return fmt.Sprintf("%ds", total)
	}
	return fmt.Sprintf("%dm %02ds", total/60, total%60)
}
