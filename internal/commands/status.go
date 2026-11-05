package commands

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/sofired/treerig/internal/docker"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/router"
	"github.com/sofired/treerig/internal/text"
	"github.com/sofired/treerig/internal/worktree"
	"github.com/sofired/treerig/internal/worktreestate"
)

// StatusOptions are `status`'s argument and flag.
type StatusOptions struct {
	// JSON prints one JSON document on stdout in place of the table.
	JSON bool
	// Worktree names the worktree to report on (registry.Resolve: a UUID, a
	// host, a path, which may lie inside its directory, or a slug); empty means
	// the one the command runs in.
	Worktree string
}

// RouterState is whether the shared router's container is up, as `status`
// reports it: running, stopped (no container, or one that is not running) or
// unknown (Docker did not answer, or the router could not be read).
type RouterState string

// The router states.
const (
	RouterRunning RouterState = "running"
	RouterStopped RouterState = "stopped"
	RouterUnknown RouterState = "unknown"
)

// StatusRouter is the router's part of `status --json`. Port is the port a
// running router serves, and Detail says why the router is unknown, or whose
// it is when it is not this treerig root's; both are absent otherwise.
type StatusRouter struct {
	State  RouterState `json:"state"`
	Port   int         `json:"port,omitempty"`
	Detail string      `json:"detail,omitempty"`
}

// StatusReport is `status --json`'s document. Registered is false, with no
// Host and no services, for a worktree that was never registered. State is the
// worktree's state in the words every command uses (running, starting,
// partly_running, failed, stopped, not_started, removing, directory_missing,
// unknown); Hint names the cause of an unknown one. DockerError says why Docker
// did not answer, when it did not. Router is absent when status stopped before
// it read the services (a worktree that is not registered, or whose directory
// is missing or unreadable).
type StatusReport struct {
	Registered  bool            `json:"registered"`
	Host        *string         `json:"host,omitempty"`
	State       *worktree.State `json:"state,omitempty"`
	Hint        string          `json:"hint,omitempty"`
	DockerError string          `json:"dockerError,omitempty"`
	Router      *StatusRouter   `json:"router,omitempty"`
	Services    []StatusService `json:"services"`
	// Shared is the declared shared services; present only when treerig.toml
	// declares some.
	Shared []StatusShared `json:"shared,omitempty"`
}

// StatusService is one declared service's state, or one host route's. State is
// a worktree.ServiceState's JSON spelling (running, starting, unhealthy,
// restarting, exited, stopped, not_created, unknown), or for a host route a
// worktree.RouteState's (listening, not_listening). ExitCode is present for an
// exited service; Detail is Docker's own word for an unknown one. URL is
// present only while the service runs (a host route: listens) and the router
// serves this root's URLs. Port is the routed service's loopback port (#119),
// or the port a host route's process listens on; absent for a service that is
// not routed or has none yet.
type StatusService struct {
	Service   string `json:"service"`
	HostRoute bool   `json:"hostRoute,omitempty"`
	State     string `json:"state"`
	ExitCode  *int   `json:"exitCode,omitempty"`
	Detail    string `json:"detail,omitempty"`
	URL       string `json:"url,omitempty"`
	Port      int    `json:"port,omitempty"`
}

// StatusShared is a shared service's entry in `status --json`: what `shared
// status --json` has, except that its url follows the rule of a service's
// (present only while the service runs and the router serves this root's URLs),
// where `shared status` always gives the routed address. The embedded entry's
// own url is cleared, so the field below is the only one marshalled.
type StatusShared struct {
	SharedStatusEntry
	URL string `json:"url,omitempty"`
}

// Status is `status`. A worktree of this repository needs its treerig.toml; one
// named by options.Worktree reads the treerig.toml of its own directory, so it
// works from anywhere (ContextWithoutConfig).
func Status(ctx context.Context, env *Env, options StatusOptions) error {
	var cctx *CommandContext
	var err error
	if options.Worktree == "" {
		cctx, err = Context(env)
	} else {
		cctx, err = ContextWithoutConfig(env)
	}
	if err != nil {
		return err
	}
	_, err = StatusWithContext(ctx, cctx, options)
	return err
}

