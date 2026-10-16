package router

// Route-file repair (docs/design/routing.md, decision 9). Once loopback ports
// are reused, a stale route file would send an old hostname to another
// worktree's service, so a file whose registration holds no port for it, or
// holds another one, is repaired by Ensure and reported by doctor. Both use
// ClassifyFragment, so they cannot drift onto two definitions of "orphaned".
//
// A file is judged only when this control home holds a record for that exact
// repository and worktree. A file with no such record is reported, never
// removed: the router's state is per treerig root while registries are per
// control home, so it may belong to another control home (the same
// repository used under two of them), and nothing here can tell.
//
// Host routes (#6) are judged the same way against the record's hostRoutes:
// the record is their source of truth, so a file whose record has no such
// route is removed, and a file that is not the one the record would produce
// (another port, or another rule after the worktree's hostname changed) is
// rewritten. A recorded route whose file is missing is reported by doctor and
// not recreated here (re-running `route add` with the same fields heals it):
// repair only ever acts on a file that exists, so it never brings back a file
// a removal has taken. A rewrite that raced a `worktree remove` can still put
// one file back, a window service files share; doctor then reports it as
// belonging to no registration.
//
// Repair reads registrations without the registry lock, which is safe
// because every registry writer replaces a record atomically, and reads a
// record again immediately before it removes or rewrites a file. That
// narrows, but does not close, the window against a concurrent `up`: `up`
// commits a port to the registration before it writes the route file, so a
// file whose port is not yet recorded cannot exist while its writer is alive
// except across that re-read. A removal in that window is repaired by the
// writer's next `up`; a rewrite to the recorded port is eventually
// consistent with the writer, which writes the same port. The router lock
// (held by Ensure) and the registry lock are never nested: nothing here
// takes the registry lock.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"

	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
)

// RegistrationStatus is what a Registrations lookup found.
type RegistrationStatus int

const (
	// Unregistered means the repository is known but has no record for the
	// worktree.
	Unregistered RegistrationStatus = iota
	// Registered means the worktree has a record.
	Registered
	// UnknownRepository means the repository has no directory in this
	// control home. A router state is per treerig root while registries are per
	// control home, so a file naming such a repository may belong to another
	// control home.
	UnknownRepository
)

// Registration is one worktree's record, as far as route repair reads it.
type Registration struct {
	Status RegistrationStatus
	// Ports are the loopback ports the record holds, by service.
	Ports map[string]int
	// HostRoutes are the host routes the record holds, by name, as the route
	// file must express them.
	HostRoutes map[string]HostTarget
}

// HostTarget is what a host route's file must say, as the record derives it.
type HostTarget struct {
	// Rule is the Traefik rule: the worktree's hostname with the route's
	// subdomain and path.
	Rule string
	// Port is the loopback port the host process listens on.
	Port int
}

// Registrations looks up a worktree's record without the registry lock.
type Registrations interface {
	// Lookup reads the record of worktreeID in repositoryID afresh. An
	// unreadable record is an error, never "unregistered".
	Lookup(repositoryID, worktreeID string) (Registration, error)
}

// RouteProblemKind says how a route file differs from its registration.
type RouteProblemKind string

const (
	// RouteUnregistered is a file whose repository this control home knows but
	// whose worktree it holds no record of: likely stale. It is reported and
	// never repaired, because the same repository may be used under another
	// control home.
	RouteUnregistered RouteProblemKind = "unregistered"
	// RouteUnknownRepository is a file whose repository this control home has
	// no directory for: likely another control home's. It is reported as a
	// notice and never repaired.
	RouteUnknownRepository RouteProblemKind = "unknown-repository"
	// RouteNoPort is a file whose registration records no port for the service.
	RouteNoPort RouteProblemKind = "no-port"
	// RouteStalePort is a file pointing at a port other than the one recorded.
	RouteStalePort RouteProblemKind = "stale-port"
	// RouteNoHostRoute is a host route's file whose registration records no
	// such route.
	RouteNoHostRoute RouteProblemKind = "no-host-route"
	// RouteStaleHostRoute is a host route's file that is not the one its
	// registration produces: another port, or another rule.
	RouteStaleHostRoute RouteProblemKind = "stale-host-route"
)

