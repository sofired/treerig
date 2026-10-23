package commands

import (
	"context"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sofired/treerig/internal/ports"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/router"
	"github.com/sofired/treerig/internal/text"
	"github.com/sofired/treerig/internal/worktree"
)

// defaultHostRouteBudget bounds the connect to every host route's port
// together; a listener that does not answer by then is not listening.
const defaultHostRouteBudget = time.Second

// hostRouteParallelism bounds how many ports are tried at once.
const hostRouteParallelism = 8

// hostRouteBudget is the time the host routes' ports get together.
func (c *CommandContext) hostRouteBudget() time.Duration {
	if c.HostRouteBudget <= 0 {
		return defaultHostRouteBudget
	}
	return c.HostRouteBudget
}

// readRouterState asks Docker what the shared router's container is doing.
func readRouterState(ctx context.Context, cctx *CommandContext) router.Observed {
	svc, err := router.New(router.Options{
		State: cctx.RouterState, Port: cctx.RouterPort, Runner: cctx.Runner, Logger: cctx.Logger,
		Environ: cctx.Environ, CommandName: cctx.CommandName, ProjectName: cctx.RouterProject, Home: cctx.Home,
	})
	if err != nil {
		return router.Observed{Condition: router.ConditionUnknown, Detail: text.OneLine(err.Error(), 300)}
	}
	return svc.State(ctx)
}

// probeHostRoutes connects to every host route's port on 127.0.0.1, a few at a
// time and all within budget, and fills states with what each said. A port
// that has not answered when the budget ends is not listening.
func probeHostRoutes(ctx context.Context, probe ports.ListenerProbe, budget time.Duration, routes []registry.HostRoute, states []worktree.RouteState) {
	if len(routes) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var group errgroup.Group
	group.SetLimit(hostRouteParallelism)
	for i, route := range routes {
		states[i] = worktree.NotListening
		group.Go(func() error {
			if probe.Answers(ctx, "127.0.0.1", route.Port) {
				states[i] = worktree.Listening
			}
			return nil
		})
	}
	// The workers return nil: a port that does not answer is a state, not an
	// error, so there is nothing to take from Wait.
	_ = group.Wait()
}
