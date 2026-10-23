// Package shared runs a repository's shared services (docs/design/routing.md,
// decisions 15-22): services such as an email catcher that run once per
// repository, in a Compose project of their own, and are connected to each
// worktree that uses them.
//
// Ownership is the whole point, as in internal/backend, but in a scope no
// worktree shares. Every resource of the stack carries the managed and
// repository labels and the shared label, and never a worktree label. Removal
// lists by those labels and verifies each resource again before it touches
// it, never through any worktree's Compose file, so it works from any
// checkout and after the last worktree is gone; and a worktree's own removal
// never lists one of these, because none carries its label.
package shared

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/docker"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/stateformat"
	"github.com/sofired/treerig/internal/worktree"
)

// Compose's own labels.
const (
	composeProjectLabel = "com.docker.compose.project"
	composeServiceLabel = "com.docker.compose.service"
)

// Docker-call timeouts, as internal/backend's: introspection is bounded at
// 30 seconds, so a hung daemon makes a command fail instead of hang under the
// shared lock (decision 18); a removal gets a minute; a graceful stop two,
// which covers a database's own grace period. `compose up` carries none of its
// own: its --wait-timeout bounds the wait, and a pull may legitimately be slow.
const (
	probeTimeout  = 30 * time.Second
	removeTimeout = 1 * time.Minute
	stopTimeout   = 2 * time.Minute
)

// DefaultWaitTimeoutSeconds is how long `compose up --wait` waits for a
// shared service's health before failing.
const DefaultWaitTimeoutSeconds = 120

// Options configure one repository's shared stack.
type Options struct {
	// RepositoryID is the repository the stack belongs to.
	RepositoryID string
	// LabelNamespace is the Docker label namespace of the project.
	LabelNamespace string
	// ControlHome holds the override file the stack is started with.
	ControlHome string
	// ComposeFile is the absolute path of the shared Compose file. It is
	// needed only to start a service or read its definition; removal and
	// inspection work from labels alone and leave it empty.
	ComposeFile string
	// ProjectName is treerig.toml's project name, offered to the shared file's
	// interpolation as TREERIG_PROJECT.
	ProjectName string
	// WorktreeRoots are the checkouts of the repository's worktrees: a bind
	// mount into one is refused, because the shared service outlives them.
	WorktreeRoots []string
	// Declared are the shared services treerig.toml declares. The shared file
	// may not publish a port of its own on any of them, or on what they
	// depend on: treerig publishes the ports they declare, on 127.0.0.1.
	Declared []string
	// Environ is the environment Compose inherits.
	Environ []string
	// WaitTimeoutSeconds is how long `up --wait` waits; nil uses
	// DefaultWaitTimeoutSeconds.
	WaitTimeoutSeconds *int
	Runner             run.Runner
	Logger             logger.Logger
	// Verbose is -v: Compose's transcript of starting the services is shown, on stderr.
	Verbose bool
	// Observe is called with each line Compose writes while a service starts.
	Observe func(line string)
}

// Stack is one repository's shared Compose project.
type Stack struct {
	o Options
}

// Observing returns the stack with observe called for each line Compose writes
// while a service starts; the receiver is not changed.
func (s *Stack) Observing(observe func(line string)) *Stack {
	clone := *s
	clone.o.Observe = observe
	return &clone
}

// New makes the stack for o.RepositoryID.
func New(o Options) *Stack {
	if o.Logger == nil {
		o.Logger = logger.Silent{}
	}
	return &Stack{o: o}
}

// Project is the stack's Compose project name.
func (s *Stack) Project() string { return layout.SharedProjectName(s.o.RepositoryID) }

// ownership is the label filter that selects the stack's resources: managed,
// this repository's, and shared. No worktree label is part of it, and a
// resource carrying one is refused by verify.
func (s *Stack) ownership() []string {
	ns := s.o.LabelNamespace
	return []string{
		brand.ManagedLabel(ns) + "=true",
		brand.RepositoryLabel(ns) + "=" + s.o.RepositoryID,
		brand.SharedLabel(ns) + "=true",
	}
}

