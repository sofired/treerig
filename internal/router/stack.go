// Package router is the shared router: one Traefik stack per user account,
// used by every worktree of every repository on this machine. It ports
// src/router/stack.ts (the stack document, built from constants) and
// src/router/router.ts (ensure/verify/status/down).
//
// Everything is pinned by digest, runs read-only, drops every capability
// (the router gets back only NET_BIND_SERVICE), and cannot escalate. Traefik
// never sees the Docker socket: it talks to a proxy, which refuses every
// POST. What that proxy still exposes is described on DockerAPIAccess below,
// and it is wider than "what a router needs".
package router

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/text"
)

// Protocol is bumped whenever the stack changes shape; a newer router is
// never downgraded. WP3 (#39) bumps it from 3 to 4: the router's state moves
// out of every control home into one machine-wide location per user account
// (#12, #13), and every `docker` call now carries a timeout (#14). See
// docs/port/divergences.md.
const Protocol = 4

// RoutedBySinceProtocol is the protocol that introduced RoutedByLabel: a
// router older than this routed every container Traefik could discover, not
// only ones labelled for it.
const RoutedBySinceProtocol = 3

// RoutedByLabel names the shared network of the router a container belongs
// to. Traefik is told to discover nothing else, so two router stacks on one
// daemon — the integration suite's beside a real installation's, say — never
// route each other's containers. Fixed to the brand's own namespace
// regardless of a project's configured label-namespace override: it is the
// wire protocol between the router and every worktree's services, not a
// per-project label (Traefik also reserves `traefik.*` for configuration, so
// its own network label cannot serve as the constraint).
const RoutedByLabel = brand.LabelNamespace + ".router"

// Image digests, service names and mount points the stack is built from.
const (
	RouterImage    = "traefik:v3.7.9@sha256:652929a140a32d7cafafb13c6cdfab5376cfeff800f51397b87b524501ed02a8"
	DockerAPIImage = "tecnativa/docker-socket-proxy:v0.5.0@sha256:1f5038b54f06c3e18422902cf00ba21803d1c97805aae032e5e6673d532d3459"
	IndexImage     = "busybox:1.37.0@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0"

	RouterService    = "router"
	DockerAPIService = "docker-api"
	IndexService     = "index"
	// IndexHost is where the machine-wide checkout index answers; a
	// storage-contract name like the others.
	IndexHost        = brand.Name + ".localhost"
	IndexMount       = "/srv/index"
	IndexPort        = 8080
	DockerAPINetwork = "docker-api"
	DockerSocket     = "/var/run/docker.sock"
	// DynamicConfigMount is where the file provider looks inside the router
	// container; the router state's dynamic/ directory is mounted here.
	DynamicConfigMount = "/etc/" + brand.Name + "/dynamic"
	RouterEntrypoint   = "web"
)

// IndexCommand is the index container's full command line.
var IndexCommand = []string{"httpd", "-f", "-p", strconv.Itoa(IndexPort), "-h", IndexMount}

// DockerAPIAccess is what the socket proxy lets through; everything not
// listed here is denied. Returns a fresh map every call so a caller building
// a stack document (or a test comparing one) can never mutate the shared
// definition.
//
// CONTAINERS=1 is not "what a router needs": it opens the whole container
// section of the API, which is read-only only in the sense that it makes no
// changes. Anything that can reach the proxy can list every container on the
// machine — not just this tool's — and read each one's environment (secrets
// passed to any of them), its logs, and its filesystem through the archive
// and export endpoints. Traefik needs labels and networks; the API has no
// narrower way to offer them. Reaching the proxy means being on its internal
// network, which only the router itself joins.
func DockerAPIAccess() map[string]string {
	return map[string]string{
		"ALLOW_PAUSE": "0", "ALLOW_RESTARTS": "0", "ALLOW_START": "0", "ALLOW_STOP": "0", "ALLOW_UNPAUSE": "0",
		"AUTH": "0", "BUILD": "0", "COMMIT": "0", "CONFIGS": "0", "CONTAINERS": "1", "DISTRIBUTION": "0",
		"EVENTS": "1", "EXEC": "0", "GRPC": "0", "IMAGES": "0", "INFO": "0", "NETWORKS": "1", "NODES": "0",
		"PING": "1", "PLUGINS": "0", "POST": "0", "SECRETS": "0", "SERVICES": "0", "SESSION": "0", "SWARM": "0",
		"SYSTEM": "0", "TASKS": "0", "VERSION": "1", "VOLUMES": "0",
	}
}

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

// IndexRouteLabels is how the index is routed: like any worktree service, by
// labels, through the shared network.
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

