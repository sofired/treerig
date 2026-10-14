package commands

// The host route lines of `doctor`'s "This checkout" section (#6,
// docs/design/routing.md, decisions 11 and 13): for each host route of the
// registered worktree, whether the process answers on the loopback address
// the router reaches it by, and whether its route file is there. The route
// file's disagreement with the record, and a file with no record, are the
// router section's (router.orphaned-routes). Like the rest of doctor, nothing
// here takes a lock or writes.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"

	"github.com/sofired/berth/internal/ports"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/router"
	"github.com/sofired/berth/internal/text"
)

// hostRouteChecks are the lines about registration's host routes, in the
// order the record lists them: `checkout.host-routes` per route, and
// `checkout.host-route-files` for a route whose file is missing.
func hostRouteChecks(ctx context.Context, opts doctorOptions, registration *registry.WorktreeRegistration) []Check {
	var modes RouteModes
	// Docker is asked for the proxy's mode only when it answered, and only
	// the ::1-only case reads it.
	if opts.Router != nil && opts.DockerAnswered {
		modes = opts.Router
	}
	probe := opts.Listeners
	if probe == nil {
		probe = ports.DialListener{}
	}
	var checks []Check
	for _, route := range registration.HostRoutes {
		subject := route.Name + " host route"
		if opts.RouterState.DynamicDir != "" {
			path := router.HostFragmentPath(opts.RouterState, registration.RepositoryID, registration.WorktreeID, route.Name)
			if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
				checks = append(checks, Check{Status: StatusWarn, ID: "checkout.host-route-files", Instance: route.Name, Subject: subject, Detail: fmt.Sprintf(
					"its route file is missing from %s, so the router does not route it. Run '%s route add %s' again with the same fields to write it ('%s route ls --json' lists them).",
					opts.RouterState.DynamicDir, opts.CommandName, hostRouteAddArguments(route), opts.CommandName)})
			} else if err != nil {
				// Absent is the only answer that means "missing"; anything
				// else (permission, I/O) means the file could not be looked for.
				failed := failureCheck("checkout.host-route-files", subject, err)
				failed.Instance = route.Name
				checks = append(checks, failed)
			}
		}
		address := "127.0.0.1:" + strconv.Itoa(route.Port)
		note := noteListener(ctx, probe, route.Port, modes)
		check := Check{ID: "checkout.host-routes", Instance: route.Name, Subject: subject}
		switch {
		case note.Warning != "":
			check.Status, check.Detail = StatusWarn, note.Warning
		case note.Silent:
			// Not a warning: a stopped dev server is the normal state between sessions.
			check.Status, check.Detail = StatusOK, fmt.Sprintf("nothing is listening on %s; requests for it get a 502 until a process is", address)
		case note.IPv6Only:
			check.Status, check.Detail = StatusOK, fmt.Sprintf("[::1]:%d answers, and the router in host mode reaches ::1; bind 127.0.0.1 to work on Docker Desktop and OrbStack too", route.Port)
		default:
			check.Status, check.Detail = StatusOK, address+" answers"
		}
		checks = append(checks, check)
	}
	return checks
}

// hostRouteAddArguments is the arguments of the `route add` that would record
// route again, as the heal for a missing route file takes them: its name and
// every field it has.
func hostRouteAddArguments(route registry.HostRoute) string {
	arguments := fmt.Sprintf("%s --port %d", route.Name, route.Port)
	if route.Subdomain != "" {
		arguments += " --subdomain " + route.Subdomain
	}
	if route.Path != nil {
		arguments += " --path " + text.ShellQuote(*route.Path)
	}
	return arguments
}
