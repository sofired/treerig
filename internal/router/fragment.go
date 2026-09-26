package router

// Dynamic-configuration fragments: what routes a worktree's services once
// Traefik's Docker-provider discovery is gone (#55, protocol 5). Traefik's
// file provider is already wired on the router's own command line
// (RouterCommand), watching DynamicConfigMount, which is state.DynamicDir
// bind-mounted read-only into the router container. berth itself — not
// Traefik, not Docker — is responsible for writing and removing one
// fragment per routed service here.
//
// One fragment file per (repository, worktree, service) triple, not one per
// worktree: a future `berth route` (#6) will write fragments into this same
// directory for native, non-Docker processes on the same worktree, and must
// neither collide with nor clobber what `up` writes for a Docker service,
// and must be able to remove its own independently. RemoveWorktreeFragments
// is deliberately generic by worktree ID alone, so it is shared by both.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/text"
)

// fragmentUUID is a lenient UUID shape check for a fragment filename's
// repository and worktree segments — enough for ListFragments to tell a
// worktree-scoped fragment apart from a hand-placed or future-format file
// sharing the directory; it does not need the registry's own stricter
// version/variant check.
var fragmentUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// dynamicNoun names state.DynamicDir for files.AssertSafeFilePathNamed's
// outside-of-root refusal, mirroring secureState's own stateNoun for a root
// that is not a control home.
const dynamicNoun = "the shared router's dynamic-configuration directory"

// fragmentRouter is one Traefik HTTP router, in the file provider's own shape.
type fragmentRouter struct {
	Rule        string   `json:"rule"`
	EntryPoints []string `json:"entryPoints"`
	Service     string   `json:"service"`
}

// fragmentServer is one load-balancer backend server.
type fragmentServer struct {
	URL string `json:"url"`
}

// fragmentLoadBalancer is one Traefik HTTP service's load balancer.
type fragmentLoadBalancer struct {
	Servers []fragmentServer `json:"servers"`
}

// fragmentService is one Traefik HTTP service, in the file provider's own shape.
type fragmentService struct {
	LoadBalancer fragmentLoadBalancer `json:"loadBalancer"`
}

// fragmentHTTP is the http section of one dynamic-configuration fragment.
type fragmentHTTP struct {
	Routers  map[string]fragmentRouter  `json:"routers"`
	Services map[string]fragmentService `json:"services"`
}

// FragmentDocument is one Traefik dynamic-configuration fragment: one HTTP
// router and one HTTP service, both named the same, routing rule to a
// single backend server at http://alias:port. Traefik's file provider reads
// every *.yaml file under DynamicDir as JSON — JSON is YAML — and merges
// them, the same way the stack document itself is written and read.
type FragmentDocument struct {
	HTTP fragmentHTTP `json:"http"`
}

// NewFragmentDocument builds the dynamic-configuration fragment that routes
// name to alias:port on rule. name is both the router's and the service's
// name — the same string a routed service's Traefik router/service name and
// its network alias share (see internal/backend's RouteName), which is what
// keeps the alias collision-free across every worktree network the router
// is simultaneously connected to.
func NewFragmentDocument(name, rule, alias string, port int) FragmentDocument {
	return FragmentDocument{HTTP: fragmentHTTP{
		Routers: map[string]fragmentRouter{
			name: {Rule: rule, EntryPoints: []string{RouterEntrypoint}, Service: name},
		},
		Services: map[string]fragmentService{
			name: {LoadBalancer: fragmentLoadBalancer{Servers: []fragmentServer{{URL: fmt.Sprintf("http://%s:%d", alias, port)}}}},
		},
	}}
}

// marshalFragment is JSON.stringify(doc, null, 2) + "\n": the same encoding
// stack.go's own document uses, and the same line-separator fix-up
// files.AtomicJSON applies, spelled out here because fragments are written
// at 0o644 (DynamicDir's own mode contract), not AtomicJSON's fixed 0o600.
func marshalFragment(doc FragmentDocument) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(doc); err != nil {
		return nil, err
	}
	return text.FixJSONLineSeparators(buf.Bytes()), nil
}

// indexFragmentFile is the checkout index's own dynamic-configuration
// fragment's fixed filename — deliberately outside the
// "<repository-uuid>.<worktree-uuid>.<service>.yaml" pattern ListFragments
// and RemoveWorktreeFragments look for, so it is never mistaken for, swept
// up by, or removed alongside any worktree's own fragments: the index is
// the router's own permanent resource, not a worktree's.
const indexFragmentFile = "index.yaml"

// IndexFragmentPath is where the checkout index's own dynamic-configuration
// fragment lives under state.DynamicDir.
func IndexFragmentPath(state layout.RouterLayout) string {
	return filepath.Join(state.DynamicDir, indexFragmentFile)
}

// IndexContainerName is the index container's own name, as Compose v2
// names a service's first container: <router project>-index-1.
func IndexContainerName(projectName string) string {
	return projectName + "-" + IndexService + "-1"
}

// IndexFragmentDocument is the checkout index's own dynamic-configuration
// fragment. As of protocol 5 (#55), Traefik's Docker-provider discovery is
// gone, so the index's own IndexRouteLabels — still stamped onto its
// container, which keeps the stack document as it was — route nothing on
// their own any more: a file-provider fragment is the only thing Traefik
// reads now.
//
// It targets the index by container name, not by its Compose service name.
// Compose aliases every container by its service name on each network it
// joins, and the router is attached to every routed worktree's network as
// well as its own, so a worktree service that happens to be called "index"
// would make that name ambiguous from the router's side, and the index's
// host could reach it. A container name is unique on the daemon.
func IndexFragmentDocument(projectName string) FragmentDocument {
	return NewFragmentDocument(indexRouteName, fmt.Sprintf("Host(`%s`)", IndexHost), IndexContainerName(projectName), IndexPort)
}

