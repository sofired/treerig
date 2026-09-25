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
func SharedRouter(env *Env, svc *router.Router, state layout.RouterLayout, controlHome string) Router {
	return sharedRouter{env: env, svc: svc, state: state, controlHome: controlHome}
}

// BestEffortIndex is the Index Up builds.
func BestEffortIndex(cctx *CommandContext) Index { return bestEffortIndex{cctx} }

// DoctorSections is doctor's own report, built through doctorOptionsFor
// against svc as the shared router instead of buildDoctorOptions' own
// resolution (a router.Router under router.ResolveProjectName(), which no
// environment variable can redirect): the real-daemon tests read a router
// of their own name, port and state, never a developer's real one, the same
// seam SharedRouter gives `up`. Sharing doctorOptionsFor with
// buildDoctorOptions means this seam can't drift from what `doctor` itself builds.
func DoctorSections(ctx context.Context, env *Env, svc *router.Router, controlHome string) ([]Section, error) {
	opts, err := doctorOptionsFor(env, controlHome, svc, nil)
	if err != nil {
		return nil, err
	}
	return doctorSections(ctx, opts)
}
