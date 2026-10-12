package shared

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/docker"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/run"
)

// ConflictError is `compose up` losing a race to create the stack's
// containers: Docker's "container name ... is already in use" or "already
// exists" (decision 18, verified). It is not a failure of the start; the
// caller inspects by label again and, if the instance is up, reuses it.
type ConflictError struct{ Err error }

// Error is the underlying failure's text.
func (e *ConflictError) Error() string { return e.Err.Error() }

// Unwrap exposes the underlying failure.
func (e *ConflictError) Unwrap() error { return e.Err }

// conflict matches what Docker says when another start created the container
// first: "Conflict. The container name "/x" is already in use by container …".
var conflict = regexp.MustCompile(`(?i)container name .* is already in use`)

var (
	// alreadyAttached is `docker network connect` to a network the container is on.
	alreadyAttached = regexp.MustCompile(`(?i)already exists in network`)
	// notAttached is `docker network disconnect` from a network it is not on.
	notAttached = regexp.MustCompile(`(?i)is not connected to`)
	// noContainer is a container that is gone.
	noContainer = regexp.MustCompile(`(?i)no such container`)
	// noNetwork is a network that is gone.
	noNetwork = regexp.MustCompile(`(?i)network .*not found|no such network`)
)

// ErrContainerGone and ErrNetworkGone say a connect found nothing to connect:
// the shared container or the network went away since it was looked up. They
// are not failures of the operation, and callers skip with a notice.
var (
	ErrContainerGone = errors.New("the shared container no longer exists")
	ErrNetworkGone   = errors.New("the network no longer exists")
)

// UpRequest asks for services of the stack to be started.
type UpRequest struct {
	// Definition is the shared file as read for the caller.
	Definition Definition
	// Services are the services to create or start if they are not running;
	// Compose starts what they depend on with them. A running container of
	// one is left exactly as it is.
	Services []string
	// Recreate are the services to replace with the caller's definition: a
	// stopped container, or a running one `--recreate` names. Each is
	// recreated alone (--no-deps); what it needs is started first if it is
	// not running, and never recreated.
	Recreate []string
	// Published maps service to container port to pinned host port.
	Published map[string]map[int]int
	// DefinedBy is the UUID of the worktree whose definition this is.
	DefinedBy string
}

// Up starts the requested services with the caller's definition: it writes
// the override and runs `compose up --detach --wait`. Services it only starts
// get --no-recreate, which matters: berth never lets Compose recreate a
// container that is running (decision 16), so a dependency already up, or a
// sibling whose definition drifted, is left as it is. Services in Recreate
// get --force-recreate --no-deps, one call for them all: Compose recreates
// the container, keeping its named and anonymous volumes, and recreates
// nothing else. A start that lost a race to create the same containers is a
// *ConflictError.
func (s *Stack) Up(ctx context.Context, request UpRequest) error {
	for _, name := range slices.Concat(request.Services, request.Recreate) {
		if !request.Definition.Has(name) {
			return clierr.Newf("The shared Compose file %s defines no service called %s.", s.o.ComposeFile, name)
		}
	}
	document, err := s.overrideDocument(request.Definition, request.Published, request.DefinedBy)
	if err != nil {
		return err
	}
	encoded, err := encodeOverride(document)
	if err != nil {
		return fmt.Errorf("encoding the shared override: %w", err)
	}
	l := layout.SharedLayout(s.o.ControlHome, s.o.RepositoryID)
	if err := files.AssertSafeFilePath(l.ControlHome, l.OverridePath); err != nil {
		return err
	}
	if err := files.AtomicWrite(l.OverridePath, encoded, 0o600); err != nil {
		return err
	}
	// What a service to recreate needs must be running first, and must not
	// itself be recreated: a plain start of those services does that.
	starting := slices.Clone(request.Services)
	for _, name := range request.Recreate {
		for _, member := range request.Definition.Closure(name) {
			if member != name && !slices.Contains(request.Recreate, member) && !slices.Contains(starting, member) {
				starting = append(starting, member)
			}
		}
	}
	if len(starting) > 0 {
		if err := s.compose(ctx, l, append([]string{"--no-recreate"}, starting...)); err != nil {
			return err
		}
	}
	if len(request.Recreate) > 0 {
		return s.compose(ctx, l, append([]string{"--force-recreate", "--no-deps"}, request.Recreate...))
	}
	return nil
}

// compose runs one `compose up --detach --wait` over the shared file and the
// override, with extra flags and then service names, mapping a lost race to a
// *ConflictError.
func (s *Stack) compose(ctx context.Context, l layout.Shared, extra []string) error {
	timeout := DefaultWaitTimeoutSeconds
	if s.o.WaitTimeoutSeconds != nil {
		timeout = *s.o.WaitTimeoutSeconds
	}
	args := []string{"up", "--detach", "--wait", "--wait-timeout", strconv.Itoa(timeout)}
	args = append(args, extra...)
	full := s.composeArgs(args, s.o.ComposeFile, l.OverridePath)
	spec := run.Spec{Env: s.environment(), AllowFailure: true, Output: run.Tee}
	result, err := docker.Run(ctx, s.o.Runner, full, spec)
	if err != nil {
		return err
	}
	if result.ExitCode == 0 {
		return nil
	}
	failure := run.Failure(run.Spec{Command: "docker", Args: full}, result)
	if !result.TimedOut && !result.OutputOverflow && result.Signal == 0 && conflict.MatchString(result.Stderr) {
		return &ConflictError{Err: failure}
	}
	return failure
}

