package commands

import (
	"context"
	"strings"
	"time"

	"github.com/sofired/berth/internal/backend"
	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/checkoutindex/card"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/hooks"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/worktree"
)

// Router is what `up` needs from the shared router. Up hands
// UpWithContext sharedRouter (router.go), the real one; tests hand it a
// double.
type Router interface {
	// Port is what a routed service's URL is printed with; 80 needs no suffix.
	Port() int
	// Network is the shared router's Docker network; routed services join it.
	Network() string
	// Ensure makes sure the shared router is up.
	Ensure(ctx context.Context) error
}

// Index is what `up` needs from the checkout index; checkoutindex.Service
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
}

// UpResult is what `up` did.
type UpResult struct {
	Registration registry.EnsuredRegistration
	// Hooks are the stages that ran, in order.
	Hooks []config.HookStage
	URLs  []ServiceURL
}

// Up is `up`: the CommandContext, the real shared router, and the real
// checkout index, all built from env.
func Up(ctx context.Context, env *Env, options UpOptions) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	svc, state, err := newRouterService(env)
	if err != nil {
		return err
	}
	router := sharedRouter{env: env, svc: svc, state: state, controlHome: cctx.ControlHome}
	_, err = UpWithContext(ctx, cctx, router, bestEffortIndex{cctx}, options)
	return err
}

// bestEffortIndex is the Index `up` refreshes afterwards: the same
// refreshIndexBestEffort `down` and `worktree remove` call, so a checkout
// index that cannot even be built (a symlinked router state directory)
// warns rather than failing an `up` that has already succeeded.
type bestEffortIndex struct{ cctx *CommandContext }

func (i bestEffortIndex) RefreshBestEffort(ctx context.Context, reason string) {
	refreshIndexBestEffort(ctx, i.cctx, reason)
}

// upRuntime is the Compose runtime up starts services through: the same
// construction as CommandContext.Runtime, plus the routes and router network
// runtime.Up needs and no other verb does. CommandContext.Runtime lives in
// context.go, a file this work package leaves alone (see the port's shared-
// file rule); duplicating its handful of fields here is cheaper than
// widening that shared method's signature for one caller.
func upRuntime(cctx *CommandContext, namespace string, router Router) *backend.Compose {
	routes := make([]backend.RouteSpec, len(cctx.Loaded.Config.Service))
	for i, service := range cctx.Loaded.Config.Service {
		routes[i] = backend.RouteSpec{Service: service.Name, Subdomain: service.Subdomain, Path: service.Path, Port: service.Port}
	}
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
		Routes:         routes,
		RouterNetwork:  router.Network(),
	})
}

// upStages is which lifecycle stages `up` asks the sequencer for: every
// stage hooks.UpStages names, except seed on a worktree that has already
// come up once — derived from that single source rather than a hard-coded
// copy, the way TypeScript's own lifecycleStages filters LIFECYCLE_ORDER by
// UP_STAGES instead of hard-coding a list of its own. Verify is never among
// them: it is not part of UpStages at all — `doctor` runs it, on its own.
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

// UpWithContext is worktree.ts's `up`, operating directly on a
// CommandContext. Register (or reconcile), make sure the shared router is
// up, start, then run hooks: prepare on every up, seed only the first time
// this worktree comes up. The worktree lock is held for the mutation phase;
// registering and waiting for the router happen unlocked, so a slow router
// start cannot hold the worktree lock the whole time.
func UpWithContext(ctx context.Context, cctx *CommandContext, router Router, index Index, options UpOptions) (*UpResult, error) {
	if err := refuseRelabelling(ctx, cctx); err != nil {
		return nil, err
	}
	registration, err := cctx.Registry.Ensure(ctx, "")
	if err != nil {
		return nil, err
	}
	if err := router.Ensure(ctx); err != nil {
		return nil, err
	}
	ref := toRef(registration.RegisteredWorktree)
	host := layout.WorktreeHost(ref.Slug, registration.Domain)
	store := cctx.Registry.Store(ref.RepositoryID)
	ownNamespace := cctx.Loaded.Config.LabelNamespace()

	urls := WorktreeURLs(cctx.Loaded.Config, ref.Slug, registration.Domain, router.Port())

	var ranHooks []config.HookStage
	err = cctx.mutate(ctx, ref, false, func(ctx context.Context) error {
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

		initialization, err := store.Initialization(ref.WorktreeID, ref.Slug)
		if err != nil {
			return err
		}
		firstTime := initialization == registry.Pending

		worktreeLayout := layout.WorktreeLayout(cctx.ControlHome, ref.RepositoryID, ref.WorktreeID)
		if err := files.SecureWorktreeDirectories(worktreeLayout); err != nil {
			return err
		}

		// Every declared service is recorded, not only the ones with a
		// [service.index] block (#16): Label (and Purpose alongside it) is
		// "" for one that never opted in, which keeps it off the page as a
		// link but still lets the index count it — a stopped, unindexed
		// service must still turn a Running row into Partial, since it is a
		// real declared service that is not answering, not one the page
		// simply never knew about. See card.IndexService's own comment.
		services := make([]card.IndexService, len(urls))
		for i, entry := range urls {
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
		runtime := upRuntime(cctx, ownNamespace, router)
		if err := withProgress(cctx.Logger, "Starting "+what+" for "+host, func() error {
			return runtime.Up(ctx, ref, options.Services)
		}); err != nil {
			return err
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
			Ref: ref, Hooks: cctx.Loaded.Config.Hooks, Logger: cctx.Logger, Runner: cctx.Runner, Environ: cctx.Environ,
		}, hooks.LifecycleOptions{Stages: upStages(firstTime)})
		if err != nil {
			return err
		}
		ranHooks = ran

		return store.CompleteInitialization(ctx, ref.WorktreeID, ref.Slug, cctx.writer())
	})
	if err != nil {
		return nil, err
	}

	index.RefreshBestEffort(ctx, "up")
	cctx.Logger.Info(host + " is up.")
	printURLs(cctx, urls)
	return &UpResult{Registration: *registration, Hooks: ranHooks, URLs: urls}, nil
}

// refuseRelabelling ports src/commands/worktree.ts:204-221. Compose relabels
// containers on the next `up` but never volumes or networks, so once
// resources exist under one namespace the record must keep naming it —
// otherwise `remove` could no longer find them. A changed label-namespace is
// therefore honoured only for a worktree that owns nothing under the old
// one; anything else must be removed first.
//
// Checked before Ensure rewrites the record, and so before the worktree
// lock. Two `up`s racing from one checkout with a config edit between them
// could still slip resources in under the old namespace; the under-lock
// re-check in UpWithContext catches the record changing, not that narrower
// case.
func refuseRelabelling(ctx context.Context, cctx *CommandContext) error {
	identity, err := cctx.Registry.Identities.Inspect(ctx, cctx.Registry.Identities.Root, registry.InspectOptions{})
	if err != nil {
		return err
	}
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
	leftovers, err := cctx.Runtime(*recorded).Inventory(ctx, toRef(*current))
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
