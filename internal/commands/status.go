package commands

import (
	"context"
	"fmt"

	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/text"
	"github.com/sofired/berth/internal/worktree"
)

// Status is `status`.
func Status(ctx context.Context, env *Env) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	_, err = StatusWithContext(ctx, cctx)
	return err
}

// StatusWithContext prints every declared service's state, or says the
// worktree is not registered. Nil statuses mean the latter.
func StatusWithContext(ctx context.Context, cctx *CommandContext) ([]worktree.ServiceStatus, error) {
	registration, err := cctx.Registry.Current(ctx)
	if err != nil {
		return nil, err
	}
	if registration == nil {
		// Prose, not data: stdout stays empty, which is the honest answer.
		cctx.Logger.Info(fmt.Sprintf("Not registered — run '%s up' to create this worktree's environment.", cctx.CommandName))
		return nil, nil
	}
	ref := toRef(registration.RegisteredWorktree)
	statuses, err := cctx.Runtime(cctx.Loaded.Config.LabelNamespace()).Status(ctx, ref)
	if err != nil {
		return nil, err
	}
	cctx.Logger.Output(text.Printable(layout.WorktreeHost(ref.Slug, registration.Domain)))
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
	return statuses, nil
}

// toRef is what a backend needs to know about a registered worktree.
func toRef(record registry.RegisteredWorktree) worktree.Ref {
	return worktree.Ref{RepositoryID: record.RepositoryID, WorktreeID: record.WorktreeID, Slug: record.Slug}
}
