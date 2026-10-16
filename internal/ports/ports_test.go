package ports_test

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"

	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/ports"
	"github.com/sofired/treerig/internal/registry/registrytest"
)

var small = layout.PortRange{Low: 30100, High: 30104}

func TestPick(t *testing.T) {
	ctx := context.Background()
	free := registrytest.FreeProbe{}

	t.Run("is deterministic for the same keys and inside the range", func(t *testing.T) {
		first, err := ports.Pick(ctx, small, free, nil, "wt", "api")
		if err != nil || !small.Contains(first) {
			t.Fatalf("Pick = %d, %v", first, err)
		}
		if again, _ := ports.Pick(ctx, small, free, nil, "wt", "api"); again != first {
			t.Fatalf("Pick = %d then %d", first, again)
		}
		if first != small.Low+ports.Start(small.Size(), "wt", "api") {
			t.Fatalf("Pick = %d, not the hash start", first)
		}
	})

	t.Run("probes forward past taken and busy ports, wrapping", func(t *testing.T) {
		start := small.Low + ports.Start(small.Size(), "wt", "api")
		next := func(p int) int {
			if p == small.High {
				return small.Low
			}
			return p + 1
		}
		taken := map[int]bool{start: true}
		busy := registrytest.FreeProbe{Busy: map[int]bool{next(start): true}}
		got, err := ports.Pick(ctx, small, busy, taken, "wt", "api")
		if want := next(next(start)); err != nil || got != want {
			t.Fatalf("Pick = %d, %v; want %d", got, err, want)
		}
		if len(taken) != 1 {
			t.Fatal("Pick modified taken")
		}
	})

	t.Run("reports an exhausted range", func(t *testing.T) {
		taken := map[int]bool{30100: true, 30101: true, 30102: true, 30103: true}
		busy := registrytest.FreeProbe{Busy: map[int]bool{30104: true}}
		if _, err := ports.Pick(ctx, small, busy, taken, "k"); !errors.Is(err, ports.ErrExhausted) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("gives up after probing too many busy ports", func(t *testing.T) {
		wide := layout.PortRange{Low: 20000, High: 29999}
		busy := map[int]bool{}
		for p := wide.Low; p <= wide.High; p++ {
			busy[p] = true
		}
		if _, err := ports.Pick(ctx, wide, registrytest.FreeProbe{Busy: busy}, nil, "k"); !errors.Is(err, ports.ErrProbeLimit) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("stops when the context ends", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := ports.Pick(cancelled, small, free, nil, "k"); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestLoopbackProbe(t *testing.T) {
	ctx := context.Background()
	probe := ports.LoopbackProbe{}

	t.Run("reports a port with a listener busy, and free again once it closes", func(t *testing.T) {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Skipf("no loopback: %v", err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		if probe.Free(ctx, port) {
			t.Fatalf("port %d has a listener but was reported free", port)
		}
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		if !probe.Free(ctx, port) {
			t.Fatalf("port %d was closed but reported busy", port)
		}
	})

	t.Run("reports a wildcard listener busy", func(t *testing.T) {
		listener, err := net.Listen("tcp", ":0")
		if err != nil {
			t.Skipf("no wildcard listener: %v", err)
		}
		defer func() { _ = listener.Close() }()
		if port := listener.Addr().(*net.TCPAddr).Port; probe.Free(ctx, port) {
			t.Fatalf("port %d has a wildcard listener but was reported free", port)
		}
	})

	t.Run("does not call a port free once the context is cancelled", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if probe.Free(cancelled, 39999) {
			t.Fatal("a cancelled context reported a port free")
		}
	})
}

func TestDialProbe(t *testing.T) {
	ctx := context.Background()
	probe := ports.DialProbe{}

	t.Run("reports a port with a listener busy, and free again once it closes", func(t *testing.T) {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Skipf("no loopback: %v", err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		if probe.Free(ctx, port) {
			t.Fatalf("port %d has a listener but was reported free", port)
		}
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		if !probe.Free(ctx, port) {
			t.Fatalf("port %d was closed but reported busy", port)
		}
	})

	t.Run("reports a privileged port nothing listens on free, which a bind probe cannot for an unprivileged user", func(t *testing.T) {
		if conn, err := net.Dial("tcp4", "127.0.0.1:80"); err == nil {
			_ = conn.Close()
			t.Skip("something listens on 127.0.0.1:80 here")
		}
		if !probe.Free(ctx, 80) {
			t.Fatal("a free privileged port was reported busy")
		}
	})

	t.Run("does not call a port free once the context is cancelled", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if probe.Free(cancelled, 39999) {
			t.Fatal("a cancelled context reported a port free")
		}
	})
}

func TestLookupHint(t *testing.T) {
	t.Run("suggests ss on Linux", func(t *testing.T) {
		if got, want := ports.LookupHint("linux", 8080), "find it with: ss -ltnp 'sport = :8080'"; got != want {
			t.Fatalf("LookupHint = %q, want %q", got, want)
		}
	})

	t.Run("suggests lsof anywhere else", func(t *testing.T) {
		want := "find it with: lsof -nP -iTCP:8080 -sTCP:LISTEN"
		for _, goos := range []string{"darwin", "freebsd", ""} {
			if got := ports.LookupHint(goos, 8080); got != want {
				t.Errorf("LookupHint(%q) = %q, want %q", goos, got, want)
			}
		}
	})
}

// stubListener answers from a fixed table of "host:port" addresses.
type stubListener map[string]bool

// Answers is ListenerProbe's Answers.
func (s stubListener) Answers(_ context.Context, host string, port int) bool {
	return s[net.JoinHostPort(host, strconv.Itoa(port))]
}

func TestListening(t *testing.T) {
	cases := []struct {
		name   string
		answer stubListener
		want   ports.ListenerState
		v6Only bool
		silent bool
	}{
		{"both addresses", stubListener{"127.0.0.1:80": true, "[::1]:80": true}, ports.ListenerState{IPv4: true, IPv6: true}, false, false},
		{"IPv4 only", stubListener{"127.0.0.1:80": true}, ports.ListenerState{IPv4: true}, false, false},
		{"IPv6 only", stubListener{"[::1]:80": true}, ports.ListenerState{IPv6: true}, true, false},
		{"nothing", stubListener{}, ports.ListenerState{}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ports.Listening(context.Background(), c.answer, 80)
			if got != c.want || got.IPv6Only() != c.v6Only || got.Silent() != c.silent {
				t.Fatalf("Listening = %+v (IPv6Only %v, Silent %v)", got, got.IPv6Only(), got.Silent())
			}
		})
	}
}

func TestDialListener(t *testing.T) {
	t.Run("answers on a port something listens on, and not once it stops", func(t *testing.T) {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Skipf("cannot listen on loopback: %v", err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				_ = conn.Close()
			}
		}()
		if !(ports.DialListener{}).Answers(context.Background(), "127.0.0.1", port) {
			t.Fatal("a listening port did not answer")
		}
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		if (ports.DialListener{}).Answers(context.Background(), "127.0.0.1", port) {
			t.Fatal("a closed port answered")
		}
	})

	t.Run("a cancelled context never answers", func(t *testing.T) {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Skipf("cannot listen on loopback: %v", err)
		}
		defer func() { _ = listener.Close() }()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if (ports.DialListener{}).Answers(ctx, "127.0.0.1", listener.Addr().(*net.TCPAddr).Port) {
			t.Fatal("answered under a cancelled context")
		}
	})
}
