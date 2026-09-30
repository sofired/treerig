package worktree

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/layout"
)

// ServiceRoute is one declared service's routed URL — the shared building
// block for `berth urls` (commands.WorktreeURLs wraps Routes), hooks
// and `berth run`'s commands (ServiceURLVariables). Subdomain and Path travel with it only so RootRoute can tell a route
// claiming the host root outright from one that merely shares that host
// under a path or a different subdomain.
type ServiceRoute struct {
	Service   string
	Subdomain string
	Path      *string
	URL       string
	// Port is the loopback port berth allocated to the service (#119), 0
	// when none has been. Routes never sets it; WithPorts does.
	Port int
}

// Routes is the URL each of services will answer on, in config order: the
// host is <slug>.<domain>, prefixed by a service's own subdomain when it
// declares one and suffixed by its path when it declares one, with the
// router's port folded in exactly as `berth urls` folds it in — omitted
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

// ServiceURLVariables is the extra environment a hook — and a `berth run`
// command (#94) — receives on top of Environment: BERTH_URL for the
// worktree's root route (RootRoute), omitted rather than empty when routes
// has none, BERTH_SERVICE_URL_<NAME> for every routed service, its name
// upper-cased with each hyphen turned into an underscore, and
// BERTH_SERVICE_PORT_<NAME> (same name mapping) for every routed service
// that has an allocated loopback port (#119): the port on 127.0.0.1 that
// bypasses the router, for tools that must reach the service directly.
//
// Deliberately not folded into Environment/Variables, which also feed
// Compose (internal/backend/compose.go): the router port is a setting of
// the whole machine, not of any one worktree's own Compose project, so
// handing it to Compose here would make a change to nothing but the
// router's port look like a configuration change to every worktree,
// recreating its containers on the next `up` (#28). This function is the
// only place these variables are built, and only the hooks runner (for hooks
// and commands alike) calls it; compose_test.go pins Compose's own environment builder never calling
// it either.
func ServiceURLVariables(routes []ServiceRoute) [][2]string {
	var pairs [][2]string
	if root := RootRoute(routes); root != nil {
		pairs = append(pairs, [2]string{"URL", root.URL})
	}
	for _, route := range routes {
		pairs = append(pairs, [2]string{"SERVICE_URL_" + serviceEnvName(route.Service), route.URL})
	}
	for _, route := range routes {
		if route.Port > 0 {
			pairs = append(pairs, [2]string{"SERVICE_PORT_" + serviceEnvName(route.Service), strconv.Itoa(route.Port)})
		}
	}
	return pairs
}

// serviceEnvName is the token BERTH_SERVICE_URL_ uses for a Compose service
// name: upper-cased, with each hyphen turned into an underscore. A
// service's name is restricted, by config's own schema, to lower-case
// letters, digits and hyphens — an alphabet with no underscore of its own —
// so this mapping is injective: two distinct valid names can never collide
// on the same BERTH_SERVICE_URL_<NAME>.
func serviceEnvName(service string) string {
	return strings.ToUpper(strings.ReplaceAll(service, "-", "_"))
}
