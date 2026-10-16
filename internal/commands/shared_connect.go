package commands

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/lifecycle"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/ports"
	"github.com/sofired/treerig/internal/procid"
	"github.com/sofired/treerig/internal/shared"
	"github.com/sofired/treerig/internal/worktree"
)

// sharedPlan is what a start of shared services is asked to do.
type sharedPlan struct {
	// Services are the shared services to start or reuse.
	Services []config.SharedService
	// Recreate replaces a running instance with the caller's definition,
	// whether or not it differs, keeping its volumes (`shared up --recreate`).
	Recreate bool
	// Strict reports a running instance whose definition differs, and leaves
	// it running, unless Recreate is set, instead of warning and reusing it
	// (`shared up`).
	Strict bool
	// Reallocate names container ports whose pinned host port moves, by service.
	Reallocate map[string][]int
	// DefinedBy is the worktree whose definition starts what is started.
	DefinedBy string
}

// sharedOutcome is what a start did.
type sharedOutcome struct {
	// Created are the services whose container was created or recreated here
	// (or is up after a start that then failed): they have no attachments, so
	// every consumer needs reconnecting.
	Created []string
	// Reused are the services found running and left alone.
	Reused []string
	// Differs are the running services Strict left alone because their
	// definition differs, one line each, naming who started them.
	Differs []string
	// Needed are declared services that were not asked for but came up as
	// dependencies of one that was, and have a route of their own to write.
	Needed []string
}

// ensureShared starts or reuses plan's shared services under the repository's
// shared lock alone, holding no worktree lock (decision 18). A running
// instance is reused unless Recreate asks for it to be replaced. A differing
// definition is recorded in Differs when Strict is set, otherwise reused
// with a warning. A stopped or absent one is started from the caller's
// definition. If the state of a container cannot be determined, this fails.
// The outcome is returned with the error too: services that came up before the
// start failed still need their consumers connected.
func ensureShared(ctx context.Context, cctx *CommandContext, repositoryID, namespace string, plan sharedPlan) (sharedOutcome, error) {
	var outcome sharedOutcome
	err := cctx.holdShared(ctx, repositoryID, func(ctx context.Context) error {
		var err error
		outcome, err = ensureSharedLocked(ctx, cctx, repositoryID, namespace, plan)
		if err != nil {
			// A start that failed partway can leave a service up: it is
			// routed anyway, so an interrupted `up` does not leave it running
			// and unrouted. Best effort; the failure reported is the start's.
			var created []config.SharedService
			for _, service := range plan.Services {
				if slices.Contains(outcome.Created, service.Name) {
					created = append(created, service)
				}
			}
			if len(created) > 0 && ctx.Err() == nil {
				if routeErr := routeShared(ctx, cctx, repositoryID, created); routeErr != nil {
					cctx.Logger.Warn(fmt.Sprintf("Could not route the shared services that came up: %s", routeErr))
				}
			}
			return err
		}
		// Under the same lock, once everything asked for is up (decision 19):
		// a route file always names a port a container has just been
		// started on or found publishing. A running instance is routed too,
		// which is how a deleted or stale file is repaired.
		routed := slices.Clone(plan.Services)
		for _, service := range cctx.Loaded.Config.SharedServices() {
			if slices.Contains(outcome.Needed, service.Name) {
				routed = append(routed, service)
			}
		}
		return routeShared(ctx, cctx, repositoryID, routed)
	})
	return outcome, err
}

// sharedFileName is the shared Compose file as treerig.toml spells it, for messages.
func sharedFileName(cctx *CommandContext) string {
	if cctx.Loaded.Config.Shared == nil {
		return ""
	}
	return cctx.Loaded.Config.Shared.File
}