// RouterCommand is Traefik's full command line; verification checks the
// running container has exactly this.
func RouterCommand(network string) []string {
	return []string{
		fmt.Sprintf("--entrypoints.%s.address=:80", RouterEntrypoint),
		"--providers.docker=true",
		fmt.Sprintf("--providers.docker.endpoint=tcp://%s:2375", DockerAPIService),
		"--providers.docker.exposedbydefault=false",
		fmt.Sprintf("--providers.docker.constraints=Label(`%s`,`%s`)", RoutedByLabel, network),
		fmt.Sprintf("--providers.docker.network=%s", network),
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

// dependsOnEntry is one Compose depends_on condition.
type dependsOnEntry struct {
	Condition string `json:"condition"`
}

// dependsOn is the router service's own depends_on: the docker-api proxy healthy.
type dependsOn struct {
	DockerAPI dependsOnEntry `json:"docker-api"`
}

// dockerAPIServiceDoc is the docker-api service's Compose document, in
// stack.ts's own field order.
type dockerAPIServiceDoc struct {
	Image       string            `json:"image"`
	Restart     string            `json:"restart"`
	Environment map[string]string `json:"environment"`
	ReadOnly    bool              `json:"read_only"`
	CapDrop     []string          `json:"cap_drop"`
	SecurityOpt []string          `json:"security_opt"`
	Tmpfs       []string          `json:"tmpfs"`
	Volumes     []mount           `json:"volumes"`
	Healthcheck healthcheck       `json:"healthcheck"`
	Labels      labelSet          `json:"labels"`
	Networks    []string          `json:"networks"`
}

// routerServiceDoc is the router (Traefik) service's Compose document, in
// stack.ts's own field order.
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
	DependsOn   dependsOn   `json:"depends_on"`
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

// stackServices is the stack's three services, in stack.ts's own field order.
type stackServices struct {
	DockerAPI dockerAPIServiceDoc `json:"docker-api"`
	Router    routerServiceDoc    `json:"router"`
	Index     indexServiceDoc     `json:"index"`
}

// namedNetwork is a Compose network declared under a fixed name.
type namedNetwork struct {
	Name string `json:"name"`
}

// internalNetwork is a Compose network with no external connectivity.
type internalNetwork struct {
	Internal bool `json:"internal"`
}

// stackNetworks is the stack's two networks, in stack.ts's own field order.
type stackNetworks struct {
	Router    namedNetwork    `json:"router"`
	DockerAPI internalNetwork `json:"docker-api"`
}

// StackDocument is the Compose document the shared router runs: hardened,
// pinned by digest, JSON that Compose reads as YAML. Its field order is
// exactly stack.ts's object-literal order, so encoding it with
// files.AtomicJSON reproduces JSON.stringify(document, null, 2) byte for
// byte apart from the ledgered protocol-4 differences (see
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
	dockerAPILabels := RouterLabels(options.LabelNamespace, DockerAPIService)
	routerLabelSet := RouterLabels(options.LabelNamespace, RouterService)
	indexLabels := append(append(labelSet{}, RouterLabels(options.LabelNamespace, IndexService)...), IndexRouteLabels(options.Network)...)

	return StackDocument{
		Services: stackServices{
			DockerAPI: dockerAPIServiceDoc{
				Image:       DockerAPIImage,
				Restart:     "unless-stopped",
				Environment: DockerAPIAccess(),
				ReadOnly:    true,
				CapDrop:     hardenedCapDrop,
				SecurityOpt: hardenedSecurityOpt,
				Tmpfs:       []string{"/run", "/tmp"},
				Volumes: []mount{
					{Type: "bind", Source: DockerSocket, Target: DockerSocket, ReadOnly: true},
				},
				Healthcheck: newHealthcheck([]string{"CMD-SHELL", "wget -qO- http://127.0.0.1:2375/_ping | grep -qx OK"}),
				Labels:      dockerAPILabels,
				Networks:    []string{DockerAPINetwork},
			},
			Router: routerServiceDoc{
				Image:       RouterImage,
				Restart:     "unless-stopped",
				Command:     RouterCommand(options.Network),
				Ports:       []string{fmt.Sprintf("127.0.0.1:%d:80", options.Port)},
				ReadOnly:    true,
				CapDrop:     hardenedCapDrop,
				SecurityOpt: hardenedSecurityOpt,
				CapAdd:      []string{"NET_BIND_SERVICE"},
				Tmpfs:       []string{"/tmp"},
				Volumes: []mount{
					{Type: "bind", Source: options.DynamicDir, Target: DynamicConfigMount, ReadOnly: true},
				},
				DependsOn:   dependsOn{DockerAPI: dependsOnEntry{Condition: "service_healthy"}},
				Healthcheck: newHealthcheck([]string{"CMD", "traefik", "healthcheck", "--ping"}),
				Labels:      routerLabelSet,
				Networks:    []string{RouterService, DockerAPINetwork},
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
			Router:    namedNetwork{Name: options.Network},
			DockerAPI: internalNetwork{Internal: true},
		},
	}
}
