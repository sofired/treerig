package files

import (
	"context"
	"fmt"
	"time"
)

// Within runs probe, a question put to the file system, and stops waiting for
// the answer after limit or when ctx ends, whichever comes first. A path on a
// hung network mount (NFS, autofs) can block a system call with no end, and
// the call cannot be cancelled, so the probe runs on a goroutine of its own:
// when it is given up on, that goroutine is left to finish whenever the call
// returns (its result goes into a buffered channel, so it never blocks and
// never leaks past the call), and Within returns an error. A caller must read
// that error as "could not check", never as a negative answer.
func Within[T any](ctx context.Context, limit time.Duration, probe func() (T, error)) (T, error) {
	type answer struct {
		value T
		err   error
	}
	answered := make(chan answer, 1)
	go func() {
		value, err := probe()
		answered <- answer{value, err}
	}()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case a := <-answered:
		return a.value, a.err
	case <-timer.C:
		var none T
		return none, fmt.Errorf("the file system did not answer within %s", limit)
	case <-ctx.Done():
		var none T
		return none, ctx.Err()
	}
}
