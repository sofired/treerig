package commands

// What the verbs share about host routes (#6, docs/design/routing.md,
// decision 11): the rule and route file a recorded route produces, the routes
// of a worktree as `urls`, hooks and `run` list them beside its services', and
// the refusal when a shared service takes a host route's name.

import (
	"context"
	"fmt"
	"slices"

	"github.com/sofired/berth/internal/backend"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/ports"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/router"
	"github.com/sofired/berth/internal/worktree"
)

// hostRouteRule is the Traefik rule of a host route: the worktree's hostname,
// with the route's subdomain and path, built exactly as a service's is.
func hostRouteRule(record registry.RegisteredWorktree, route registry.HostRoute) string {
	return backend.RouteRule(backend.RouteSpec{Subdomain: route.Subdomain, Path: route.Path}, layout.WorktreeHost(record.Slug, record.Domain))
}

// hostRouteDocument is the route file the record produces for route.
func hostRouteDocument(record registry.RegisteredWorktree, route registry.HostRoute) router.FragmentDocument {
	return router.HostFragmentDocument(record.RepositoryID, record.WorktreeID, route.Name, hostRouteRule(record, route), route.Port)
}

// hostRouteTargets are what every route file of record's host routes must
// say, by route name, for route repair (router.Registration); nil when it has
// none.
func hostRouteTargets(record registry.RegisteredWorktree) map[string]router.HostTarget {
	if len(record.HostRoutes) == 0 {
		return nil
	}
	targets := make(map[string]router.HostTarget, len(record.HostRoutes))
	for _, route := range record.HostRoutes {
		targets[route.Name] = router.HostTarget{Rule: hostRouteRule(record, route), Port: route.Port}
	}
	return targets
}

// hostRouteServices are routes in the shape of a [service.<name>] table
// (registry.HostRoute.AsService), for worktree.Routes.
func hostRouteServices(routes []registry.HostRoute) []config.Service {
	services := make([]config.Service, len(routes))
	for i, route := range routes {
		services[i] = route.AsService()
	}
	return services
}

// worktreeRoutes are the routes a registered worktree answers on: its
// declared services' (with their loopback ports) and its host routes' (with
// the port the host process listens on).
type worktreeRoutes struct {
	services, host []config.Service
	// Services and Host are the routes of services and host, in the same order.
	Services, Host []worktree.ServiceRoute
}

// routesOf builds a worktree's routes for the router port routerPort. ports
// are its services' allocated ports; host routes carry their own.
func routesOf(cfg config.Config, slug, domain string, routerPort int, ports map[string]int, hostRoutes []registry.HostRoute) worktreeRoutes {
	routes := worktreeRoutes{services: cfg.Service, host: hostRouteServices(hostRoutes)}
	routes.Services = worktree.WithPorts(worktree.Routes(routes.services, slug, domain, routerPort), ports)
	hostPorts := make(map[string]int, len(hostRoutes))
	for _, route := range hostRoutes {
		hostPorts[route.Name] = route.Port
	}
	routes.Host = worktree.WithPorts(worktree.Routes(routes.host, slug, domain, routerPort), hostPorts)
	return routes
}

// All is the services' routes and then the host routes', for the variables a
// hook or command gets (worktree.ServiceURLVariables).
func (r worktreeRoutes) All() []worktree.ServiceRoute {
	return slices.Concat(r.Services, r.Host)
}

// ServiceURLs are the services' URLs with their [service.<name>.index]
// metadata: what the checkout index card records. A host route is left out
// on purpose: the index counts a card's services against the containers it
// finds, and a host process is not one.
func (r worktreeRoutes) ServiceURLs() []ServiceURL {
	return urlsFromRoutes(r.services, r.Services)
}

// URLs are every route's URL, services' first: what `urls` and `up` print.
func (r worktreeRoutes) URLs() []ServiceURL {
	host := urlsFromRoutes(r.host, r.Host)
	for i := range host {
		host[i].HostRoute = true
	}
	return slices.Concat(r.ServiceURLs(), host)
}

