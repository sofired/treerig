package files_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/files"
)

func TestWithin(t *testing.T) {
	t.Run("returns what the probe answered", func(t *testing.T) {
		got, err := files.Within(context.Background(), time.Minute, func() (int, error) { return 7, nil })
		if err != nil || got != 7 {
			t.Fatalf("got %d, %v", got, err)
		}
		want := errors.New("denied")
		if _, err := files.Within(context.Background(), time.Minute, func() (int, error) { return 0, want }); !errors.Is(err, want) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("gives up on a probe that does not answer, without waiting for it", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		start := time.Now()
		_, err := files.Within(context.Background(), 20*time.Millisecond, func() (bool, error) {
			<-release
			return true, nil
		})
		if err == nil || !strings.Contains(err.Error(), "did not answer within 20ms") {
			t.Fatalf("err = %v", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("waited %s", elapsed)
		}
	})

	t.Run("stops waiting when the context ends", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		cancel()
		if _, err := files.Within(ctx, time.Minute, func() (bool, error) { <-release; return true, nil }); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}