// Reap removes one resource the stack owns. It inspects the resource again
// and verifies it first, so a label that changed since the inventory stops it,
// except for a resource marked Anonymous: the caller must have proven that
// volume through a verified container, and Reap checks Docker's anonymous
// label instead. A container is stopped gracefully before it is removed;
// its volumes are not touched. A resource that is already gone is success.
// Other inspection, ownership, stop and removal failures are returned.
func (s *Stack) Reap(ctx context.Context, resource Resource) error {
	switch resource.Kind {
	case "container":
		container, found, err := docker.InspectContainer(ctx, s.o.Runner, resource.ID, probeTimeout)
		if err != nil || !found {
			return err
		}
		if err := s.verify("container", resource.ID, container.Labels); err != nil {
			return err
		}
		if container.Status == "running" || container.Status == "restarting" || container.Status == "paused" {
			if err := s.tolerant(ctx, []string{"stop", resource.ID}, stopTimeout); err != nil {
				return err
			}
		}
		return s.remove(ctx, []string{"rm", "--force", resource.ID})
	case "network":
		network, found, err := docker.InspectNetwork(ctx, s.o.Runner, resource.ID, probeTimeout)
		if err != nil || !found {
			return err
		}
		if err := s.verify("network", resource.ID, network.Labels); err != nil {
			return err
		}
		return s.remove(ctx, []string{"network", "rm", resource.ID})
	case "volume":
		volume, found, err := docker.InspectVolume(ctx, s.o.Runner, resource.ID, probeTimeout)
		if err != nil || !found {
			return err
		}
		if resource.Anonymous {
			// An anonymous volume carries none of berth's labels; Docker marks
			// it, and the container that mounted it was proven the stack's.
			if _, anonymous := volume.Labels[anonymousVolumeLabel]; !anonymous {
				return clierr.Newf("Refusing to touch volume %s: it is not an anonymous volume.", resource.ID)
			}
		} else if err := s.verify("volume", resource.ID, volume.Labels); err != nil {
			return err
		}
		return s.remove(ctx, []string{"volume", "rm", resource.ID})
	}
	return clierr.Newf("Unknown resource kind: %s", resource.Kind)
}

// anonymousVolumeLabel is the label Docker puts on a volume it created for a
// mount with no source.
const anonymousVolumeLabel = "com.docker.volume.anonymous"

// tolerant runs one docker call, treating a resource that vanished meanwhile
// as done.
func (s *Stack) tolerant(ctx context.Context, args []string, timeout time.Duration) error {
	spec := run.Spec{AllowFailure: true, Timeout: timeout}
	result, err := docker.Run(ctx, s.o.Runner, args, spec)
	if err != nil {
		return err
	}
	if result.ExitCode == 0 || docker.NoSuchObject(result.Stderr) {
		return nil
	}
	return run.Failure(run.Spec{Command: "docker", Args: args, Timeout: timeout}, result)
}

// remove runs one removal, tolerating a resource that vanished meanwhile.
func (s *Stack) remove(ctx context.Context, args []string) error {
	return s.tolerant(ctx, args, removeTimeout)
}

// ProvenNetwork is a network the caller has proven is a worktree's own by its
// ownership labels. Attach and Detach take one, not a bare ID, so a container
// ID cannot be passed where a network is meant, and an unproven network
// cannot be passed at all.
type ProvenNetwork struct{ id string }

// Proven is a network whose ownership by a worktree the caller has proven.
func Proven(id string) ProvenNetwork { return ProvenNetwork{id: id} }

// ID is the network's ID.
func (n ProvenNetwork) ID() string { return n.id }

// Attach connects the shared container to a network of a worktree under the
// given alias. The container is looked up again and verified by its shared
// labels first; the caller has proven the network is the worktree's own.
// Already attached is success. A container or network that went away is
// ErrContainerGone or ErrNetworkGone.
func (s *Stack) Attach(ctx context.Context, containerID string, network ProvenNetwork, alias string) error {
	if err := s.verifyContainer(ctx, containerID); err != nil {
		return err
	}
	args := []string{"network", "connect", "--alias", alias, network.id, containerID}
	result, err := docker.Run(ctx, s.o.Runner, args, run.Spec{AllowFailure: true, Timeout: probeTimeout})
	if err != nil {
		return err
	}
	switch {
	case result.ExitCode == 0, alreadyAttached.MatchString(result.Stderr):
		return nil
	case noContainer.MatchString(result.Stderr):
		return ErrContainerGone
	case noNetwork.MatchString(result.Stderr):
		return ErrNetworkGone
	}
	return run.Failure(run.Spec{Command: "docker", Args: args, Timeout: probeTimeout}, result)
}

// Detach disconnects the shared container from a network of a worktree. Not
// connected, a container that is gone and a network that is gone are all
// done: the attachment went with what it was attached to.
func (s *Stack) Detach(ctx context.Context, containerID string, network ProvenNetwork) error {
	err := s.verifyContainer(ctx, containerID)
	if errors.Is(err, ErrContainerGone) {
		return nil
	}
	if err != nil {
		return err
	}
	args := []string{"network", "disconnect", network.id, containerID}
	result, err := docker.Run(ctx, s.o.Runner, args, run.Spec{AllowFailure: true, Timeout: probeTimeout})
	if err != nil {
		return err
	}
	if result.ExitCode == 0 || notAttached.MatchString(result.Stderr) || noContainer.MatchString(result.Stderr) || noNetwork.MatchString(result.Stderr) {
		return nil
	}
	return run.Failure(run.Spec{Command: "docker", Args: args, Timeout: probeTimeout}, result)
}

// verifyContainer re-inspects a container and proves it is the stack's own.
// A container that is gone is ErrContainerGone.
func (s *Stack) verifyContainer(ctx context.Context, id string) error {
	container, found, err := docker.InspectContainer(ctx, s.o.Runner, id, probeTimeout)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("container %s: %w", id, ErrContainerGone)
	}
	return s.verify("container", id, container.Labels)
}
