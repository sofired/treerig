// Package router is the shared router: one Traefik proxy container per
// Docker daemon, used by every worktree of every repository on this
// machine; its state and lock are per berth root (#80). It builds the
// Compose document the proxy runs from constants and implements
// ensure/status/down against it (docs/design/routing.md).
//
// The proxy reaches every target over loopback. Each routed Compose service
// publishes a pinned port on 127.0.0.1 (#119) and a host process listens on
// its own; the route files berth writes under DynamicDir name the target as
// berth-loopback:<port>, an alias the proxy's extra_hosts maps to the host's
// loopback (host mode) or to the engine's host gateway (bridge mode). No
// network is shared between worktrees and the proxy has no Docker socket:
// routes come only from Traefik's file provider, watching the read-only
// DynamicConfigMount directory, in which berth writes and removes one file
// per routed service (fragment.go).
//
// Everything is pinned by digest, runs read-only, drops every capability
// (getting back only NET_BIND_SERVICE where a privileged port needs it),
// and cannot escalate. Compose recreates the proxy whenever its
// configuration changes, which is how a mode or port change takes effect.
package router

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/text"
)

// Protocol names the generation of the stack's layout this build starts; it
// is stamped on both containers. Bump it only when Compose cannot recreate a
// router of the older generation into the new one (a change of how routes
// reach services, say, which no Compose configuration change expresses). A
// change of mode, port, image digest or any other Compose setting never bumps
// it: Compose recreates the containers itself. checkProtocol refuses a
// running router at a higher protocol (never downgrade it) and one at a lower
// or missing protocol (stop it with `router down` first), and `router status`
// and doctor report the same mismatch with the same advice.
const Protocol = 6

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
	// IndexContainerPort is the port the index's httpd listens on inside its
	// container; the host-side port is allocated (State.IndexPort).
	IndexContainerPort = 8080
	// DynamicConfigMount is where the file provider looks inside the router
	// container; the router state's dynamic/ directory is mounted here.
	DynamicConfigMount = "/etc/" + brand.Name + "/dynamic"
	// RouterEntrypoint is the entrypoint every route is served on.
	RouterEntrypoint = "web"
	// PingEntrypoint is the dedicated entrypoint Traefik's ping is served
	// on: never RouterEntrypoint, which would shadow every routed service's
	// own /ping on every hostname.
	PingEntrypoint = "traefik"
	// LoopbackAlias is the host name route files target. The proxy's
	// extra_hosts maps it to the host's loopback (host mode) or to the
	// engine's host gateway (bridge mode), so route files never depend on
	// the mode.
	LoopbackAlias = "berth-loopback"
	// BridgePingPort is the ping entrypoint's port inside the proxy
	// container in bridge mode. It is bound to the container's own loopback
	// and never published, and takes no port from the allocation range.
	BridgePingPort = 8082
	// bridgeRouterPort is the web entrypoint's port inside the proxy
	// container in bridge mode.
	bridgeRouterPort = 80
	// loopbackIP is the address everything the stack publishes or binds on
	// the host is restricted to.
	loopbackIP = "127.0.0.1"
	// nonRootUser is the numeric user (nobody) a host-mode proxy runs as when
	// its port needs no privilege.
	nonRootUser = "65534:65534"
	// privilegedPortLimit is the first port a non-root user may bind.
	privilegedPortLimit = 1024
)

// IndexCommand is the index container's full command line.
var IndexCommand = []string{"httpd", "-f", "-p", strconv.Itoa(IndexContainerPort), "-h", IndexMount}

