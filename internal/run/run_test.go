package run_test

import (
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

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

func TestFailureQuotesTheEndOfTheOutput(t *testing.T) {
	spec := run.Spec{Command: "docker", Args: []string{"compose", "up"}, Timeout: 2 * time.Second}
	var lines []string
	for i := 1; i <= 80; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	output := strings.Join(lines, "\n")

	for _, tc := range []struct {
		name   string
		result run.Result
		reason string
	}{
		{"an exit", run.Result{Stderr: output, ExitCode: 3}, ""},
		{"a timeout", run.Result{Stderr: output, ExitCode: 1, TimedOut: true}, "timed out after 2000 ms"},
		{"a signal", run.Result{Stderr: output, ExitCode: 137, Signal: syscall.SIGKILL}, "terminated by SIGKILL"},
		{"an overflow", run.Result{Stderr: output, ExitCode: 1, OutputOverflow: true}, "produced more than"},
	} {
		t.Run(tc.name+" quotes the last 50 lines, not the whole transcript", func(t *testing.T) {
			message := run.Failure(spec, tc.result).Error()
			if !strings.Contains(message, tc.reason) || !strings.Contains(message, "line 80") || !strings.Contains(message, "line 31") || strings.Contains(message, "line 30\n") || strings.Contains(message, "line 1\n") {
				t.Fatalf("message = %q", message)
			}
			if !strings.Contains(message, "(30 earlier lines left out)") {
				t.Errorf("message does not say what was left out: %q", message)
			}
		})
	}

	t.Run("a short output is quoted whole, from stdout when stderr is blank", func(t *testing.T) {
		message := run.Failure(spec, run.Result{Stdout: "only this\n", Stderr: " \n", ExitCode: 2, TimedOut: true}).Error()
		if !strings.Contains(message, "timed out after 2000 ms\nLast output:\nonly this") {
			t.Fatalf("message = %q", message)
		}
	})

	t.Run("an exit with no output says the code", func(t *testing.T) {
		if message := run.Failure(spec, run.Result{ExitCode: 7}).Error(); !strings.HasSuffix(message, ": exit 7") {
			t.Fatalf("message = %q", message)
		}
	})
}

func TestFailureTailIsBoundedInBytes(t *testing.T) {
	spec := run.Spec{Command: "dd", Args: []string{"if=/dev/zero"}}
	t.Run("one enormous line is cut", func(t *testing.T) {
		message := run.Failure(spec, run.Result{Stderr: strings.Repeat("x", 50<<20), ExitCode: 1, OutputOverflow: true}).Error()
		if len(message) > 2000 {
			t.Fatalf("message is %d bytes", len(message))
		}
	})
	t.Run("many long lines keep the end within 8 KiB", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < 50; i++ {
			fmt.Fprintf(&b, "%03d %s\n", i, strings.Repeat("y", 400))
		}
		message := run.Failure(spec, run.Result{Stderr: b.String(), ExitCode: 1}).Error()
		if len(message) > 9*1024 || !strings.Contains(message, "049 yyy") || !strings.Contains(message, "earlier lines left out") {
			t.Fatalf("message is %d bytes: %.200q", len(message), message)
		}
	})
}
