package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sofired/treerig/internal/backend"
	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/hooks"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/overview/card"
	"github.com/sofired/treerig/internal/progress"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/router"
	"github.com/sofired/treerig/internal/worktree"
)

// Router is what `up` needs from the shared router. Up hands
// UpWithContext sharedRouter (router.go), the real one; tests hand it a
// double.
type Router interface {
	// Port is what a routed service's URL is printed with; 80 needs no suffix.
	Port() int
	// Ensure makes sure the shared router is up.
	Ensure(ctx context.Context) error
}

// Index is what `up` needs from the overview; overview.Service
// (#41) satisfies this, and Up hands UpWithContext bestEffortIndex, which
// also survives the service failing to build.
type Index interface {
	// RefreshBestEffort regenerates the shared index page, warning rather
	// than failing the caller if it cannot — a stale index is an
	// inconvenience, a failed `up` because of it would not be.
	RefreshBestEffort(ctx context.Context, reason string)
}

// UpOptions are `up`'s flags.
type UpOptions struct {
	// Services are the Compose services to start; empty means every service
	// the backend's definition declares.
	Services []string
	// ReallocatePorts names routed services to give a new loopback port
	// (--reallocate-port), refused while this worktree's own container
	// holds the current one.
	ReallocatePorts []string

	// follow, set only by Dev, turns this `up` into the first half of a
	// development session: the mutation runs holding the development-session
	// lock beside the operation lock, and once the operation lock is released
	// follow runs holding the development lock alone. It reports how the
	// session ended and so always returns an error (an interrupted session
	// included); UpWithContext returns it as its own.
	follow func(ctx context.Context, ref worktree.Ref, host string, runtime *backend.Compose) error
}

// UpResult is what `up` did.
type UpResult struct {
	Registration registry.EnsuredRegistration
	// Hooks are the stages that ran, in order.
	Hooks []config.HookStage
	URLs  []ServiceURL
}

// Up is `up`: the CommandContext, the real shared router, and the real
// overview, all built from env.
func Up(ctx context.Context, env *Env, options UpOptions) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	// Named routerSvc, not router (found in review, round 2 of #55): a
	// local `router` would shadow the internal/router package this file
	// also imports.
	routerSvc, err := realRouter(env, cctx)
	if err != nil {
		return err
	}
	_, err = UpWithContext(ctx, cctx, routerSvc, bestEffortIndex{cctx}, options)
	return err
}

// realRouter is the shared router `up` and `dev` start, built from env and
// from cctx's own resolution of the treerig root and port, not a second one
// (found in review of #79): the URL table `up` prints and the router it
// starts must never read settings.toml separately.
func realRouter(env *Env, cctx *CommandContext) (Router, error) {
	state := cctx.RouterState
	svc, err := routerServiceWith(env, cctx.Home, state, cctx.RouterPortResolution)
	if err != nil {
		return nil, err
	}
	return sharedRouter{env: env, svc: svc, state: state}, nil
}

// bestEffortIndex is the Index `up` refreshes afterwards: the same
// refreshOverviewBestEffort `down` and `worktree remove` call, so a checkout
// index that cannot even be built (a symlinked router state directory)
// warns rather than failing an `up` that has already succeeded.
type bestEffortIndex struct{ cctx *CommandContext }

// RefreshBestEffort satisfies Index by delegating to refreshOverviewBestEffort
// against the wrapped CommandContext.
func (i bestEffortIndex) RefreshBestEffort(ctx context.Context, reason string) {
	refreshOverviewBestEffort(ctx, i.cctx, reason)
}

// routeSpecsFromConfig is every declared [service.<name>] as a backend.RouteSpec,
// in config order — shared by upRuntime (which needs it to build the
// worktree's own Compose runtime) and UpWithContext's own fragment-writing
// step (which needs it again, after routerSvc's Router is out of scope, to
// name and rule each route the same way). ports are the loopback ports
// allocated to the routed services (#119); a service without one publishes
// nothing.
func routeSpecsFromConfig(services []config.Service, ports map[string]int) []backend.RouteSpec {
	routes := make([]backend.RouteSpec, len(services))
	for i, service := range services {
		routes[i] = backend.RouteSpec{Service: service.Name, Subdomain: service.Subdomain, Path: service.Path, Port: service.Port, HostPort: ports[service.Name]}
	}
	return routes
}

