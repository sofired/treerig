package router

// The proxy's network mode (docs/design/routing.md, decision 1). The daemon
// decides what "localhost" means to a container, not the operating system
// the CLI runs on, so the mode is read from `docker info` and the endpoint
// the CLI talks to. DetectMode is a pure function over those facts; the
// table in mode_test.go pins every rule with real field values.

import (
	"encoding/json"
	"net/url"
	"slices"
	"strings"

	"github.com/sofired/treerig/internal/clierr"
)

// Mode is how the proxy container is attached to the network.
type Mode string

const (
	// ModeHost shares the host's network namespace: every hop to a routed
	// target is a loopback hop, so the host firewall is never involved. Only
	// a native, rootful Linux engine can run it.
	ModeHost Mode = "host"
	// ModeBridge runs the proxy on a bridge network with its port published
	// on 127.0.0.1; it reaches loopback targets through the engine's
	// host-gateway forwarding, which Docker Desktop and OrbStack provide.
	ModeBridge Mode = "bridge"
)

// Engine is what mode detection needs to know about the Docker daemon and
// the client talking to it.
type Engine struct {
	// OperatingSystem is `docker info`'s OperatingSystem ("Ubuntu 24.04.4
	// LTS", "Docker Desktop", "OrbStack").
	OperatingSystem string
	// Name is `docker info`'s Name: the daemon's host name ("docker-desktop",
	// "orbstack").
	Name string
	// OSType is `docker info`'s OSType ("linux", "windows").
	OSType string
	// SecurityOptions is `docker info`'s SecurityOptions; a rootless daemon
	// lists "name=rootless".
	SecurityOptions []string
	// Endpoint is the Docker host the client talks to: DOCKER_HOST, else the
	// current context's docker endpoint ("unix:///var/run/docker.sock").
	Endpoint string
	// ClientOS is runtime.GOOS of the client, passed in by the entry point.
	ClientOS string
}

// ParseEngineInfo reads the fields mode detection needs from the output of
// `docker info --format '{{json .}}'`. Endpoint and ClientOS are not part
// of it; the caller fills them in.
func ParseEngineInfo(document string) (Engine, error) {
	var info struct {
		OperatingSystem string
		Name            string
		OSType          string
		SecurityOptions []string
	}
	if err := json.Unmarshal([]byte(document), &info); err != nil {
		return Engine{}, clierr.Newf("Could not read `docker info`: %s", err)
	}
	return Engine{
		OperatingSystem: info.OperatingSystem,
		Name:            info.Name,
		OSType:          info.OSType,
		SecurityOptions: info.SecurityOptions,
	}, nil
}

// isLocalEndpoint reports whether a Docker host names a daemon on this
// machine: a unix socket, a Windows named pipe, or TCP on a loopback
// address. Anything else (ssh, TCP to another host, an unparseable value)
// is remote.
func isLocalEndpoint(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	switch parsed.Scheme {
	case "unix", "npipe":
		return true
	case "tcp":
		host := parsed.Hostname()
		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	}
	return false
}

// isDesktopOrOrbStack reports whether the daemon runs in the VM of Docker
// Desktop or OrbStack, which `docker info` names in OperatingSystem and Name.
func isDesktopOrOrbStack(engine Engine) bool {
	for _, value := range []string{engine.OperatingSystem, engine.Name} {
		switch strings.ToLower(value) {
		case "docker desktop", "docker-desktop", "orbstack":
			return true
		}
	}
	return false
}

// vmEngineNames are the lower-cased `docker info` Names (or Name prefixes
// followed by a dash, as in lima-rancher-desktop) of the
// Linux-VM engines treerig does not recognise: Colima, Lima and Rancher
// Desktop. Their host network is the VM's, whichever OS the client runs on.
// A Linux client reaching a VM engine through a forwarded socket or an SSH
// tunnel under another name cannot be told apart from a native engine and
// gets host mode.
var vmEngineNames = []string{"colima", "lima", "rancher-desktop"}

// isVMEngine reports whether the daemon's Name says it runs in a Colima,
// Lima or Rancher Desktop VM.
func isVMEngine(engine Engine) bool {
	name := strings.ToLower(engine.Name)
	for _, vm := range vmEngineNames {
		if name == vm || strings.HasPrefix(name, vm+"-") {
			return true
		}
	}
	return false
}

// DetectMode is the mode the proxy runs in on engine. override is
// settings.toml's [router] mode ("host", "bridge" or "" to detect it).
// settingsFile names that file in the advice a refusal prints.
//
// The rules, in order:
//  1. An endpoint that is not on this machine is refused: the proxy could not
//     reach this machine's loopback.
//  2. A daemon whose OSType is not linux is refused.
//  3. A rootless daemon is refused (#121): its "host" network is
//     RootlessKit's namespace, not the machine's.
//  4. An override then decides. It replaces only the "unrecognized" refusal
//     of rule 7: it never lifts rules 1 to 3.
//  5. Docker Desktop and OrbStack, named by OperatingSystem or Name, are
//     bridge: their engine runs in a VM, and host-gateway forwards to the
//     host's loopback.
//  6. A Name equal to, or beginning with a dash after, colima, lima or
//     rancher-desktop (a Linux
//     VM engine) is refused as unrecognized, on any client.
//  7. Any other Linux engine under a Linux client is host. Anything else (a
//     Linux engine reached from macOS: a VM whose host network is the VM's)
//     is refused as unrecognized. The override is the way through.
func DetectMode(engine Engine, override, settingsFile string) (Mode, error) {
	if !isLocalEndpoint(engine.Endpoint) {
		return "", clierr.Newf(
			"treerig routes through a proxy that reaches your services on this machine's loopback, so it needs a Docker daemon on this machine, "+
				"but Docker is at %s. Use a local Docker socket (unset DOCKER_HOST or switch `docker context`). "+
				"An ssh:// or tcp:// host is remote.", engine.Endpoint)
	}
	if engine.OSType != "linux" {
		return "", clierr.Newf("Docker reports OSType %q; treerig needs a Linux engine. Switch Docker to Linux containers.", engine.OSType)
	}
	if slices.Contains(engine.SecurityOptions, "name=rootless") {
		return "", clierr.New(
			"Rootless Docker is not supported yet (#121): its host network is RootlessKit's, not this machine's, so the proxy could not reach your services. " +
				"Use a rootful Docker Engine, Docker Desktop or OrbStack.")
	}
	switch override {
	case string(ModeHost):
		return ModeHost, nil
	case string(ModeBridge):
		return ModeBridge, nil
	}
	if isDesktopOrOrbStack(engine) {
		return ModeBridge, nil
	}
	if !isVMEngine(engine) && engine.ClientOS == "linux" {
		return ModeHost, nil
	}
	bridge := "\"bridge\" if it forwards host.docker.internal to this machine (Docker Desktop, OrbStack, and Colima, Lima or Rancher Desktop reached from macOS; untested)"
	host := "\"host\" only for a Linux engine on this machine's own kernel"
	options := bridge + ", " + host
	if engine.ClientOS == "linux" {
		options = host + ", " + bridge
	}
	return "", clierr.Newf(
		"Could not tell how the proxy should reach this machine's loopback: Docker is %q (host %q) and treerig is running on %s. "+
			"Set [router] mode in %s: %s.",
		engine.OperatingSystem, engine.Name, engine.ClientOS, settingsFile, options)
}
