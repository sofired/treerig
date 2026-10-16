package commands

import (
	"context"
	"time"

	"github.com/sofired/treerig/internal/releasefetch"
	"github.com/sofired/treerig/internal/router"
	"github.com/sofired/treerig/internal/shared"
	"github.com/sofired/treerig/internal/worktree"
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
		RouterState: cctx.RouterState, RouterPort: cctx.RouterPortResolution,
	}
	return sharedSection(ctx, opts)
}

// LockChecksForTest is the Locks section of `doctor` for a control home.
func LockChecksForTest(controlHome string) ([]Check, error) {
	return lockChecks(controlHome, nil, "treerig")
}

// sharedResource is a container, network or volume of the shared stack by
// its ID, for a test to hand to Reap.
func SharedResourceForTest(kind, id string) shared.Resource {
	return shared.Resource{Kind: kind, ID: id}
}

// DescribeAnonymousForTest exposes describeAnonymous to the black-box tests.
func DescribeAnonymousForTest(volume shared.Resource) string { return describeAnonymous(volume) }

// ControlHomeRegistrations is the router.Registrations route repair reads a
// control home's records through, for the black-box tests.
func ControlHomeRegistrations(controlHome string) router.Registrations {
	return controlHomeRegistrations{controlHome: controlHome}
}

// SelectionNeedsRouterForTest exposes selectionNeedsRouter for the services
// of cctx's config that names selects.
func SelectionNeedsRouterForTest(ctx context.Context, cctx *CommandContext, names ...string) bool {
	selected, err := selectShared(cctx.Loaded.Config, names)
	if err != nil {
		return false
	}
	return selectionNeedsRouter(ctx, cctx, selected)
}

// SetContainerPollForTest shortens how often a dev session checks that its
// worktree still has a container, for the life of a test. It is not safe for
// tests that run in parallel with each other.
func SetContainerPollForTest(interval time.Duration) (restore func()) {
	previous := containerPoll
	containerPoll = interval
	return func() { containerPoll = previous }
}

// SetMaxTeardownBinaryForTest lowers the size `teardown pin` accepts, and
// returns a func that restores it.
func SetMaxTeardownBinaryForTest(n int64) (restore func()) {
	previous := maxTeardownBinary
	maxTeardownBinary = n
	return func() { maxTeardownBinary = previous }
}

// SetReleaseLimitsForTest replaces the bounds `teardown pin --release` puts on
// an archive's expansion, and returns a func that restores them.
func SetReleaseLimitsForTest(limits releasefetch.Limits) (restore func()) {
	previous := releaseLimits
	releaseLimits = limits
	return func() { releaseLimits = previous }
}

// ReleaseLimitsForTest reports the bounds `teardown pin --release` uses now.
func ReleaseLimitsForTest() releasefetch.Limits { return releaseLimits }
