package commands

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sofired/berth/internal/backend"
	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/docker"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/shared"
	"github.com/sofired/berth/internal/text"
	"github.com/sofired/berth/internal/worktree"
)

// `berth shared up|down|status` (docs/design/routing.md, decision 21).
// `up` starts what a worktree's services use on its own; these are the
// explicit verbs: repair and replace, stop, and look.

// SharedUpOptions are `shared up`'s arguments and flags.
type SharedUpOptions struct {
	// Names are the shared services to start; empty means every declared one.
	Names []string
	// Recreate replaces the running services Names names with this
	// worktree's definition, keeping their volumes. It needs at least one name.
	Recreate bool
	// ReallocatePorts are <name>:<container port> values to give a new
	// pinned loopback port, refused while the service runs.
	ReallocatePorts []string
}

// SharedUp is `shared up`.
func SharedUp(ctx context.Context, env *Env, options SharedUpOptions) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	return SharedUpWithContext(ctx, cctx, options)
}

// selectShared are the declared shared services options names, or all of
// them, in the file's order; a name the file does not declare is refused.
func selectShared(cfg config.Config, names []string) ([]config.SharedService, error) {
	declared := cfg.SharedServices()
	if len(declared) == 0 {
		return nil, clierr.Newf("%s declares no shared services. Add a [shared] table and a [shared.service.<name>] table for each.", brand.ConfigFile)
	}
	if len(names) == 0 {
		return declared, nil
	}
	for _, name := range names {
		if !slices.ContainsFunc(declared, func(s config.SharedService) bool { return s.Name == name }) {
			return nil, clierr.Newf("No shared service called %s. %s", name, declaredSharedHint(cfg))
		}
	}
	var selected []config.SharedService
	for _, service := range declared {
		if slices.Contains(names, service.Name) {
			selected = append(selected, service)
		}
	}
	return selected, nil
}

// runningHint lists the shared services that are up among names, or, when
// none is, the declared ones, for a refusal that asks for a name.
func runningHint(ctx context.Context, cctx *CommandContext, repositoryID string) string {
	cfg := cctx.Loaded.Config
	declared := sharedNames(cfg.SharedServices())
	instances, _, err := cctx.sharedStack(repositoryID, cfg.LabelNamespace(), nil).Statuses(ctx, declared)
	if err == nil {
		var running []string
		for _, name := range declared {
			if instances[name].Live() {
				running = append(running, name)
			}
		}
		if len(running) > 0 {
			return "Running: " + strings.Join(running, ", ") + "."
		}
	}
	return declaredSharedHint(cfg)
}