// verify proves a resource is the stack's own from its labels: managed, this
// repository's, shared, in the stack's Compose project, and carrying no
// worktree label. Anything else is refused, so removal and connection can
// never reach a worktree's resource or another repository's.
func (s *Stack) verify(kind, id string, labels map[string]string) error {
	ns := s.o.LabelNamespace
	_, hasWorktree := labels[brand.WorktreeLabel(ns)]
	if labels[brand.ManagedLabel(ns)] != "true" || labels[brand.RepositoryLabel(ns)] != s.o.RepositoryID ||
		labels[brand.SharedLabel(ns)] != "true" || labels[composeProjectLabel] != s.Project() || hasWorktree {
		return clierr.Newf("Refusing to touch %s %s: its labels do not say it belongs to this repository's shared services.", kind, id)
	}
	// As for a worktree's resources: the labels above prove ownership only in
	// the format they were written in (docs/design/state.md), except for a
	// volume, which carries no format label.
	if kind == "volume" {
		return nil
	}
	switch verdict, found := stateformat.Label(labels[brand.FormatLabel(ns)], stateformat.DockerLabel); verdict {
	case stateformat.Current:
		return nil
	case stateformat.FromNewer:
		return stateformat.Newer("touch", stateformat.DescribeResource(kind, id, labels[composeServiceLabel]), found, stateformat.DockerLabel)
	}
	return clierr.Newf("Refusing to touch %s: %s", stateformat.DescribeResource(kind, id, labels[composeServiceLabel]),
		stateformat.Unproven(kind, id, brand.FormatLabel(ns), "it belongs to this repository's shared services"))
}

// State is where a shared service stands, in the words a worktree's own
// services use (worktree.ServiceState): running, starting, unhealthy,
// restarting, exited, stopped, not created or unknown.
type State = worktree.ServiceState

// Instance is one shared service as Docker has it.
type Instance struct {
	// Service is the service's name in the shared Compose file.
	Service string
	// State is where the service stands; NotCreated when it has no container.
	State State
	// ExitCode is how an Exited service ended.
	ExitCode int
	// Detail is Docker's reason for an Unknown state.
	Detail string
	// Container is the service's container; the zero value when Absent.
	Container docker.Container
	// Definition is the definition hash the container was started from
	// ("v1:<sha256>"), or "" when it carries none.
	Definition string
	// DefinedBy is the UUID of the worktree whose definition started it.
	DefinedBy string
}

// Live reports whether the instance is up: running, still starting, failing
// its health check or restarting. Anything else is started by the caller's
// definition, not reused.
func (i Instance) Live() bool {
	return i.State == worktree.Running || i.State == worktree.Starting || i.State == worktree.Unhealthy || i.State == worktree.Restarting
}

// Attached reports whether the instance's container is attached to the
// network with this ID.
func (i Instance) Attached(networkID string) bool {
	for _, endpoint := range i.Container.Networks {
		if endpoint.NetworkID == networkID {
			return true
		}
	}
	return false
}

// stateOf maps a container's Docker state to a State and, for one that
// exited, its exit code. A state it does not know is an error: if the state
// of a container cannot be determined, nothing may assume it is absent or
// stopped (decision 18).
func stateOf(container docker.Container) (State, int, error) {
	reading := worktree.ContainerStatus(container.Status, container.Health, container.ExitCode, true, container.OOMKilled, true)
	if reading.State == worktree.Unknown {
		return "", 0, clierr.Newf("Could not tell what state the shared container %s is in (Docker says %q); not assuming it is stopped or gone.", container.Name, container.Status)
	}
	return reading.State, reading.ExitCode, nil
}

// list is every ID of a kind carrying all of the label filters, sorted and
// deduplicated.
func (s *Stack) list(ctx context.Context, kind string, filters []string) ([]string, error) {
	var args []string
	switch kind {
	case "container":
		args = []string{"ps", "-aq"}
	case "volume":
		args = []string{"volume", "ls", "-q"}
	case "network":
		args = []string{"network", "ls", "-q"}
	default:
		return nil, clierr.Newf("Unknown resource kind: %s", kind)
	}
	for _, filter := range filters {
		args = append(args, "--filter", "label="+filter)
	}
	result, err := docker.Run(ctx, s.o.Runner, args, run.Spec{Timeout: probeTimeout})
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(result.Stdout)
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

// Containers lists the stack's containers, running or not, every one
// verified. One that is listed but fails verification refuses the whole
// listing, as a worktree's inventory does, rather than being skipped: a
// resource that claims to be ours and cannot be proven is something to stop
// for. A container that vanishes between the list and the inspect is left out.
func (s *Stack) Containers(ctx context.Context) ([]docker.Container, error) {
	containers, unproven, err := s.containers(ctx)
	if err != nil {
		return nil, err
	}
	if len(unproven) > 0 {
		return nil, unproven[0]
	}
	return containers, nil
}

// Unproven is a container that carries the shared labels and fails
// verification: it claims to be the stack's and cannot be shown to be.
type Unproven struct {
	// ID is the container's ID.
	ID string
	// Err is the refusal.
	Err error
}

// Error is the refusal's text.
func (u Unproven) Error() string { return u.Err.Error() }

// Unwrap exposes the refusal.
func (u Unproven) Unwrap() error { return u.Err }

// Verified is Containers for a caller that must not be stopped by a stray:
// the containers that pass verification, and for each that claims to be the
// stack's and fails, the refusal to report. It is for disconnecting a
// worktree's networks, which must proceed with the ones it can prove.
func (s *Stack) Verified(ctx context.Context) (containers []docker.Container, unproven []Unproven, err error) {
	return s.containers(ctx)
}

// containers lists the stack's containers by label, split into those that
// verify and the refusals for those that do not.
func (s *Stack) containers(ctx context.Context) (verified []docker.Container, unproven []Unproven, err error) {
	ids, err := s.list(ctx, "container", s.ownership())
	if err != nil {
		return nil, nil, err
	}
	for _, id := range ids {
		container, found, err := docker.InspectContainer(ctx, s.o.Runner, id, probeTimeout)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			continue
		}
		if err := s.verify("container", id, container.Labels); err != nil {
			unproven = append(unproven, Unproven{ID: id, Err: err})
			continue
		}
		verified = append(verified, container)
	}
	return verified, unproven, nil
}

