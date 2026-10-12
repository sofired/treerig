package worktree

import (
	"strconv"
	"strings"

	"github.com/sofired/berth/internal/brand"
)

// SharedAddr is one pinned address of a shared service (docs/design/routing.md,
// decisions 19 and 20): the loopback host port its container port is
// published on, for a process on the host. Containers reach the service by
// name on their network and never use this.
type SharedAddr struct {
	// Name is the shared service's name.
	Name string
	// Port is the container port.
	Port int
	// Addr is host:port, "127.0.0.1:<host port>".
	Addr string
}

// SharedAddrPrefix starts every variable SharedAddrVariables builds, without
// BERTH_ itself (Variables adds it).
const SharedAddrPrefix = "SHARED_ADDR_"

// SharedAddrVariables returns name/value pairs for the repository's shared
// services, without the BERTH_ prefix. Variables adds that prefix to produce
// the environment a hook or a `berth run` command receives:
// BERTH_SHARED_ADDR_<NAME>_<PORT>=127.0.0.1:<host port> for every declared
// container port that has a pinned one. <NAME> maps as BERTH_SERVICE_URL_<NAME>
// does (upper-cased, each hyphen an underscore), and the trailing number
// keeps it unambiguous: a name has no underscore and a port is digits, so no
// two (name, port) pairs share a variable. Deliberately not injected into
// Compose, which would tie a worktree's Compose environment to shared state
// and recreate its containers when the state changes (#28).
func SharedAddrVariables(addrs []SharedAddr) [][2]string {
	pairs := make([][2]string, 0, len(addrs))
	for _, addr := range addrs {
		pairs = append(pairs, [2]string{SharedAddrPrefix + serviceEnvName(addr.Name) + "_" + strconv.Itoa(addr.Port), addr.Addr})
	}
	return pairs
}

// IsSharedAddrVariable reports whether name starts with BERTH_SHARED_ADDR_,
// so inherited shared addresses are dropped before this worktree's own are
// set. It does not validate the service name or port suffix.
func IsSharedAddrVariable(name string) bool {
	return strings.HasPrefix(name, brand.EnvPrefix+"_"+SharedAddrPrefix)
}
