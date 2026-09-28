package osrun

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/sofired/berth/internal/clierr"
)

// TestSetForegroundLeavesAlreadyIgnoredSIGTTOUIgnored proves setForeground
// never un-ignores SIGTTOU on a process that had already chosen to ignore
// it (some parent shell's own doing) — it is never this call's place to
// change that. The target fd need not be valid: this only asserts on the
// signal's process-wide disposition, not on the ioctl's own outcome.
func TestSetForegroundLeavesAlreadyIgnoredSIGTTOUIgnored(t *testing.T) {
	signal.Ignore(syscall.SIGTTOU)
	defer signal.Reset(syscall.SIGTTOU)

	_ = setForeground(-1, 0)

	if !signal.Ignored(syscall.SIGTTOU) {
		t.Fatal("setForeground reset SIGTTOU that was already ignored before it was called")
	}
}

func TestWaitStopped(t *testing.T) {
	cases := []struct {
		name   string
		status unix.WaitStatus
		want   bool
	}{
		{"stopped by SIGSTOP", unix.WaitStatus(int(syscall.SIGSTOP)<<8 | 0x7f), true},
		{"stopped by SIGTSTP", unix.WaitStatus(int(syscall.SIGTSTP)<<8 | 0x7f), true},
		{"exited 0", unix.WaitStatus(0), false},
		{"exited 3", unix.WaitStatus(3 << 8), false},
		{"killed by SIGKILL", unix.WaitStatus(int(syscall.SIGKILL)), false},
		{"Linux's continued status", unix.WaitStatus(0xffff), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := waitStopped(c.status); got != c.want {
				t.Fatalf("waitStopped(%#x) = %v, want %v", uint32(c.status), got, c.want)
			}
		})
	}
}

func TestForwardSignal(t *testing.T) {
	interrupted := &clierr.Cancelled{Signal: syscall.SIGINT}
	cases := []struct {
		name        string
		cause       error
		interactive bool
		want        syscall.Signal
	}{
		{"an interactive child gets the signal berth received", interrupted, true, syscall.SIGINT},
		{"even when the cause is wrapped", fmt.Errorf("stopping: %w", interrupted), true, syscall.SIGINT},
		{"a hook keeps SIGTERM", interrupted, false, syscall.SIGTERM},
		{"a timeout sends SIGTERM", context.DeadlineExceeded, true, syscall.SIGTERM},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := forwardSignal(c.cause, c.interactive); got != c.want {
				t.Fatalf("forwardSignal = %v, want %v", got, c.want)
			}
		})
	}
}
