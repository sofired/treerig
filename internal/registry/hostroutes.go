package registry

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
)

// Host routes (docs/design/routing.md, decision 11): a route from the
// worktree's hostname to a process running on the host. They live in the
// registration record, which is their source of truth; the route files are
// derived from them and repaired against them (decision 9). A route takes the
// routing fields of a [service.<name>] table, and the hostname a service with
// that name and those fields would get.

// HostRoute is one route to a host process.
type HostRoute struct {
	// Name is the route's name: lower-case letters, digits and hyphens, unique
	// among the worktree's host routes, and never a service's name.
	Name string
	// Port is the port the process listens on at 127.0.0.1.
	Port int
	// Subdomain under the worktree host; empty routes at the host itself.
	Subdomain string
	// Path, if set, routes only this prefix.
	Path *string
}

// SamePlace reports whether r and other claim the same hostname and path:
// two routes that do would answer the same request.
func (r HostRoute) SamePlace(other HostRoute) bool {
	if r.Subdomain != other.Subdomain || (r.Path == nil) != (other.Path == nil) {
		return false
	}
	return r.Path == nil || *r.Path == *other.Path
}

// AsService is the route in the shape of a [service.<name>] table, because a
// host route is routed as a service with that name and those fields would be:
// worktree.Routes then gives it the same hostname and URL.
func (r HostRoute) AsService() config.Service {
	return config.Service{Name: r.Name, Subdomain: r.Subdomain, Path: r.Path}
}

// hostRouteJSON is one host route as written, in a fixed key order.
type hostRouteJSON struct {
	Name      string  `json:"name"`
	Port      int     `json:"port"`
	Subdomain string  `json:"subdomain,omitempty"`
	Path      *string `json:"path,omitempty"`
}

// rawHostRoute is one host route as read, decoded so that a missing or
// non-numeric field is a shape error.
type rawHostRoute struct {
	Name      *string `json:"name"`
	Port      *int    `json:"port"`
	Subdomain string  `json:"subdomain"`
	Path      *string `json:"path"`
}

// hostRoutesJSON is routes as written, in the order given (UpdateHostRoutes
// sorts them by name), and nil for none, so a
// record with no host routes has no key.
func hostRoutesJSON(routes []HostRoute) []hostRouteJSON {
	if len(routes) == 0 {
		return nil
	}
	written := make([]hostRouteJSON, len(routes))
	for i, route := range routes {
		written[i] = hostRouteJSON(route)
	}
	return written
}

// validHostRoute reports why route cannot be recorded, or "" when it can.
func validHostRoute(route HostRoute) string {
	switch {
	case config.CheckRouteName(route.Name) != "":
		return "the name " + config.CheckRouteName(route.Name)
	case route.Port < 1 || route.Port > 65_535:
		return "the port must be between 1 and 65535"
	case config.CheckSubdomain(route.Subdomain) != "":
		return "the subdomain " + config.CheckSubdomain(route.Subdomain)
	case route.Path != nil && config.CheckRoutePath(*route.Path) != "":
		return "the path " + config.CheckRoutePath(*route.Path)
	}
	return ""
}

// parseHostRoutes validates raw routes as read from a record: every field
// present and valid, and no name twice. An absent list is valid and nil.
func parseHostRoutes(raw []rawHostRoute) ([]HostRoute, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	routes := make([]HostRoute, len(raw))
	seen := map[string]bool{}
	for i, entry := range raw {
		if entry.Name == nil || entry.Port == nil {
			return nil, false
		}
		route := HostRoute{Name: *entry.Name, Port: *entry.Port, Subdomain: entry.Subdomain, Path: entry.Path}
		if validHostRoute(route) != "" || seen[route.Name] {
			return nil, false
		}
		seen[route.Name] = true
		routes[i] = route
	}
	return routes, true
}

// FindHostRoute is the host route called name, if the record has one.
func (r RegisteredWorktree) FindHostRoute(name string) (HostRoute, bool) {
	for _, route := range r.HostRoutes {
		if route.Name == name {
			return route, true
		}
	}
	return HostRoute{}, false
}

// UpdateHostRoutes makes the host routes of worktreeID's record what change
// returns, under the registry lock, and returns them. change is handed the
// record as it stands under the lock and returns the complete list; it is
// where a caller refuses (a name taken, a port in use). The list is validated
// and stored sorted by name, and nothing is written when it is unchanged. The
// record must exist and still carry expectedSlug.
func (s *Store) UpdateHostRoutes(
	ctx context.Context, worktreeID, expectedSlug string, writer Writer,
	change func(record RegisteredWorktree) ([]HostRoute, error),
) ([]HostRoute, error) {
	if err := files.SecureRegistrationDirectories(layout.WorktreeLayout(s.ControlHome, s.RepositoryID, worktreeID), writer.UserHome); err != nil {
		return nil, err
	}
	var result []HostRoute
	err := s.withRegistryLock(ctx, writer, func(context.Context) error {
		file, stored, err := s.readOwnedRecord(worktreeID, expectedSlug)
		if err != nil {
			return err
		}
		next, err := change(stored.Registration)
		if err != nil {
			return err
		}
		next = slices.Clone(next)
		slices.SortFunc(next, func(a, b HostRoute) int { return strings.Compare(a.Name, b.Name) })
		for i, route := range next {
			if why := validHostRoute(route); why != "" {
				return fmt.Errorf("host route %q cannot be recorded: %s", route.Name, why)
			}
			if i > 0 && next[i-1].Name == route.Name {
				return fmt.Errorf("host route %q is listed twice", route.Name)
			}
		}
		result = next
		if len(next) == 0 {
			result = nil
		}
		if slices.EqualFunc(stored.Registration.HostRoutes, result, HostRoute.Same) {
			return nil
		}
		registration := stored.Registration
		registration.HostRoutes = result
		return files.AtomicJSON(file, recordFor(registration, stored.Initialization))
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Same reports whether r and other are the same route in every field: name,
// port, subdomain and path.
func (r HostRoute) Same(other HostRoute) bool {
	return r.Name == other.Name && r.Port == other.Port && r.SamePlace(other)
}

// PortHolder says what holds port as a pinned loopback port in any record of
// controlHome: a worktree's routed service or a repository's shared service.
// It is "" when nothing does. A record that cannot be read refuses the call,
// as an allocation does, since it may hold the port. Host routes are not
// holders: they name a port a process listens on, which treerig did not pin.
func PortHolder(controlHome string, port int) (string, error) {
	scan, err := ScanRegistrations(controlHome)
	if err != nil {
		return "", err
	}
	if len(scan.Problems) > 0 {
		return "", refuseUnreadable("a host route", "a record that cannot be read may hold this port.", scan.Problems)
	}
	for _, record := range scan.Records {
		for _, service := range slices.Sorted(maps.Keys(record.Ports)) {
			if record.Ports[service] == port {
				return fmt.Sprintf("service %s of %s", service, layout.WorktreeHost(record.Slug, record.Domain)), nil
			}
		}
	}
	shared, sharedProblems, err := ScanSharedRecords(controlHome)
	if err != nil {
		return "", err
	}
	if len(sharedProblems) > 0 {
		return "", refuseUnreadable("a host route", "a record that cannot be read may hold this port.", sharedProblems)
	}
	for _, record := range shared {
		for _, host := range record.Ports {
			if host == port {
				return "shared service " + record.Name, nil
			}
		}
	}
	return "", nil
}