// ensureSharedLocked is ensureShared with the shared lock held.
func ensureSharedLocked(ctx context.Context, cctx *CommandContext, repositoryID, namespace string, plan sharedPlan) (sharedOutcome, error) {
	var outcome sharedOutcome
	roots, err := sharedWorktreeRoots(ctx, cctx, repositoryID)
	if err != nil {
		return outcome, err
	}
	stack := cctx.sharedStack(repositoryID, namespace, roots)
	definition, err := stack.ReadDefinition(ctx)
	if err != nil {
		return outcome, err
	}
	names := sharedNames(plan.Services)
	for _, name := range names {
		if !definition.Has(name) {
			return outcome, clierr.Newf("The shared Compose file %s defines no service called %s, which [shared.service.%s] names.", sharedFileName(cctx), name, name)
		}
	}
	instances, err := stack.Instances(ctx, names)
	if err != nil {
		return outcome, err
	}

	// Decide everything before changing anything. start are absent services,
	// recreate those a container exists for and is to be replaced: a stopped
	// one, or a running one Recreate names.
	var start, recreate []string
	for _, service := range plan.Services {
		instance := instances[service.Name]
		expected, err := definition.Hash(service.Name)
		if err != nil {
			return outcome, err
		}
		if moves := plan.Reallocate[service.Name]; len(moves) > 0 && instance.Live() {
			return outcome, clierr.Newf(
				"%s is running (container %s), so its pinned port cannot move from under it. Run '%s shared down %s' first, then '%s shared up --reallocate-port %s:%d'.",
				service.Name, instance.Container.Name, cctx.CommandName, service.Name, cctx.CommandName, service.Name, moves[0])
		}
		switch {
		case instance.State == shared.Absent:
			start = append(start, service.Name)
			continue
		case !instance.Live():
			// Stopped: replaced, not started, because Compose would start it
			// with the definition it was created from.
			recreate = append(recreate, service.Name)
			continue
		}
		if instance.Detail == "unhealthy" {
			cctx.Logger.Warn(fmt.Sprintf("Shared service %s is running but its health check fails.", service.Name))
		}
		if instance.State == shared.Starting {
			cctx.Logger.Info(fmt.Sprintf("Shared service %s is still starting (its container is restarting or its health check is pending); using it.", service.Name))
		}
		who := definedByHost(cctx, repositoryID, instance.DefinedBy)
		switch {
		case plan.Recreate:
			// Asked for by name, so replaced whether or not it differs: the
			// way to restart one, or to give it new ports or a new image.
			recreate = append(recreate, service.Name)
		case instance.Definition == expected:
			outcome.Reused = append(outcome.Reused, service.Name)
		case plan.Strict:
			outcome.Differs = append(outcome.Differs, fmt.Sprintf("%s is running with a different definition than this worktree's (started by %s)", service.Name, who))
		default:
			outcome.Reused = append(outcome.Reused, service.Name)
			cctx.Logger.Warn(fmt.Sprintf(
				"Shared service %s is running with a different definition than this worktree's (started by %s). It was left running; "+
					"run '%s shared up --recreate %s' to replace it, which restarts it and keeps its data.",
				service.Name, who, cctx.CommandName, service.Name))
		}
	}
	for _, service := range plan.Services {
		if slices.Contains(outcome.Reused, service.Name) {
			record, err := cctx.Registry.Store(repositoryID).FindShared(service.Name)
			if err != nil {
				return outcome, err
			}
			if record == nil && len(service.ContainerPorts()) > 0 {
				cctx.Logger.Warn(fmt.Sprintf(
					"Shared service %s is running, but this control home has no record of its ports; they are left as they are. "+
						"'%s shared up --recreate %s' pins new ones.", service.Name, cctx.CommandName, service.Name))
			}
		}
	}
	if len(start) == 0 && len(recreate) == 0 {
		return outcome, nil
	}

	// Pinned ports are committed before anything starts, and a pin something
	// else holds is refused with the way out, instead of Docker's bind error.
	// A declared service that is not asked for but is absent is created too
	// when something that is asked for needs it (depends_on), so its ports
	// are pinned and published like the others'; one already there keeps
	// the ports it was created with.
	toPin, needed, err := sharedToPin(ctx, cctx, stack, definition, plan, start, recreate)
	if err != nil {
		return outcome, err
	}
	outcome.Needed = needed
	published := map[string]map[int]int{}
	for _, service := range toPin {
		result, err := pinSharedPorts(ctx, cctx, repositoryID, service, true, plan.Reallocate[service.Name])
		if err != nil {
			return outcome, err
		}
		for _, container := range plan.Reallocate[service.Name] {
			if previous, moved := result.Moved[container]; moved {
				cctx.Logger.Info(fmt.Sprintf("%s's port %d moved from 127.0.0.1:%d to %s.", service.Name, container, previous, loopbackAddr(result.Ports[container])))
			}
		}
		published[service.Name] = result.Ports
	}
	for _, name := range slices.Concat(start, recreate, needed) {
		// A running container that is to be replaced holds its own pins, so
		// only services that are not running are probed.
		if instances[name].Live() {
			continue
		}
		if refusal := heldPin(ctx, cctx, name, published[name], nil); refusal != nil {
			return outcome, refusal
		}
	}

	for _, name := range recreate {
		if instances[name].Live() {
			cctx.Logger.Info(fmt.Sprintf("Replacing shared service %s.", name))
		}
	}
	everything := slices.Concat(start, recreate)
	err = withProgress(cctx.Logger, "Starting "+sharedPhrase(everything), func() error {
		return stack.Up(ctx, shared.UpRequest{
			Definition: definition, Services: start, Recreate: recreate, Published: published, DefinedBy: plan.DefinedBy,
		})
	})
	var conflict *shared.ConflictError
	if errors.As(err, &conflict) {
		// Another control home on this daemon created it first (decision
		// 18). If it is up now, it is theirs to use; otherwise this is a
		// real failure.
		again, inspectErr := stack.Instances(ctx, everything)
		if inspectErr != nil {
			return outcome, err
		}
		for _, name := range everything {
			if !again[name].Live() {
				return outcome, err
			}
		}
		for _, name := range everything {
			cctx.Logger.Info(fmt.Sprintf("Shared service %s was started by another command; using it.", name))
			outcome.Reused = append(outcome.Reused, name)
		}
		return outcome, nil
	}
	if err != nil {
		// A start that failed partway can leave a container up, with none of
		// the worktrees' attachments: the caller still reconnects them.
		if ctx.Err() == nil {
			if again, inspectErr := stack.Instances(ctx, everything); inspectErr == nil {
				for _, name := range everything {
					if again[name].Live() {
						outcome.Created = append(outcome.Created, name)
					}
				}
				for _, name := range everything {
					if !again[name].Live() {
						if refusal := heldPin(ctx, cctx, name, published[name], err); refusal != nil {
							return outcome, refusal
						}
					}
				}
			}
		}
		return outcome, err
	}
	for _, name := range everything {
		cctx.Logger.Info(fmt.Sprintf("Started shared service %s.", name))
	}
	outcome.Created = everything
	return outcome, nil
}

