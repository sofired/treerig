package worktree

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/layout"
)

// ServiceRoute is one declared service's routed URL — the shared building
// block for `treerig status` and `up`, hooks
// and `treerig run`'s commands (ServiceURLVariables). Subdomain and Path travel with it only so RootRoute can tell a route
// claiming the host root outright from one that merely shares that host
// under a path or a different subdomain.
type ServiceRoute struct {
	Service   string
	Subdomain string
	Path      *string
	URL       string
	// Port is the loopback port treerig allocated to the service (#119), 0
	// when none has been. Routes never sets it; WithPorts does.
	Port int
}

// Routes is the URL each of services will answer on, in config order: the
// host is <slug>.<domain>, prefixed by a service's own subdomain when it
// declares one and suffixed by its path when it declares one, with the
// router's port folded in exactly as `treerig status` folds it in — omitted
// when it is the default HTTP port, 80.
func Routes(services []config.Service, slug, domain string, port int) []ServiceRoute {
	host := layout.WorktreeHost(slug, domain)
	suffix := ""
	if port != 80 {
		suffix = fmt.Sprintf(":%d", port)
	}
	routes := make([]ServiceRoute, 0, len(services))
	for _, service := range services {
		prefix := ""
		if service.Subdomain != "" {
			prefix = service.Subdomain + "."
		}
		path := ""
		if service.Path != nil {
			path = *service.Path
		}
		routes = append(routes, ServiceRoute{
			Service:   service.Name,
			Subdomain: service.Subdomain,
			Path:      service.Path,
			URL:       "http://" + prefix + host + suffix + path,
		})
	}
	return routes
}

// WithPorts is routes with each service's allocated loopback port filled in
// from ports, keyed by service name. A service with no entry keeps Port 0.
// routes itself is not modified.
func WithPorts(routes []ServiceRoute, ports map[string]int) []ServiceRoute {
	withPorts := make([]ServiceRoute, len(routes))
	for i, route := range routes {
		route.Port = ports[route.Service]
		withPorts[i] = route
	}
	return withPorts
}

// RootRoute is the route claiming the host root outright — no subdomain, no
// path — or nil if none does; config's duplicate-route check allows at most
// one.
func RootRoute(routes []ServiceRoute) *ServiceRoute {
	for i, route := range routes {
		if route.Subdomain == "" && route.Path == nil {
			return &routes[i]
		}
	}
	return nil
}

// URLVariables is the routed-URL part of a worktree's environment, the one
// every consumer of a worktree's routes shares: TREERIG_URL for the
// worktree's root route (RootRoute), omitted rather than empty when routes
// has none, and TREERIG_SERVICE_URL_<NAME> for every routed service, its
// name upper-cased with each hyphen turned into an underscore.
//
// Hooks and `treerig run` commands receive these through ServiceURLVariables;
// the worktree's own Compose invocations receive exactly these pairs, built
// by the same call from the same routes, so ${TREERIG_URL} in compose.yaml is
// the URL `treerig status` shows (#177). Shared services' Compose invocations
// never do: the shared definition must not vary by worktree.
func URLVariables(routes []ServiceRoute) [][2]string {
	var pairs [][2]string
	if root := RootRoute(routes); root != nil {
		pairs = append(pairs, [2]string{"URL", root.URL})
	}
	for _, route := range routes {
		pairs = append(pairs, [2]string{"SERVICE_URL_" + serviceEnvName(route.Service), route.URL})
	}
	return pairs
}

// IsURLVariable reports whether name (an environment variable's name) is one
// URLVariables sets: TREERIG_URL or TREERIG_SERVICE_URL_<anything>. Whoever
// lays URLVariables over an inherited environment drops these first, so a
// variable inherited from another worktree's hook never stands in for a
// route this worktree doesn't have.
func IsURLVariable(name string) bool {
	return name == brand.EnvName("URL") || strings.HasPrefix(name, brand.EnvName("SERVICE_URL_"))
}

// ServiceURLVariables is the extra environment a hook — and a `treerig run`
// command (#94) — receives on top of Environment: URLVariables, and
// TREERIG_SERVICE_PORT_<NAME> (same name mapping) for every routed service
// that has an allocated loopback port (#119): the port on 127.0.0.1 that
// bypasses the router, for tools that must reach the service directly.
//
// Deliberately not folded into Environment/Variables, which also feed
// every Compose invocation: the loopback ports are not Compose's business,
// and a hook's environment is built per stage. Compose gets URLVariables
// alone (internal/backend, #177), only for the worktree's own project.
func ServiceURLVariables(routes []ServiceRoute) [][2]string {
	pairs := URLVariables(routes)
	for _, route := range routes {
		if route.Port > 0 {
			pairs = append(pairs, [2]string{"SERVICE_PORT_" + serviceEnvName(route.Service), strconv.Itoa(route.Port)})
		}
	}
	return pairs
}

// serviceEnvName is the token TREERIG_SERVICE_URL_ uses for a Compose service
// name: upper-cased, with each hyphen turned into an underscore. A
// service's name is restricted, by config's own schema, to lower-case
// letters, digits and hyphens — an alphabet with no underscore of its own —
// so this mapping is injective: two distinct valid names can never collide
// on the same TREERIG_SERVICE_URL_<NAME>.
func serviceEnvName(service string) string {
	return strings.ToUpper(strings.ReplaceAll(service, "-", "_"))
}