// StackOptions is what a stack document is built from.
type StackOptions struct {
	// Mode is the proxy's network mode.
	Mode Mode
	// Port is the loopback port the proxy serves routes on.
	Port int
	// IndexPort is the loopback port the checkout index is published on.
	IndexPort int
	// PingPort is the host-mode ping entrypoint's loopback port; unused in
	// bridge mode, which has a fixed container-internal one.
	PingPort int
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

// IndexRouteName is the index's own Traefik router/service name: fixed,
// unlike a worktree's, since there is only ever one index.
const IndexRouteName = brand.Name + "-index"

// RouterLabels are the two labels every service in the stack carries.
func RouterLabels(labelNamespace, component string) labelSet {
	return labels(
		kv(labelNamespace+".component", component),
		kv(labelNamespace+".router-protocol", strconv.Itoa(Protocol)),
	)
}

// RouterHomeLabel names, on both the router's and the index's containers,
// the berth root (BERTH_HOME) whose Ensure most recently started or
// recreated them (#82). Router.Owner reads this label back so Ensure can
// refuse to take over a router another berth root started; doctor reports
// it read-only. Fixed to the brand's own namespace regardless of a
// project's configured label-namespace override: it identifies the berth
// root that owns the router, not any project.
const RouterHomeLabel = brand.LabelNamespace + ".router-home"

// RouterModeLabel names, on the router's container, the network mode it was
// started in. `router status` and `doctor` report the running mode from it,
// not the configured one. Fixed to the brand's namespace like
// RouterHomeLabel.
const RouterModeLabel = brand.LabelNamespace + ".router-mode"

// RouterPortLabel names, on the router's container, the loopback port it
// serves routes on, so the running port can be read in either mode (a host
// mode proxy publishes nothing Docker could report). Fixed to the brand's
// namespace like RouterHomeLabel.
const RouterPortLabel = brand.LabelNamespace + ".router-port"

// webAddress is the web entrypoint's listen address.
func webAddress(options StackOptions) string {
	if options.Mode == ModeHost {
		return fmt.Sprintf("%s:%d", loopbackIP, options.Port)
	}
	return fmt.Sprintf(":%d", bridgeRouterPort)
}

// pingAddress is the ping entrypoint's listen address: loopback in both
// modes, a fixed port inside the container in bridge mode.
func pingAddress(options StackOptions) string {
	if options.Mode == ModeHost {
		return fmt.Sprintf("%s:%d", loopbackIP, options.PingPort)
	}
	return fmt.Sprintf("%s:%d", loopbackIP, BridgePingPort)
}

// RouterCommand is Traefik's full command line. There is no Docker-provider
// configuration at all: routes come only from the file provider watching
// DynamicConfigMount. Ping gets its own entrypoint; `--ping=true` alone
// would open Traefik's default `*:8080`.
func RouterCommand(options StackOptions) []string {
	return []string{
		fmt.Sprintf("--entrypoints.%s.address=%s", RouterEntrypoint, webAddress(options)),
		fmt.Sprintf("--entrypoints.%s.address=%s", PingEntrypoint, pingAddress(options)),
		fmt.Sprintf("--providers.file.directory=%s", DynamicConfigMount),
		"--providers.file.watch=true",
		"--api.dashboard=false",
		"--ping=true",
		fmt.Sprintf("--ping.entryPoint=%s", PingEntrypoint),
	}
}

// routerHealthcheckTest is the router's healthcheck command. It runs as a
// separate process that reads the static configuration from its own flags,
// not the proxy's, so it carries the ping entrypoint's address.
func routerHealthcheckTest(options StackOptions) []string {
	return []string{
		"CMD", "traefik", "healthcheck", "--ping",
		fmt.Sprintf("--ping.entryPoint=%s", PingEntrypoint),
		fmt.Sprintf("--entrypoints.%s.address=%s", PingEntrypoint, pingAddress(options)),
	}
}

/* ------------------------------------------------------- ordered JSON objects */

// labelSet is an ordered set of string labels, rendered in a fixed order:
// JSON.stringify keeps insertion order, but Go's encoding/json sorts a
// map's keys instead, and this order is not alphabetical. Its strings are
// quoted with text.JSONString, JSON.stringify's own escaping, so the bytes
// match whatever a label holds.
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

// mount is one Compose bind mount.
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
// interval, timeout and retry count.
func newHealthcheck(test []string) healthcheck {
	return healthcheck{Test: test, Interval: "2s", Timeout: "3s", Retries: 30}
}

// routerServiceDoc is the router (Traefik) service's Compose document. The
// fields that differ by mode are omitted when empty: a host-mode proxy has
// a network_mode and no ports, a bridge-mode one the reverse.
type routerServiceDoc struct {
	Image       string      `json:"image"`
	Restart     string      `json:"restart"`
	Command     []string    `json:"command"`
	NetworkMode string      `json:"network_mode,omitempty"`
	User        string      `json:"user,omitempty"`
	Ports       []string    `json:"ports,omitempty"`
	ExtraHosts  []string    `json:"extra_hosts"`
	ReadOnly    bool        `json:"read_only"`
	CapDrop     []string    `json:"cap_drop"`
	SecurityOpt []string    `json:"security_opt"`
	CapAdd      []string    `json:"cap_add,omitempty"`
	Tmpfs       []string    `json:"tmpfs"`
	Volumes     []mount     `json:"volumes"`
	Healthcheck healthcheck `json:"healthcheck"`
	Labels      labelSet    `json:"labels"`
}

// indexServiceDoc is the index service's Compose document. Init runs
// Docker's init process as PID 1: busybox httpd installs no SIGTERM handler,
// and the kernel ignores a signal PID 1 does not handle, so without it every
// stop of the stack waits out Docker's 10-second grace period.
type indexServiceDoc struct {
	Image       string      `json:"image"`
	Restart     string      `json:"restart"`
	Init        bool        `json:"init"`
	Command     []string    `json:"command"`
	Ports       []string    `json:"ports"`
	ReadOnly    bool        `json:"read_only"`
	CapDrop     []string    `json:"cap_drop"`
	SecurityOpt []string    `json:"security_opt"`
	Volumes     []mount     `json:"volumes"`
	Healthcheck healthcheck `json:"healthcheck"`
	Labels      labelSet    `json:"labels"`
}

// stackServices is the stack's two services.
type stackServices struct {
	Router routerServiceDoc `json:"router"`
	Index  indexServiceDoc  `json:"index"`
}

// StackDocument is the Compose document the shared router runs: hardened,
// pinned by digest, JSON that Compose reads as YAML. Its field order is
// fixed, so encoding it with files.AtomicJSON always produces the same
// bytes; TestStackGolden pins them against testdata/stack-golden-*.json.
type StackDocument struct {
	Services stackServices `json:"services"`
}

// hardenedCapDrop is every service's own cap_drop.
var hardenedCapDrop = []string{"ALL"}

// hardenedSecurityOpt is every service's own security_opt.
var hardenedSecurityOpt = []string{"no-new-privileges:true"}

// Stack builds the stack document for options.
func Stack(options StackOptions) StackDocument {
	routerLabelSet := append(append(labelSet{}, RouterLabels(options.LabelNamespace, RouterService)...),
		kv(RouterHomeLabel, options.Home),
		kv(RouterModeLabel, string(options.Mode)),
		kv(RouterPortLabel, strconv.Itoa(options.Port)))
	indexLabels := append(append(labelSet{}, RouterLabels(options.LabelNamespace, IndexService)...), kv(RouterHomeLabel, options.Home))

	router := routerServiceDoc{
		Image:       RouterImage,
		Restart:     "unless-stopped",
		Command:     RouterCommand(options),
		ReadOnly:    true,
		CapDrop:     hardenedCapDrop,
		SecurityOpt: hardenedSecurityOpt,
		Tmpfs:       []string{"/tmp"},
		Volumes: []mount{
			{Type: "bind", Source: options.DynamicDir, Target: DynamicConfigMount, ReadOnly: true},
		},
		Healthcheck: newHealthcheck(routerHealthcheckTest(options)),
		Labels:      routerLabelSet,
	}
	switch options.Mode {
	case ModeHost:
		// Mapping the alias to both loopback addresses lets Go's dialer reach
		// a listener bound to either family.
		router.NetworkMode = "host"
		router.ExtraHosts = []string{LoopbackAlias + ":127.0.0.1", LoopbackAlias + ":::1"}
		// Binding a privileged port takes root and NET_BIND_SERVICE: a
		// non-root user does not inherit the capability. Any other port runs
		// as nobody with no capability at all.
		if options.Port < privilegedPortLimit {
			router.CapAdd = []string{"NET_BIND_SERVICE"}
		} else {
			router.User = nonRootUser
		}
	default:
		router.Ports = []string{fmt.Sprintf("%s:%d:%d", loopbackIP, options.Port, bridgeRouterPort)}
		router.ExtraHosts = []string{LoopbackAlias + ":host-gateway"}
		router.CapAdd = []string{"NET_BIND_SERVICE"}
	}

	return StackDocument{Services: stackServices{
		Router: router,
		Index: indexServiceDoc{
			Image:       IndexImage,
			Restart:     "unless-stopped",
			Init:        true,
			Command:     IndexCommand,
			Ports:       []string{fmt.Sprintf("%s:%d:%d", loopbackIP, options.IndexPort, IndexContainerPort)},
			ReadOnly:    true,
			CapDrop:     hardenedCapDrop,
			SecurityOpt: hardenedSecurityOpt,
			Volumes: []mount{
				{Type: "bind", Source: options.IndexDir, Target: IndexMount, ReadOnly: true},
			},
			Healthcheck: newHealthcheck([]string{"CMD", "wget", "-qO-", fmt.Sprintf("http://127.0.0.1:%d/index.html", IndexContainerPort)}),
			Labels:      indexLabels,
		},
	}}
}
