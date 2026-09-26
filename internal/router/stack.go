// Package router is the shared router: one Traefik stack per Docker daemon,
// used by every worktree of every repository on this machine; its state and
// lock are per berth root (#80). It ports
// src/router/stack.ts (the stack document, built from constants) and
// src/router/router.ts (ensure/verify/status/down).
//
// Everything is pinned by digest, runs read-only, drops every capability
// (the router gets back only NET_BIND_SERVICE), and cannot escalate. As of
// protocol 5 (#55), Traefik never sees the Docker socket at all: there is no
// docker-socket-proxy and no Docker-provider discovery. Routes are read
// exclusively from Traefik's file provider, watching the read-only
// DynamicConfigMount directory; berth itself (internal/backend and
// internal/commands) writes and removes one fragment per routed service —
// see fragment.go.
package router

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/text"
)

// Protocol is bumped whenever the stack changes shape; a newer router is
// never downgraded. WP3 (#39) bumped it from 3 to 4: the router's state
// moves out of every control home into its own location, shared by every
// control home under the same berth root (#12, #13; #80 later made that
// location the berth root itself, rather than a fixed one per user
// account), and every `docker` call now carries a timeout (#14). #55 bumps
// it again, from 4 to 5: Traefik's Docker-provider discovery and the
// docker-socket-proxy (docker-api) service are dropped outright — the
// router container never has any access to the Docker socket — and routing
// moves entirely to Traefik's file provider. berth writes and removes one
// dynamic-config fragment per routed service (fragment.go), and every
// worktree gets its own dedicated Docker network instead of sharing one
// (internal/backend's RouterNetworkName); the router connects to each one
// explicitly and reconnects them all on every Ensure. See
// docs/port/divergences.md.
const Protocol = 5

// RoutedBySinceProtocol is the protocol that introduced RoutedByLabel as a
// *container* label: a router older than this routed every container
// Traefik could discover, not only ones labelled for it. Superseded by
// Protocol 5's routing model (RoutedByLabel now names a *network*, not a
// container — see its own comment), but left in place as the historical
// threshold noteUnroutedAfterUpgrade still consults for an upgrade that
// crosses it.
const RoutedBySinceProtocol = 3

// RoutedByLabel, as of protocol 5, names the shared router (by its own
// Network value) that manages one worktree's dedicated router network —
// applied to that *network*, not to any container, since Traefik's Docker
// provider is gone and no container label reaches Traefik at all any more.
// It is what lets Ensure (see labelledWorktreeNetworks) enumerate exactly
// the per-worktree networks one router instance owns and reconnect them,
// even when another router instance or project exists on the same daemon —
// the integration suite runs more than one. Fixed to the brand's own
// namespace regardless of a project's configured label-namespace override:
// it is the wire protocol between the router and every worktree's networks,
// not a per-project label.
const RoutedByLabel = brand.LabelNamespace + ".router"

// Image digests, service names and mount points the stack is built from.
const (
	RouterImage = "traefik:v3.7.9@sha256:652929a140a32d7cafafb13c6cdfab5376cfeff800f51397b87b524501ed02a8"
	IndexImage  = "busybox:1.37.0@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0"

	RouterService = "router"
	IndexService  = "index"
	// IndexHost is where the machine-wide checkout index answers; a
	// storage-contract name like the others.
	IndexHost  = brand.Name + ".localhost"
	IndexMount = "/srv/index"
	IndexPort  = 8080
	// DynamicConfigMount is where the file provider looks inside the router
	// container; the router state's dynamic/ directory is mounted here.
	DynamicConfigMount = "/etc/" + brand.Name + "/dynamic"
	RouterEntrypoint   = "web"
)

// IndexCommand is the index container's full command line.
var IndexCommand = []string{"httpd", "-f", "-p", strconv.Itoa(IndexPort), "-h", IndexMount}

// RouterImageEntrypoint and IndexImageEntrypoint are the pinned images' own
// baked-in ENTRYPOINT, exactly as `docker image inspect --format
// '{{json .Config.Entrypoint}}'` reports it for RouterImage and IndexImage
// above. Neither stack service overrides entrypoint (only command), so a
// container started from the pinned image carries its entrypoint unchanged;
// the verifier (#55/#74) checks the running container against these directly
// rather than re-reading the image on every Verify.
var (
	RouterImageEntrypoint = []string{"/entrypoint.sh"}
	IndexImageEntrypoint  []string // busybox:1.37.0 sets none.
)

// StackOptions is what a stack document is built from.
type StackOptions struct {
	// ProjectName is the Compose project name; also prefixes the internal network.
	ProjectName string
	// Network is the shared network worktree services join to be routed.
	Network string
	// Port is the loopback port to publish.
	Port int
	// DynamicDir is the host directory the file provider watches.
	DynamicDir string
	// IndexDir is the host directory the index container serves.
	IndexDir       string
	LabelNamespace string
	// Home is the berth root stamped on both containers as RouterHomeLabel
	// (#82); "" stamps an empty value rather than omitting the label, which
	// is only ever what a caller with no real berth root to give (a test,
	// mainly) asks for.
	Home string
}