// WriteIndexFragment writes the checkout index's own dynamic-configuration
// fragment, atomically, at 0o644 — the same mode contract WriteFragment
// uses. Called by write() on every Ensure, alongside the stack document
// itself: both describe the same router's own stack, and both are rewritten
// every time regardless of whether anything actually changed.
func WriteIndexFragment(state layout.RouterLayout, projectName string) error {
	path := IndexFragmentPath(state)
	if err := files.AssertSafeFilePathNamed(state.DynamicDir, path, dynamicNoun); err != nil {
		return err
	}
	encoded, err := marshalFragment(IndexFragmentDocument(projectName))
	if err != nil {
		return err
	}
	return files.AtomicWrite(path, encoded, 0o644)
}

// FragmentPath is where one routed service's dynamic-configuration fragment
// lives under state.DynamicDir:
// "<repository-uuid>.<worktree-uuid>.<service>.yaml". repositoryID and
// worktreeID are fixed-shape UUIDs and never contain a '.', so the filename
// splits back into its three parts unambiguously (strings.SplitN(base, ".",
// 3)) even when service itself contains one — Compose's own service-name
// grammar permits that.
func FragmentPath(state layout.RouterLayout, repositoryID, worktreeID, service string) string {
	return filepath.Join(state.DynamicDir, repositoryID+"."+worktreeID+"."+service+".yaml")
}

// WriteFragment writes one routed service's dynamic-configuration fragment,
// atomically, at 0o644 — DynamicDir's own mode contract (bind-mounted
// read-only into the router; nothing under it is ever secret, matching the
// README's own description of what lives there).
func WriteFragment(state layout.RouterLayout, repositoryID, worktreeID, service string, doc FragmentDocument) error {
	path := FragmentPath(state, repositoryID, worktreeID, service)
	if err := files.AssertSafeFilePathNamed(state.DynamicDir, path, dynamicNoun); err != nil {
		return err
	}
	encoded, err := marshalFragment(doc)
	if err != nil {
		return err
	}
	return files.AtomicWrite(path, encoded, 0o644)
}

// RemoveWorktreeFragments removes every dynamic-configuration fragment one
// worktree owns, whatever wrote them — `up`'s own per-service fragments
// today, and a future `berth route`'s (#6) native-process fragments once
// that exists, since both are named and found by worktree ID alone, never
// by writer. Tolerates there being nothing to remove: a directory that does
// not exist yet, or no fragment matching this worktree, is success.
func RemoveWorktreeFragments(state layout.RouterLayout, repositoryID, worktreeID string) error {
	return RemoveWorktreeFragmentsExcept(state, repositoryID, worktreeID, nil)
}

// RemoveWorktreeFragmentsExcept removes every dynamic-configuration
// fragment one worktree owns whose service name is not in keep; a nil or
// empty keep removes every fragment this worktree owns, the same as
// RemoveWorktreeFragments. This is the write-then-remove half of `up`'s own
// full-refresh update (found in review, round 2 of #55): `up` writes every
// currently declared route's fragment first, then calls this with those
// service names as keep, so only a route no longer declared is actually
// removed — a re-up with the same or overlapping routes is never briefly
// unrouted the way removing everything up front and writing second would
// leave it. Tolerates there being nothing to remove, the same as
// RemoveWorktreeFragments.
func RemoveWorktreeFragmentsExcept(state layout.RouterLayout, repositoryID, worktreeID string, keep map[string]bool) error {
	entries, err := os.ReadDir(state.DynamicDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	prefix := repositoryID + "." + worktreeID + "."
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		// The service name recovered exactly, not by ListFragments' own
		// SplitN(name, ".", 3): that would truncate at the first dot within
		// a service name that carries one, but prefix already fixes exactly
		// where the service name starts, so trimming it and the suffix
		// leaves the rest whole however many dots it has.
		service := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), prefix), ".yaml")
		if keep[service] {
			continue
		}
		path := filepath.Join(state.DynamicDir, entry.Name())
		if err := files.AssertSafeFilePathNamed(state.DynamicDir, path, dynamicNoun); err != nil {
			return err
		}
		if err := files.RemoveWithin(state.DynamicDir, path); err != nil {
			return err
		}
	}
	return nil
}

// FragmentInfo is one dynamic-configuration fragment's identity, as parsed
// from its filename by ListFragments.
type FragmentInfo struct {
	RepositoryID string
	WorktreeID   string
	Service      string
	Path         string
}

// ListFragments is every dynamic-configuration fragment under
// state.DynamicDir whose filename parses as
// "<repository-uuid>.<worktree-uuid>.<service>.yaml". Anything else found
// there — a hand-placed file, or one a future format writes — is silently
// skipped rather than failing the whole listing, so doctor and Ensure's own
// reconciliation never trip over a file neither of them wrote.
func ListFragments(state layout.RouterLayout) ([]FragmentInfo, error) {
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
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		parts := strings.SplitN(name, ".", 3)
		if len(parts) != 3 || !fragmentUUID.MatchString(parts[0]) || !fragmentUUID.MatchString(parts[1]) {
			continue
		}
		service := strings.TrimSuffix(parts[2], ".yaml")
		if service == "" {
			continue
		}
		fragments = append(fragments, FragmentInfo{
			RepositoryID: parts[0],
			WorktreeID:   parts[1],
			Service:      service,
			Path:         filepath.Join(state.DynamicDir, name),
		})
	}
	return fragments, nil
}
