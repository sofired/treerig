package commands

import (
	"context"
	"errors"
	"fmt"

	"github.com/sofired/treerig/internal/docker"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/text"
	"github.com/sofired/treerig/internal/worktree"
	"github.com/sofired/treerig/internal/worktreestate"
)

// StatusReport is `status --json`'s document. Registered is false, with no
// Host and no services, for a worktree that was never registered. State is the
// worktree's state in the words every command uses (running, starting,
// partly_running, failed, stopped, not_started, removing, directory_missing,
// unknown); Hint names the cause of an unknown one.
type StatusReport struct {
	Registered bool            `json:"registered"`
	Host       *string         `json:"host,omitempty"`
	State      *worktree.State `json:"state,omitempty"`
	Hint       string          `json:"hint,omitempty"`
	Services   []StatusService `json:"services"`
	// Shared is the declared shared services; present only when treerig.toml
	// declares some.
	Shared []SharedStatusEntry `json:"shared,omitempty"`
}

// StatusService is one declared service's state. State is one of the
// worktree.ServiceState words (JSON: running, starting, unhealthy, restarting,
// exited, stopped, not_created, unknown); ExitCode is present for an exited
// service; Detail is Docker's own word for an unknown one. Port is the routed
// service's loopback port (#119); absent for a service that is not routed or
// has none yet.
type StatusService struct {
	Service  string                `json:"service"`
	State    worktree.ServiceState `json:"state"`
	ExitCode *int                  `json:"exitCode,omitempty"`
	Detail   string                `json:"detail,omitempty"`
	Port     int                   `json:"port,omitempty"`
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
// notice still goes to stderr. Declared shared services are included in the
// output, but the returned slice contains only the worktree's services.
// A shared lookup failure omits that section; cancellation still returns
// an error.
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
	// Docker (or Compose) not answering is not the end of the report: the
	// worktree is then unknown, and the failure is returned once it is printed.
	containers, statusErr := cctx.RuntimeFor(cctx.Loaded.Config.LabelNamespace(), &registration.RegisteredWorktree, cctx.RouterPort).Status(ctx, ref)
	if statusErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		containers = nil
	}
	// Which of them failed decides what the unknown state says: a Docker that
	// is not there is one thing, a Compose file or a Compose that cannot read
	// this worktree is another.
	servicesHint := ""
	if statusErr != nil {
		var unavailable *docker.UnavailableError
		if errors.As(statusErr, &unavailable) {
			servicesHint = worktreestate.DockerHint(unavailable.Error())
		} else {
			servicesHint = "Compose couldn't read this worktree: " + text.OneLine(statusErr.Error(), 250)
		}
	}
	state := worktreestate.New(worktreestate.Options{ControlHome: cctx.ControlHome, Runner: cctx.Runner}).
		Current(registration.RegisteredWorktree, containers, worktree.DirectoryPresent, servicesHint)
	statuses := worktree.Summarize(containers)
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
		report := StatusReport{Registered: true, Host: &host, State: &state.State, Hint: state.Hint, Services: make([]StatusService, 0, len(statuses)), Shared: sharedEntries}
		for _, entry := range statuses {
			service := StatusService{Service: entry.Service, State: entry.State, Detail: entry.Detail, Port: registration.Ports[entry.Service]}
			if entry.State == worktree.Exited {
				code := entry.ExitCode
				service.ExitCode = &code
			}
			report.Services = append(report.Services, service)
		}
		if err := printJSON(cctx, report); err != nil {
			return nil, err
		}
		return statuses, statusErr
	}
	cctx.Logger.Output(text.Printable(fmt.Sprintf("%s   %s", host, state.State)))
	if len(statuses) == 0 && statusErr == nil {
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
		cctx.Logger.Output(text.Printable(fmt.Sprintf("  %s  %s%s", text.PadEnd(entry.Service, width), entry.Text(), detail)))
	}
	if len(sharedEntries) > 0 {
		cctx.Logger.Output("  shared services")
		for _, line := range renderShared(sharedEntries, "    ") {
			cctx.Logger.Output(line)
		}
	}
	if state.Hint != "" && statusErr == nil {
		cctx.Logger.Info(text.Printable(fmt.Sprintf("%s is unknown. %s", host, state.Hint)))
	}
	return statuses, statusErr
}

// toRef is what a backend needs to know about a registered worktree.
func toRef(record registry.RegisteredWorktree) worktree.Ref {
	return worktree.Ref{RepositoryID: record.RepositoryID, WorktreeID: record.WorktreeID, Slug: record.Slug}
}
