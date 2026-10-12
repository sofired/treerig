package commands

import (
	"context"
	"fmt"

	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/text"
	"github.com/sofired/berth/internal/worktree"
)

// StatusReport is `status --json`'s document. Registered is false, with no
// Host and no services, for a worktree that was never registered.
type StatusReport struct {
	Registered bool            `json:"registered"`
	Host       *string         `json:"host,omitempty"`
	Services   []StatusService `json:"services"`
	// Shared is the declared shared services; present only when berth.toml
	// declares some.
	Shared []SharedStatusEntry `json:"shared,omitempty"`
}

// StatusService is one declared service's state. State is one of the
// worktree.ServiceState words; Detail is absent when there is nothing to add.
// Port is the routed service's loopback port (#119); absent for a service
// that is not routed or has none yet.
type StatusService struct {
	Service string `json:"service"`
	State   string `json:"state"`
	Detail  string `json:"detail,omitempty"`
	Port    int    `json:"port,omitempty"`
}

// Status is `status`.
func Status(ctx context.Context, env *Env, options OutputOptions) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	_, err = StatusWithContext(ctx, cctx, options)
	return err
}

// StatusWithContext prints every declared service's state, or says the
// worktree is not registered. Nil statuses mean the latter. With
// options.JSON it prints a StatusReport instead, and the not-registered
// notice still goes to stderr.
func StatusWithContext(ctx context.Context, cctx *CommandContext, options OutputOptions) ([]worktree.ServiceStatus, error) {
	registration, err := cctx.Registry.Current(ctx)
	if err != nil {
		return nil, err
	}
	if registration == nil {
		// Prose, not data: in the text form stdout stays empty, which is the
		// honest answer; --json prints its report below.
		cctx.Logger.Info(fmt.Sprintf("Not registered — run '%s up' to create this worktree's environment.", cctx.CommandName))
		if options.JSON {
			if err := printJSON(cctx, StatusReport{Services: []StatusService{}}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	ref := toRef(registration.RegisteredWorktree)
	statuses, err := cctx.Runtime(cctx.Loaded.Config.LabelNamespace()).Status(ctx, ref)
	if err != nil {
		return nil, err
	}
	host := layout.WorktreeHost(ref.Slug, registration.Domain)
	sharedEntries, err := sharedStatuses(ctx, cctx, ref.RepositoryID, ref.WorktreeID)
	if err != nil {
		// A cancelled command stops here like any other; only a failed
		// lookup falls back.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// The worktree's own services are the answer; a shared lookup that
		// failed must not take them with it.
		cctx.Logger.Warn(fmt.Sprintf("Could not read the shared services' state, so they are left out: %s", text.OneLine(err.Error(), 300)))
		sharedEntries = nil
	}
	if options.JSON {
		report := StatusReport{Registered: true, Host: &host, Services: make([]StatusService, 0, len(statuses)), Shared: sharedEntries}
		for _, entry := range statuses {
			report.Services = append(report.Services, StatusService{Service: entry.Service, State: string(entry.State), Detail: entry.Detail, Port: registration.Ports[entry.Service]})
		}
		if err := printJSON(cctx, report); err != nil {
			return nil, err
		}
		return statuses, nil
	}
	cctx.Logger.Output(text.Printable(host))
	if len(statuses) == 0 {
		cctx.Logger.Output("  (no services)")
	}
	width := 0
	for _, entry := range statuses {
		width = max(width, text.JSLen(entry.Service))
	}
	for _, entry := range statuses {
		detail := ""
		if entry.Detail != "" {
			detail = "  (" + entry.Detail + ")"
		}
		// The service names come from the branch's own compose file.
		cctx.Logger.Output(text.Printable(fmt.Sprintf("  %s  %s%s", text.PadEnd(entry.Service, width), entry.State, detail)))
	}
	if len(sharedEntries) > 0 {
		cctx.Logger.Output("  shared services")
		for _, line := range renderShared(sharedEntries, "    ") {
			cctx.Logger.Output(line)
		}
	}
	return statuses, nil
}

// toRef is what a backend needs to know about a registered worktree.
func toRef(record registry.RegisteredWorktree) worktree.Ref {
	return worktree.Ref{RepositoryID: record.RepositoryID, WorktreeID: record.WorktreeID, Slug: record.Slug}
}