// refuseHostRouteClashes refuses a berth.toml that declares something a host
// route of this worktree already is: a shared service named like one
// (docs/design/routing.md, decision 15: the alias containers reach it by would
// mean two things), a [service.<name>] with its name (two BERTH_SERVICE_URL
// variables, two Traefik routers), or a [service.<name>] answering at the
// route's subdomain and path (two routers with one rule). `route add` refuses
// the other direction. config cannot check any of this, since host routes live
// in the registration and not in berth.toml, so `up` and `shared up` do.
func refuseHostRouteClashes(commandName string, cfg config.Config, routes []registry.HostRoute) error {
	for _, route := range routes {
		for _, service := range cfg.Service {
			switch {
			case service.Name == route.Name:
				return clierr.Newf(
					"Service %s has the name of a host route of this worktree. Remove the route with '%s route rm %s', or rename the service.",
					service.Name, commandName, route.Name)
			case (registry.HostRoute{Subdomain: service.Subdomain, Path: service.Path}).SamePlace(route):
				return clierr.Newf(
					"Service %s answers where the host route %s does, so both would claim one hostname. Remove the route with '%s route rm %s', or change the service's subdomain or path.",
					service.Name, route.Name, commandName, route.Name)
			}
		}
		for _, shared := range cfg.SharedServices() {
			if shared.Name == route.Name {
				return clierr.Newf(
					"Shared service %s has the name of a host route of this worktree, so the alias containers reach it by would mean two things. "+
						"Remove the route with '%s route rm %s', or rename the shared service.",
					shared.Name, commandName, shared.Name)
			}
		}
	}
	return nil
}

// listenerAdvice is the warning for a host route whose port answers on ::1
// and not on 127.0.0.1, or "" when the router reaches it anyway. mode is the
// proxy's mode and known says whether it could be read: in host mode the
// proxy maps berth-loopback to ::1 as well, which reaches such a listener
// (docs/design/routing.md, Open items); on Docker Desktop and OrbStack the
// engine's host-gateway is IPv4 only.
func listenerAdvice(port int, mode router.Mode, known bool) string {
	if known && mode == router.ModeHost {
		return ""
	}
	reach := "The router cannot reach ::1 on Docker Desktop and OrbStack."
	if !known {
		reach = "The router cannot reach ::1 on Docker Desktop and OrbStack, and its mode could not be read here."
	}
	return fmt.Sprintf("Port %d answers on ::1 but not on 127.0.0.1. %s Bind the process to 127.0.0.1 instead (its host or listen address setting).", port, reach)
}

// listenerNote is what a probe of a host route's port found.
type listenerNote struct {
	// Reachable is true when the router can reach the process: 127.0.0.1
	// answers, or only ::1 does and the proxy runs in host mode.
	Reachable bool
	// Warning is non-empty when the port answers on ::1 only and the router
	// may not reach it (listenerAdvice).
	Warning string
	// Silent is true when neither loopback address answers.
	Silent bool
	// IPv6Only is true when only ::1 answers.
	IPv6Only bool
}

// noteListener probes port and says what it found. It is the one reading
// that `route add` and `doctor` both use, so their words cannot differ. modes
// is asked for the proxy's mode only in the IPv6-only case, where it decides
// the answer; nil, or an error, reads as an unknown mode.
func noteListener(ctx context.Context, probe ports.ListenerProbe, port int, modes RouteModes) listenerNote {
	state := ports.Listening(ctx, probe, port)
	switch {
	case state.IPv4:
		return listenerNote{Reachable: true}
	case state.IPv6Only():
		var choice router.ModeChoice
		known := false
		if modes != nil {
			var err error
			choice, err = modes.ModeChoice(ctx)
			known = err == nil
		}
		warning := listenerAdvice(port, choice.Mode, known)
		return listenerNote{Reachable: warning == "", Warning: warning, IPv6Only: true}
	}
	return listenerNote{Silent: true}
}
