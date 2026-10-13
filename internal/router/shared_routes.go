package router

// Route files of repository-scoped shared services (docs/design/routing.md,
// decision 19): the web UI of a `[shared.service.<name>]` with a routed
// `port` is routed at <name>.shared.<domain> through
//
//	shared.<repository-uuid>.<name>.yaml
//
// with the Traefik router and service named <shared project>_shared_<name>.
// The `shared.` prefix is not a UUID, so ListFragments and
// RemoveWorktreeFragments, which match <uuid>.<uuid>.<service>.yaml only,
// never see these files, and a shared name can never be mistaken for a
// worktree's service. The record under the control home is the source of
// truth and the file is a cache of it, so decision 9's repair covers these
// files against the shared records exactly as it covers a worktree's against
// its registration: judged only when this control home holds the record,
// rewritten when the port differs, removed when the record routes nothing.
// Repair never creates a file: a record whose file is not written yet (or was
// removed by `shared down`) stays unrouted until `up` or `shared up` writes it.
// Like the worktree repair it reads without the shared lock and re-reads the
// record before acting, which narrows the window against a concurrent `shared
// down` (which removes the file under that lock) without closing it: a file
// repaired in that window is removed by the next `shared down`, or reported by
// doctor as a route file for a service that is not running.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
)

// sharedFragmentPrefix starts every shared route file's name.
const sharedFragmentPrefix = "shared."

// sharedName is the alphabet of a shared service's name, which names a
// record file and a route file: lower-case letters, digits and hyphens, so no
// path separator and no dot.
var sharedName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// SharedRouteName is the Traefik router and service name of a shared
// service's route: <shared project>_shared_<name>. An underscore is outside
// the service-name alphabet, so no worktree service's route can share it.
func SharedRouteName(repositoryID, name string) string {
	return layout.SharedProjectName(repositoryID) + "_shared_" + name
}

// SharedRouteRule is the rule a shared service's route matches, its host
// under the domain recorded at allocation.
func SharedRouteRule(name, domain string) string {
	return fmt.Sprintf("Host(`%s`)", layout.SharedHost(name, domain))
}

// SharedFragmentPath is where a shared service's route file lives under
// state.DynamicDir: "shared.<repository-uuid>.<name>.yaml". Both parts are
// dot-free (a UUID, and a service name from the slug alphabet), so the file
// name splits back into them unambiguously.
func SharedFragmentPath(state layout.RouterLayout, repositoryID, name string) string {
	return filepath.Join(state.DynamicDir, sharedFragmentPrefix+repositoryID+"."+name+".yaml")
}

// WriteSharedFragment writes the route file of one shared service, atomically
// at 0o644 (WriteFragment's mode contract): its host under domain, to the
// loopback port hostPort its UI is published on.
func WriteSharedFragment(state layout.RouterLayout, repositoryID, name, domain string, hostPort int) error {
	path := SharedFragmentPath(state, repositoryID, name)
	if err := files.AssertSafeFilePathNamed(state.DynamicDir, path, dynamicNoun); err != nil {
		return err
	}
	encoded, err := marshalFragment(NewFragmentDocument(SharedRouteName(repositoryID, name), SharedRouteRule(name, domain), hostPort))
	if err != nil {
		return err
	}
	return files.AtomicWrite(path, encoded, 0o644)
}

// RemoveSharedFragment removes one shared service's route file. There being
// no file, or no dynamic directory yet, is success.
func RemoveSharedFragment(state layout.RouterLayout, repositoryID, name string) error {
	path := SharedFragmentPath(state, repositoryID, name)
	if err := files.AssertSafeFilePathNamed(state.DynamicDir, path, dynamicNoun); err != nil {
		return err
	}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return files.RemoveWithin(state.DynamicDir, path)
}

// SharedFragmentInfo is one shared route file's identity, as parsed from its
// filename by ListSharedFragments.
type SharedFragmentInfo struct {
	RepositoryID string
	Name         string
	Path         string
}

