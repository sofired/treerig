package commands

import (
	"context"
	"time"

	"github.com/sofired/berth/internal/shared"
	"github.com/sofired/berth/internal/worktree"
)

// DisconnectSharedForTest exposes disconnectShared to the black-box tests.
func DisconnectSharedForTest(ctx context.Context, cctx *CommandContext, ref worktree.Ref, stackNamespace, worktreeNamespace string) error {
	return disconnectShared(ctx, cctx, ref, stackNamespace, worktreeNamespace)
}

// SharedStackForTest exposes a repository's shared stack to the black-box tests.
func SharedStackForTest(cctx *CommandContext, repositoryID string) *shared.Stack {
	return cctx.sharedStack(repositoryID, cctx.Loaded.Config.LabelNamespace(), nil)
}

// SharedDoctorForTest is the Shared services section of `doctor` for cctx's
// checkout, with Docker taken to be answering.
func SharedDoctorForTest(ctx context.Context, cctx *CommandContext) ([]Check, error) {
	opts := doctorOptions{
		CommandName: cctx.CommandName, ControlHome: cctx.ControlHome, UserHome: cctx.UserHome,
		Runner: cctx.Runner, Logger: cctx.Logger, Environ: cctx.Environ, PortProbe: cctx.PortProbe,
		Checkout: checkoutIsLoaded(cctx.Loaded), DockerAnswered: true,
	}
	return sharedSection(ctx, opts)
}

// LockChecksForTest is the Locks section of `doctor` for a control home.
func LockChecksForTest(controlHome string) ([]Check, error) {
	return lockChecks(controlHome, nil, "berth")
}

// sharedResource is a container, network or volume of the shared stack by
// its ID, for a test to hand to Reap.
func SharedResourceForTest(kind, id string) shared.Resource {
	return shared.Resource{Kind: kind, ID: id}
}

// DescribeAnonymousForTest exposes describeAnonymous to the black-box tests.
func DescribeAnonymousForTest(volume shared.Resource) string { return describeAnonymous(volume) }

// SetContainerPollForTest shortens how often a dev session checks that its
// worktree still has a container, for the life of a test. It is not safe for
// tests that run in parallel with each other.
func SetContainerPollForTest(interval time.Duration) (restore func()) {
	previous := containerPoll
	containerPoll = interval
	return func() { containerPoll = previous }
}
