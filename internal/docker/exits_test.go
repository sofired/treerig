package docker_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/docker"
)

// exited is a container the test asks about.
type exited struct {
	id      string
	reading docker.ExitReading
	set     bool
}

// setReading records what ReadExits found on the container.
func setReading(e *exited, reading docker.ExitReading) { e.reading, e.set = reading, true }

// idOf is the container's ID.
func idOf(e *exited) string { return e.id }

func TestReadExits(t *testing.T) {
	ctx := context.Background()

	t.Run("reads what Docker says, tells a container that is gone from one that failed, and skips what has no ID", func(t *testing.T) {
		failure := errors.New("Command failed (docker container inspect c3 --some-long-flags): permission denied")
		inspect := func(_ context.Context, id string, _ time.Duration) (docker.Container, bool, error) {
			switch id {
			case "c1":
				return docker.Container{ExitCode: 137, OOMKilled: true}, true, nil
			case "c2":
				return docker.Container{}, false, nil
			}
			return docker.Container{}, false, failure
		}
		records := []exited{{id: "c1"}, {id: "c2"}, {id: "c3"}, {id: ""}}
		if err := docker.ReadExits(ctx, records, time.Minute, inspect, idOf, setReading); err != nil {
			t.Fatal(err)
		}
		if r := records[0].reading; !r.Read || r.ExitCode != 137 || !r.OOMKilled || r.Why() != "" {
			t.Errorf("read: %+v", r)
		}
		if r := records[1].reading; r.Read || !r.Gone || r.Why() != "the container is gone" {
			t.Errorf("gone: %+v", r)
		}
		if r := records[2].reading; r.Read || r.Gone || !errors.Is(r.Err, failure) || r.Why() != "permission denied" {
			t.Errorf("failed: %+v, why %q", r, r.Why())
		}
		if records[3].set {
			t.Error("a record with no ID was asked about")
		}
	})

	t.Run("inspects at most InspectConcurrency containers at once", func(t *testing.T) {
		var running, peak atomic.Int32
		inspect := func(context.Context, string, time.Duration) (docker.Container, bool, error) {
			now := running.Add(1)
			for {
				seen := peak.Load()
				if now <= seen || peak.CompareAndSwap(seen, now) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			running.Add(-1)
			return docker.Container{}, true, nil
		}
		records := make([]exited, 4*docker.InspectConcurrency)
		for i := range records {
			records[i].id = "c"
		}
		if err := docker.ReadExits(ctx, records, time.Minute, inspect, idOf, setReading); err != nil {
			t.Fatal(err)
		}
		if got := peak.Load(); got > docker.InspectConcurrency || got < 2 {
			t.Fatalf("peak concurrency = %d, want 2..%d", got, docker.InspectConcurrency)
		}
	})

	t.Run("gives every inspect one shared deadline, and keeps a container it ran out of time for unread", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		inspect := func(ctx context.Context, _ string, _ time.Duration) (docker.Container, bool, error) {
			select {
			case <-ctx.Done():
				return docker.Container{}, false, ctx.Err()
			case <-release:
				return docker.Container{}, true, nil
			}
		}
		records := []exited{{id: "a"}, {id: "b"}, {id: "c"}}
		start := time.Now()
		if err := docker.ReadExits(ctx, records, 50*time.Millisecond, inspect, idOf, setReading); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatal("the inspects did not share one deadline")
		}
		for _, r := range records {
			if r.reading.Read || r.reading.Err == nil || !strings.Contains(r.reading.Why(), "deadline") {
				t.Errorf("%s: %+v", r.id, r.reading)
			}
		}
	})

	t.Run("returns a cancellation, the one error", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		inspect := func(ctx context.Context, _ string, _ time.Duration) (docker.Container, bool, error) {
			return docker.Container{}, false, ctx.Err()
		}
		records := []exited{{id: "a"}}
		if err := docker.ReadExits(cancelled, records, time.Minute, inspect, idOf, setReading); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}