// SharedUpWithContext starts the shared services named, or all of them, and
// then always reconnects every worktree that uses them: the repair path
// (decision 21), whether the instance was just created or has been running
// all along, and also when a running instance's definition differs from this
// worktree's and is therefore left alone. That is refused, without
// --recreate, after the reconnecting, with a non-zero exit; nothing running
// is ever restarted otherwise.
func SharedUpWithContext(ctx context.Context, cctx *CommandContext, options SharedUpOptions) error {
	cfg := cctx.Loaded.Config
	services, err := selectShared(cfg, options.Names)
	if err != nil {
		return err
	}
	moves, err := parseReallocation(cfg, options.ReallocatePorts)
	if err != nil {
		return err
	}
	for name := range moves {
		if !slices.ContainsFunc(services, func(s config.SharedService) bool { return s.Name == name }) {
			return clierr.Newf("--reallocate-port names %s, which is not among the services named on the command line.", name)
		}
	}
	if err := config.CheckStart(cctx.Loaded); err != nil {
		return err
	}
	registration, err := cctx.requireRegistration(ctx)
	if err != nil {
		return err
	}
	ref := toRef(registration.RegisteredWorktree)
	namespace := cfg.LabelNamespace()
	if options.Recreate && len(options.Names) == 0 {
		return clierr.Newf(
			"--recreate replaces a running shared service, so it needs the name of the one to replace: '%s shared up --recreate <name>'. %s",
			cctx.CommandName, runningHint(ctx, cctx, ref.RepositoryID))
	}

	outcome, ensureErr := ensureShared(ctx, cctx, ref.RepositoryID, namespace, sharedPlan{
		Services: services, Recreate: options.Recreate, Strict: true, Reallocate: moves, DefinedBy: ref.WorktreeID,
	})
	for _, name := range outcome.Reused {
		if !slices.ContainsFunc(outcome.Differs, func(line string) bool { return strings.HasPrefix(line, name+" ") }) {
			cctx.Logger.Info(fmt.Sprintf("Shared service %s is already running with this definition.", name))
		}
	}
	var sweepErr error
	var result sweepResult
	// Whether or not anything was created: this is how a consumer left
	// unconnected by a busy sweep or a daemon restart is repaired, and a
	// start that failed with a container up leaves it unconnected too.
	if ensureErr == nil || len(outcome.Created) > 0 {
		result, sweepErr = sweepShared(ctx, cctx, ref.RepositoryID, namespace, sharedNames(services), "")
		reportSweep(cctx, result)
	}
	switch {
	case ensureErr != nil:
		return ensureErr
	case sweepErr != nil:
		return sweepErr
	case len(outcome.Differs) > 0:
		var differing []string
		for _, line := range outcome.Differs {
			differing = append(differing, strings.SplitN(line, " ", 2)[0])
		}
		return clierr.Newf(
			"Not replacing a running shared service:\n  %s.\nRun '%s shared up --recreate %s' to replace it with this worktree's definition, which restarts it and keeps its data.",
			strings.Join(outcome.Differs, ".\n  "), cctx.CommandName, strings.Join(differing, " "))
	case result.Failed > 0:
		return clierr.Newf("Could not reconnect %d worktree(s) to the shared services; see above. Run '%s shared up' again once that is fixed.", result.Failed, cctx.CommandName)
	}
	return nil
}

// SharedStatusEntry is one shared service in `status --json` and `shared
// status --json`.
type SharedStatusEntry struct {
	Name string `json:"name"`
	// State is running, starting, stopped, absent or unknown: the words a
	// worktree's own services use. Starting is a container restarting or
	// running with its health check still pending.
	State string `json:"state"`
	// Detail is "unhealthy" for a running service failing its health check,
	// and Docker's reason for an unknown state.
	Detail string `json:"detail,omitempty"`
	// Addrs are the pinned loopback addresses of the declared ports.
	Addrs []SharedStatusAddr `json:"addrs"`
	// Definition is "current" when the running instance was started from the
	// definition this worktree's shared file hashes to, "differs" when it was
	// started from another, and "unknown" when there is nothing running to
	// compare or the file cannot be read.
	Definition string `json:"definition"`
	// Connected is whether the shared container is attached to every network
	// of this worktree's own that one of its running services using it is
	// on, and at least one. It is false for a worktree that has no such
	// service running (UsedHere false), which is not a fault.
	Connected bool `json:"connected"`
	// DefinedBy is the hostname of the worktree whose definition started the
	// running instance, or "a removed worktree".
	DefinedBy string `json:"definedBy,omitempty"`
	// UsedHere is whether a running service of this worktree uses it. It
	// only tells the text form "not used here" from "not connected".
	UsedHere bool `json:"-"`
}

// SharedStatusAddr is one pinned address of a shared service.
type SharedStatusAddr struct {
	Port int    `json:"port"`
	Addr string `json:"addr"`
}

