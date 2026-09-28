package osrun

import (
	"os/signal"
	"syscall"
	"testing"
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