// upRuntime is the Compose runtime up starts services through: the same
// construction as CommandContext.RuntimeFor, plus the routes runtime.Up needs
// and no other verb does (urlVariables is composeURLVariables' result).
// RuntimeFor has neither the explicit ports nor the routes, which only `up`
// knows, so up builds its runtime here.
func upRuntime(cctx *CommandContext, namespace string, ports map[string]int, urlVariables [][2]string, observe func(line string)) *backend.Compose {
	routes := routeSpecsFromConfig(cctx.Loaded.Config.Service, ports)
	return backend.NewCompose(backend.Options{
		ComposeFile:    cctx.ComposeFile(),
		ProjectRoot:    cctx.Loaded.Root,
		ConfigFile:     cctx.Loaded.File,
		ProjectName:    cctx.Loaded.Config.Project.Name,
		Domain:         cctx.Loaded.Config.Project.Domain,
		ControlHome:    cctx.ControlHome,
		LabelNamespace: namespace,
		Runner:         cctx.Runner,
		Logger:         cctx.Logger,
		Environ:        cctx.Environ,
		URLVariables:   urlVariables,
		Routes:         routes,
		SharedUse:      sharedUseMap(cctx.Loaded.Config),
		Verbose:        cctx.Verbose,
		Observe:        observe,
	})
}

// upStages is which lifecycle stages `up` asks the sequencer for: every
// stage hooks.UpStages names, except seed on a worktree that has already
// come up once — derived from that single source rather than a hard-coded
// copy of its own. Verify is never among them: it is not part of UpStages
// at all — `doctor` runs it, on its own.
func upStages(firstTime bool) []config.HookStage {
	var stages []config.HookStage
	for _, stage := range hooks.UpStages() {
		if stage == config.Seed && !firstTime {
			continue
		}
		stages = append(stages, stage)
	}
	return stages
}

// hostForIdentity is the best host UpWithContext can name identity by
// before Registry.Ensure has run — the nested-invocation check (#108) fires
// ahead of Ensure, since Ensure itself takes the worktree lock. When
// identity is already registered (the ordinary case: a hook only exists
// once its own `up` has registered the worktree at least once), its stored
// slug and domain are read without taking any lock, exactly as Ensure reads
// them to name the lock it is about to wait on. Otherwise there is no
// record yet to read, and the host is our best guess: the slug
// AllocateSlug would start from, under the config's own domain, which may
// differ from what Ensure ends up allocating if that base slug collides
// with another worktree's. A record that cannot be read falls back the same
// way, deliberately: the host only names the worktree in a refusal that is
// already certain, so a registry error is no reason to replace it, and
// Registry.Ensure reports that error on any up that is not refused.
func hostForIdentity(cctx *CommandContext, identity registry.WorktreeIdentity) string {
	if record, err := cctx.Registry.Store(identity.RepositoryID).Find(identity.WorktreeID); err == nil && record != nil {
		return layout.WorktreeHost(record.Slug, record.Domain)
	}
	return layout.WorktreeHost(registry.BranchSlug(identity.Branch, identity.Root), cctx.Loaded.Config.Project.Domain)
}

