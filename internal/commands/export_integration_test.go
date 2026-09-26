//go:build integration

package commands

import (
	"context"

	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/router"
)

// SharedRouter is the Router Up builds, over svc instead of the default
// shared router: the real-daemon tests run `up` through a router of their
// own name, port and state, never a developer's real one.
func SharedRouter(env *Env, svc *router.Router, state layout.RouterLayout) Router {
	return sharedRouter{env: env, svc: svc, state: state}
}

// BestEffortIndex is the Index Up builds.
func BestEffortIndex(cctx *CommandContext) Index { return bestEffortIndex{cctx} }

// DoctorSections is doctor's own report, built through doctorOptionsFor
// against svc and its own state as the shared router instead of
// buildDoctorOptions' own resolution (a router.Router under
// router.ResolveProjectName(), which no environment variable can redirect):
// the real-daemon tests read a router of their own name, port and state,
// never a developer's real one, the same seam SharedRouter gives `up`.
// Sharing doctorOptionsFor with buildDoctorOptions means this seam can't
// drift from what `doctor` itself builds. checkedHome resolves the berth
// root from env.Getenv itself, exactly once, the same way Doctor does —
// env's own BERTH_CONTROL_HOME (or BERTH_HOME) is what points this at the
// caller's own control home, so no separate controlHome parameter is needed.
func DoctorSections(ctx context.Context, env *Env, svc *router.Router, state layout.RouterLayout) ([]Section, error) {
	home, userHome, err := checkedHome(env.Getenv)
	if err != nil {
		return nil, err
	}
	opts, err := doctorOptionsFor(env, home, userHome, svc, state, nil)
	if err != nil {
		return nil, err
	}
	return doctorSections(ctx, opts)
}