// StatusWithContext prints the header line, then every declared service's
// state and URL, then its host routes' and shared services', or says the
// worktree is not registered. Nil statuses mean the latter, or that status
// stopped before it read the services. With options.JSON it prints a
// StatusReport instead, and the not-registered notice still goes to stderr.
// The returned slice contains only the worktree's Compose services. A shared
// lookup failure omits that section; cancellation still returns an error, and so
// does Docker (or Compose) not answering, after the output is printed.
func StatusWithContext(ctx context.Context, cctx *CommandContext, options StatusOptions) ([]worktree.ServiceStatus, error) {
	subject, err := resolveStatusSubject(ctx, cctx, options)
	if err != nil {
		return nil, err
	}
	if subject == nil {
		// Prose, not data: in the text form stdout stays empty, which is the
		// honest answer; --json prints its report below.
		cctx.Logger.Info(text.Printable(fmt.Sprintf("%s is not registered. Run '%s up' to create its environment.",
			unregisteredName(ctx, cctx), cctx.CommandName)))
		if options.JSON {
			if err := printJSON(cctx, StatusReport{Services: []StatusService{}}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	record := subject.record
	host := layout.WorktreeHost(record.Slug, record.Domain)
	gatherer := worktreestate.New(worktreestate.Options{ControlHome: cctx.ControlHome, Runner: cctx.Runner})

	if !subject.here {
		stop, err := subject.checkDirectory(ctx, gatherer, host)
		if err != nil {
			return nil, err
		}
		if stop != nil {
			result := gatherer.Current(record, nil, stop.directory, stop.unknown)
			return nil, printStoppedStatus(cctx, options, host, result, stop.note)
		}
	}
	tcx := subject.cctx
	ref := toRef(record)
	routes := routesOf(tcx.Loaded.Config, record.Slug, record.Domain, cctx.RouterPort, record.Ports, record.HostRoutes)

	// The router and the host routes' ports are asked while Compose is.
	var side sync.WaitGroup
	defer side.Wait()
	var observed router.Observed
	side.Go(func() { observed = readRouterState(ctx, cctx) })
	listening := make([]worktree.RouteState, len(record.HostRoutes))
	side.Go(func() { probeHostRoutes(ctx, cctx.listeners(), cctx.hostRouteBudget(), record.HostRoutes, listening) })

	// Docker (or Compose) not answering is not the end of the report: the
	// worktree is then unknown, and the failure is returned once it is printed.
	namespace := recordedOr(cctx.ControlHome, record, tcx.Loaded.Config.LabelNamespace())
	containers, statusErr := tcx.RuntimeFor(namespace, &record, cctx.RouterPort).Status(ctx, ref)
	if statusErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		containers = nil
	}
	// Which of them failed decides what the unknown state says: a Docker that
	// is not there is one thing, a Compose file or a Compose that cannot read
	// this worktree is another.
	servicesHint, dockerError := "", ""
	if statusErr != nil {
		var unavailable *docker.UnavailableError
		if errors.As(statusErr, &unavailable) {
			servicesHint = worktreestate.DockerHint(unavailable.Error())
			dockerError = text.OneLine(unavailable.Error(), 300)
		} else {
			servicesHint = "Compose couldn't read this worktree: " + text.OneLine(statusErr.Error(), 250)
		}
	}
	state := gatherer.Current(record, containers, worktree.DirectoryPresent, servicesHint)
	statuses := worktree.Summarize(containers)

	var sharedEntries []SharedStatusEntry
	if statusErr == nil {
		sharedEntries, err = sharedStatuses(ctx, tcx, ref.RepositoryID, ref.WorktreeID, namespace)
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
	}
	side.Wait()
	if ctxErr := ctx.Err(); ctxErr != nil {
		// A cancelled probe says "not listening", and the router "unknown":
		// neither is an answer to print.
		return nil, ctxErr
	}

	serves := routerServesURLs(observed, cctx.RouterPort)
	view := newStatusView(statuses, record, routes, listening, sharedEntries, serves)
	noticeAboutRouter(cctx, observed, serves, view.anyRoute, statusErr == nil)

	if options.JSON {
		report := StatusReport{
			Registered: true, Host: &host, State: &state.State, Hint: state.Hint, DockerError: dockerError,
			Router: routerReport(observed), Services: view.services, Shared: view.shared,
		}
		if err := printJSON(cctx, report); err != nil {
			return nil, err
		}
		return statuses, statusErr
	}
	cctx.Logger.Output(text.Printable(fmt.Sprintf("%s   %s", host, state.State)))
	if len(view.rows) == 0 && statusErr == nil {
		cctx.Logger.Output("  (no services)")
	}
	for _, line := range formatStatusTable(view.rows) {
		cctx.Logger.Output(line)
	}
	if state.Hint != "" && statusErr == nil {
		cctx.Logger.Info(text.Printable(fmt.Sprintf("%s is unknown. %s", host, state.Hint)))
	}
	return statuses, statusErr
}

// routerServesURLs reports whether the router that runs serves the URLs this
// root builds: it runs, this treerig root started it, and it listens on the
// port the URLs name (a router that carries no port label is taken at its
// word).
func routerServesURLs(observed router.Observed, port int) bool {
	return observed.Condition == router.ConditionRunning && !observed.Foreign && (observed.Port == 0 || observed.Port == port)
}

// routerReport is the router's part of the JSON document.
func routerReport(observed router.Observed) *StatusRouter {
	report := &StatusRouter{State: RouterUnknown, Port: observed.Port, Detail: observed.Detail}
	switch observed.Condition {
	case router.ConditionRunning:
		report.State = RouterRunning
	case router.ConditionStopped:
		report.State = RouterStopped
	}
	return report
}

// noticeAboutRouter says on stderr why no URL is shown, when something is
// routed and the router is the reason. read is false when Docker did not answer
// at all, which is the error status returns and needs no second word.
func noticeAboutRouter(cctx *CommandContext, observed router.Observed, serves, anyRoute, read bool) {
	if serves || !anyRoute {
		return
	}
	switch {
	case observed.Condition == router.ConditionStopped:
		cctx.Logger.Info(fmt.Sprintf("The router is stopped, so no URL loads. Run '%s up' to start it.", cctx.CommandName))
	case observed.Condition == router.ConditionUnknown && read:
		cctx.Logger.Info(fmt.Sprintf("The router's state couldn't be read, so no URL is shown. Run '%s doctor' to see why.", cctx.CommandName))
	case observed.Condition == router.ConditionRunning && observed.Foreign:
		cctx.Logger.Info(fmt.Sprintf("The router belongs to a different treerig root, so no URL is shown. Run '%s router down', then '%s up'.", cctx.CommandName, cctx.CommandName))
	case observed.Condition == router.ConditionRunning:
		cctx.Logger.Info(fmt.Sprintf("The router runs on port %d, not %d. Run '%s router up'.", observed.Port, cctx.RouterPort, cctx.CommandName))
	}
}

// toRef is what a backend needs to know about a registered worktree.
func toRef(record registry.RegisteredWorktree) worktree.Ref {
	return worktree.Ref{RepositoryID: record.RepositoryID, WorktreeID: record.WorktreeID, Slug: record.Slug}
}