// sharedToPin is the declared services whose ports this start pins and
// publishes: those to start or recreate, and every other declared service
// that is absent and in the dependency closure of one of them, which Compose
// creates as a side effect. needed names the latter.
func sharedToPin(ctx context.Context, cctx *CommandContext, stack *shared.Stack, definition shared.Definition, plan sharedPlan, start, recreate []string) (toPin []config.SharedService, needed []string, err error) {
	asked := slices.Concat(start, recreate)
	for _, service := range plan.Services {
		if slices.Contains(asked, service.Name) {
			toPin = append(toPin, service)
		}
	}
	var candidates []string
	for _, name := range asked {
		for _, member := range definition.Closure(name) {
			if !slices.Contains(asked, member) && !slices.Contains(candidates, member) {
				candidates = append(candidates, member)
			}
		}
	}
	if len(candidates) == 0 {
		return toPin, nil, nil
	}
	instances, err := stack.Instances(ctx, candidates)
	if err != nil {
		return nil, nil, err
	}
	for _, service := range cctx.Loaded.Config.SharedServices() {
		if slices.Contains(candidates, service.Name) && instances[service.Name].State == shared.Absent {
			toPin = append(toPin, service)
			needed = append(needed, service.Name)
		}
	}
	return toPin, needed, nil
}

// sharedPhrase names services for a progress line: "shared service a" or
// "shared services a, b".
func sharedPhrase(names []string) string {
	if len(names) == 1 {
		return "shared service " + names[0]
	}
	return "shared services " + strings.Join(names, ", ")
}

// heldPin is the refusal for a pinned host port that something else holds
// while the service that publishes it is not running: it names the service,
// the container port and address, how to find the holder, and the way out,
// instead of Docker's bind error. nil when every pin is free. cause, when a
// start has already failed, becomes the refusal's cause.
func heldPin(ctx context.Context, cctx *CommandContext, service string, pins map[int]int, cause error) *clierr.Error {
	for _, container := range slices.Sorted(mapKeys(pins)) {
		host := pins[container]
		if cctx.portProbe().Free(ctx, host) {
			continue
		}
		refusal := clierr.Newf(
			"Cannot start shared service %s: its pinned port %s (container port %d) is held by another process (%s). "+
				"Stop what holds it, or run '%s shared up --reallocate-port %s:%d' to give the port another number.",
			service, loopbackAddr(host), container, ports.LookupHint(cctx.OS, host), cctx.CommandName, service, container)
		if cause != nil {
			refusal.ExitCode = clierr.Code(cause)
			refusal.Cause = cause
		}
		return refusal
	}
	return nil
}

