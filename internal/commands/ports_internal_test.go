package commands

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/backend"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/worktree"
)

// cancellingProbe cancels its context while it probes, and calls the port
// busy with the reason a probe gives then.
type cancellingProbe struct{ cancel context.CancelFunc }

// Free is ports.Probe's Free.
func (p cancellingProbe) Free(context.Context, int) (bool, string) {
	p.cancel()
	return false, "the check was cancelled before it could tell"
}

// noPublishers is a publisherLister with nothing publishing any port.
type noPublishers struct{}

// PortPublishers is publisherLister's PortPublishers.
func (noPublishers) PortPublishers(context.Context, worktree.Ref, int) ([]backend.PortPublisher, error) {
	return nil, nil
}

func TestInspectPortCancelled(t *testing.T) {
	t.Run("returns the context's error, not another process, when the probe was cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		state, err := inspectPort(ctx, cancellingProbe{cancel}, noPublishers{}, worktree.Ref{}, 30111, false, func() []registry.PortClaim { return nil })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, state = %+v, want context.Canceled", err, state)
		}
		if len(state.Others) != 0 || strings.Contains(describeHolders("linux", 30111, state.Others, state.Why), "cancelled") {
			t.Fatalf("state = %+v names a holder", state)
		}
	})
}

func TestHeldPinCancelled(t *testing.T) {
	t.Run("returns the context's error, not a refusal naming another process, when the probe was cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cctx := &CommandContext{CommandName: "treerig", OS: "linux", PortProbe: cancellingProbe{cancel}}
		err := heldPin(ctx, cctx, "mailpit", map[int]int{1025: 30111}, nil)
		if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "another process") {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}
