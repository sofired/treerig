package commands

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/sofired/berth/internal/backend"
	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/ports"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/router"
	"github.com/sofired/berth/internal/worktree"
)

// Loopback ports (#119, docs/design/routing.md decisions 3-6 and 12): every
// routed service gets one port on 127.0.0.1, allocated under the registry
// lock, recorded in the worktree's registration and published by the
// generated override. This file is `up`'s side of it: allocating, refusing
// a port something else holds, and moving one on request.

// portRange is the range new loopback ports come from: the context's own, or
// the default when a hand-built context sets none.
func (c *CommandContext) portRange() layout.PortRange {
	if c.PortRange == (layout.PortRange{}) {
		return layout.DefaultPortRange
	}
	return c.PortRange
}

// portProbe is the probe that decides whether a port is free: the context's
// own, or the operating system's.
func (c *CommandContext) portProbe() ports.Probe {
	if c.PortProbe == nil {
		return ports.LoopbackProbe{}
	}
	return c.PortProbe
}

// listeners is the probe that says which loopback addresses a host route's
// port answers on: the context's own, or one that dials.
func (c *CommandContext) listeners() ports.ListenerProbe {
	if c.Listeners == nil {
		return ports.DialListener{}
	}
	return c.Listeners
}

// routedServiceNames are the services berth.toml routes, in file order.
func routedServiceNames(services []config.Service) []string {
	names := make([]string, len(services))
	for i, service := range services {
		names[i] = service.Name
	}
	return names
}

// startingRouted are the routed services `up` is about to start: all of them
// when no services were named, otherwise those named. A pinned port is only
// checked for a service that will publish it.
func startingRouted(routed, named []string) []string {
	if len(named) == 0 {
		return routed
	}
	var starting []string
	for _, service := range routed {
		if slices.Contains(named, service) {
			starting = append(starting, service)
		}
	}
	return starting
}

// publisherLister is what port inspection needs from the Compose runtime.
// *backend.Compose satisfies it.
type publisherLister interface {
	// PortPublishers lists the running containers publishing a host port,
	// classified against ref by their ownership labels.
	PortPublishers(ctx context.Context, ref worktree.Ref, port int) ([]backend.PortPublisher, error)
}

// portState is what is known about one allocated port.
type portState struct {
	// Free is true when nothing answers on the port.
	Free bool
	// Own are this worktree's containers publishing the port.
	Own []backend.PortPublisher
	// Others describe every other holder: another worktree's registration,
	// a container, or "another process". Empty when the port is free or held
	// by this worktree's own containers alone.
	Others []string
}

// anotherProcess is the holder named when a port is busy and no container or
// registration accounts for it.
const anotherProcess = "another process"

// describeHolders joins holders for a message, giving "another process" its
// lookup hint.
func describeHolders(goos string, port int, holders []string) string {
	described := make([]string, len(holders))
	for i, holder := range holders {
		described[i] = holder
		if holder == anotherProcess {
			described[i] = fmt.Sprintf("%s (%s)", holder, ports.LookupHint(goos, port))
		}
	}
	return strings.Join(described, " and ")
}

// inspectPort asks the probe, then Docker, who holds port. A free port needs
// no Docker call, unless askDocker is set (a failed start, where Docker's own
// publish list is asked whatever the probe said: with the userland proxy off,
// a published port can be invisible to a bind probe). A busy port is this
// worktree's own when every container publishing it carries this worktree's
// ownership labels, proven by labels and never by name; anything else is
// described for the message. claimants reads the registrations of other
// worktrees (or services) recording the same port; it is called only for a
// busy port that no container of this worktree publishes. When no container
// and no registration accounts for a busy port it is "another process".
func inspectPort(ctx context.Context, probe ports.Probe, lister publisherLister, ref worktree.Ref, port int, askDocker bool, claimants func() []registry.PortClaim) (portState, error) {
	free := probe.Free(ctx, port)
	if free && !askDocker {
		return portState{Free: true}, nil
	}
	publishers, err := lister.PortPublishers(ctx, ref, port)
	if err != nil {
		return portState{}, err
	}
	if free && len(publishers) == 0 {
		return portState{Free: true}, nil
	}
	var state portState
	for _, publisher := range publishers {
		switch {
		case publisher.Own:
			state.Own = append(state.Own, publisher)
		case publisher.Managed:
			state.Others = append(state.Others, fmt.Sprintf("container %s (belongs to another berth worktree)", publisher.Name))
		default:
			state.Others = append(state.Others, "container "+publisher.Name)
		}
	}
	// A record naming the port is the holder only when this worktree's own
	// container is not the one publishing it.
	if len(state.Own) == 0 {
		var recorded []string
		for _, claim := range claimants() {
			recorded = append(recorded, fmt.Sprintf("worktree %s (its %s service)",
				layout.WorktreeHost(claim.Worktree.Slug, claim.Worktree.Domain), claim.Service))
		}
		state.Others = append(recorded, state.Others...)
	}
	if len(state.Own) == 0 && len(state.Others) == 0 {
		state.Others = []string{anotherProcess}
	}
	return state, nil
}

