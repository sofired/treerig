package commands

import (
	"context"
	"fmt"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/text"
	"github.com/sofired/berth/internal/worktree"
)

// ServiceURL is one routed service's URL.
type ServiceURL struct {
	Service string
	URL     string
	Label   *string
	Purpose *string
}

// URLReportEntry is one element of `urls --json`'s array. Label and
// Purpose are absent for a service without [service.<name>.index]; when it has
// one, both are present, "" included.
type URLReportEntry struct {
	Service string  `json:"service"`
	URL     string  `json:"url"`
	Label   *string `json:"label,omitempty"`
	Purpose *string `json:"purpose,omitempty"`
}

// urlsReport maps the URLs onto their JSON array, never nil.
func urlsReport(list []ServiceURL) []URLReportEntry {
	report := make([]URLReportEntry, len(list))
	for i, entry := range list {
		// A conversion, so a field added to ServiceURL stops compiling here
		// until someone decides whether the document gets it.
		report[i] = URLReportEntry(entry)
	}
	return report
}

// WorktreeURLs is pure: the URL each routed service will answer on, in
// config order. It wraps worktree.Routes, the same route builder hooks use
// for BERTH_URL and BERTH_SERVICE_URL_<NAME> (#28).
func WorktreeURLs(cfg config.Config, slug, domain string, port int) []ServiceURL {
	return urlsFromRoutes(cfg.Service, worktree.Routes(cfg.Service, slug, domain, port))
}

// urlsFromRoutes pairs routes with each service's [service.<name>.index] metadata,
// aligned one-to-one and in order since worktree.Routes never filters or
// reorders — a caller that already has both (up.go, so `berth urls` and the
// hooks it runs derive from the one worktree.Routes call) skips computing
// routes twice.
func urlsFromRoutes(services []config.Service, routes []worktree.ServiceRoute) []ServiceURL {
	urls := make([]ServiceURL, len(routes))
	for i, route := range routes {
		entry := ServiceURL{Service: route.Service, URL: route.URL}
		if index := services[i].Index; index != nil {
			label, purpose := index.Label, index.Purpose
			entry.Label, entry.Purpose = &label, &purpose
		}
		urls[i] = entry
	}
	return urls
}

// Urls is `urls`.
func Urls(ctx context.Context, env *Env, options OutputOptions) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	_, err = UrlsWithContext(ctx, cctx, options)
	return err
}

// UrlsWithContext is `urls`, operating directly on a CommandContext. With
// options.JSON it prints the URLReportEntry array, `[]` when nothing is routed.
func UrlsWithContext(ctx context.Context, cctx *CommandContext, options OutputOptions) ([]ServiceURL, error) {
	registration, err := cctx.Registry.Current(ctx)
	if err != nil {
		return nil, err
	}
	if registration == nil {
		return nil, clierr.Newf("This worktree is not registered. Run '%s up' first.", cctx.CommandName)
	}
	list := WorktreeURLs(cctx.Loaded.Config, registration.Slug, registration.Domain, cctx.RouterPort)
	if len(list) == 0 {
		cctx.Logger.Warn(fmt.Sprintf("No [service.<name>] tables in %s; nothing is routed.", brand.ConfigFile))
	}
	if options.JSON {
		if err := printJSON(cctx, urlsReport(list)); err != nil {
			return nil, err
		}
		return list, nil
	}
	printURLs(cctx, list)
	return list, nil
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
