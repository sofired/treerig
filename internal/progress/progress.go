// Package progress is one small utility for logging a long phase and an
// elapsed-time heartbeat while it runs. It is its own package — rather than
// living beside one of its callers — so that both internal/commands (a
// mutation's own phases) and internal/hooks (one hook run) can use it
// without either importing the other.
package progress

import (
	"fmt"
	"time"

	"github.com/sofired/berth/internal/logger"
)

// heartbeat is how often a long phase says it is still going.
var heartbeat = 10 * time.Second

// SetHeartbeatForTest overrides how often WithProgress reports a
// still-going heartbeat, for a test that needs to observe one without
// waiting out the real interval. Returns a func that restores the previous
// value; callers defer it so an override never leaks into another test.
func SetHeartbeatForTest(d time.Duration) (restore func()) {
	previous := heartbeat
	heartbeat = d
	return func() { heartbeat = previous }
}

// WithProgress logs phase, then an elapsed-time heartbeat every heartbeat
// while action runs. No heartbeat is ever logged once WithProgress has
// returned: the deferred cleanup waits for the heartbeat goroutine to have
// actually stopped, not just for it to have been told to, since a tick
// ready at the same moment as done being closed is not guaranteed to lose
// a select.
func WithProgress(log logger.Logger, phase string, action func() error) error {
	log.Info(phase + ".")
	started := time.Now()
	done := make(chan struct{})
	stopped := make(chan struct{})
	ticker := time.NewTicker(heartbeat)
	go func() {
		defer close(stopped)
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				// select does not prefer done over a tick that became ready
				// at the same time; this second, non-blocking check is what
				// actually keeps a heartbeat from logging once action has
				// returned.
				select {
				case <-done:
					return
				default:
				}
				log.Info(fmt.Sprintf("%s (%s elapsed).", phase, elapsed(started)))
			}
		}
	}()
	defer func() {
		ticker.Stop()
		close(done)
		<-stopped
	}()
	return action()
}

// elapsed is the time since started, rounded down to whole seconds (never
// below 1s) and formatted as "Ns" under a minute or "Mm SSs" at or above it.
func elapsed(started time.Time) string {
	total := max(1, int(time.Since(started)/time.Second))
	if total < 60 {
		return fmt.Sprintf("%ds", total)
	}
	return fmt.Sprintf("%dm %02ds", total/60, total%60)
}
