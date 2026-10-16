package router

// Host routes (docs/design/routing.md, decision 11): route files for
// processes running on the host. They are derived from the registration
// record's hostRoutes and repaired against it (routes.go), and are named
//
//	host.<repository-uuid>.<worktree-uuid>.<name>.yaml
//
// with the Traefik router and service named <compose project>_host_<name>.
// The `host.` prefix is not a UUID, so the sweeps of a routed service's files
// (ListFragments, RemoveWorktreeFragments, RemoveWorktreeFragmentsExcept,
// which `up` and `down` run) never match a host route, and the sweeps here
// never match a service's file. The target is the same
// http://treerig-loopback:<port> a service's file has, so a host route needs
// nothing of the proxy's mode.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
)

// hostFragmentPrefix starts the name of every host route file.
const hostFragmentPrefix = "host."

// hostRouteNamePattern is a host route's name as a file name carries it: the
// config service-name alphabet, which has no dot, so the name is the part of
// the file name after the two UUIDs.
var hostRouteNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// HostRouteName is the Traefik router and service name of a worktree's host
// route: <compose project>_host_<name>. The underscore is outside the
// service-name alphabet, so it cannot be the name of a routed service's
// router (<project>-<service>).
func HostRouteName(repositoryID, worktreeID, name string) string {
	return layout.ComposeProjectName(repositoryID, worktreeID) + "_host_" + name
}

// HostFragmentDocument is the route file of a host route: rule to the
// loopback port the host process listens on.
func HostFragmentDocument(repositoryID, worktreeID, name, rule string, port int) FragmentDocument {
	return NewFragmentDocument(HostRouteName(repositoryID, worktreeID, name), rule, port)
}

// HostFragmentPath is where a host route's file lives under state.DynamicDir.
func HostFragmentPath(state layout.RouterLayout, repositoryID, worktreeID, name string) string {
	return filepath.Join(state.DynamicDir, hostFragmentPrefix+repositoryID+"."+worktreeID+"."+name+".yaml")
}

// WriteHostFragment writes a host route's file, atomically and at 0o644 like
// every route file. The state directory is secured first (a symbolic link
// planted in it is refused, and the directory is created when the router has
// not run yet), since a host route can be added before any `up` has started
// the router.
func WriteHostFragment(state layout.RouterLayout, repositoryID, worktreeID, name string, doc FragmentDocument) error {
	if err := secureState(state); err != nil {
		return err
	}
	path := HostFragmentPath(state, repositoryID, worktreeID, name)
	if err := files.AssertSafeFilePathNamed(state.DynamicDir, path, dynamicNoun); err != nil {
		return err
	}
	encoded, err := marshalFragment(doc)
	if err != nil {
		return err
	}
	return files.AtomicWrite(path, encoded, 0o644)
}

// RemoveHostFragment removes one host route's file; a file that is not there
// is success.
func RemoveHostFragment(state layout.RouterLayout, repositoryID, worktreeID, name string) error {
	return removeHostFragments(state, repositoryID, worktreeID, func(candidate string) bool { return candidate == name })
}

// RemoveWorktreeHostFragments removes every host route file one worktree
// owns, found by its two IDs alone, never by what the record says: the
// record is deleted after this, and a file it forgot must not outlive it.
// `worktree remove` calls it; `down` and `up` never do.
func RemoveWorktreeHostFragments(state layout.RouterLayout, repositoryID, worktreeID string) error {
	return removeHostFragments(state, repositoryID, worktreeID, func(string) bool { return true })
}

// removeHostFragments removes the host route files of one worktree whose
// name match accepts. A missing directory is success.
func removeHostFragments(state layout.RouterLayout, repositoryID, worktreeID string, match func(name string) bool) error {
	fragments, err := ListHostFragments(state)
	if err != nil {
		return err
	}
	for _, fragment := range fragments {
		if fragment.RepositoryID != repositoryID || fragment.WorktreeID != worktreeID || !match(fragment.Service) {
			continue
		}
		if err := files.AssertSafeFilePathNamed(state.DynamicDir, fragment.Path, dynamicNoun); err != nil {
			return err
		}
		if err := files.RemoveWithin(state.DynamicDir, fragment.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// ListHostFragments is every host route file under state.DynamicDir, parsed
// from its name. Anything else there, including a service's file, is skipped.
func ListHostFragments(state layout.RouterLayout) ([]FragmentInfo, error) {
	entries, err := os.ReadDir(state.DynamicDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var fragments []FragmentInfo
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		rest, ok := strings.CutPrefix(entry.Name(), hostFragmentPrefix)
		if !ok {
			continue
		}
		rest, ok = strings.CutSuffix(rest, ".yaml")
		if !ok {
			continue
		}
		parts := strings.SplitN(rest, ".", 3)
		if len(parts) != 3 || !fragmentUUID.MatchString(parts[0]) || !fragmentUUID.MatchString(parts[1]) || !hostRouteNamePattern.MatchString(parts[2]) {
			continue
		}
		fragments = append(fragments, FragmentInfo{
			RepositoryID: parts[0], WorktreeID: parts[1], Service: parts[2],
			Path: filepath.Join(state.DynamicDir, entry.Name()), HostRoute: true,
		})
	}
	return fragments, nil
}