// otherClaims are the registrations, in this control home, other than ref's
// own service that record port. Unreadable records are ignored: this only
// names a holder, and allocation has already refused on them.
func otherClaims(controlHome string, ref worktree.Ref, service string, port int) []registry.PortClaim {
	scan, err := registry.ScanRegistrations(controlHome)
	if err != nil {
		return nil
	}
	var claims []registry.PortClaim
	for _, record := range scan.Records {
		for name, recorded := range record.Ports {
			if recorded == port && (record.WorktreeID != ref.WorktreeID || name != service) {
				claims = append(claims, registry.PortClaim{Worktree: record, Service: name})
			}
		}
	}
	slices.SortFunc(claims, func(a, b registry.PortClaim) int {
		if c := strings.Compare(a.Worktree.WorktreeID, b.Worktree.WorktreeID); c != 0 {
			return c
		}
		return strings.Compare(a.Service, b.Service)
	})
	return claims
}

// blockedPort is a routed service whose pinned port something else holds.
type blockedPort struct {
	Service string
	Port    int
	Others  []string
}

// blockedPorts inspects every routed service's port and returns the ones held
// by something other than this worktree's own containers, in config order.
func blockedPorts(ctx context.Context, cctx *CommandContext, lister publisherLister, ref worktree.Ref, services []string, pinned map[string]int, askDocker bool) ([]blockedPort, error) {
	var blocked []blockedPort
	for _, service := range services {
		port, ok := pinned[service]
		if !ok {
			continue
		}
		state, err := inspectPort(ctx, cctx.portProbe(), lister, ref, port, askDocker, func() []registry.PortClaim {
			return otherClaims(cctx.ControlHome, ref, service, port)
		})
		if err != nil {
			return nil, err
		}
		if len(state.Others) > 0 {
			blocked = append(blocked, blockedPort{Service: service, Port: port, Others: state.Others})
		}
	}
	return blocked, nil
}

// portRefusal is the refusal to start while pinned ports are held: it names
// each service, its port and its holder, then the two ways out.
func portRefusal(commandName, goos string, blocked []blockedPort) *clierr.Error {
	var lines []string
	for _, entry := range blocked {
		lines = append(lines, fmt.Sprintf("  %s: 127.0.0.1:%d is held by %s", entry.Service, entry.Port, describeHolders(goos, entry.Port, entry.Others)))
	}
	return clierr.Newf(
		"Cannot start: a routed service's pinned loopback port is in use.\n%s\n"+
			"Stop what holds it, or run '%s up --reallocate-port <service>' to give the service another port.",
		strings.Join(lines, "\n"), commandName)
}

// checkPortsFree refuses the start when a pinned port is held by anything
// but this worktree's own containers.
func checkPortsFree(ctx context.Context, cctx *CommandContext, lister publisherLister, ref worktree.Ref, services []string, pinned map[string]int) error {
	blocked, err := blockedPorts(ctx, cctx, lister, ref, services, pinned, false)
	if err != nil {
		return err
	}
	if len(blocked) == 0 {
		return nil
	}
	return portRefusal(cctx.CommandName, cctx.OS, blocked)
}

// explainStartFailure turns a failed compose up into the port refusal when a
// pinned port is now held by something else: Docker's bind error is not
// parsed, the ports are probed again and Docker is asked who publishes each
// pinned port whatever the probe said, and the holders named. Compose's own
// output has already been streamed, and its error stays the refusal's cause.
// A failure that no held port explains, and a cancelled one, is returned as
// it was.
func explainStartFailure(ctx context.Context, cctx *CommandContext, lister publisherLister, ref worktree.Ref, services []string, pinned map[string]int, cause error) error {
	if ctx.Err() != nil {
		return cause
	}
	blocked, err := blockedPorts(ctx, cctx, lister, ref, services, pinned, true)
	if err != nil || len(blocked) == 0 {
		return cause
	}
	refusal := portRefusal(cctx.CommandName, cctx.OS, blocked)
	refusal.ExitCode = clierr.Code(cause)
	refusal.Cause = cause
	return refusal
}

