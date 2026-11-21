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
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/text"
)

// Probe answers whether a loopback port is free to publish on. The allocator
// takes one so a test decides which ports are busy without binding any;
// LoopbackProbe is the real one.
type Probe interface {
	// Free reports whether nothing is bound to or listening on
	// 127.0.0.1:port. A cancelled context is never "free". When the port is
	// not free, why says what the probe saw, as a phrase that completes
	// "held by another process: ..." ("binding it failed (address already
	// in use)"); it is empty when the port is free.
	Free(ctx context.Context, port int) (free bool, why string)
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
func (LoopbackProbe) Free(ctx context.Context, port int) (bool, string) {
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	var listen net.ListenConfig
	listener, err := listen.Listen(ctx, "tcp4", address)
	if err != nil {
		return false, bindReason(ctx, err)
	}
	// A listener that will not close still proved the port was free.
	_ = listener.Close()
	// A refusal, or any other failure to connect, after a bind that worked:
	// free.
	return dialVerdict(ctx, address, false)
}

// DialProbe is the Probe for a port treerig does not allocate but must find
// free before it starts something on it (the router's own, which may be
// below 1024): it only dials 127.0.0.1:port, because a bind to a privileged
// port fails for an unprivileged user whether or not anything holds it. A
// refused connection (ECONNREFUSED) proves the port free; a connection that
// succeeds, times out or fails any other way (a filtered port, an
// unreachable network) proves nothing and counts as busy.
type DialProbe struct{}

// Free is Probe's Free.
func (DialProbe) Free(ctx context.Context, port int) (bool, string) {
	return dialVerdict(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), true)
}

// dialVerdict dials address and judges the port by the answer, for both
// probes. A refusal proves it free; a connection proves it held, and the
// probe closes it at once without sending anything. Any other failure
// (a timeout, a filtered port) proves nothing: it counts as busy when
// strict, which DialProbe is because it has no bind to fall back on, and as
// free otherwise, because LoopbackProbe's bind has already succeeded.
func dialVerdict(ctx context.Context, address string, strict bool) (bool, string) {
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp4", address)
	switch {
	case err == nil:
		_ = conn.Close()
		return false, acceptedReason
	case ctx.Err() != nil:
		return false, cancelledReason
	case errors.Is(err, syscall.ECONNREFUSED) || !strict:
		return true, ""
	}
	return false, dialReason(err)
}

// What a probe says when it judges a port busy. Each completes "held by
// another process: ...", except cancelledReason, which says nothing about a
// holder: callers return the context's error and never print it.
const (
	acceptedReason  = "connecting to it succeeded"
	cancelledReason = "the check was cancelled before it could tell"
)

// bindReason says why binding 127.0.0.1:port failed, from the error's
// operating-system code where it has one. The codes are the same on Linux
// and macOS: only the wording of an error without one varies, and that is
// quoted.
func bindReason(ctx context.Context, err error) string {
	switch {
	case ctx.Err() != nil:
		return cancelledReason
	case errors.Is(err, syscall.EADDRINUSE):
		return "binding it failed (address already in use)"
	case errors.Is(err, syscall.EACCES):
		return "binding it failed (permission denied)"
	}
	return "binding it failed (" + quoteError(err) + ")"
}

// dialReason says why dialling 127.0.0.1:port neither connected nor was
// refused, so the port counts as busy.
func dialReason(err error) string {
	var netErr net.Error
	if errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return "connecting to it timed out"
	}
	return "connecting to it failed (" + quoteError(err) + ")"
}

// quoteError is err for a message: the operating system's own text when
// the error carries an error number, which drops the "dial tcp4
// 127.0.0.1:1: connect:" a reader already knows, else the whole error.
func quoteError(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return text.OneLine(errno.Error(), 100)
	}
	return text.OneLine(err.Error(), 100)
}

// DescribeProcess is how a message names a holder that is neither a container
// nor a registration: "another process: <why>; find it with: <hint>", where
// why is what the probe saw (Probe's Free) and the hint is LookupHint's. An
// empty why gives "another process (<hint>)".
func DescribeProcess(goos string, port int, why string) string {
	if why == "" {
		return "another process (" + LookupHint(goos, port) + ")"
	}
	return "another process: " + why + "; " + LookupHint(goos, port)
}

// LookupHint is how to find the process holding port, for the operating
// system goos names (runtime.GOOS, passed in from cmd/treerig's Env): `ss` on
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
		if free, _ := probe.Free(ctx, port); free {
			return port, nil
		}
	}
	return 0, ErrExhausted
}

// ListenerProbe says whether something accepts connections on a loopback
// address. A host route's target (#6) is a process treerig did not start, so
// `route add` and `doctor` ask it which loopback addresses the process
// listens on; a test supplies its own answers and binds nothing.
type ListenerProbe interface {
	// Answers reports whether a TCP connection to host:port succeeds, where
	// host is "127.0.0.1" or "::1". A cancelled context never answers.
	Answers(ctx context.Context, host string, port int) bool
}

// DialListener is the ListenerProbe that dials. It connects to whatever
// answers and closes at once without sending anything.
type DialListener struct{}

// Answers is ListenerProbe's Answers.
func (DialListener) Answers(ctx context.Context, host string, port int) bool {
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ListenerState is which loopback addresses a port answers on.
type ListenerState struct {
	// IPv4 is true when 127.0.0.1 answers.
	IPv4 bool
	// IPv6 is true when ::1 answers.
	IPv6 bool
}

// Listening asks probe about both loopback addresses of port.
func Listening(ctx context.Context, probe ListenerProbe, port int) ListenerState {
	return ListenerState{
		IPv4: probe.Answers(ctx, "127.0.0.1", port),
		IPv6: probe.Answers(ctx, "::1", port),
	}
}

// IPv6Only reports whether the port answers on ::1 and not on 127.0.0.1: the
// case a host route cannot use through a proxy that reaches the host over
// IPv4 alone.
func (s ListenerState) IPv6Only() bool { return s.IPv6 && !s.IPv4 }

// Silent reports whether the port answers on neither loopback address.
func (s ListenerState) Silent() bool { return !s.IPv4 && !s.IPv6 }
