// Package ports picks loopback ports (#119): a pure, lock-free allocator over
// a range, a probe for whether a port is free, and nothing else. It owns no
// state. internal/registry calls it under the registry lock, with the ports
// every registration already holds as taken; a later caller with other
// records (shared services, #123) passes its own taken set the same way.
package ports

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sofired/berth/internal/layout"
)

// Probe answers whether a loopback port is free to publish on. The allocator
// takes one so a test decides which ports are busy without binding any;
// LoopbackProbe is the real one.
type Probe interface {
	// Free reports whether nothing is bound to or listening on
	// 127.0.0.1:port. A cancelled context is never "free".
	Free(ctx context.Context, port int) bool
}

// dialTimeout bounds LoopbackProbe's connection attempt. A refused
// connection on loopback returns at once; the bound is for a firewall that
// drops instead of refusing.
const dialTimeout = 250 * time.Millisecond

// LoopbackProbe is the Probe that asks the operating system. It binds
// 127.0.0.1:port and, when that works, also dials it: on BSD and macOS a
// bind to 127.0.0.1 succeeds beside a listener on the wildcard address, and
// only the dial notices that something answers there. The dial connects to
// whatever answers, so on macOS and BSD it can briefly touch a foreign
// listener: it sends nothing and closes at once.
type LoopbackProbe struct{}

// Free is Probe's Free.
func (LoopbackProbe) Free(ctx context.Context, port int) bool {
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	var listen net.ListenConfig
	listener, err := listen.Listen(ctx, "tcp4", address)
	if err != nil {
		return false
	}
	// A listener that will not close still proved the port was free.
	_ = listener.Close()
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp4", address)
	if err != nil {
		// Refused means free; a cancelled context proves nothing.
		return ctx.Err() == nil
	}
	_ = conn.Close()
	return false
}

// DialProbe is the Probe for a port berth does not allocate but must find
// free before it starts something on it (the router's own, which may be
// below 1024): it only dials 127.0.0.1:port, because a bind to a privileged
// port fails for an unprivileged user whether or not anything holds it. A
// refused connection (ECONNREFUSED) proves the port free; a connection that
// succeeds, times out or fails any other way (a filtered port, an
// unreachable network) proves nothing and counts as busy.
type DialProbe struct{}

// Free is Probe's Free.
func (DialProbe) Free(ctx context.Context, port int) bool {
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return ctx.Err() == nil && errors.Is(err, syscall.ECONNREFUSED)
	}
	_ = conn.Close()
	return false
}

// LookupHint is how to find the process holding port, for the operating
// system goos names (runtime.GOOS, passed in from cmd/berth's Env): `ss` on
// Linux, `lsof` elsewhere. On Docker Desktop and OrbStack these name the VM
// backend rather than a container; `docker ps` is the way there.
func LookupHint(goos string, port int) string {
	if goos == "linux" {
		return "find it with: ss -ltnp 'sport = :" + strconv.Itoa(port) + "'"
	}
	return "find it with: lsof -nP -iTCP:" + strconv.Itoa(port) + " -sTCP:LISTEN"
}

// MaxBusyProbes caps how many candidate ports one Pick asks the probe about.
// Ports in the taken set cost nothing and are not counted; each probe binds
// and dials a socket, so a range that is mostly busy stops here instead of
// walking all of it.
const MaxBusyProbes = 1000

var (
	// ErrExhausted means every port in the range was taken or busy.
	ErrExhausted = errors.New("no free port in the range")
	// ErrProbeLimit means Pick stopped after MaxBusyProbes probes, having
	// found only busy ports.
	ErrProbeLimit = errors.New("gave up after probing too many busy ports")
)

// Start is where a probe begins in a range of size ports: a hash of keys, so
// the same keys land on the same port again after a lost record.
func Start(size int, keys ...string) int {
	sum := sha256.Sum256([]byte(strings.Join(keys, "\x00")))
	return int(binary.BigEndian.Uint64(sum[:8]) % uint64(size))
}

// Pick is the first port at or after Start(keys...) in r, wrapping, that is
// not in taken and that probe reports free. It returns ErrExhausted when no
// port qualifies, ErrProbeLimit when it gave up on busy ones, and the
// context's error when ctx ends. taken is not modified.
func Pick(ctx context.Context, r layout.PortRange, probe Probe, taken map[int]bool, keys ...string) (int, error) {
	size := r.Size()
	start := Start(size, keys...)
	probed := 0
	for offset := range size {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		port := r.Low + (start+offset)%size
		if taken[port] {
			continue
		}
		if probed == MaxBusyProbes {
			return 0, ErrProbeLimit
		}
		probed++
		if probe.Free(ctx, port) {
			return port, nil
		}
	}
	return 0, ErrExhausted
}