// RouteProblem is one route file that disagrees with its registration.
type RouteProblem struct {
	Fragment FragmentInfo
	Kind     RouteProblemKind
	// Routed is the port the file points at.
	Routed int
	// Recorded is the port the registration holds, for RouteStalePort and
	// RouteStaleHostRoute.
	Recorded int
	// RoutedRule and RecordedRule are the rule the file claims and the one
	// the registration produces, for RouteStaleHostRoute.
	RoutedRule, RecordedRule string
}

// Repairable reports whether Ensure repairs the problem. A file matching no
// record in this control home is only reported.
func (p RouteProblem) Repairable() bool {
	switch p.Kind {
	case RouteNoPort, RouteStalePort, RouteNoHostRoute, RouteStaleHostRoute:
		return true
	}
	return false
}

// Describe is the problem in words, naming the file's service.
func (p RouteProblem) Describe() string {
	switch p.Kind {
	case RouteUnregistered:
		return fmt.Sprintf("%s: no registration of this control home holds its worktree", p.Fragment.Service)
	case RouteUnknownRepository:
		return fmt.Sprintf("%s: this control home does not know its repository", p.Fragment.Service)
	case RouteNoPort:
		return fmt.Sprintf("%s: its registration records no port for the service", p.Fragment.Service)
	case RouteNoHostRoute:
		return fmt.Sprintf("%s (host route): its registration records no such host route", p.Fragment.Service)
	case RouteStaleHostRoute:
		return fmt.Sprintf("%s (host route): routes %s to port %d, but its registration records %s to port %d",
			p.Fragment.Service, p.RoutedRule, p.Routed, p.RecordedRule, p.Recorded)
	default:
		return fmt.Sprintf("%s: points at port %d, but its registration records %d", p.Fragment.Service, p.Routed, p.Recorded)
	}
}

// ReadFragment decodes one route file, refusing a path outside the dynamic
// directory first.
func ReadFragment(state layout.RouterLayout, path string) (FragmentDocument, error) {
	if err := files.AssertSafeFilePathNamed(state.DynamicDir, path, dynamicNoun); err != nil {
		return FragmentDocument{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return FragmentDocument{}, err
	}
	var doc FragmentDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return FragmentDocument{}, err
	}
	return doc, nil
}

// ClassifyFragment reads fragment's registration afresh and says how the
// file disagrees with it; nil when it agrees, or is not a file this build can
// judge (it cannot be read, or names no loopback target). A repository this
// control home does not know is RouteUnknownRepository, and a worktree of a
// known repository it holds no record of is RouteUnregistered.
func ClassifyFragment(state layout.RouterLayout, regs Registrations, fragment FragmentInfo) (*RouteProblem, error) {
	registration, err := regs.Lookup(fragment.RepositoryID, fragment.WorktreeID)
	if err != nil {
		return nil, err
	}
	switch registration.Status {
	case UnknownRepository:
		return &RouteProblem{Fragment: fragment, Kind: RouteUnknownRepository}, nil
	case Unregistered:
		return &RouteProblem{Fragment: fragment, Kind: RouteUnregistered}, nil
	}
	if fragment.HostRoute {
		return classifyHostFragment(state, registration, fragment)
	}
	recorded, held := registration.Ports[fragment.Service]
	if !held {
		return &RouteProblem{Fragment: fragment, Kind: RouteNoPort}, nil
	}
	doc, err := ReadFragment(state, fragment.Path)
	if err != nil {
		return nil, nil
	}
	routed := doc.TargetPort()
	if routed == 0 || routed == recorded {
		return nil, nil
	}
	return &RouteProblem{Fragment: fragment, Kind: RouteStalePort, Routed: routed, Recorded: recorded}, nil
}