// Consumer is a running container of some worktree that uses a shared
// service, found by the shared-use label alone, without loading any
// worktree's configuration.
type Consumer struct {
	// WorktreeID is the worktree the container belongs to, from its label.
	WorktreeID string
	// ID is the container's ID.
	ID string
	// Uses are the shared services it is labelled as using.
	Uses []string
}

// Consumers lists the running containers of this repository that are
// labelled as using any of the shared services in names, each re-read on
// inspect: managed, this repository's, carrying a worktree label and the
// shared-use label. A container that carries the shared label itself is the
// stack's, not a consumer, and is never listed.
func (s *Stack) Consumers(ctx context.Context, names []string) ([]Consumer, error) {
	ns := s.o.LabelNamespace
	found := map[string]*Consumer{}
	var order []string
	for _, name := range names {
		result, err := docker.Run(ctx, s.o.Runner, []string{"ps", "-q",
			"--filter", "label=" + brand.ManagedLabel(ns) + "=true",
			"--filter", "label=" + brand.RepositoryLabel(ns) + "=" + s.o.RepositoryID,
			"--filter", "label=" + brand.SharedUseLabel(ns, name) + "=true"}, run.Spec{Timeout: probeTimeout})
		if err != nil {
			return nil, err
		}
		for _, id := range strings.Fields(result.Stdout) {
			if existing, ok := found[id]; ok {
				existing.Uses = append(existing.Uses, name)
				continue
			}
			container, present, err := docker.InspectContainer(ctx, s.o.Runner, id, probeTimeout)
			if err != nil {
				return nil, err
			}
			worktreeID, ok := docker.VerifyConsumer(container, ns, s.o.RepositoryID, name)
			if !present || !ok {
				continue
			}
			found[id] = &Consumer{WorktreeID: worktreeID, ID: container.ID, Uses: []string{name}}
			order = append(order, id)
		}
	}
	consumers := make([]Consumer, 0, len(order))
	for _, id := range order {
		consumers = append(consumers, *found[id])
	}
	return consumers, nil
}

// Instance is the shared service called name as Docker has it. Absent means
// Docker answered and there is no container; any failure to answer is an
// error, never Absent.
func (s *Stack) Instance(ctx context.Context, name string) (Instance, error) {
	containers, err := s.Containers(ctx)
	if err != nil {
		return Instance{}, err
	}
	return s.instanceIn(containers, name)
}

// Instances is Instance for each of names, from one listing.
func (s *Stack) Instances(ctx context.Context, names []string) (map[string]Instance, error) {
	containers, err := s.Containers(ctx)
	if err != nil {
		return nil, err
	}
	found := make(map[string]Instance, len(names))
	for _, name := range names {
		instance, err := s.instanceIn(containers, name)
		if err != nil {
			return nil, err
		}
		found[name] = instance
	}
	return found, nil
}

// Statuses is Instances for a report that must not stop at one bad container:
// a service whose container is in a state treerig does not know, or that has
// more than one, is Unknown with the reason in Detail, and a container that
// claims to be the stack's and fails verification comes back as a refusal to
// report, not as a failure of the whole listing. Docker not answering is
// still an error.
func (s *Stack) Statuses(ctx context.Context, names []string) (found map[string]Instance, unproven []Unproven, err error) {
	containers, unproven, err := s.containers(ctx)
	if err != nil {
		return nil, nil, err
	}
	found = make(map[string]Instance, len(names))
	for _, name := range names {
		instance, instanceErr := s.instanceIn(containers, name)
		if instanceErr != nil {
			instance = Instance{Service: name, State: worktree.Unknown, Detail: instanceErr.Error()}
		}
		found[name] = instance
	}
	return found, unproven, nil
}

