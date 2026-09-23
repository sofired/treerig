package lifecycle

import (
	"runtime"
	"testing"
	"time"

	"github.com/sofired/berth/internal/logger"
)

func TestStop(t *testing.T) {
	t.Run("stops listening and ends its goroutine once a command finished uninterrupted", func(t *testing.T) {
		before := runtime.NumGoroutine()
		fake := newFakeOS()
		life := NewLifecycle(fake, logger.Silent{})
		life.Start()
		life.Stop()
		life.Stop() // twice is harmless

		fake.mu.Lock()
		stopped := fake.stopped
		fake.mu.Unlock()
		if !stopped {
			t.Fatal("the signal registration was not stopped")
		}
		deadline := time.Now().Add(2 * time.Second)
		for runtime.NumGoroutine() > before {
			if time.Now().After(deadline) {
				t.Fatalf("goroutines: %d before, %d after", before, runtime.NumGoroutine())
			}
			time.Sleep(5 * time.Millisecond)
		}
		if life.Pending() != 0 {
			t.Fatal("stopping recorded a signal")
		}
	})
}
