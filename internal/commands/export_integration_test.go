//go:build integration

package commands

import (
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
