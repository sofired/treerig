package router

// Dynamic-configuration route files: what routes a worktree's services.
// Traefik's file provider is wired on the router's own command line
// (RouterCommand), watching DynamicConfigMount, which is state.DynamicDir
// bind-mounted read-only into the router container. treerig itself, not
// Traefik and not Docker, writes and removes one file per routed service.
//
// Every file targets http://treerig-loopback:<port> (LoopbackAlias), so the
// files never depend on the proxy's mode. A file's name decides what owns
// it, and a sweep only ever touches the names it understands:
//
//	<repository-uuid>.<worktree-uuid>.<service>.yaml   a worktree's routed service
//	index.yaml                                         the overview
//	host.<repository-uuid>.<worktree-uuid>.<name>.yaml a worktree's host route (#6, hostroutes.go)
//	shared.<repository-uuid>.<name>.yaml               a shared service's routed web UI (shared_routes.go)
//
// ListFragments and RemoveWorktreeFragments match the first form only: the
// other prefixes are not UUIDs, so a service sweep never matches them. Host
// routes have sweeps of their own (hostroutes.go), so `down` and `up`, which
// sweep services, leave them alone.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/text"
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
// name on rule to the loopback port port. name is both the router's and the
// service's name; for a routed Compose service it is internal/backend's
// RouteName.
func NewFragmentDocument(name, rule string, port int) FragmentDocument {
	return FragmentDocument{HTTP: fragmentHTTP{
		Routers: map[string]fragmentRouter{
			name: {Rule: rule, EntryPoints: []string{RouterEntrypoint}, Service: name},
		},
		Services: map[string]fragmentService{
			name: {LoadBalancer: fragmentLoadBalancer{Servers: []fragmentServer{{URL: TargetURL(port)}}}},
		},
	}}
}

// TargetURL is where a route file sends requests for a service published on
// the loopback port port.
func TargetURL(port int) string {
	return fmt.Sprintf("http://%s:%d", LoopbackAlias, port)
}

// TargetPort is the loopback port a route file's first backend names, or 0
// when it names none this build writes (a hand-placed file, another host).
func (d FragmentDocument) TargetPort() int {
	for _, service := range d.HTTP.Services {
		for _, server := range service.LoadBalancer.Servers {
			if port, ok := parseTargetURL(server.URL); ok {
				return port
			}
		}
	}
	return 0
}

// parseTargetURL reads the port out of a TargetURL.
func parseTargetURL(target string) (int, bool) {
	rest, ok := strings.CutPrefix(target, "http://"+LoopbackAlias+":")
	if !ok {
		return 0, false
	}
	port, err := strconv.Atoi(rest)
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}

// withTargetPort is the document with every backend of ours retargeted to port.
func (d FragmentDocument) withTargetPort(port int) FragmentDocument {
	services := make(map[string]fragmentService, len(d.HTTP.Services))
	for name, service := range d.HTTP.Services {
		servers := make([]fragmentServer, len(service.LoadBalancer.Servers))
		for i, server := range service.LoadBalancer.Servers {
			servers[i] = server
			if _, ok := parseTargetURL(server.URL); ok {
				servers[i] = fragmentServer{URL: TargetURL(port)}
			}
		}
		services[name] = fragmentService{LoadBalancer: fragmentLoadBalancer{Servers: servers}}
	}
	return FragmentDocument{HTTP: fragmentHTTP{Routers: d.HTTP.Routers, Services: services}}
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

// indexFragmentFile is the overview's own dynamic-configuration
// fragment's fixed filename — deliberately outside the
// "<repository-uuid>.<worktree-uuid>.<service>.yaml" pattern ListFragments
// and RemoveWorktreeFragments look for, so it is never mistaken for, swept
// up by, or removed alongside any worktree's own fragments: the index is
// the router's own permanent resource, not a worktree's.
const indexFragmentFile = "index.yaml"

// IndexFragmentPath is where the overview's own dynamic-configuration
// fragment lives under state.DynamicDir.
func IndexFragmentPath(state layout.RouterLayout) string {
	return filepath.Join(state.DynamicDir, indexFragmentFile)
}

// IndexFragmentDocument is the overview's own dynamic-configuration
// fragment: the index is routed like any service, to the loopback port it is
// published on.
func IndexFragmentDocument(indexPort int) FragmentDocument {
	return NewFragmentDocument(IndexRouteName, fmt.Sprintf("Host(`%s`)", IndexHost), indexPort)
}

// WriteIndexFragment writes the overview's own dynamic-configuration
// fragment, atomically, at 0o644 (WriteFragment's mode contract). Called by
// Ensure on every start, alongside the stack document itself.
func WriteIndexFragment(state layout.RouterLayout, indexPort int) error {
	path := IndexFragmentPath(state)
	if err := files.AssertSafeFilePathNamed(state.DynamicDir, path, dynamicNoun); err != nil {
		return err
	}
	encoded, err := marshalFragment(IndexFragmentDocument(indexPort))
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
// read-only into the router; nothing under it is ever secret, matching
// docs/concepts.md's own description of what lives there).
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

// RemoveWorktreeFragments removes every routed service's dynamic-configuration
// fragment one worktree owns, found by worktree ID alone, never by writer.
// Host routes (#6) are not among them: see RemoveWorktreeHostFragments.
// Tolerates there being nothing to remove: a directory that does
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
	// Service is the routed service's name, or the host route's name when
	// HostRoute is set.
	Service string
	Path    string
	// HostRoute marks a fragment of a host route (#6), listed by
	// ListHostFragments rather than ListFragments.
	HostRoute bool
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

// RoutedWorktrees is how many worktrees have a route file for a service: the
// worktrees whose URLs the router serves. A stopped worktree has none, since
// `down` removes them, so after the router stops this is how many keep
// running but cannot be reached by their URLs. A listing that cannot be read
// is an error.
func RoutedWorktrees(state layout.RouterLayout) (int, error) {
	fragments, err := ListFragments(state)
	if err != nil {
		return 0, err
	}
	seen := map[[2]string]bool{}
	for _, fragment := range fragments {
		seen[[2]string{fragment.RepositoryID, fragment.WorktreeID}] = true
	}
	return len(seen), nil
}
