package docker

import (
	"context"
	"strings"
	"time"

	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/text"
	"golang.org/x/sync/errgroup"
)

// InspectConcurrency bounds how many containers ReadExits inspects at once.
const InspectConcurrency = 8

// Inspector reads one container, as InspectContainer does for a runner.
type Inspector func(ctx context.Context, id string, timeout time.Duration) (Container, bool, error)

// ExitReading is what asking Docker about one exited container came to.
type ExitReading struct {
	// Read says Docker answered: ExitCode and OOMKilled are then its words.
	Read      bool
	ExitCode  int
	OOMKilled bool
	// Gone says the container no longer exists (removed between the listing and
	// the inspect); Err is why it could not be read otherwise. Both are unset
	// when Read is.
	Gone bool
	Err  error
}

// ReadExits asks Docker about each of records whose id is not empty, the
// exited containers whose story a listing leaves open: one that reported no
// exit code, and one that exited 137, which a stop, a reboot, a daemon restart
// and the out-of-memory killer all leave, and only Docker says which. The
// inspects run concurrently (InspectConcurrency at a time) under one shared
// deadline (budget), so many such containers cost no more than one, and set is
// called once for each, from its goroutine, with that record: it must write
// only to the record. A container that cannot be inspected is reported as such
// and keeps whatever the caller had; nothing here guesses. Only a cancellation
// is an error.
func ReadExits[T any](ctx context.Context, records []T, budget time.Duration, inspect Inspector, id func(*T) string, set func(*T, ExitReading)) error {
	deadline, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var group errgroup.Group
	group.SetLimit(InspectConcurrency)
	for i := range records {
		record := &records[i]
		containerID := id(record)
		if containerID == "" {
			continue
		}
		group.Go(func() error {
			container, found, err := inspect(deadline, containerID, budget)
			switch {
			case err != nil:
				set(record, ExitReading{Err: err})
			case !found:
				set(record, ExitReading{Gone: true})
			default:
				set(record, ExitReading{Read: true, ExitCode: container.ExitCode, OOMKilled: container.OOMKilled})
			}
			return nil
		})
	}
	_ = group.Wait()
	return ctx.Err()
}

// Why words an unread exit for a hint: that the container is gone, or the
// short reason Docker could not say (without a command line, whose path could
// crowd it out). Empty for one that was read.
func (r ExitReading) Why() string {
	switch {
	case r.Read:
		return ""
	case r.Gone:
		return "the container is gone"
	case r.Err != nil:
		return text.OneLine(strings.TrimSuffix(run.Reason(r.Err), "."), 160)
	}
	return ""
}