// UpWithContext is `up`, operating directly on a CommandContext. Register
// (or reconcile), make sure the shared router is up, start, then run hooks:
// prepare on every up, seed only the first time this worktree comes up.
// The worktree lock is held for the mutation phase; registering and
// waiting for the router happen unlocked, so a slow router start cannot
// hold the worktree lock the whole time.
func UpWithContext(ctx context.Context, cctx *CommandContext, routerSvc Router, index Index, options UpOptions) (*UpResult, error) {
	// Refuses before anything else here — including refuseRelabelling
	// below, which can itself make a Docker call — a control character in
	// [service.<name>.index] label or purpose (#19). The base loader that built
	// cctx.Loaded stays lenient; only a command about to start something checks.
	if err := config.CheckStart(cctx.Loaded); err != nil {
		return nil, err
	}
	if err := validateReallocation(cctx.Loaded.Config.Service, options.ReallocatePorts); err != nil {
		return nil, err
	}
	// Registry.Ensure takes the worktree's operation lock before mutate
	// does, so the nested-invocation check runs here too, on the identity
	// as it stands now: a checkout with none yet has no lock to wait on.
	// mutate still checks on its own, against the registered ref.
	identity, err := cctx.Registry.Identities.Inspect(ctx, cctx.Registry.Identities.Root, registry.InspectOptions{})
	if err != nil {
		return nil, err
	}
	// The host is looked up only when refusing: every other up would pay
	// for a registry read it never uses.
	if identity != nil && cctx.insideWorktree(identity.WorktreeID) {
		return nil, ownWorktreeLockRefusal(cctx.CommandName, hostForIdentity(cctx, *identity))
	}
	if err := refuseRelabelling(ctx, cctx, identity); err != nil {
		return nil, err
	}
	if err := refuseHostRouteClashesOf(cctx, identity); err != nil {
		return nil, err
	}
	registration, err := cctx.Registry.Ensure(ctx, "")
	if err != nil {
		return nil, err
	}
	if err := routerSvc.Ensure(ctx); err != nil {
		return nil, err
	}
	ref := toRef(registration.RegisteredWorktree)
	host := layout.WorktreeHost(ref.Slug, registration.Domain)
	store := cctx.Registry.Store(ref.RepositoryID)
	ownNamespace := cctx.Loaded.Config.LabelNamespace()

	// Shared services (docs/design/routing.md, decision 18): started or
	// reused under the shared lock alone, before the worktree's operation
	// lock is taken, so that lock is never held waiting on a slow start. If
	// one cannot start, nothing of the worktree is touched.
	sharedUsed, err := startSharedForUp(ctx, cctx, &registration.RegisteredWorktree, routerSvc.Port(), ownNamespace)
	if err != nil {
		return nil, err
	}

	// serviceRoutes (named apart from the mutation closure's own routes,
	// routeSpecsFromConfig below) feeds both urls, printed below, and the
	// hooks' own TREERIG_URL/TREERIG_SERVICE_URL_<NAME> (#28) — computed once.
	// Compose gets the same values (#177), built inside the mutation from the
	// record.
	serviceRoutes := worktree.Routes(cctx.Loaded.Config.Service, ref.Slug, registration.Domain, routerSvc.Port())
	urls := urlsFromRoutes(cctx.Loaded.Config.Service, serviceRoutes)

	// hookRoutes are serviceRoutes with each service's loopback port, which
	// only exists once the mutation has allocated it (#119).
	hookRoutes := serviceRoutes
	var ranHooks []config.HookStage
	mutation := func(ctx context.Context) error {
		// The router may have taken minutes to start, and the lock was free
		// meanwhile: a removal can have finished. Nothing is created unless
		// the registration and the identity are still the ones Ensure saw.
		found, err := store.Find(ref.WorktreeID)
		if err != nil {
			return err
		}
		if found == nil {
			return clierr.Newf(
				"%s was removed while '%s up' was waiting for the shared router; run it again to re-register.",
				host, cctx.CommandName,
			)
		}
		if found.LabelNamespace != nil && *found.LabelNamespace != ownNamespace {
			return clierr.Newf(
				"%s was re-labelled under %s while '%s up' was waiting for the shared router; run it again.",
				host, *found.LabelNamespace, cctx.CommandName,
			)
		}
		identity, err := cctx.Registry.Identities.Inspect(ctx, cctx.Registry.Identities.Root, registry.InspectOptions{})
		if err != nil {
			return err
		}
		if identity == nil || identity.WorktreeID != ref.WorktreeID {
			return clierr.Newf(
				"This checkout's %s markers changed or were removed while '%s up' was waiting for the shared router; run it again.",
				brand.Name, cctx.CommandName,
			)
		}

		// Again, against the record as it stands under the lock: a `route add`
		// may have finished since the check before the router started.
		if err := refuseHostRouteClashes(cctx.CommandName, cctx.Loaded.Config, found.HostRoutes); err != nil {
			return err
		}

		// A removal that was started and never finished (the worktree reads as
		// removing) is called off by bringing the worktree up again: the
		// marker would otherwise outrank everything the containers say. A
		// marker of a newer format is refused, not cleared.
		if removing, err := store.Removing(ref.WorktreeID); err != nil {
			return err
		} else if removing {
			if err := store.ClearRemoving(ctx, ref.WorktreeID, cctx.writer()); err != nil {
				return err
			}
		}

		initialization, err := store.Initialization(ref.WorktreeID, ref.Slug)
		if err != nil {
			return err
		}
		firstTime := initialization == registry.Pending

		worktreeLayout := layout.WorktreeLayout(cctx.ControlHome, ref.RepositoryID, ref.WorktreeID)
		if err := files.SecureWorktreeDirectories(worktreeLayout, cctx.UserHome); err != nil {
			return err
		}

		// Ports exist before anything starts: allocated under the registry
		// lock (inside this operation lock), refused if something else holds
		// one, and published by the override the runtime writes (#119).
		// The worktree's routed URLs for Compose (#177), from the record as it
		// stands under the lock and the router port the hooks get: one value
		// for every Compose call below. startSharedForUp, earlier, builds the
		// same from the registration Ensure returned and the same router port.
		urlVariables := composeURLVariables(cctx.Loaded.Config, found, routerSvc.Port())
		inspector := upRuntime(cctx, ownNamespace, nil, urlVariables, nil)
		// Refused before allocating, so no port is ever recorded for a
		// service that cannot publish one.
		if err := inspector.CheckPublishable(ctx, ref); err != nil {
			return err
		}
		ports, err := allocatePorts(ctx, cctx, inspector, ref, host, options)
		if err != nil {
			return err
		}
		routed := startingRouted(routedServiceNames(cctx.Loaded.Config.Service), options.Services)
		if err := checkPortsFree(ctx, cctx, inspector, ref, routed, ports); err != nil {
			return err
		}
		// Host routes (#6) are listed beside the services' routes, from the
		// record as it stands under the lock; `up` itself never writes or
		// removes one.
		allRoutes := routesOf(cctx.Loaded.Config, ref.Slug, registration.Domain, routerSvc.Port(), ports, found.HostRoutes)
		hookRoutes = allRoutes.All()
		urls = allRoutes.URLs()

		// Every declared service is recorded, not only the ones with a
		// [service.<name>.index] block (#16): Label (and Purpose alongside it) is
		// "" for one that never opted in, which keeps it off the page as a
		// link but still lets the index count it — a stopped, unindexed
		// service must still turn a running row into partly running, since it is a
		// real declared service that is not answering, not one the page
		// simply never knew about. See card.IndexService's own comment.
		// The card lists the services only: a host process is no container for
		// the index to count (the index shows host routes from the record).
		cardURLs := allRoutes.ServiceURLs()
		services := make([]card.IndexService, len(cardURLs))
		for i, entry := range cardURLs {
			var label, purpose string
			if entry.Label != nil {
				label = *entry.Label
			}
			if entry.Purpose != nil {
				purpose = *entry.Purpose
			}
			services[i] = card.IndexService{Name: entry.Service, URL: entry.URL, Label: label, Purpose: purpose}
		}
		if err := card.WriteIndexCard(cctx.ControlHome, ref.RepositoryID, ref.WorktreeID, card.IndexCard{
			Project: cctx.Loaded.Config.Project.Name, Domain: registration.Domain,
			Slug: ref.Slug, Host: host, Root: registration.Root, Branch: registration.Branch,
			LabelNamespace: ownNamespace, Services: services, UpdatedAt: card.Timestamp(time.Now()),
		}); err != nil {
			return err
		}

		what := "all services"
		if len(options.Services) > 0 {
			what = strings.Join(options.Services, ", ")
		}
		// What Compose last said, for the heartbeat to carry: without -v a hung
		// or killed start still leaves its last words.
		var latest progress.Latest
		runtime := upRuntime(cctx, ownNamespace, ports, urlVariables, latest.Set)
		start := func() error {
			return cctx.phase(progress.PhaseSpec{Verb: "Starting", What: what + " for " + host, Note: latest.Get}, func() error {
				return runtime.Up(ctx, ref, options.Services)
			})
		}
		err = start()
		if err != nil && len(sharedUsed) > 0 && backend.IsActiveEndpoints(err) {
			// Compose wanted to recreate the worktree's network, which a shared
			// container is attached to, and had already stopped the containers
			// when it failed: a second `compose up` cannot succeed. Detach the
			// shared containers, which needs only the operation lock this
			// closure holds, so the `down` and `up` the message asks for work.
			if derr := disconnectShared(ctx, cctx, ref, ownNamespace, ownNamespace); derr != nil {
				cctx.Logger.Warn(fmt.Sprintf("Could not disconnect the shared services from this worktree's network: %v", derr))
			} else {
				cctx.Logger.Info("Disconnected the shared services from this worktree's network.")
			}
			return clierr.Wrap(fmt.Sprintf("Compose cannot recreate this worktree's network while a shared service is attached to it. Run '%s down' and then '%s up' again; the shared services keep running.", cctx.CommandName, cctx.CommandName), err)
		}
		if err != nil {
			return explainStartFailure(ctx, cctx, inspector, ref, routed, ports, err)
		}
		if err := releaseDroppedPorts(ctx, cctx, ref, options); err != nil {
			return err
		}

		// Connect the shared services to this worktree's networks: after its
		// own `compose up` and before its hooks, which may send mail. This
		// is inside the operation lock, with the shared lock nested.
		if len(sharedUsed) > 0 {
			if err := connectWorktree(ctx, cctx, ref, ownNamespace, sharedUsed, true); err != nil {
				return err
			}
		}

		// Routing: route files are rewritten in full on every `up`, but
		// unconditionally — not only when this worktree currently declares a
		// [service.<name>] route: a worktree whose last route was dropped
		// since the last `up` must lose its stale file. Each file targets the
		// service's loopback port (treerig-loopback:<port>), which was
		// committed to the registration above, before any file names it
		// (docs/design/routing.md, decision 9). New files are written before
		// old ones are removed, and only the ones no longer current are
		// removed, so a re-up with the same or overlapping routes is never
		// briefly unrouted.
		routes := routeSpecsFromConfig(cctx.Loaded.Config.Service, ports)
		keep := make(map[string]bool, len(routes))
		for _, route := range routes {
			name := backend.RouteName(ref, route)
			rule := backend.RouteRule(route, host)
			doc := router.NewFragmentDocument(name, rule, ports[route.Service])
			if err := router.WriteFragment(cctx.RouterState, ref.RepositoryID, ref.WorktreeID, route.Service, doc); err != nil {
				return clierr.Newf("Could not write %s's route file: %s", route.Service, err)
			}
			keep[route.Service] = true
		}
		if err := router.RemoveWorktreeFragmentsExcept(cctx.RouterState, ref.RepositoryID, ref.WorktreeID, keep); err != nil {
			return clierr.Newf("Could not remove this worktree's earlier route files: %s", err)
		}

		ran, err := hooks.RunLifecycle(ctx, hooks.HookContext{
			Source: worktree.EnvironmentSource{
				ProjectName: cctx.Loaded.Config.Project.Name,
				// The registered domain, not the config's: hooks address the
				// hostname that is actually routed.
				Domain:      registration.Domain,
				ControlHome: cctx.ControlHome,
				ConfigFile:  cctx.Loaded.File,
				ProjectRoot: cctx.Loaded.Root,
			},
			Ref: ref, Hooks: cctx.Loaded.Config.Hooks, Logger: cctx.Logger, Progress: cctx.Progress, Runner: cctx.Runner, Environ: cctx.Environ,
			ComposeFile: cctx.ComposeFile(),
			Routes:      hookRoutes, Shared: sharedAddrs(cctx, ref.RepositoryID),
			SharedURLs: sharedURLs(cctx, ref.RepositoryID, routerSvc.Port()),
		}, hooks.LifecycleOptions{Stages: upStages(firstTime)})
		if err != nil {
			return err
		}
		ranHooks = ran

		return store.CompleteInitialization(ctx, ref.WorktreeID, ref.Slug, cctx.writer())
	}
	// What follows a successful mutation, once the operation lock is back:
	// the overview is refreshed, and the worktree is announced.
	refresh := func(ctx context.Context) { index.RefreshBestEffort(ctx, "up") }
	announce := func() {
		cctx.Logger.Info(host + " is up.")
		printURLs(cctx, urls)
	}

	if options.follow == nil {
		if err := cctx.mutate(ctx, ref, host, false, mutation); err != nil {
			return nil, err
		}
		// The announcement first, as `dev` does: the refresh may wait on the
		// router's lock, and the user should not wait to see the URLs.
		announce()
		refresh(ctx)
		return &UpResult{Registration: *registration, Hooks: ranHooks, URLs: urls}, nil
	}
	// `dev`: the same mutation, but under the development-session lock too,
	// which outlives it. The operation lock goes back as soon as the
	// mutation is done, and announcing the worktree, refreshing the index
	// and following its logs happen with only the development lock held.
	// The announcement comes first: the refresh may wait on the router lock
	// for up to a minute, and the user should not wait to see the URLs.
	err = cctx.mutateSession(ctx, ref, host, mutation, func(ctx context.Context) error {
		announce()
		refresh(ctx)
		return options.follow(ctx, ref, host, cctx.RuntimeFor(ownNamespace, &registration.RegisteredWorktree, routerSvc.Port()))
	})
	return nil, err
}