// validateReallocation refuses a --reallocate-port that names something
// berth.toml does not route.
func validateReallocation(services []config.Service, requested []string) error {
	routed := routedServiceNames(services)
	for _, name := range requested {
		if !slices.Contains(routed, name) {
			known := fmt.Sprintf("%s has no [service.<name>] tables.", brand.ConfigFile)
			if len(routed) > 0 {
				known = fmt.Sprintf("%s routes %s.", brand.ConfigFile, strings.Join(routed, ", "))
			}
			return clierr.Newf("--reallocate-port %s: no such routed service. %s", name, known)
		}
	}
	return nil
}

// allocatePorts makes every routed service's port true in the registration:
// it refuses to move a port this worktree's own container still holds, then
// allocates whatever is missing or was asked to move, and says what moved.
// It releases nothing: a service dropped from berth.toml keeps its port until
// releaseDroppedPorts, after a full `compose up` has removed its container.
// Called inside `up`'s mutation, so the operation lock is held and the
// registry lock is taken by SyncPorts alone.
func allocatePorts(ctx context.Context, cctx *CommandContext, lister publisherLister, ref worktree.Ref, host string, options UpOptions) (map[string]int, error) {
	store := cctx.Registry.Store(ref.RepositoryID)
	services := routedServiceNames(cctx.Loaded.Config.Service)
	record, err := store.Find(ref.WorktreeID)
	if err != nil {
		return nil, err
	}
	var recorded map[string]int
	if record != nil {
		recorded = record.Ports
	}
	if len(services) == 0 && len(recorded) == 0 && len(options.ReallocatePorts) == 0 {
		return map[string]int{}, nil
	}
	for _, service := range options.ReallocatePorts {
		port, held := recorded[service]
		if !held {
			continue
		}
		publishers, err := lister.PortPublishers(ctx, ref, port)
		if err != nil {
			return nil, err
		}
		for _, publisher := range publishers {
			if publisher.Own {
				return nil, clierr.Newf(
					"%s's port %d is held by this worktree's own container %s, so it cannot be moved from under it. "+
						"Run '%s down' first, then '%s up --reallocate-port %s'.",
					service, port, publisher.Name, cctx.CommandName, cctx.CommandName, service)
			}
		}
	}
	reserved, err := routerReservedPorts(cctx)
	if err != nil {
		return nil, err
	}
	result, err := store.SyncPorts(ctx, ref.WorktreeID, ref.Slug, cctx.writer(), registry.PortPlan{
		Services: services, Reallocate: options.ReallocatePorts, Range: cctx.portRange(), Probe: cctx.portProbe(),
		Reserved: reserved,
	})
	if err != nil {
		return nil, err
	}
	for _, service := range options.ReallocatePorts {
		if previous, moved := result.Moved[service]; moved {
			cctx.Logger.Info(fmt.Sprintf(
				"%s's loopback port changed from %d to %d (http://127.0.0.1:%d). Its hostname URL through the router, %s, is unchanged.",
				service, previous, result.Ports[service], result.Ports[service], host))
		}
	}
	return result.Ports, nil
}

// releaseDroppedPorts releases the port of every service berth.toml no longer
// routes, once `compose up` has succeeded for all services: its
// --remove-orphans has then removed the dropped service's container, so
// nothing of the old owner can still hold the port (docs/design/routing.md,
// decision 5). When `up` named only some services the sweep did not run over
// the rest, so nothing is released and a later full `up` does it. It is
// idempotent, and a failure here fails the `up`: the port stays recorded and
// the next `up` retries.
func releaseDroppedPorts(ctx context.Context, cctx *CommandContext, ref worktree.Ref, options UpOptions) error {
	if len(options.Services) > 0 {
		return nil
	}
	store := cctx.Registry.Store(ref.RepositoryID)
	// Read first: an up with nothing dropped takes no registry lock, so it
	// cannot fail here after its containers have started.
	record, err := store.Find(ref.WorktreeID)
	if err != nil {
		return err
	}
	routed := routedServiceNames(cctx.Loaded.Config.Service)
	dropped := false
	if record != nil {
		for service := range record.Ports {
			dropped = dropped || !slices.Contains(routed, service)
		}
	}
	if !dropped {
		return nil
	}
	_, err = store.SyncPorts(ctx, ref.WorktreeID, ref.Slug, cctx.writer(), registry.PortPlan{
		Services: routed, Release: true, Range: cctx.portRange(), Probe: cctx.portProbe(),
	})
	return err
}

// routerReservedPorts are the router's own ports (the index's and, in host
// mode, the ping's) from its state file. They are in no registration, so
// every allocator beside the registry, a worktree's and a shared service's
// alike, passes them as Reserved and skips them like a taken port.
func routerReservedPorts(cctx *CommandContext) ([]int, error) {
	state, _, err := router.ReadState(cctx.RouterState)
	if err != nil {
		return nil, err
	}
	return state.Reserved(), nil
}