// sharedStatuses reads every declared shared service's state from Docker and
// the records. One bad thing does not fail the report: a container in a state
// berth does not know is an entry with state unknown, an unreadable record
// leaves its addresses out with a warning, and a shared file that cannot be
// read makes every definition unknown with a warning.
func sharedStatuses(ctx context.Context, cctx *CommandContext, repositoryID, worktreeID string) ([]SharedStatusEntry, error) {
	cfg := cctx.Loaded.Config
	services := cfg.SharedServices()
	if len(services) == 0 {
		return nil, nil
	}
	namespace := cfg.LabelNamespace()
	stack := cctx.sharedStack(repositoryID, namespace, nil)
	instances, unproven, err := stack.Statuses(ctx, sharedNames(services))
	if err != nil {
		return nil, err
	}
	for _, problem := range unproven {
		cctx.Logger.Warn(fmt.Sprintf("A container claims to be a shared service's and was left out: %s", problem))
	}
	var definition shared.Definition
	haveDefinition := false
	for _, instance := range instances {
		if instance.Live() {
			loaded, readErr := cctx.sharedStack(repositoryID, namespace, nil).ReadDefinition(ctx)
			if readErr != nil {
				cctx.Logger.Warn(fmt.Sprintf("Could not read the shared file, so every definition is reported unknown: %s", text.OneLine(readErr.Error(), 300)))
			} else {
				definition, haveDefinition = loaded, true
			}
			break
		}
	}
	store := cctx.Registry.Store(repositoryID)
	runtime := cctx.Runtime(namespace)
	ref := worktree.Ref{RepositoryID: repositoryID, WorktreeID: worktreeID}
	entries := make([]SharedStatusEntry, 0, len(services))
	for _, service := range services {
		instance := instances[service.Name]
		entry := SharedStatusEntry{Name: service.Name, State: string(instance.State), Detail: instance.Detail, Addrs: []SharedStatusAddr{}, Definition: "unknown"}
		record, recordErr := store.FindShared(service.Name)
		if recordErr != nil {
			cctx.Logger.Warn(fmt.Sprintf("Could not read the record of %s's pinned ports, so its addresses are left out: %s", service.Name, text.OneLine(recordErr.Error(), 300)))
		}
		if record != nil {
			for _, container := range service.ContainerPorts() {
				if host := record.HostPort(container); host != 0 {
					entry.Addrs = append(entry.Addrs, SharedStatusAddr{Port: container, Addr: loopbackAddr(host)})
				}
			}
		}
		if instance.Live() {
			entry.DefinedBy = definedByHost(cctx, repositoryID, instance.DefinedBy)
			if haveDefinition {
				if expected, hashErr := definition.Hash(service.Name); hashErr == nil {
					entry.Definition = "differs"
					if expected == instance.Definition {
						entry.Definition = "current"
					}
				}
			}
			if worktreeID != "" {
				connected, used, err := connectedTo(ctx, runtime, ref, service.Name, instance)
				if err != nil {
					return nil, err
				}
				entry.Connected, entry.UsedHere = connected, used
			}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// connectedTo reports whether instance is attached to every network of ref's
// own that a running consumer of the service is on, and there is at least
// one; used is whether there is any such network. It is also what `doctor`
// asks to find an unconnected consumer.
func connectedTo(ctx context.Context, runtime *backend.Compose, ref worktree.Ref, name string, instance shared.Instance) (connected, used bool, err error) {
	consumers, err := runtime.SharedConsumers(ctx, ref, name)
	if err != nil {
		return false, false, err
	}
	owned, attached := 0, 0
	for _, consumer := range consumers {
		for _, networkID := range consumer.Networks {
			proof, err := runtime.ProveNetwork(ctx, ref, networkID)
			if err != nil {
				return false, false, err
			}
			if !proof.Owned {
				continue
			}
			owned++
			if instance.Attached(networkID) {
				attached++
			}
		}
	}
	return owned > 0 && owned == attached, owned > 0, nil
}

// renderShared is the text form of statuses, one block per service, each line
// prefixed with indent: the state, notes in parentheses, then each pinned
// address labelled with the container port it reaches.
func renderShared(statuses []SharedStatusEntry, indent string) []string {
	width := 0
	for _, entry := range statuses {
		width = max(width, text.JSLen(entry.Name))
	}
	var lines []string
	for _, entry := range statuses {
		var notes []string
		if entry.Detail != "" {
			notes = append(notes, entry.Detail)
		}
		if entry.DefinedBy != "" {
			notes = append(notes, "definition "+entry.Definition)
			switch {
			case entry.Connected:
				notes = append(notes, "connected")
			case entry.UsedHere:
				notes = append(notes, "not connected")
			default:
				notes = append(notes, "not used here")
			}
			notes = append(notes, "started by "+entry.DefinedBy)
		}
		line := fmt.Sprintf("%s%s  %s", indent, text.PadEnd(entry.Name, width), entry.State)
		if len(notes) > 0 {
			line += "  (" + strings.Join(notes, ", ") + ")"
		}
		lines = append(lines, text.Printable(line))
		for _, addr := range entry.Addrs {
			lines = append(lines, fmt.Sprintf("%s  container port %d -> %s", indent, addr.Port, addr.Addr))
		}
	}
	return lines
}

// SharedStatusReport is `shared status --json`'s document.
type SharedStatusReport struct {
	Shared []SharedStatusEntry `json:"shared"`
}

// SharedStatus is `shared status`.
func SharedStatus(ctx context.Context, env *Env, options OutputOptions) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	return SharedStatusWithContext(ctx, cctx, options)
}

// SharedStatusWithContext prints each declared shared service: its state, its
// pinned addresses, whether its definition is the one this worktree's file
// hashes to, whether this worktree is connected to it, and who started it. A
// repository that was never brought up has every declared service absent.
func SharedStatusWithContext(ctx context.Context, cctx *CommandContext, options OutputOptions) error {
	cfg := cctx.Loaded.Config
	if len(cfg.SharedServices()) == 0 {
		if options.JSON {
			return printJSON(cctx, SharedStatusReport{Shared: []SharedStatusEntry{}})
		}
		cctx.Logger.Info(fmt.Sprintf("%s declares no shared services.", brand.ConfigFile))
		return nil
	}
	var statuses []SharedStatusEntry
	identity, err := cctx.Registry.Identities.Inspect(ctx, cctx.Registry.Identities.Root, registry.InspectOptions{})
	if err != nil {
		return err
	}
	repositoryID, worktreeID, present := "", "", false
	if identity != nil {
		repositoryID, worktreeID, present = identity.RepositoryID, identity.WorktreeID, true
	} else if id, found, err := cctx.Registry.Identities.RepositoryID(ctx, cctx.Registry.Identities.Root); err != nil {
		return err
	} else {
		repositoryID, present = id, found
	}
	if present {
		statuses, err = sharedStatuses(ctx, cctx, repositoryID, worktreeID)
		if err != nil {
			return err
		}
	} else {
		// Never brought up: nothing can be running, and nothing is pinned.
		for _, service := range cfg.SharedServices() {
			statuses = append(statuses, SharedStatusEntry{Name: service.Name, State: string(shared.Absent), Addrs: []SharedStatusAddr{}, Definition: "unknown"})
		}
	}
	if options.JSON {
		return printJSON(cctx, SharedStatusReport{Shared: statuses})
	}
	for _, line := range renderShared(statuses, "") {
		cctx.Logger.Output(line)
	}
	return nil
}

// SharedDownOptions are `shared down`'s arguments and flags.
type SharedDownOptions struct {
	// Names are the shared services to stop; empty means all of them.
	Names []string
	// Volumes also removes their volumes and releases their pinned ports: the
	// only path that deletes a shared service's data. With names it removes
	// the volumes the named services' containers mount; without, every
	// volume of the stack.
	Volumes bool
	// Yes skips the confirmation phrase.
	Yes bool
	// LabelNamespace names the Docker label namespace when there is no usable
	// berth.toml to say it.
	LabelNamespace string
}

// SharedDown is `shared down`. It needs no berth.toml: it finds the
// repository's shared resources by their labels, from any checkout of it.
func SharedDown(ctx context.Context, env *Env, options SharedDownOptions) error {
	cctx, err := ContextWithoutConfig(env)
	if err != nil {
		return err
	}
	return SharedDownWithContext(ctx, cctx, env, options)
}

// sharedDownNamespace is the label namespace shared down looks under: the
// flag, else the config's, else the product's own.
func sharedDownNamespace(cctx *CommandContext, flag string) (string, error) {
	if flag != "" {
		if !brand.LabelNamespacePattern.MatchString(flag) {
			return "", clierr.Newf("--label-namespace must look like a reverse domain, e.g. dev.example, not \"%s\".", flag)
		}
		return flag, nil
	}
	if cctx.Loaded != nil {
		return cctx.Loaded.Config.LabelNamespace(), nil
	}
	return brand.LabelNamespace, nil
}

// SharedDownWithContext stops the shared services named, or all of them: each
// container is stopped gracefully and removed, the stack's own network goes
// once nothing of the stack is left, and the volumes and pinned ports are
// kept — except with --volumes, which also deletes the data and releases the
// ports, after the containers and volumes are gone, and asks first. The
// confirmation lists what will go; if the plan has grown when the lock is
// taken, nothing is removed. Everything is found and re-verified by the
// shared labels, never through any worktree's Compose file, and it never asks
// whether worktrees are using the services: this is the explicit stop. env
// serves the confirmation prompt.
func SharedDownWithContext(ctx context.Context, cctx *CommandContext, env *Env, options SharedDownOptions) error {
	namespace, err := sharedDownNamespace(cctx, options.LabelNamespace)
	if err != nil {
		return err
	}
	for _, name := range options.Names {
		if err := checkServiceName(name); err != nil {
			return err
		}
	}
	empty := fmt.Sprintf("No shared services of this repository to stop (Docker label namespace %s).", namespace)
	repositoryID, present, err := cctx.Registry.Identities.RepositoryID(ctx, cctx.Registry.Identities.Root)
	if err != nil {
		return err
	}
	if !present {
		// Never brought up from this repository: there is nothing of it in Docker.
		cctx.Logger.Info(empty)
		return nil
	}
	stack := cctx.sharedStack(repositoryID, namespace, nil)

	confirmed, err := planSharedDown(ctx, stack, cctx, repositoryID, options)
	if err != nil {
		return err
	}
	if confirmed.empty() {
		cctx.Logger.Info(empty)
		return nil
	}
	label := stack.Project()
	if cctx.Loaded != nil {
		label = cctx.Loaded.Config.Project.Name + " shared services"
	}
	if options.Volumes {
		if err := confirm(env, confirmed.describe(label), "remove "+label, options.Yes); err != nil {
			return err
		}
	}

	return cctx.holdShared(ctx, repositoryID, func(ctx context.Context) error {
		// The lock was free while the prompt was up: planned again, and
		// every resource verified again, under it.
		plan, err := planSharedDown(ctx, stack, cctx, repositoryID, options)
		if err != nil {
			return err
		}
		if options.Volumes {
			if added := plan.addedTo(confirmed); len(added) > 0 {
				return clierr.Newf(
					"What would be removed changed while you were deciding (new: %s). Nothing was removed; run '%s shared down' again.",
					strings.Join(added, ", "), cctx.CommandName)
			}
		}
		services := plan.services()
		consumers, err := stack.Consumers(ctx, services)
		if err != nil {
			return err
		}
		worktrees := map[string]bool{}
		for _, consumer := range consumers {
			worktrees[consumer.WorktreeID] = true
		}
		for _, resource := range plan.resources {
			if err := withProgressIf(resource, cctx, func() error { return stack.Reap(ctx, resource) }); err != nil {
				return err
			}
		}
		if len(plan.resources) > 0 {
			cctx.Logger.Info(fmt.Sprintf("Removed %s.", countSharedResources(plan.resources)))
		}
		if options.Volumes {
			// Gone first, released second (decision 5): an interruption in
			// between leaves the ports pinned from a record that still names them.
			store := cctx.Registry.Store(repositoryID)
			for _, name := range plan.released {
				if err := store.RemoveSharedRecord(ctx, cctx.writer(), name); err != nil {
					return err
				}
			}
			if len(plan.released) > 0 {
				cctx.Logger.Info(fmt.Sprintf("Released the pinned ports of %s.", strings.Join(plan.released, ", ")))
			}
		}
		if len(plan.stranded) > 0 {
			cctx.Logger.Warn(fmt.Sprintf(
				"%s %s anonymous (no name in the shared file), so the next start gets a fresh one and this data will not be reattached. A named volume in the shared Compose file keeps its data across a down; '%s shared down --volumes' deletes these now.",
				strings.Join(plan.stranded, ", "), pluralIs(len(plan.stranded)), cctx.CommandName))
		}
		if len(worktrees) > 0 {
			cctx.Logger.Info(fmt.Sprintf(
				"%d worktree(s) use %s, which is no longer running; '%s up' or '%s shared up' starts it again.",
				len(worktrees), strings.Join(services, ", "), cctx.CommandName, cctx.CommandName))
		}
		return nil
	})
}

// checkServiceName refuses a name that cannot be a shared service's, before
// it can reach a path or a Docker filter.
func checkServiceName(name string) error {
	if !registry.ValidSharedName(name) {
		return clierr.Newf("%q is not a shared service name: lower-case letters, digits and hyphens, at most 63.", name)
	}
	return nil
}

// withProgressIf runs removal of one resource, with a progress line for a
// container, which can take a while to stop.
func withProgressIf(resource shared.Resource, cctx *CommandContext, action func() error) error {
	if resource.Kind != "container" {
		return action()
	}
	return withProgress(cctx.Logger, "Stopping "+resource.Name, action)
}

// sharedDownPlan is what `shared down` will remove, all verified.
type sharedDownPlan struct {
	// resources are removed in this order.
	resources []shared.Resource
	// released are the services whose pinned ports are released: those with a
	// record, and only with --volumes.
	released []string
	// stranded are the anonymous volumes (as describeAnonymous names them) of the containers a plain down
	// removes: Compose gives the next container a fresh one, so their data is
	// not reattached. Only set without --volumes.
	stranded []string
}

// empty reports whether there is nothing to remove or release.
func (p sharedDownPlan) empty() bool { return len(p.resources) == 0 && len(p.released) == 0 }

// services are the Compose services of the containers to remove.
func (p sharedDownPlan) services() []string {
	var names []string
	for _, resource := range p.resources {
		if resource.Kind == "container" && !slices.Contains(names, resource.Service) {
			names = append(names, resource.Service)
		}
	}
	return names
}

// keys name every item of the plan, for comparing two plans.
func (p sharedDownPlan) keys() map[string]string {
	keys := map[string]string{}
	for _, resource := range p.resources {
		keys[resource.Kind+":"+resource.ID] = resource.String()
	}
	for _, name := range p.released {
		keys["ports:"+name] = "the pinned ports of " + name
	}
	return keys
}

// addedTo lists what p would remove that confirmed did not, sorted.
func (p sharedDownPlan) addedTo(confirmed sharedDownPlan) []string {
	was := confirmed.keys()
	var added []string
	for key, described := range p.keys() {
		if _, known := was[key]; !known {
			added = append(added, described)
		}
	}
	slices.Sort(added)
	return added
}

// describe is the confirmation screen for a removal that deletes data: every
// container, network and volume by name, with a volume Docker created for a
// mount with no source marked, and the ports that are released.
func (p sharedDownPlan) describe(label string) string {
	names := func(kind string) string {
		var listed []string
		for _, resource := range p.resources {
			if resource.Kind != kind {
				continue
			}
			name := resource.Name
			if name == "" {
				name = resource.ID
			}
			if resource.Anonymous {
				name += " (anonymous)"
			}
			listed = append(listed, name)
		}
		return strings.Join(listed, ", ")
	}
	lines := []string{fmt.Sprintf("This will permanently remove %s, including their data:", text.Printable(label))}
	for _, kind := range []string{"container", "network", "volume"} {
		if listed := names(kind); listed != "" {
			lines = append(lines, "  "+kind+"s: "+text.Printable(listed))
		}
	}
	if len(p.released) > 0 {
		lines = append(lines, "  their pinned loopback ports ("+text.Printable(strings.Join(p.released, ", "))+")")
	}
	return strings.Join(lines, "\n")
}

// countSharedResources renders resources as "N kind, M kind" in first-seen order.
func countSharedResources(resources []shared.Resource) string {
	var kinds []string
	counts := map[string]int{}
	for _, resource := range resources {
		if counts[resource.Kind] == 0 {
			kinds = append(kinds, resource.Kind)
		}
		counts[resource.Kind]++
	}
	if len(kinds) == 0 {
		return "no Docker resources"
	}
	parts := make([]string, len(kinds))
	for i, kind := range kinds {
		plural := "s"
		if counts[kind] == 1 {
			plural = ""
		}
		parts[i] = fmt.Sprintf("%d %s%s", counts[kind], kind, plural)
	}
	return strings.Join(parts, ", ")
}

// planSharedDown lists what options asks to remove, verified by the shared
// labels. A name that is no running service, record or declared service is
// refused up front. With names, only the containers of those services go,
// and those that were started only to serve them once no declared service's
// container is left; the stack's networks go when every container does.
// With --volumes, named services lose the volumes their containers mount
// (named and anonymous) that no container that stays mounts, and unnamed
// removes every volume of the stack and the anonymous ones its containers
// mount. The services whose ports are released are those named that have a
// record, or every record the repository has.
func planSharedDown(ctx context.Context, stack *shared.Stack, cctx *CommandContext, repositoryID string, options SharedDownOptions) (sharedDownPlan, error) {
	containers, err := stack.Containers(ctx)
	if err != nil {
		return sharedDownPlan{}, err
	}
	inventory, err := stack.Inventory(ctx)
	if err != nil {
		return sharedDownPlan{}, err
	}
	records, _, err := cctx.Registry.Store(repositoryID).SharedRecords()
	if err != nil {
		return sharedDownPlan{}, err
	}
	var declared []string
	if cctx.Loaded != nil {
		declared = sharedNames(cctx.Loaded.Config.SharedServices())
	}
	if err := refuseUnknownNames(options.Names, containers, records, declared, cctx.CommandName); err != nil {
		return sharedDownPlan{}, err
	}

	var selected, remaining []docker.Container
	if len(options.Names) == 0 {
		selected = containers
	} else {
		for _, container := range containers {
			if slices.Contains(options.Names, container.Labels[backend.ComposeServiceLabel]) {
				selected = append(selected, container)
			} else {
				remaining = append(remaining, container)
			}
		}
		// What was only started to serve the declared services goes once
		// none of them is left. Without a config nothing is declared, so
		// nothing else is assumed to be a helper.
		if len(selected) > 0 && len(declared) > 0 {
			left := false
			for _, container := range remaining {
				left = left || slices.Contains(declared, container.Labels[backend.ComposeServiceLabel])
			}
			if !left {
				selected, remaining = containers, nil
			}
		}
	}
	all := len(remaining) == 0

	var plan sharedDownPlan
	for _, container := range selected {
		plan.resources = append(plan.resources, shared.Resource{
			Kind: "container", ID: container.ID, Name: container.Name, Service: container.Labels[backend.ComposeServiceLabel],
		})
	}
	for _, resource := range inventory {
		if resource.Kind == "network" && all {
			plan.resources = append(plan.resources, resource)
		}
	}
	if options.Volumes {
		held := map[string]bool{}
		for _, container := range remaining {
			for _, volume := range container.Volumes {
				held[volume] = true
			}
		}
		mounted := map[string]bool{}
		for _, container := range selected {
			for _, volume := range container.Volumes {
				mounted[volume] = true
			}
		}
		for _, resource := range inventory {
			if resource.Kind != "volume" || held[resource.ID] {
				continue
			}
			// Unnamed removes every volume of the stack, mounted or not; named
			// only what a selected container mounts.
			if len(options.Names) == 0 || mounted[resource.ID] {
				plan.resources = append(plan.resources, resource)
			}
		}
		anonymous, err := stack.AnonymousVolumes(ctx, selected)
		if err != nil {
			return sharedDownPlan{}, err
		}
		for _, volume := range anonymous {
			if !held[volume.ID] {
				plan.resources = append(plan.resources, volume)
			}
		}
		for _, record := range records {
			if len(options.Names) == 0 || slices.Contains(options.Names, record.Name) {
				plan.released = append(plan.released, record.Name)
			}
		}
	} else if len(selected) > 0 {
		anonymous, err := stack.AnonymousVolumes(ctx, selected)
		if err != nil {
			return sharedDownPlan{}, err
		}
		for _, volume := range anonymous {
			plan.stranded = append(plan.stranded, describeAnonymous(volume))
		}
	}
	return plan, nil
}

// refuseUnknownNames refuses a name that is none of: a service with a
// container, a service with a pinned-port record, or a declared service. It
// lists the running and declared ones, so a typo is not a silent success.
func refuseUnknownNames(names []string, containers []docker.Container, records []registry.SharedRecord, declared []string, commandName string) error {
	var running []string
	for _, container := range containers {
		if service := container.Labels[backend.ComposeServiceLabel]; !slices.Contains(running, service) {
			running = append(running, service)
		}
	}
	slices.Sort(running)
	for _, name := range names {
		known := slices.Contains(running, name) || slices.Contains(declared, name) ||
			slices.ContainsFunc(records, func(r registry.SharedRecord) bool { return r.Name == name })
		if known {
			continue
		}
		hint := "Nothing is running and nothing is declared."
		switch {
		case len(running) > 0 && len(declared) > 0:
			hint = fmt.Sprintf("Running: %s. Declared: %s.", strings.Join(running, ", "), strings.Join(declared, ", "))
		case len(running) > 0:
			hint = "Running: " + strings.Join(running, ", ") + "."
		case len(declared) > 0:
			hint = "Declared: " + strings.Join(declared, ", ") + "."
		}
		return clierr.Newf("No shared service called %s. %s ('%s shared down' with no name stops them all.)", name, hint, commandName)
	}
	return nil
}

// hintSharedDown tells the person who just removed a repository's last
// registered worktree that its shared services are still running, and how to
// stop them. It is best effort with a bounded Docker probe, and never fails
// the removal: any error is swallowed.
func hintSharedDown(ctx context.Context, cctx *CommandContext, repositoryID, namespace string) {
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	registered, err := cctx.Registry.Store(repositoryID).List()
	if err != nil || len(registered) > 0 {
		return
	}
	containers, _, err := cctx.sharedStack(repositoryID, namespace, nil).Verified(probe)
	if err != nil {
		return
	}
	var services []string
	for _, container := range containers {
		service := container.Labels[backend.ComposeServiceLabel]
		if (container.Status == "running" || container.Status == "restarting") && !slices.Contains(services, service) {
			services = append(services, service)
		}
	}
	if len(services) == 0 {
		return
	}
	slices.Sort(services)
	cctx.Logger.Info(fmt.Sprintf(
		"That was this repository's last registered worktree, and its shared services are still running (%s). "+
			"'%s shared down' stops them; add --volumes to delete their data too.",
		strings.Join(services, ", "), cctx.CommandName))
}

// describeAnonymous names an anonymous volume for a person: the service that
// mounted it, where, and a short form of its generated ID, such as
// "mailpit's /data volume (a1b2c3d4…)".
func describeAnonymous(volume shared.Resource) string {
	id := volume.ID
	if len(id) > 12 {
		id = id[:8] + "…"
	}
	what := "anonymous volume"
	if volume.Path != "" {
		what = volume.Path + " volume"
	}
	if volume.Service != "" {
		what = volume.Service + "'s " + what
	}
	return fmt.Sprintf("%s (%s)", what, id)
}

// pluralIs is "is" for one and "are" for more.
func pluralIs(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}
