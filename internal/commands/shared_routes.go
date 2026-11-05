package commands

// The routed web UI of shared services (docs/design/routing.md, decisions 19
// and 20): the route file, the URL hooks and `status` report, and the sweep
// that keeps both true to the records. The record is the source of truth and
// the route file a cache of it, so every writer here derives the file from
// the record and from nothing else.

import (
	"context"
	"fmt"
	"os"
	"slices"

	"github.com/sofired/treerig/internal/backend"
	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/router"
	"github.com/sofired/treerig/internal/shared"
	"github.com/sofired/treerig/internal/worktree"
)

// anyRouted reports whether any of services declares a routed web UI.
func anyRouted(services []config.SharedService) bool {
	for _, service := range services {
		if service.Port != nil {
			return true
		}
	}
	return false
}

// sharedURLs are the routed web UIs of the repository's declared shared
// services, one per service whose record routes a port, in the file's order.
// Like sharedAddrs they come from the records and not from Docker, so a hook
// gets the same URL whether or not the service is running, and an unreadable
// record is skipped here (`doctor` reports it).
func sharedURLs(cctx *CommandContext, repositoryID string, routerPort int) []worktree.SharedURL {
	if cctx.Loaded == nil {
		return nil
	}
	return sharedURLsFor(cctx.ControlHome, repositoryID, cctx.Loaded.Config, routerPort, cctx.Logger)
}

// sharedURLsFor is sharedURLs from a control home and a config alone.
func sharedURLsFor(controlHome, repositoryID string, cfg config.Config, routerPort int, log logger.Logger) []worktree.SharedURL {
	if len(cfg.SharedServices()) == 0 {
		return nil
	}
	records, problems, err := registry.NewStore(controlHome, repositoryID).SharedRecords()
	if err != nil {
		log.Warn(fmt.Sprintf("Could not read the shared services' records, so %s* is left out: %s", brand.EnvName(worktree.SharedURLPrefix), err))
		return nil
	}
	for _, problem := range problems {
		log.Warn(fmt.Sprintf("A shared service's record could not be read, so its %s* is left out: %s: %s", brand.EnvName(worktree.SharedURLPrefix), problem.File, problem.Reason))
	}
	byName := make(map[string]registry.SharedRecord, len(records))
	for _, record := range records {
		byName[record.Name] = record
	}
	var urls []worktree.SharedURL
	for _, service := range cfg.SharedServices() {
		if record, ok := byName[service.Name]; ok && record.Host() != "" {
			urls = append(urls, worktree.NewSharedURL(record.Name, record.Domain, routerPort))
		}
	}
	return urls
}

// routeShared makes the route file of each of services match its record, under
// the shared lock the caller holds: written to the pinned host port when the
// record routes a port, removed when it routes none. It writes only from a
// record this control home holds; a service with no record is left alone,
// since a file without one may be another control home's (decision 9).
//
// A running instance whose record does not yet route the port treerig.toml
// declares as its web UI adopts it, when the port is already pinned and so
// already published by the container: the case of a port that was a raw one
// when the container was created. A port that is not pinned needs the
// container recreated to publish it, and is left alone.
func routeShared(ctx context.Context, cctx *CommandContext, repositoryID string, services []config.SharedService) error {
	store := cctx.Registry.Store(repositoryID)
	secured := false
	for _, service := range services {
		record, err := store.FindShared(service.Name)
		if err != nil {
			return err
		}
		if record == nil {
			continue
		}
		if service.Port != nil && record.Routed != *service.Port && record.Ports[*service.Port] != 0 {
			routed := *service.Port
			if _, err := store.SyncSharedPorts(ctx, cctx.writer(), registry.SharedPortPlan{
				Name: service.Name, Domain: cctx.Loaded.Config.Project.Domain, Range: cctx.portRange(), Probe: cctx.portProbe(), Routed: &routed,
			}); err != nil {
				return err
			}
			if record, err = store.FindShared(service.Name); err != nil {
				return err
			}
			if record == nil {
				continue
			}
		}
		if service.Port == nil && record.Routed != 0 {
			// The declaration was dropped from treerig.toml while the container
			// keeps running: its record stops routing, and the file goes. The
			// port stays pinned and published until the next recreate.
			zero := 0
			if _, err := store.SyncSharedPorts(ctx, cctx.writer(), registry.SharedPortPlan{
				Name: service.Name, Domain: cctx.Loaded.Config.Project.Domain, Range: cctx.portRange(), Probe: cctx.portProbe(), Routed: &zero,
			}); err != nil {
				return err
			}
			if record, err = store.FindShared(service.Name); err != nil {
				return err
			}
			if record == nil {
				continue
			}
		}
		if service.Port != nil && record.Routed != *service.Port && record.Ports[*service.Port] == 0 {
			cctx.Logger.Warn(fmt.Sprintf(
				"%s's port %d is not pinned, so the running container does not publish it and it is not routed. '%s shared up --recreate %s' replaces the container and routes it.",
				service.Name, *service.Port, cctx.CommandName, service.Name))
		}
		if host := record.RoutedHostPort(); host != 0 {
			if !secured {
				if err := router.SecureState(cctx.RouterState); err != nil {
					return clierr.Newf("Could not prepare the router's state directory for %s's route file: %s", service.Name, err)
				}
				secured = true
			}
			if err := router.WriteSharedFragment(cctx.RouterState, repositoryID, record.Name, record.Domain, host); err != nil {
				return clierr.Newf("Could not write %s's route file: %s", service.Name, err)
			}
			continue
		}
		if err := router.RemoveSharedFragment(cctx.RouterState, repositoryID, record.Name); err != nil {
			return clierr.Newf("Could not remove %s's route file: %s", service.Name, err)
		}
	}
	return nil
}