// mapKeys is the keys of a map, as an iterator for slices.Sorted.
func mapKeys[K comparable, V any](m map[K]V) func(yield func(K) bool) {
	return func(yield func(K) bool) {
		for key := range m {
			if !yield(key) {
				return
			}
		}
	}
}

// connectWorktree connects the live shared services in names to the networks
// of ref's running consumers. It runs inside ref's operation lock and takes
// the shared lock nested after it, for the connect only (decision 18), so it
// excludes this worktree's own disconnect and can never attach the shared
// container to a network that is being removed.
func connectWorktree(ctx context.Context, cctx *CommandContext, ref worktree.Ref, namespace string, names []string, requireLive bool) error {
	scope := cctx.sharedScope(ref.RepositoryID)
	return cctx.nest(ctx, lifecycle.Requests{Shared: &scope}, func(ctx context.Context) error {
		return connectLocked(ctx, cctx, ref, namespace, namespace, names, requireLive)
	})
}

// connectLocked is connectWorktree with both locks held. For each shared
// service that is up, it reads the networks of ref's running containers that
// use it, and connects the shared container, under its name as alias, to
// each one that is proven this worktree's own by its labels. A network that
// fails proof is reported and never touched. requireLive makes a service that
// is not up an error, as it is after `up` has just started it; the sweep skips
// it. stackNamespace labels the shared stack; worktreeNamespace labels ref's
// own containers and networks: they are the same unless the sweeping command
// and the worktree were labelled under different namespaces.
func connectLocked(ctx context.Context, cctx *CommandContext, ref worktree.Ref, stackNamespace, worktreeNamespace string, names []string, requireLive bool) error {
	stack := cctx.sharedStack(ref.RepositoryID, stackNamespace, nil)
	runtime := cctx.Runtime(worktreeNamespace)
	instances, err := stack.Instances(ctx, names)
	if err != nil {
		return err
	}
	host := layout.WorktreeHost(ref.Slug, worktreeDomain(cctx, ref))
	for _, name := range names {
		instance := instances[name]
		if !instance.Live() {
			if requireLive {
				return clierr.Newf("Shared service %s is not running, so %s cannot be connected to it. Run '%s shared up' and then '%s up' again.",
					name, host, cctx.CommandName, cctx.CommandName)
			}
			continue
		}
		consumers, err := runtime.SharedConsumers(ctx, ref, name)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, consumer := range consumers {
			for networkName, networkID := range consumer.Networks {
				if seen[networkID] {
					continue
				}
				seen[networkID] = true
				proof, err := runtime.ProveNetwork(ctx, ref, networkID)
				if err != nil {
					return err
				}
				if !proof.Owned {
					cctx.Logger.Info(fmt.Sprintf("Not connecting shared service %s to network %s: it is not %s's own (%s).", name, networkName, host, proof.Reason))
					continue
				}
				if instance.Attached(networkID) {
					continue
				}
				err = stack.Attach(ctx, instance.Container.ID, shared.Proven(networkID), name)
				switch {
				case err == nil:
					cctx.Logger.Info(fmt.Sprintf("Connected shared service %s to %s.", name, host))
				case errors.Is(err, shared.ErrNetworkGone):
					cctx.Logger.Info(fmt.Sprintf("Network %s of %s went away before shared service %s could be connected to it.", networkName, host, name))
				case errors.Is(err, shared.ErrContainerGone):
					return clierr.Newf("Shared service %s went away while %s was being connected to it. Run '%s up' again.", name, host, cctx.CommandName)
				default:
					return err
				}
			}
		}
	}
	return nil
}

// worktreeDomain is the domain ref is registered under, falling back to the
// config's.
func worktreeDomain(cctx *CommandContext, ref worktree.Ref) string {
	if record, err := cctx.Registry.Store(ref.RepositoryID).Find(ref.WorktreeID); err == nil && record != nil {
		return record.Domain
	}
	if cctx.Loaded != nil {
		return cctx.Loaded.Config.Project.Domain
	}
	return ""
}

