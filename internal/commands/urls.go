package commands

import (
	"context"
	"fmt"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/text"
)

// ServiceURL is one routed service's URL.
type ServiceURL struct {
	Service string  `json:"service"`
	URL     string  `json:"url"`
	Label   *string `json:"label,omitempty"`
	Purpose *string `json:"purpose,omitempty"`
}

// WorktreeURLs is pure: the URL each routed service will answer on, in
// config order.
func WorktreeURLs(cfg config.Config, slug, domain string, port int) []ServiceURL {
	host := layout.WorktreeHost(slug, domain)
	suffix := ""
	if port != 80 {
		suffix = fmt.Sprintf(":%d", port)
	}
	urls := make([]ServiceURL, 0, len(cfg.Service))
	for _, service := range cfg.Service {
		prefix := ""
		if service.Subdomain != "" {
			prefix = service.Subdomain + "."
		}
		path := ""
		if service.Path != nil {
			path = *service.Path
		}
		entry := ServiceURL{
			Service: service.Name,
			URL:     "http://" + prefix + host + suffix + path,
		}
		if service.Index != nil {
			label, purpose := service.Index.Label, service.Index.Purpose
			entry.Label, entry.Purpose = &label, &purpose
		}
		urls = append(urls, entry)
	}
	return urls
}

// Urls is `urls`.
func Urls(ctx context.Context, env *Env) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	_, err = UrlsWithContext(ctx, cctx)
	return err
}

// UrlsWithContext is worktree.ts's `urls`, operating directly on a
// CommandContext.
func UrlsWithContext(ctx context.Context, cctx *CommandContext) ([]ServiceURL, error) {
	registration, err := cctx.Registry.Current(ctx)
	if err != nil {
		return nil, err
	}
	if registration == nil {
		return nil, clierr.Newf("This worktree is not registered. Run '%s up' first.", cctx.CommandName)
	}
	list := WorktreeURLs(cctx.Loaded.Config, registration.Slug, registration.Domain, cctx.RouterPort)
	if len(list) == 0 {
		cctx.Logger.Warn(fmt.Sprintf("No [[service]] entries in %s; nothing is routed.", brand.ConfigFile))
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