// unrouteShared removes the route files of the shared services in names, for
// `shared down`: no hostname keeps routing to a port nothing is about to
// listen on. Compose service names outside the shared-name alphabet (a
// dependency started only to serve a declared service) have no route file
// and are skipped. Failing to remove one is a hard failure, like unrouteWorktree.
func unrouteShared(cctx *CommandContext, repositoryID string, names []string) error {
	for _, name := range names {
		if !registry.ValidSharedName(name) {
			continue
		}
		if err := router.RemoveSharedFragment(cctx.RouterState, repositoryID, name); err != nil {
			return clierr.Newf("Could not remove %s's route file: %s", name, err)
		}
	}
	return nil
}

// sharedRoutedPort is the container port a shared service declares as its web
// UI, or 0 when it declares none.
func sharedRoutedPort(service config.SharedService) int {
	if service.Port == nil {
		return 0
	}
	return *service.Port
}

// hadRoute reports whether any of names has a route file now, so a caller
// that is about to remove them knows the overview is about to change.
func hadRoute(cctx *CommandContext, repositoryID string, names []string) bool {
	for _, name := range names {
		if !registry.ValidSharedName(name) {
			continue
		}
		if _, err := os.Lstat(router.SharedFragmentPath(cctx.RouterState, repositoryID, name)); err == nil {
			return true
		}
	}
	return false
}

// hasRoutedRecord reports whether any of names has a record that routes a web
// UI, so the overview is refreshed after the service goes even when its
// route file was already gone. An unreadable record counts as not.
func hasRoutedRecord(cctx *CommandContext, repositoryID string, names []string) bool {
	store := cctx.Registry.Store(repositoryID)
	for _, name := range names {
		if !registry.ValidSharedName(name) {
			continue
		}
		if record, err := store.FindShared(name); err == nil && record != nil && record.Routed != 0 {
			return true
		}
	}
	return false
}

// routeNamesFor are the shared services `shared down` considers unrouting
// besides the ones whose containers it removes: those it was given, else the
// declared ones, else every service the repository has a record of.
func routeNamesFor(cctx *CommandContext, repositoryID string, given []string) []string {
	if len(given) > 0 {
		return given
	}
	if cctx.Loaded != nil {
		if names := sharedNames(cctx.Loaded.Config.SharedServices()); len(names) > 0 {
			return names
		}
	}
	var names []string
	if records, _, err := cctx.Registry.Store(repositoryID).SharedRecords(); err == nil {
		for _, record := range records {
			names = append(names, record.Name)
		}
	}
	return names
}

// idleServices are the services among names that have no running container of
// the stack, read under the shared lock: only those can have a route that
// points at nothing. A service whose container is up keeps its route. The
// filter is a defensive safety net: a running container that fails ownership
// proof already makes planSharedDown fail before this runs, so today it never
// changes the answer, and it stays so a future change to the plan cannot
// unroute a service that is still serving.
func idleServices(ctx context.Context, stack *shared.Stack, names []string) ([]string, error) {
	containers, err := stack.Containers(ctx)
	if err != nil {
		return nil, err
	}
	var idle []string
	for _, name := range names {
		running := false
		for _, container := range containers {
			if container.Labels[backend.ComposeServiceLabel] == name && (container.Status == "running" || container.Status == "restarting") {
				running = true
			}
		}
		if !running {
			idle = append(idle, name)
		}
	}
	return idle, nil
}

// selectionNeedsRouter reports whether starting selected can route a web UI:
// one of them declares a `port`, or a declared service one of them depends on
// does, since a start brings its dependencies up and routes them too. The
// dependencies come from the shared file, with the same closure the start
// uses. When another declared service routes and the file or the repository
// cannot be read to rule it out, the answer is yes: the start that follows
// reports the real problem, and a router that was not needed is harmless.
func selectionNeedsRouter(ctx context.Context, cctx *CommandContext, selected []config.SharedService) bool {
	if anyRouted(selected) {
		return true
	}
	declared := cctx.Loaded.Config.SharedServices()
	var others []config.SharedService
	for _, service := range declared {
		if service.Port != nil && !slices.ContainsFunc(selected, func(s config.SharedService) bool { return s.Name == service.Name }) {
			others = append(others, service)
		}
	}
	if len(others) == 0 {
		return false
	}
	repositoryID, present, err := cctx.Registry.Identities.RepositoryID(ctx, cctx.Registry.Identities.Root)
	if err != nil {
		return true
	}
	if !present {
		return false // nothing can start for a repository that was never brought up
	}
	roots, err := sharedWorktreeRoots(ctx, cctx, repositoryID)
	if err != nil {
		return true
	}
	definition, err := cctx.sharedStack(repositoryID, cctx.Loaded.Config.LabelNamespace(), roots).ReadDefinition(ctx)
	if err != nil {
		return true
	}
	for _, service := range selected {
		closure := definition.Closure(service.Name)
		for _, other := range others {
			if slices.Contains(closure, other.Name) {
				return true
			}
		}
	}
	return false
}
