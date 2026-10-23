package commands

import (
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/text"
	"github.com/sofired/treerig/internal/worktree"
)

// ServiceURL is one routed service's URL.
type ServiceURL struct {
	Service string
	URL     string
	Label   *string
	Purpose *string
	// Port is the loopback port treerig allocated to the service (#119), 0
	// when none has been. URL is unchanged by it: still the hostname URL
	// through the router.
	Port int
	// HostRoute marks a route to a process on the host (#6) rather than a
	// service; its Port is the one that process listens on.
	HostRoute bool
}

// urlsFromRoutes pairs routes with each service's [service.<name>.index] metadata,
// aligned one-to-one and in order since worktree.Routes never filters or
// reorders — a caller that already has both (up.go, so the URLs `up` prints and the
// hooks it runs derive from the one worktree.Routes call) skips computing
// routes twice.
func urlsFromRoutes(services []config.Service, routes []worktree.ServiceRoute) []ServiceURL {
	urls := make([]ServiceURL, len(routes))
	for i, route := range routes {
		entry := ServiceURL{Service: route.Service, URL: route.URL, Port: route.Port}
		if index := services[i].Index; index != nil {
			label, purpose := index.Label, index.Purpose
			entry.Label, entry.Purpose = &label, &purpose
		}
		urls[i] = entry
	}
	return urls
}

// printURLs prints one line per service, service names aligned into a
// column so the URLs line up underneath.
func printURLs(cctx *CommandContext, list []ServiceURL) {
	width := 0
	for _, entry := range list {
		if l := text.JSLen(entry.Service); l > width {
			width = l
		}
	}
	for _, entry := range list {
		cctx.Logger.Output(text.Printable(text.PadEnd(entry.Service, width) + "  " + entry.URL))
	}
}
