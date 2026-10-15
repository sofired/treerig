package lock

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/unix"

	"github.com/sofired/treerig/internal/procid"
)

type heldContextKey struct{}

// WithHeld carries the given locks on ctx, in order, so a runner can hand
// them to a Docker child as ExtraFiles — the operation lock first, at fd 3 —
// even though the run call itself never sees a *Lock.
func WithHeld(ctx context.Context, held ...*Lock) context.Context {
	return context.WithValue(ctx, heldContextKey{}, held)
}

// AddHeld is ctx with l carried after the locks it already carries, so a lock
// taken later than the ones a Hold acquired (the shared lock nested inside a
// worktree's operation lock, docs/design/routing.md decision 18) is handed to
// a Docker child behind them, at the next descriptor.
func AddHeld(ctx context.Context, l *Lock) context.Context {
	held, _ := ctx.Value(heldContextKey{}).([]*Lock)
	return WithHeld(ctx, append(append([]*Lock(nil), held...), l)...)
}

// Inherited is the *os.File of every lock WithHeld carried on ctx that is
// still held, in the same order — ExtraFiles for the child.
func Inherited(ctx context.Context) []*os.File {
	held, _ := ctx.Value(heldContextKey{}).([]*Lock)
	files := make([]*os.File, 0, len(held))
	for _, l := range held {
		if f := l.File(); f != nil {
			files = append(files, f)
		}
	}
	return files
}

// State is whether a described lock is currently held.
type State int

const (
	// Free means nobody currently holds the flock.
	Free State = iota
	// Held means something does.
	Held
)

// Description is everything a read-only probe can learn about a lock
// without acquiring or changing anything.
type Description struct {
	State  State
	Record *Record
}

// Describe is a read-only probe for doctor: LOCK_SH|LOCK_NB, then close at
// once. A shared lock, not LOCK_EX like ProbeHeld and Clear take: Describe
// never mutates anything, so it only needs to prove nobody holds the flock
// exclusively, not to hold it exclusively itself — and a shared probe can
// run alongside another shared probe (another `doctor`, say) without either
// waiting on the other. table is accepted for a future judgement of the
// record it reads; Describe itself never acts on what it learns.
func Describe(scope Scope, table procid.Table) (Description, error) {
	_ = table
	var described Description
	file, err := os.OpenFile(scope.Target, os.O_RDWR, 0o600)
	if err != nil {
		if os.IsNotExist(err) {
			return described, nil
		}
		return Description{}, err
	}
	defer file.Close()

	if err := unix.Flock(int(file.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return Description{}, err
		}
		described.State = Held
	}
	if described.Record, err = readRecord(scope.Target + ".holder"); err != nil {
		return Description{}, err
	}
	return described, nil
}