// sweepResult is what a reconnect sweep did.
type sweepResult struct {
	// Failed is how many worktrees could not be reconnected for a reason
	// other than being busy.
	Failed int
	// Skipped is how many were busy, or could not be locked safely, and are
	// not connected yet.
	Skipped int
}

// sweepShared reconnects every worktree whose running containers use one of
// names, after a shared container was created and has none of their
// attachments (decision 22). Consumers are found by label, not by loading any
// treerig.toml. It runs only after the shared lock is released: each worktree
// is one Hold of its operation lock and the shared lock with a short timeout,
// and a worktree that is busy is skipped with a warning and left to its own
// next `up`, to `treerig shared up` or to `doctor`. skip is a worktree to leave
// out because the caller connects it itself. The stack is read under the
// sweeping command's namespace and each worktree's containers and networks
// under the worktree's own.
func sweepShared(ctx context.Context, cctx *CommandContext, repositoryID, namespace string, names []string, skip string) (sweepResult, error) {
	var result sweepResult
	store := cctx.Registry.Store(repositoryID)
	// A worktree's containers carry its own label namespace, which is the
	// stack's only when the project says so: look under each one registered.
	namespaces := []string{namespace}
	registered, err := store.List()
	if err != nil {
		cctx.Logger.Warn(fmt.Sprintf("Could not list the registered worktrees, so only those under Docker label namespace %s are looked for: %s", namespace, err))
	}
	for _, record := range registered {
		if record.LabelNamespace != nil && !slices.Contains(namespaces, *record.LabelNamespace) {
			namespaces = append(namespaces, *record.LabelNamespace)
		}
	}
	var worktrees []string
	for _, candidate := range namespaces {
		consumers, err := cctx.sharedStack(repositoryID, candidate, nil).Consumers(ctx, names)
		if err != nil {
			return result, err
		}
		for _, consumer := range consumers {
			if consumer.WorktreeID != skip && !slices.Contains(worktrees, consumer.WorktreeID) {
				worktrees = append(worktrees, consumer.WorktreeID)
			}
		}
	}
	sharedLock := cctx.sharedScope(repositoryID)
	for _, id := range worktrees {
		record, err := store.Find(id)
		if err != nil {
			cctx.Logger.Warn(fmt.Sprintf("Could not read the registration of worktree %s, so it was not reconnected: %s", id, err))
			result.Failed++
			continue
		}
		if record == nil {
			cctx.Logger.Warn(fmt.Sprintf("Worktree %s has containers that use a shared service but no registration here, so it was not reconnected.", id))
			result.Skipped++
			continue
		}
		host := layout.WorktreeHost(record.Slug, record.Domain)
		if cctx.insideWorktree(id) {
			// Expected from a hook: the lock is its caller's, and its own
			// `up` connects it.
			cctx.Logger.Info(fmt.Sprintf("Skipped %s: this command runs inside it, so it holds its lock. Its own 'up' connects it.", host))
			result.Skipped++
			continue
		}
		ref := toRef(*record)
		workNamespace := namespace
		if record.LabelNamespace != nil {
			workNamespace = *record.LabelNamespace
		}
		markers := procid.Markers{Project: layout.ComposeProjectName(ref.RepositoryID, ref.WorktreeID), WorktreeID: ref.WorktreeID}
		operation, _ := lock.WorktreeScopes(cctx.ControlHome, ref.RepositoryID, ref.WorktreeID, ref.Slug, markers)
		err = cctx.hold(ctx, lifecycle.Requests{Operation: &operation, Shared: &sharedLock, Timeout: lock.ReconnectTimeout, Quiet: true},
			func(ctx context.Context) error {
				return connectLocked(ctx, cctx, ref, namespace, workNamespace, names, false)
			})
		var busy *lock.BusyError
		switch {
		case err == nil:
		case errors.As(err, &busy):
			cctx.Logger.Warn(fmt.Sprintf("Skipped %s: it is busy. Its next '%s up', or '%s shared up', reconnects it.", host, cctx.CommandName, cctx.CommandName))
			result.Skipped++
		case ctx.Err() != nil:
			return result, err
		default:
			cctx.Logger.Warn(fmt.Sprintf("Could not reconnect %s: %s", host, err))
			result.Failed++
		}
	}
	return result, nil
}

