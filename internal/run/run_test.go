package run_test

import (
	"testing"

	"github.com/sofired/treerig/internal/run"
)

// A failure of Git names what was asked, not the `-c` settings that hardened
// the run, which would push Git's own reason out of a message cut short.
func TestFailureLeavesOutGitsLeadingConfig(t *testing.T) {
	spec := run.Spec{Command: "git", Args: []string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-C", "/work", "rev-parse"}}
	err := run.Failure(spec, run.Result{ExitCode: 128, Stderr: "fatal: no\n"})
	if want := "Command failed (git -C /work rev-parse): fatal: no"; err.Error() != want {
		t.Fatalf("got %q, want %q", err.Error(), want)
	}
	other := run.Spec{Command: "docker", Args: []string{"-c", "x", "ps"}}
	if got := run.Failure(other, run.Result{ExitCode: 1}).Error(); got != "Command failed (docker -c x ps): exit 1" {
		t.Fatalf("got %q", got)
	}
}
