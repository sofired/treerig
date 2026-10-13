package commands

// The route verbs (#6, docs/design/routing.md, decision 11): `route add`,
// `route rm` and `route ls` send the worktree's hostname to a process running
// on the host through the same proxy, and the same route files, as a Compose
// service. The registration record holds the routes (hostRoutes) and the
// route files are derived from it. Adding and removing take the worktree's
// operation lock, nest the registry lock inside it for the record, and write
// a route file only for a record this control home holds. `up` and `down`
// leave host routes alone; `worktree remove` removes them before the record.

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/router"
	"github.com/sofired/berth/internal/text"
	"github.com/sofired/berth/internal/worktree"
)

// RouteModes reads the proxy's mode, which decides whether a listener on ::1
// alone is reachable. *router.Router satisfies it; nil reads as unknown.
type RouteModes interface {
	ModeChoice(ctx context.Context) (router.ModeChoice, error)
}

// RouteAddOptions are `route add`'s argument and flags.
type RouteAddOptions struct {
	// Name is the route's name.
	Name string
	// Port is the port the host process listens on, at 127.0.0.1.
	Port int
	// Subdomain and Path are a [service.<name>] table's routing fields.
	Subdomain string
	Path      *string
}

// RouteAdd is `route add`.
func RouteAdd(ctx context.Context, env *Env, options RouteAddOptions) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	// The mode only matters for a warning, so a router that cannot even be
	// described just leaves it unknown.
	var modes RouteModes
	if svc, err := routerServiceWith(env, cctx.Home, cctx.RouterState, cctx.RouterPortResolution); err == nil {
		modes = svc
	}
	return RouteAddWithContext(ctx, cctx, modes, options)
}

// checkRouteFields refuses a name, port, subdomain or path a host route
// cannot have, in the words the schema uses for a service's.
func checkRouteFields(options RouteAddOptions) error {
	if why := config.CheckRouteName(options.Name); why != "" {
		return clierr.Newf("The route name %q %s.", options.Name, why)
	}
	if options.Port < 1 || options.Port > 65_535 {
		return clierr.Newf("--port must be a port between 1 and 65535, not %d.", options.Port)
	}
	if why := config.CheckSubdomain(options.Subdomain); why != "" {
		return clierr.Newf("--subdomain %s.", why)
	}
	if options.Path != nil {
		if why := config.CheckRoutePath(*options.Path); why != "" {
			return clierr.Newf("--path %s.", why)
		}
	}
	return nil
}