// IndexURL is where the checkout index answers.
func IndexURL(port int) string {
	if port == 80 {
		return "http://" + IndexHost
	}
	return fmt.Sprintf("http://%s:%d", IndexHost, port)
}

// indexRouteName is the index's own Traefik router/service name: fixed,
// unlike a worktree's, since there is only ever one index.
const indexRouteName = brand.Name + "-index"

// IndexRouteLabels is a vestige of how the index used to be routed, before
// protocol 5 (#55) dropped Traefik's Docker-provider discovery: these
// container labels route nothing on their own any more — the index is
// routed by its own dynamic-configuration fragment instead
// (WriteIndexFragment), the same way a worktree's routed service is. Still
// stamped on the index container (Stack) and checked by Verify
// (indexProblems), on purpose: dropping the labels here without also
// dropping indexProblems' own check for them would make an unlabelled,
// correctly-routed index container look drifted, forcing a needless
// recreate every Ensure. Left as a deliberate, documented no-op rather than
// unwound in the same change that made it one; see router.go's own write()
// for where the fragment that actually routes the index gets written.
func IndexRouteLabels(network string) labelSet {
	return labels(
		kv("traefik.enable", "true"),
		kv(RoutedByLabel, network),
		kv(fmt.Sprintf("traefik.http.routers.%s.rule", indexRouteName), fmt.Sprintf("Host(`%s`)", IndexHost)),
		kv(fmt.Sprintf("traefik.http.routers.%s.entrypoints", indexRouteName), RouterEntrypoint),
		kv(fmt.Sprintf("traefik.http.services.%s.loadbalancer.server.port", indexRouteName), strconv.Itoa(IndexPort)),
	)
}

// RouterLabels are the two labels every service in the stack carries.
func RouterLabels(labelNamespace, component string) labelSet {
	return labels(
		kv(labelNamespace+".component", component),
		kv(labelNamespace+".router-protocol", strconv.Itoa(Protocol)),
	)
}

// RouterHomeLabel names, on both the router's and the index's containers,
// the berth root (BERTH_HOME) whose Ensure most recently started or
// recreated them (#82). Before this label existed, two berth roots each
// held their own router lock over their own state but drove the same
// machine-wide container (row 9/29 in docs/port/divergences.md): neither
// could tell its own router apart from one another root had just started,
// so a second root's Ensure force-recreated it out from under the first
// with no warning. Router.Owner reads this label back so Ensure can refuse
// instead (row 35); doctor reports it read-only. Fixed to the brand's own
// namespace regardless of a project's configured label-namespace override,
// the same way RoutedByLabel is (see its own comment): it identifies the
// berth root that owns the router, not any project.
const RouterHomeLabel = brand.LabelNamespace + ".router-home"

// RouterCommand is Traefik's full command line; verification checks the
// running container has exactly this. As of protocol 5, there is no
// Docker-provider configuration at all: routes come only from the file
// provider watching DynamicConfigMount.
func RouterCommand() []string {
	return []string{
		fmt.Sprintf("--entrypoints.%s.address=:80", RouterEntrypoint),
		fmt.Sprintf("--providers.file.directory=%s", DynamicConfigMount),
		"--providers.file.watch=true",
		"--api.dashboard=false",
		"--ping=true",
	}
}

/* ------------------------------------------------------- ordered JSON objects */

// labelSet is Record<string, string> in TypeScript, rendered in exactly the
// order stack.ts builds it: JSON.stringify keeps insertion order, but Go's
// encoding/json sorts a map's keys instead, and this order is not
// alphabetical (dev.berth.router-protocol before traefik.enable, entrypoints
// before rule). Its strings are quoted with text.JSONString, JSON.stringify's
// own escaping, so the bytes match TypeScript's whatever a label holds.
type labelSet []labelEntry

// labelEntry is one key/value pair of a labelSet, in the position it was given.
type labelEntry struct {
	key   string
	value string
}

// labels builds a labelSet from entries, in the order given.
func labels(entries ...labelEntry) labelSet { return labelSet(entries) }

// kv is one labelEntry.
func kv(key, value string) labelEntry { return labelEntry{key: key, value: value} }

// Map is the set as a plain map, for a caller that only needs to look values
// up (tests building a fixture to compare against, mainly) and does not need
// the order that makes this type worth having in the first place.
func (s labelSet) Map() map[string]string {
	m := make(map[string]string, len(s))
	for _, entry := range s {
		m[entry.key] = entry.value
	}
	return m
}