// refuseHostRouteClashesOf is refuseHostRouteClashes for an `up` that has not
// registered yet, so that a clash is refused before anything is registered or
// started. It reads the record without a lock, so UpWithContext checks again
// under the operation lock, against the record as it stands there. identity is
// this checkout's, as UpWithContext read it; nil for one with no identity yet,
// which has no host routes.
func refuseHostRouteClashesOf(cctx *CommandContext, identity *registry.WorktreeIdentity) error {
	if identity == nil {
		return nil
	}
	current, err := cctx.Registry.Store(identity.RepositoryID).Find(identity.WorktreeID)
	if err != nil || current == nil {
		return err
	}
	return refuseHostRouteClashes(cctx.CommandName, cctx.Loaded.Config, current.HostRoutes)
}

// refuseRelabelling refuses to change a worktree's label namespace once it
// owns anything under the old one. Compose relabels containers on the next
// `up` but never volumes or networks, so once
// resources exist under one namespace the record must keep naming it —
// otherwise `remove` could no longer find them. A changed label-namespace is
// therefore honoured only for a worktree that owns nothing under the old
// one; anything else must be removed first.
//
// Checked before Ensure rewrites the record, and so before the worktree
// lock. Two `up`s racing from one checkout with a config edit between them
// could still slip resources in under the old namespace; the under-lock
// re-check in UpWithContext catches the record changing, not that narrower
// case. identity is this checkout's, as UpWithContext read it; nil for one
// with no identity yet.
func refuseRelabelling(ctx context.Context, cctx *CommandContext, identity *registry.WorktreeIdentity) error {
	if identity == nil {
		return nil
	}
	current, err := cctx.Registry.Store(identity.RepositoryID).Find(identity.WorktreeID)
	if err != nil {
		return err
	}
	if current == nil {
		return nil
	}
	own := cctx.Loaded.Config.LabelNamespace()
	recorded := card.RecordedLabelNamespace(cctx.ControlHome, card.Registration{
		RepositoryID: current.RepositoryID, WorktreeID: current.WorktreeID, LabelNamespace: current.LabelNamespace,
	})
	if recorded == nil || *recorded == own {
		return nil
	}
	leftovers, err := cctx.LabelRuntime(*recorded).Inventory(ctx, toRef(*current))
	if err != nil {
		return err
	}
	if len(leftovers) == 0 {
		return nil
	}
	return clierr.Newf(
		"This worktree's resources are labelled under %s, but %s now says %s. "+
			"Run '%s worktree remove' first — it uses the recorded labels — then '%s up' again.",
		*recorded, brand.ConfigFile, own, cctx.CommandName, cctx.CommandName,
	)
}