// RouteAddWithContext adds a host route to this registered worktree: under
// its operation lock, it records the route and writes its route file. The
// record comes first, because it is the source of truth (decision 9), and is
// taken back if the file cannot be written. Adding a route identical to one
// already recorded in every field is the heal for a recorded route whose file
// is missing: it rewrites the file and changes no record. The file and the
// URL are built from the record as it stands under the registry lock, not
// from the earlier read. Afterwards it refreshes the checkout index and says
// what the port answers on.
func RouteAddWithContext(ctx context.Context, cctx *CommandContext, modes RouteModes, options RouteAddOptions) error {
	if err := checkRouteFields(options); err != nil {
		return err
	}
	registration, err := cctx.requireRegistration(ctx)
	if err != nil {
		return err
	}
	ref := toRef(registration.RegisteredWorktree)
	host := layout.WorktreeHost(ref.Slug, registration.Domain)
	store := cctx.Registry.Store(ref.RepositoryID)
	route := registry.HostRoute{Name: options.Name, Port: options.Port, Subdomain: options.Subdomain, Path: options.Path}

	// locked is the record the registry lock saw; healed is true when the
	// route was already recorded and only its file is written again.
	var locked registry.RegisteredWorktree
	healed := false
	err = cctx.mutate(ctx, ref, host, false, func(ctx context.Context) error {
		_, err := store.UpdateHostRoutes(ctx, ref.WorktreeID, ref.Slug, cctx.writer(), func(record registry.RegisteredWorktree) ([]registry.HostRoute, error) {
			locked = record
			if existing, held := record.FindHostRoute(route.Name); held && existing.Same(route) {
				healed = true
				return record.HostRoutes, nil
			}
			if err := cctx.checkNewHostRoute(record, route); err != nil {
				return nil, err
			}
			return append(slices.Clone(record.HostRoutes), route), nil
		})
		if err != nil {
			return err
		}
		if err := router.WriteHostFragment(cctx.RouterState, ref.RepositoryID, ref.WorktreeID, route.Name, hostRouteDocument(locked, route)); err != nil {
			if !healed {
				// Taken back, so the record never names a route that has no
				// file. The caller's context may be the reason the write
				// failed, so the undo gets one of its own.
				undo, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				if _, undoErr := store.UpdateHostRoutes(undo, ref.WorktreeID, ref.Slug, cctx.writer(), withoutHostRoute(route.Name)); undoErr != nil {
					cctx.Logger.Warn(fmt.Sprintf("Could not take the route back out of the registration: %s", undoErr))
				}
			}
			return clierr.Newf("Could not write the route file of %q: %s", route.Name, err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	refreshIndexBestEffort(ctx, cctx, "route add")
	url := hostRouteURL(cctx, locked, route)
	if healed {
		cctx.Logger.Info(fmt.Sprintf("Host route %s was already recorded; wrote its route file again: %s to 127.0.0.1:%d.", route.Name, url, route.Port))
	} else {
		cctx.Logger.Info(fmt.Sprintf("Routed %s to 127.0.0.1:%d (host route %s).", url, route.Port, route.Name))
	}
	note := noteListener(ctx, cctx.listeners(), route.Port, modes)
	switch {
	case note.Warning != "":
		cctx.Logger.Warn(note.Warning)
	case note.Silent:
		cctx.Logger.Info(fmt.Sprintf("Nothing is listening on 127.0.0.1:%d yet; requests get a 502 until a process is.", route.Port))
	}
	return nil
}

// withoutHostRoute is the change that drops the route called name.
func withoutHostRoute(name string) func(registry.RegisteredWorktree) ([]registry.HostRoute, error) {
	return func(record registry.RegisteredWorktree) ([]registry.HostRoute, error) {
		return slices.DeleteFunc(slices.Clone(record.HostRoutes), func(route registry.HostRoute) bool { return route.Name == name }), nil
	}
}

// hostRouteURL is the URL route answers on for the worktree record, the one
// a service with that name and those fields would have.
func hostRouteURL(cctx *CommandContext, record registry.RegisteredWorktree, route registry.HostRoute) string {
	routes := routesOf(cctx.Loaded.Config, record.Slug, record.Domain, cctx.RouterPort, nil, []registry.HostRoute{route})
	return routes.Host[0].URL
}

// checkNewHostRoute refuses route when it cannot join record: under the
// registry lock, so what it reads cannot change before the route is written.
// A name is refused when it is a [service.<name>] table's, a shared service's
// (decision 15) or another host route's; a place (subdomain and path) when a
// service or another host route already answers there; a port when it is the
// router's own or a port berth pinned for a service.
func (c *CommandContext) checkNewHostRoute(record registry.RegisteredWorktree, route registry.HostRoute) error {
	cfg := c.Loaded.Config
	for _, service := range cfg.Service {
		if service.Name == route.Name {
			return clierr.Newf("%s is a service in %s; a host route needs a name of its own.", route.Name, brand.ConfigFile)
		}
	}
	for _, shared := range cfg.SharedServices() {
		if shared.Name == route.Name {
			return clierr.Newf("%s is a shared service in %s; a host route needs a name of its own, because containers reach the shared service by that name.", route.Name, brand.ConfigFile)
		}
	}
	if _, taken := record.FindHostRoute(route.Name); taken {
		return clierr.Newf("There is already a host route called %s. Remove it with '%s route rm %s' first.", route.Name, c.CommandName, route.Name)
	}
	for _, service := range cfg.Service {
		if (registry.HostRoute{Subdomain: service.Subdomain, Path: service.Path}).SamePlace(route) {
			return clierr.Newf("Service %s already answers at %s. Choose another --subdomain or --path.", service.Name, hostRouteURL(c, record, route))
		}
	}
	for _, other := range record.HostRoutes {
		if other.SamePlace(route) {
			return clierr.Newf("Host route %s already answers at %s. Choose another --subdomain or --path.", other.Name, hostRouteURL(c, record, route))
		}
	}
	if route.Port == c.RouterPort {
		return clierr.Newf("Port %d is the router's own, so a route to it would loop. Use the port the process listens on.", route.Port)
	}
	holder, err := registry.PortHolder(c.ControlHome, route.Port)
	if err != nil {
		return err
	}
	if holder != "" {
		return clierr.Newf("Port %d is the pinned loopback port of %s. A host route names a port a process on the host listens on.", route.Port, holder)
	}
	return nil
}

// RouteRm is `route rm`.
func RouteRm(ctx context.Context, env *Env, name string) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	return RouteRmWithContext(ctx, cctx, name)
}

// RouteRmWithContext removes a host route of this registered worktree, under
// its operation lock: the route file first, so a route that cannot be taken
// off the router is still recorded and can be removed again, and then the
// record's entry.
func RouteRmWithContext(ctx context.Context, cctx *CommandContext, name string) error {
	registration, err := cctx.requireRegistration(ctx)
	if err != nil {
		return err
	}
	ref := toRef(registration.RegisteredWorktree)
	host := layout.WorktreeHost(ref.Slug, registration.Domain)
	store := cctx.Registry.Store(ref.RepositoryID)

	var removed registry.HostRoute
	err = cctx.mutate(ctx, ref, host, false, func(ctx context.Context) error {
		current, err := store.Find(ref.WorktreeID)
		if err != nil {
			return err
		}
		if current == nil {
			return clierr.Newf("%s was removed while '%s route rm' was waiting; nothing left to do.", host, cctx.CommandName)
		}
		route, held := current.FindHostRoute(name)
		if !held {
			return clierr.Newf("There is no host route called %q. '%s route ls' lists them.", name, cctx.CommandName)
		}
		removed = route
		if err := router.RemoveHostFragment(cctx.RouterState, ref.RepositoryID, ref.WorktreeID, name); err != nil {
			return clierr.Newf("Could not remove the route file of %q: %s", name, err)
		}
		if _, err = store.UpdateHostRoutes(ctx, ref.WorktreeID, ref.Slug, cctx.writer(), withoutHostRoute(name)); err != nil {
			return clierr.Wrap(fmt.Sprintf("Removed the route file of %q but could not update its registration. Run '%s route rm %s' again.", name, cctx.CommandName, name), err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	refreshIndexBestEffort(ctx, cctx, "route rm")
	cctx.Logger.Info(fmt.Sprintf("Removed host route %s (%s).", name, hostRouteURL(cctx, registration.RegisteredWorktree, removed)))
	return nil
}

// RouteReportEntry is one element of `route ls --json`'s array.
type RouteReportEntry struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Port int    `json:"port"`
}

// RouteLs is `route ls`.
func RouteLs(ctx context.Context, env *Env, options OutputOptions) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	return RouteLsWithContext(ctx, cctx, options)
}

// RouteLsWithContext lists this registered worktree's host routes, by name:
// the name, the URL it answers on and the port it targets. It reads the
// record and takes no lock.
func RouteLsWithContext(ctx context.Context, cctx *CommandContext, options OutputOptions) error {
	registration, err := cctx.requireRegistration(ctx)
	if err != nil {
		return err
	}
	routes := routesOf(cctx.Loaded.Config, registration.Slug, registration.Domain, cctx.RouterPort, nil, registration.HostRoutes)
	entries := make([]RouteReportEntry, len(routes.Host))
	for i, route := range routes.Host {
		entries[i] = RouteReportEntry{Name: route.Service, URL: route.URL, Port: route.Port}
	}
	if len(entries) == 0 {
		cctx.Logger.Info(fmt.Sprintf("No host routes. '%s route add <name> --port <port>' routes a process on the host.", cctx.CommandName))
	}
	if options.JSON {
		return printJSON(cctx, entries)
	}
	if len(entries) == 0 {
		return nil
	}
	nameWidth, urlWidth := 0, 0
	for _, entry := range entries {
		nameWidth = max(nameWidth, text.JSLen(entry.Name))
		urlWidth = max(urlWidth, text.JSLen(entry.URL))
	}
	for _, entry := range entries {
		cctx.Logger.Output(text.Printable(fmt.Sprintf("%s  %s  127.0.0.1:%d",
			text.PadEnd(entry.Name, nameWidth), text.PadEnd(entry.URL, urlWidth), entry.Port)))
	}
	return nil
}

// unrouteHostRoutes removes every host route file of ref's worktree, for
// `worktree remove`, before the record that holds the routes goes. `down`
// never calls it.
func unrouteHostRoutes(cctx *CommandContext, ref worktree.Ref) error {
	if err := router.RemoveWorktreeHostFragments(cctx.RouterState, ref.RepositoryID, ref.WorktreeID); err != nil {
		return clierr.Newf("Could not remove this worktree's host route files: %s", err)
	}
	return nil
}

// HostRouteName is one host route of this worktree, for shell completion.
type HostRouteName struct {
	Name string
	// Port is the port the route's process listens on.
	Port int
}

// ListHostRouteNames are this registered worktree's host routes, by name, for
// `route rm`'s shell completion. A checkout that is not registered has none.
func ListHostRouteNames(ctx context.Context, env *Env) ([]HostRouteName, error) {
	cctx, err := Context(env)
	if err != nil {
		return nil, err
	}
	return ListHostRouteNamesWithContext(ctx, cctx)
}

// ListHostRouteNamesWithContext is ListHostRouteNames operating directly on a
// CommandContext.
func ListHostRouteNamesWithContext(ctx context.Context, cctx *CommandContext) ([]HostRouteName, error) {
	current, err := cctx.Registry.Current(ctx)
	if err != nil || current == nil {
		return nil, err
	}
	names := make([]HostRouteName, len(current.HostRoutes))
	for i, route := range current.HostRoutes {
		names[i] = HostRouteName{Name: route.Name, Port: route.Port}
	}
	return names, nil
}