// instanceIn picks the container of service name out of containers.
func (s *Stack) instanceIn(containers []docker.Container, name string) (Instance, error) {
	var matches []docker.Container
	for _, container := range containers {
		if container.Labels[composeServiceLabel] == name {
			matches = append(matches, container)
		}
	}
	switch len(matches) {
	case 0:
		return Instance{Service: name, State: worktree.NotCreated}, nil
	case 1:
		container := matches[0]
		state, exitCode, err := stateOf(container)
		if err != nil {
			return Instance{}, err
		}
		ns := s.o.LabelNamespace
		return Instance{
			Service: name, State: state, ExitCode: exitCode, Container: container,
			Definition: container.Labels[brand.SharedDefinitionLabel(ns)],
			DefinedBy:  container.Labels[brand.SharedDefinedByLabel(ns)],
		}, nil
	}
	names := make([]string, len(matches))
	for i, match := range matches {
		names[i] = match.Name
	}
	return Instance{}, clierr.Newf("Shared service %s has more than one container (%s); remove the extra ones with 'docker rm' and try again.", name, strings.Join(names, ", "))
}

// Resource is something the stack owns, verified.
type Resource struct {
	Kind string
	ID   string
	// Name is the container's or volume's name where it has one.
	Name string
	// Service is the Compose service of a container, or of the container that
	// mounted an anonymous volume.
	Service string
	// Path is where that container mounts an anonymous volume.
	Path string
	// Anonymous marks a volume Docker created for a mount with no source. It
	// carries none of treerig's labels; it is the stack's because a container
	// proven the stack's mounted it.
	Anonymous bool
}

// Inventory lists everything the stack owns in removal order — containers,
// then networks, then volumes — every item verified, by the shared labels
// alone. It refuses everything if any single resource fails verification.
func (s *Stack) Inventory(ctx context.Context) ([]Resource, error) {
	var owned []Resource
	containers, err := s.Containers(ctx)
	if err != nil {
		return nil, err
	}
	for _, container := range containers {
		owned = append(owned, Resource{Kind: "container", ID: container.ID, Name: container.Name, Service: container.Labels[composeServiceLabel]})
	}
	networks, err := s.list(ctx, "network", s.ownership())
	if err != nil {
		return nil, err
	}
	for _, id := range networks {
		network, found, err := docker.InspectNetwork(ctx, s.o.Runner, id, probeTimeout)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if err := s.verify("network", id, network.Labels); err != nil {
			return nil, err
		}
		owned = append(owned, Resource{Kind: "network", ID: network.ID, Name: network.Name})
	}
	volumes, err := s.list(ctx, "volume", s.ownership())
	if err != nil {
		return nil, err
	}
	for _, name := range volumes {
		volume, found, err := docker.InspectVolume(ctx, s.o.Runner, name, probeTimeout)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if err := s.verify("volume", name, volume.Labels); err != nil {
			return nil, err
		}
		owned = append(owned, Resource{Kind: "volume", ID: volume.Name, Name: volume.Name})
	}
	return owned, nil
}

// String names a resource for a message.
func (r Resource) String() string {
	if r.Name != "" && r.Name != r.ID {
		return fmt.Sprintf("%s %s (%s)", r.Kind, r.Name, r.ID)
	}
	return fmt.Sprintf("%s %s", r.Kind, r.ID)
}

// AnonymousVolumes are the volumes Docker created for a mount with no source
// (`volumes: ["/data"]`) on the given containers, which the caller has
// proven are the stack's own (Containers or Verified). Such a volume carries
// none of treerig's labels, so the containers that mounted it are the only
// proof it is the stack's. It is deduplicated, in the order first mounted.
func (s *Stack) AnonymousVolumes(ctx context.Context, containers []docker.Container) ([]Resource, error) {
	var found []Resource
	seen := map[string]bool{}
	for _, container := range containers {
		for _, name := range container.Volumes {
			if seen[name] {
				continue
			}
			seen[name] = true
			volume, present, err := docker.InspectVolume(ctx, s.o.Runner, name, probeTimeout)
			if err != nil {
				return nil, err
			}
			if !present {
				continue
			}
			if _, anonymous := volume.Labels[anonymousVolumeLabel]; anonymous && volume.Labels[brand.SharedLabel(s.o.LabelNamespace)] != "true" {
				found = append(found, Resource{
					Kind: "volume", ID: volume.Name, Name: volume.Name, Anonymous: true,
					Service: container.Labels[composeServiceLabel], Path: container.MountPaths[name],
				})
			}
		}
	}
	return found, nil
}
