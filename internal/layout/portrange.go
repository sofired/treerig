package layout

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sofired/berth/internal/clierr"
)

// PortRange is an inclusive range of TCP ports berth allocates loopback ports
// from (#119). Its bounds are ports a process may bind without privilege.
type PortRange struct {
	Low, High int
}

// DefaultPortRange is where a routed service's loopback port comes from when
// settings.toml sets no [ports] range. It sits below Linux's ephemeral range
// (from 32768) and macOS's (from 49152), so the OS does not hand these
// ports out on its own.
var DefaultPortRange = PortRange{Low: 20000, High: 29999}

// minPortRangeLow keeps the range clear of the privileged and well-known ports.
const minPortRangeLow = 1024

// Size is how many ports the range holds.
func (r PortRange) Size() int { return r.High - r.Low + 1 }

// Contains reports whether port is inside the range.
func (r PortRange) Contains(port int) bool { return port >= r.Low && port <= r.High }

// String is the range as settings.toml spells it: "<low>-<high>".
func (r PortRange) String() string { return fmt.Sprintf("%d-%d", r.Low, r.High) }

// ParsePortRange reads "<low>-<high>": both whole numbers, low no greater
// than high, low at least 1024 and high at most 65535.
func ParsePortRange(value string) (PortRange, error) {
	low, high, found := strings.Cut(value, "-")
	lowPort, lowErr := strconv.Atoi(low)
	highPort, highErr := strconv.Atoi(high)
	if !found || lowErr != nil || highErr != nil || strings.HasPrefix(low, "+") || strings.HasPrefix(high, "+") {
		return PortRange{}, clierr.Newf("%q is not a port range; write it as <low>-<high>, for example 20000-29999.", value)
	}
	if lowPort < minPortRangeLow || highPort > 65_535 || lowPort > highPort {
		return PortRange{}, clierr.Newf(
			"%q is not a usable port range; it needs %d <= low <= high <= 65535.", value, minPortRangeLow)
	}
	return PortRange{Low: lowPort, High: highPort}, nil
}