// ListSharedFragments is every shared route file under state.DynamicDir
// whose filename parses as "shared.<repository-uuid>.<name>.yaml". Any other
// file is skipped, as ListFragments skips what it does not understand.
func ListSharedFragments(state layout.RouterLayout) ([]SharedFragmentInfo, error) {
	entries, err := os.ReadDir(state.DynamicDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var found []SharedFragmentInfo
	for _, entry := range entries {
		rest, ok := strings.CutPrefix(entry.Name(), sharedFragmentPrefix)
		if entry.IsDir() || !ok {
			continue
		}
		rest, ok = strings.CutSuffix(rest, ".yaml")
		if !ok {
			continue
		}
		repositoryID, name, ok := strings.Cut(rest, ".")
		if !ok || !fragmentUUID.MatchString(repositoryID) || len(name) > 63 || !sharedName.MatchString(name) {
			continue
		}
		found = append(found, SharedFragmentInfo{RepositoryID: repositoryID, Name: name, Path: filepath.Join(state.DynamicDir, entry.Name())})
	}
	return found, nil
}

// SharedRegistration is what a SharedRegistrations lookup found.
type SharedRegistration struct {
	// Status is Registered when this control home holds the service's
	// record, Unregistered when it knows the repository but holds no record
	// of that service, and UnknownRepository when it has no directory for
	// the repository.
	Status RegistrationStatus
	// Domain is the domain the record was allocated under.
	Domain string
	// HostPort is the loopback port the web UI is published on; 0 when the
	// record routes nothing.
	HostPort int
}

// SharedRegistrations looks up shared services' records without the registry
// lock, as Registrations does for worktrees.
type SharedRegistrations interface {
	// LookupShared reads the record of the shared service name in
	// repositoryID afresh. An unreadable record is an error, never
	// "unregistered".
	LookupShared(repositoryID, name string) (SharedRegistration, error)
}

// SharedRouteProblem is one shared route file that disagrees with its record.
type SharedRouteProblem struct {
	Fragment SharedFragmentInfo
	Kind     RouteProblemKind
	// Routed is the port the file points at.
	Routed int
	// Recorded is the port the record holds, for RouteStalePort.
	Recorded int
}

// Repairable reports whether Ensure repairs the problem. A file matching no
// record in this control home is only reported.
func (p SharedRouteProblem) Repairable() bool {
	return p.Kind == RouteNoPort || p.Kind == RouteStalePort
}

// Describe is the problem in words, naming the file's service.
func (p SharedRouteProblem) Describe() string {
	service := "shared service " + p.Fragment.Name
	switch p.Kind {
	case RouteUnregistered:
		return fmt.Sprintf("%s: this control home holds no record of it", service)
	case RouteUnknownRepository:
		return fmt.Sprintf("%s: this control home does not know its repository", service)
	case RouteNoPort:
		return fmt.Sprintf("%s: its record routes no port", service)
	default:
		return fmt.Sprintf("%s: points at port %d, but its record says %d", service, p.Routed, p.Recorded)
	}
}

// ClassifySharedFragment reads the fragment's record afresh and says how the
// file disagrees with it; nil when it agrees, or is not a file this build can
// judge (it cannot be read, or names no loopback target).
func ClassifySharedFragment(state layout.RouterLayout, regs SharedRegistrations, fragment SharedFragmentInfo) (*SharedRouteProblem, error) {
	registration, err := regs.LookupShared(fragment.RepositoryID, fragment.Name)
	if err != nil {
		return nil, err
	}
	switch registration.Status {
	case UnknownRepository:
		return &SharedRouteProblem{Fragment: fragment, Kind: RouteUnknownRepository}, nil
	case Unregistered:
		return &SharedRouteProblem{Fragment: fragment, Kind: RouteUnregistered}, nil
	}
	if registration.HostPort == 0 {
		return &SharedRouteProblem{Fragment: fragment, Kind: RouteNoPort}, nil
	}
	doc, err := ReadFragment(state, fragment.Path)
	if err != nil {
		return nil, nil
	}
	routed := doc.TargetPort()
	if routed == 0 || routed == registration.HostPort {
		return nil, nil
	}
	return &SharedRouteProblem{Fragment: fragment, Kind: RouteStalePort, Routed: routed, Recorded: registration.HostPort}, nil
}

// FindSharedRouteProblems classifies every shared route file under state, in
// ListSharedFragments' order. It changes nothing: doctor's report.
func FindSharedRouteProblems(state layout.RouterLayout, regs SharedRegistrations) ([]SharedRouteProblem, error) {
	fragments, err := ListSharedFragments(state)
	if err != nil {
		return nil, err
	}
	var problems []SharedRouteProblem
	for _, fragment := range fragments {
		problem, err := ClassifySharedFragment(state, regs, fragment)
		if err != nil {
			return nil, err
		}
		if problem != nil {
			problems = append(problems, *problem)
		}
	}
	return problems, nil
}

// RepairSharedRoutes removes a shared route file whose record routes nothing
// and points a file at the port its record holds when they differ, each only
// when a fresh read taken immediately before still finds the same problem.
// Files that match no record of this control home are left alone. It checks
// ctx before each file and returns what it repaired.
func RepairSharedRoutes(ctx context.Context, state layout.RouterLayout, regs SharedRegistrations) ([]SharedRouteProblem, error) {
	problems, err := FindSharedRouteProblems(state, regs)
	if err != nil {
		return nil, err
	}
	var repaired []SharedRouteProblem
	for _, problem := range problems {
		if err := ctx.Err(); err != nil {
			return repaired, err
		}
		if !problem.Repairable() {
			continue
		}
		again, err := ClassifySharedFragment(state, regs, problem.Fragment)
		if err != nil {
			return repaired, err
		}
		if again == nil || *again != problem {
			continue
		}
		if err := repairSharedRoute(state, problem); err != nil {
			return repaired, err
		}
		repaired = append(repaired, problem)
	}
	return repaired, nil
}

// repairSharedRoute applies one confirmed problem's repair.
func repairSharedRoute(state layout.RouterLayout, problem SharedRouteProblem) error {
	path := problem.Fragment.Path
	if err := files.AssertSafeFilePathNamed(state.DynamicDir, path, dynamicNoun); err != nil {
		return err
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
