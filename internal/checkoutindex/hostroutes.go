package checkoutindex

import (
	"fmt"

	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/worktree"
)

// hostRouteLinks are the links a checkout's host routes (#6) add to its row,
// built from the registration record, which holds them, and not from the
// card, which is `up`'s: a route added after the last `up` is on the page
// the moment `route add` refreshes it. They count toward nothing: the row's
// status is its containers', and a host process is not one. Nothing here
// knows whether the process is up, so a link is available unless the
// checkout is gone, and says where the process listens.
func hostRouteLinks(record registry.RegisteredWorktree, routerPort int, status IndexStatus) []IndexServiceLink {
	if len(record.HostRoutes) == 0 {
		return nil
	}
	services := make([]config.Service, len(record.HostRoutes))
	for i, route := range record.HostRoutes {
		services[i] = route.AsService()
	}
	routes := worktree.Routes(services, record.Slug, record.Domain, routerPort)
	links := make([]IndexServiceLink, len(routes))
	for i, route := range routes {
		links[i] = IndexServiceLink{
			Name: route.Service, Label: route.Service,
			Purpose:   fmt.Sprintf("host process on port %d", record.HostRoutes[i].Port),
			URL:       route.URL,
			Available: status != StatusMissing,
		}
	}
	return links
}
