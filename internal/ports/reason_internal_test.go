package ports

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
)

// timeoutError is a net.Error that reports a timeout.
type timeoutError struct{}

// Error is error's Error.
func (timeoutError) Error() string { return "i/o timeout" }

// Timeout is net.Error's Timeout.
func (timeoutError) Timeout() bool { return true }

// Temporary is net.Error's Temporary.
func (timeoutError) Temporary() bool { return false }

func TestDescribeProcess(t *testing.T) {
	t.Run("says what the probe saw, then how to find the process", func(t *testing.T) {
		got := DescribeProcess("linux", 30111, "binding it failed (address already in use)")
		want := "another process: binding it failed (address already in use); find it with: ss -ltnp 'sport = :30111'"
		if got != want {
			t.Fatalf("DescribeProcess = %q, want %q", got, want)
		}
	})

	t.Run("keeps the parenthesised hint when the probe gave no reason", func(t *testing.T) {
		got := DescribeProcess("darwin", 8080, "")
		want := "another process (find it with: lsof -nP -iTCP:8080 -sTCP:LISTEN)"
		if got != want {
			t.Fatalf("DescribeProcess = %q, want %q", got, want)
		}
	})
}

func TestDialVerdict(t *testing.T) {
	ctx := context.Background()
	var listen net.ListenConfig
	listener, err := listen.Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback: %v", err)
	}
	address := listener.Addr().String()

	t.Run("calls a port that accepts the connection held, in both modes: the path LoopbackProbe takes after a bind that worked", func(t *testing.T) {
		for _, strict := range []bool{false, true} {
			if free, why := dialVerdict(ctx, address, strict); free || why != acceptedReason {
				t.Errorf("strict=%v: dialVerdict = %v, %q", strict, free, why)
			}
		}
	})

	t.Run("calls a refused connection free in both modes", func(t *testing.T) {
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		for _, strict := range []bool{false, true} {
			if free, why := dialVerdict(ctx, address, strict); !free || why != "" {
				t.Errorf("strict=%v: dialVerdict = %v, %q", strict, free, why)
			}
		}
	})

	t.Run("says the check was cancelled", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if free, why := dialVerdict(cancelled, address, true); free || why != cancelledReason {
			t.Fatalf("dialVerdict = %v, %q", free, why)
		}
	})
}

func TestReasons(t *testing.T) {
	op := func(err error) error {
		return &net.OpError{Op: "listen", Net: "tcp4", Err: os.NewSyscallError("bind", err)}
	}

	t.Run("names a bind that failed because the address is in use", func(t *testing.T) {
		got := bindReason(context.Background(), op(syscall.EADDRINUSE))
		if want := "binding it failed (address already in use)"; got != want {
			t.Fatalf("bindReason = %q, want %q", got, want)
		}
	})

	t.Run("names a bind that failed for lack of permission", func(t *testing.T) {
		got := bindReason(context.Background(), op(syscall.EACCES))
		if want := "binding it failed (permission denied)"; got != want {
			t.Fatalf("bindReason = %q, want %q", got, want)
		}
	})

	t.Run("quotes any other bind error by its operating-system text", func(t *testing.T) {
		got := bindReason(context.Background(), op(syscall.EADDRNOTAVAIL))
		if want := "binding it failed (" + syscall.EADDRNOTAVAIL.Error() + ")"; got != want {
			t.Fatalf("bindReason = %q, want %q", got, want)
		}
	})

	t.Run("quotes an error without an error number whole, on one line and clamped", func(t *testing.T) {
		got := bindReason(context.Background(), errors.New("odd\nfailure "+strings.Repeat("x", 300)))
		if strings.ContainsAny(got, "\n\r") || len(got) > 200 || !strings.HasPrefix(got, "binding it failed (odd failure ") {
			t.Fatalf("bindReason = %q", got)
		}
	})

	t.Run("says the check was cancelled instead of blaming the bind", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if got := bindReason(ctx, op(syscall.EADDRINUSE)); got != cancelledReason {
			t.Fatalf("bindReason = %q, want %q", got, cancelledReason)
		}
	})

	t.Run("names a dial that timed out", func(t *testing.T) {
		for name, err := range map[string]error{
			"a net timeout": &net.OpError{Op: "dial", Net: "tcp4", Err: timeoutError{}},
			"a deadline":    fmt.Errorf("dial: %w", os.ErrDeadlineExceeded),
		} {
			if got, want := dialReason(err), "connecting to it timed out"; got != want {
				t.Errorf("%s: dialReason = %q, want %q", name, got, want)
			}
		}
	})

	t.Run("quotes a dial that failed any other way", func(t *testing.T) {
		got := dialReason(op(syscall.ENETUNREACH))
		if want := "connecting to it failed (" + syscall.ENETUNREACH.Error() + ")"; got != want {
			t.Fatalf("dialReason = %q, want %q", got, want)
		}
	})
}
