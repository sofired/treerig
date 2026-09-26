package commands

// checkedHome is what every verb in this package uses in place of calling
// layout.ResolveHome, layout.ResolveControlHome or layout.RouterState
// directly.
//
// It resolves the berth root (BERTH_HOME) and the caller's own home
// directory exactly once per command, and applies files.AssertSafeHome to
// the berth root before returning either. A command that needs more than
// one thing derived from it — the control home, the router's state, or
// (via userHome) files.AssertAdoptableControlHome's own check — calls this
// once and derives every one of them from its two return values (via
// layout.ControlHomeFrom, layout.RouterStateFrom, or by handing userHome to
// whatever Secure*Directories call comes next), rather than re-resolving or
// re-checking BERTH_HOME per value: "checked" and "used" can never drift
// apart (#80, review round 2 item 8; round 3 item 1 made this true of every
// caller that needs more than one derived value, not just of
// resolveControlHome/resolveRouterState individually).
//
// Centralising the check here, rather than in internal/layout (which stays
// a pure path-derivation package touching no filesystem) or internal/router
// (kept to state-directory resolution alone while #55 is also in flight
// there), means no call site can reach the control home or the router's
// state directory without it.

import (
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
)

// checkedHome resolves the berth root (BERTH_HOME), refusing it when it is
// unsafe to derive anything from (files.AssertSafeHome), and the caller's
// own home directory (userHome, layout.HomeDir(getenv)) — resolved here,
// once, and handed to AssertSafeHome directly and to every later
// files.AssertAdoptableControlHome check (SecureControlDirectories and
// friends) that needs it too, rather than either reading the environment
// itself: only this entry point does.
//
// userHome is refused outright if it comes back empty — layout.HomeDir
// falls back to the password database's own home-directory field when HOME
// is unset, which some environments (containers especially) leave blank —
// rather than letting "" reach AssertSafeHome/AssertAdoptableControlHome,
// where it would resolve as the current directory (files.canonical's own
// fallback for a path that does not exist) and compare against that
// instead of refusing outright. files.AssertSafeHome and
// AssertAdoptableControlHome both guard the same way, for any caller that
// reaches them with an empty userHome some other path than this one.
func checkedHome(getenv layout.Getenv) (home, userHome string, err error) {
	home, err = layout.ResolveHome(getenv, "")
	if err != nil {
		return "", "", err
	}
	userHome, err = resolveUserHome(getenv)
	if err != nil {
		return "", "", err
	}
	if userHome == "" {
		return "", "", errEmptyUserHome
	}
	if err := files.AssertSafeHome(home, userHome); err != nil {
		return "", "", err
	}
	return home, userHome, nil
}

// resolveUserHome is layout.HomeDir(getenv); a var, not a direct call, so a
// test can make it answer "" the way a real HOME-unset, empty-password-
// database-field environment would, without needing an environment this
// suite cannot portably fake (the real layout.HomeDir falls back to the
// password database, not anything a Getenv can override, when HOME is
// unset — and on every machine this runs on, that database's own entry has
// a home directory). Production never reassigns it.
var resolveUserHome = layout.HomeDir

// errEmptyUserHome is checkedHome's own refusal when resolveUserHome
// answers "" with no error.
var errEmptyUserHome = clierr.New("Could not determine your home directory; set HOME.")

// resolveControlHome is layout.ResolveControlHome with the berth-root safety
// check applied first, for a caller that needs only the control home itself
// (not userHome, and not the router's state) from one checked berth root.
func resolveControlHome(getenv layout.Getenv) (string, error) {
	home, _, err := checkedHome(getenv)
	if err != nil {
		return "", err
	}
	return layout.ControlHomeFrom(home, getenv, "")
}

// resolveRouterState is layout.RouterState with the berth-root safety check
// applied first, for a caller that needs only the router's own layout from
// one checked berth root.
func resolveRouterState(getenv layout.Getenv) (layout.RouterLayout, error) {
	home, _, err := checkedHome(getenv)
	if err != nil {
		return layout.RouterLayout{}, err
	}
	return layout.RouterStateFrom(home), nil
}