// classifyHostFragment is ClassifyFragment for a host route's file whose
// worktree is registered: nil when the file is the one the record produces.
// A file that cannot be read is not judged, as a service's is not.
func classifyHostFragment(state layout.RouterLayout, registration Registration, fragment FragmentInfo) (*RouteProblem, error) {
	target, held := registration.HostRoutes[fragment.Service]
	if !held {
		return &RouteProblem{Fragment: fragment, Kind: RouteNoHostRoute}, nil
	}
	doc, err := ReadFragment(state, fragment.Path)
	if err != nil {
		return nil, nil
	}
	want := HostFragmentDocument(fragment.RepositoryID, fragment.WorktreeID, fragment.Service, target.Rule, target.Port)
	if reflect.DeepEqual(doc, want) {
		return nil, nil
	}
	return &RouteProblem{
		Fragment: fragment, Kind: RouteStaleHostRoute,
		Routed: doc.TargetPort(), Recorded: target.Port, RoutedRule: firstRule(doc), RecordedRule: target.Rule,
	}, nil
}

// firstRule is the rule of doc's router, "" when it has none; a file treerig
// wrote has exactly one.
func firstRule(doc FragmentDocument) string {
	for _, entry := range doc.HTTP.Routers {
		return entry.Rule
	}
	return ""
}

// FindRouteProblems classifies every route file under state, services' first
// (in ListFragments' order) and then host routes' (ListHostFragments'). It
// changes nothing: doctor's report.
func FindRouteProblems(state layout.RouterLayout, regs Registrations) ([]RouteProblem, error) {
	fragments, err := ListFragments(state)
	if err != nil {
		return nil, err
	}
	hostFragments, err := ListHostFragments(state)
	if err != nil {
		return nil, err
	}
	fragments = append(fragments, hostFragments...)
	var problems []RouteProblem
	for _, fragment := range fragments {
		problem, err := ClassifyFragment(state, regs, fragment)
		if err != nil {
			return nil, err
		}
		if problem != nil {
			problems = append(problems, *problem)
		}
	}
	return problems, nil
}

// RepairRoutes removes a route file whose registration holds no port for its
// service (or no host route of that name), and points a file at the port its
// registration records when they differ (a host route's file is rewritten
// whole, port and rule). It returns what it repaired and checks ctx before each file. A file
// is acted on only when a fresh read, taken immediately before the removal or
// rewrite, still finds the same problem. Files that match no record of this
// control home are left alone (RouteProblem.Repairable).
func RepairRoutes(ctx context.Context, state layout.RouterLayout, regs Registrations) ([]RouteProblem, error) {
	problems, err := FindRouteProblems(state, regs)
	if err != nil {
		return nil, err
	}
	var repaired []RouteProblem
	for _, problem := range problems {
		if err := ctx.Err(); err != nil {
			return repaired, err
		}
		if !problem.Repairable() {
			continue
		}
		again, err := ClassifyFragment(state, regs, problem.Fragment)
		if err != nil {
			return repaired, err
		}
		if again == nil || *again != problem {
			continue
		}
		if err := repairRoute(state, problem); err != nil {
			return repaired, err
		}
		repaired = append(repaired, problem)
	}
	return repaired, nil
}

// repairRoute applies one confirmed problem's repair.
func repairRoute(state layout.RouterLayout, problem RouteProblem) error {
	path := problem.Fragment.Path
	if err := files.AssertSafeFilePathNamed(state.DynamicDir, path, dynamicNoun); err != nil {
		return err
	}
	if problem.Kind == RouteStaleHostRoute {
		fragment := problem.Fragment
		doc := HostFragmentDocument(fragment.RepositoryID, fragment.WorktreeID, fragment.Service, problem.RecordedRule, problem.Recorded)
		encoded, err := marshalFragment(doc)
		if err != nil {
			return err
		}
		return files.AtomicWrite(path, encoded, 0o644)
	}
	if problem.Kind != RouteStalePort {
		return files.RemoveWithin(state.DynamicDir, path)
	}
	doc, err := ReadFragment(state, path)
	if err != nil {
		return err
	}
	encoded, err := marshalFragment(doc.withTargetPort(problem.Recorded))
	if err != nil {
		return err
	}
	return files.AtomicWrite(path, encoded, 0o644)
}