// MarshalJSON writes the set in insertion order.
func (s labelSet) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, entry := range s {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(text.JSONString(entry.key))
		b.WriteByte(':')
		b.WriteString(text.JSONString(entry.value))
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

/* ---------------------------------------------------------------- the stack */

// mount is one Compose bind mount, in stack.ts's own field order.
type mount struct {
	Type     string `json:"type"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

// healthcheck is one service's Compose healthcheck.
type healthcheck struct {
	Test     []string `json:"test"`
	Interval string   `json:"interval"`
	Timeout  string   `json:"timeout"`
	Retries  int      `json:"retries"`
}

// newHealthcheck is a healthcheck running test, with every service's shared
// interval, timeout and retry count (stack.ts's HEALTHCHECK).
func newHealthcheck(test []string) healthcheck {
	return healthcheck{Test: test, Interval: "2s", Timeout: "3s", Retries: 30}
}

// routerServiceDoc is the router (Traefik) service's Compose document, in
// stack.ts's own field order. As of protocol 5 there is no depends_on: the
// docker-api proxy it used to wait on is gone.
type routerServiceDoc struct {
	Image       string      `json:"image"`
	Restart     string      `json:"restart"`
	Command     []string    `json:"command"`
	Ports       []string    `json:"ports"`
	ReadOnly    bool        `json:"read_only"`
	CapDrop     []string    `json:"cap_drop"`
	SecurityOpt []string    `json:"security_opt"`
	CapAdd      []string    `json:"cap_add"`
	Tmpfs       []string    `json:"tmpfs"`
	Volumes     []mount     `json:"volumes"`
	Healthcheck healthcheck `json:"healthcheck"`
	Labels      labelSet    `json:"labels"`
	Networks    []string    `json:"networks"`
}

// indexServiceDoc is the index service's Compose document, in stack.ts's own
// field order.
type indexServiceDoc struct {
	Image       string      `json:"image"`
	Restart     string      `json:"restart"`
	Command     []string    `json:"command"`
	ReadOnly    bool        `json:"read_only"`
	CapDrop     []string    `json:"cap_drop"`
	SecurityOpt []string    `json:"security_opt"`
	Volumes     []mount     `json:"volumes"`
	Healthcheck healthcheck `json:"healthcheck"`
	Labels      labelSet    `json:"labels"`
	Networks    []string    `json:"networks"`
}

// stackServices is the stack's two services, in stack.ts's own field order.
// As of protocol 5 there is no docker-api service.
type stackServices struct {
	Router routerServiceDoc `json:"router"`
	Index  indexServiceDoc  `json:"index"`
}

// namedNetwork is a Compose network declared under a fixed name.
type namedNetwork struct {
	Name string `json:"name"`
}

// stackNetworks is the stack's own network. As of protocol 5 there is no
// docker-api network: nothing needs it once Traefik never talks to Docker.
type stackNetworks struct {
	Router namedNetwork `json:"router"`
}

// StackDocument is the Compose document the shared router runs: hardened,
// pinned by digest, JSON that Compose reads as YAML. Its field order is
// exactly stack.ts's object-literal order, so encoding it with
// files.AtomicJSON reproduces JSON.stringify(document, null, 2) byte for
// byte apart from the ledgered protocol-4 and protocol-5 differences (see
// docs/port/divergences.md and testdata/stack-oracle.json).
type StackDocument struct {
	Services stackServices `json:"services"`
	Networks stackNetworks `json:"networks"`
}

// hardenedCapDrop is every service's own cap_drop (stack.ts's HARDENED).
var hardenedCapDrop = []string{"ALL"}

// hardenedSecurityOpt is every service's own security_opt (stack.ts's HARDENED).
var hardenedSecurityOpt = []string{"no-new-privileges:true"}

// Stack builds the stack document for options.
func Stack(options StackOptions) StackDocument {
	routerLabelSet := append(append(labelSet{}, RouterLabels(options.LabelNamespace, RouterService)...), kv(RouterHomeLabel, options.Home))
	indexLabels := append(append(append(labelSet{}, RouterLabels(options.LabelNamespace, IndexService)...), IndexRouteLabels(options.Network)...), kv(RouterHomeLabel, options.Home))

	return StackDocument{
		Services: stackServices{
			Router: routerServiceDoc{
				Image:       RouterImage,
				Restart:     "unless-stopped",
				Command:     RouterCommand(),
				Ports:       []string{fmt.Sprintf("127.0.0.1:%d:80", options.Port)},
				ReadOnly:    true,
				CapDrop:     hardenedCapDrop,
				SecurityOpt: hardenedSecurityOpt,
				CapAdd:      []string{"NET_BIND_SERVICE"},
				Tmpfs:       []string{"/tmp"},
				Volumes: []mount{
					{Type: "bind", Source: options.DynamicDir, Target: DynamicConfigMount, ReadOnly: true},
				},
				Healthcheck: newHealthcheck([]string{"CMD", "traefik", "healthcheck", "--ping"}),
				Labels:      routerLabelSet,
				Networks:    []string{RouterService},
			},
			Index: indexServiceDoc{
				Image:       IndexImage,
				Restart:     "unless-stopped",
				Command:     IndexCommand,
				ReadOnly:    true,
				CapDrop:     hardenedCapDrop,
				SecurityOpt: hardenedSecurityOpt,
				Volumes: []mount{
					{Type: "bind", Source: options.IndexDir, Target: IndexMount, ReadOnly: true},
				},
				Healthcheck: newHealthcheck([]string{"CMD", "wget", "-qO-", fmt.Sprintf("http://127.0.0.1:%d/index.html", IndexPort)}),
				Labels:      indexLabels,
				Networks:    []string{RouterService},
			},
		},
		Networks: stackNetworks{
			Router: namedNetwork{Name: options.Network},
		},
	}
}