// reportSweep says what a sweep left undone: a warning for worktrees that
// failed, and one line for those skipped, with the way to finish.
func reportSweep(cctx *CommandContext, result sweepResult) {
	if result.Failed > 0 {
		cctx.Logger.Warn(fmt.Sprintf("%d worktree(s) could not be reconnected; see above. '%s shared up' tries them again.", result.Failed, cctx.CommandName))
	}
	if result.Skipped > 0 {
		cctx.Logger.Warn(fmt.Sprintf("%d worktree(s) were skipped and are not connected yet; run '%s shared up' again when they are idle.", result.Skipped, cctx.CommandName))
	}
}

// disconnectShared disconnects the repository's shared containers from the
// networks ref owns, before its own teardown: Compose's `down` exits 0 but
// leaves a network that something is attached to, and Reap cannot remove it
// (decision 22). It needs the worktree's operation lock and no other. Every
// target is proven by labels: the networks by ref's ownership labels and the
// containers by the shared labels, and one that fails proof is reported and
// left alone. A shared container that is gone, or not attached, is done.
// stackNamespace labels the shared stack and worktreeNamespace labels ref's
// networks: the same unless the removing command and the worktree were
// labelled under different namespaces, as in connectLocked. Listing,
// inspection and detach errors are returned so callers stop teardown.
func disconnectShared(ctx context.Context, cctx *CommandContext, ref worktree.Ref, stackNamespace, worktreeNamespace string) error {
	// The shared containers first: a repository without any is the common
	// case, and one listing by label is all it costs.
	stack := cctx.sharedStack(ref.RepositoryID, stackNamespace, nil)
	containers, unproven, err := stack.Verified(ctx)
	if err != nil {
		return err
	}
	for _, problem := range unproven {
		cctx.Logger.Info(fmt.Sprintf("Not touching a container: %s", problem))
	}
	if len(containers) == 0 {
		return nil
	}
	runtime := cctx.Runtime(worktreeNamespace)
	proofs, err := runtime.OwnedNetworks(ctx, ref)
	if err != nil {
		return err
	}
	var owned []shared.ProvenNetwork
	for _, proof := range proofs {
		if proof.Owned {
			owned = append(owned, shared.Proven(proof.ID))
		} else {
			cctx.Logger.Info(fmt.Sprintf("Not touching network %s: %s.", proof, proof.Reason))
		}
	}
	for _, container := range containers {
		for _, network := range owned {
			attached := false
			for _, endpoint := range container.Networks {
				attached = attached || endpoint.NetworkID == network.ID()
			}
			if !attached {
				continue
			}
			if err := stack.Detach(ctx, container.ID, network); err != nil {
				return err
			}
		}
	}
	return nil
}

// startSharedForUp is `up`'s shared-services step, before the worktree's own
// lock: it refuses a worktree whose Compose file cannot take the shared
// services it is configured to use, starts or reuses the ones its services
// use, and, when it created any, reconnects the other worktrees that use
// them, after the shared lock is released — also when the start then failed
// with a container up. It returns the names of the shared services this
// worktree uses, for the connect step; none when it uses none.
func startSharedForUp(ctx context.Context, cctx *CommandContext, ref worktree.Ref, namespace string) ([]string, error) {
	needed := sharedNeeded(cctx.Loaded.Config)
	if len(needed) == 0 {
		return nil, nil
	}
	if err := cctx.Runtime(namespace).CheckSharedUse(ctx, ref, sharedSpecs(needed)); err != nil {
		return nil, err
	}
	outcome, err := ensureShared(ctx, cctx, ref.RepositoryID, namespace, sharedPlan{Services: needed, DefinedBy: ref.WorktreeID})
	if len(outcome.Created) > 0 {
		// A new container has none of the other worktrees' attachments. This
		// worktree is skipped: its own connect step follows its `compose up`.
		result, sweepErr := sweepShared(ctx, cctx, ref.RepositoryID, namespace, outcome.Created, ref.WorktreeID)
		if sweepErr != nil {
			cctx.Logger.Warn(fmt.Sprintf("Could not look for other worktrees to reconnect: %s\nRun '%s shared up' to reconnect them.", sweepErr, cctx.CommandName))
		}
		reportSweep(cctx, result)
	}
	if err != nil {
		return nil, err
	}
	return sharedNames(needed), nil
}
