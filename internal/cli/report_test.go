package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/logger"
)

// report prints a clierr.Wrap's message, then its cause on its own "caused
// by" line, so a cause the message does not repeat reaches the person once.
// The error here stands in for the shape internal/commands's
// wrapPartialClearFailure returns; that function's own output, and that its
// message leaves the cause out, is pinned by TestWrapPartialClearFailure.
func TestReportDoesNotDuplicateTheCause(t *testing.T) {
	cause := errors.New("could not mark the operation lock's record released: disk full")
	err := clierr.Wrap("Already cleared the development lock before this failed. Run this again to clear the rest.", cause)

	var stdout, stderr bytes.Buffer
	console := logger.NewConsole("treerig", &stdout, &stderr)
	report(console, func(string) (string, bool) { return "", false }, err)

	got := stderr.String()
	count := strings.Count(got, cause.Error())
	if count != 1 {
		t.Fatalf("stderr = %q, the cause's own text appears %d times, want exactly once", got, count)
	}
	if !strings.Contains(got, "Already cleared the development lock before this failed. Run this again to clear the rest.") {
		t.Fatalf("stderr = %q, want the wrapping message too", got)
	}
	if !strings.Contains(got, "caused by: "+cause.Error()) {
		t.Fatalf("stderr = %q, want the cause on its own \"caused by\" line", got)
	}
}

// TestReportPrintsALockTimeoutOnce pins that a lock held past the wait budget
// is reported as one line: its type is for callers that may skip a busy lock,
// not a second link in the error chain.
func TestReportPrintsALockTimeoutOnce(t *testing.T) {
	scope := lock.Scope{Target: filepath.Join(t.TempDir(), "busy"), Description: "worktree busy"}
	holder := lock.New(scope, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
	if _, err := holder.Acquire(context.Background(), "treerig up"); err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	delay := time.Hour
	waiter := lock.New(scope, lock.Options{Logger: logger.Silent{}, Watchdog: -1, Timeout: 60 * time.Millisecond, Retry: 20 * time.Millisecond, NoticeDelay: &delay})
	_, err := waiter.Acquire(context.Background(), "treerig down")
	var busy *lock.BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("err = %T %v, want a *lock.BusyError", err, err)
	}

	var stdout, stderr bytes.Buffer
	report(logger.NewConsole("treerig", &stdout, &stderr), func(string) (string, bool) { return "", false }, err)
	got := stderr.String()
	if n := strings.Count(got, "Another process holds the worktree busy lock"); n != 1 {
		t.Fatalf("the refusal appears %d times, want once:\n%s", n, got)
	}
	if strings.Contains(got, "caused by") || strings.Contains(got, "also:") {
		t.Fatalf("a lock timeout has no cause:\n%s", got)
	}
	var refusal *clierr.Error
	if !errors.As(err, &refusal) || refusal.Message != err.Error() {
		t.Fatalf("the refusal is not reachable as a *clierr.Error")
	}
}

// TestReportPrintsWhatAWrappedErrorSuppressed pins that a clierr.Error inside
// fmt.Errorf's wrapper still has its suppressed errors reported.
func TestReportPrintsWhatAWrappedErrorSuppressed(t *testing.T) {
	inner := clierr.New("the hook failed")
	inner.Suppressed = []error{errors.New("could not release the lock")}
	err := fmt.Errorf("while running: %w", inner)

	var stdout, stderr bytes.Buffer
	report(logger.NewConsole("treerig", &stdout, &stderr), func(string) (string, bool) { return "", false }, err)
	got := stderr.String()
	if !strings.Contains(got, "also: could not release the lock") {
		t.Fatalf("the suppressed error behind a wrapper was dropped:\n%s", got)
	}
}

// TestReportPrintsAnEndedSessionAsANotice pins that a normal end carrying an
// exit status (a `dev` session the user stopped) is information, not an
// error, and that one which hid a further failure is still an error.
func TestReportPrintsAnEndedSessionAsANotice(t *testing.T) {
	noEnv := func(string) (string, bool) { return "", false }

	var stdout, stderr bytes.Buffer
	report(logger.NewConsole("treerig", &stdout, &stderr), noEnv, clierr.Ended("The development session ended.", 130))
	if got := stderr.String(); strings.Contains(got, "ERROR") || !strings.Contains(got, "The development session ended.") {
		t.Fatalf("stderr = %q, want the message as a notice, not an ERROR", got)
	}

	stderr.Reset()
	ended := clierr.Ended("The development session ended.", 130)
	ended.Suppressed = []error{errors.New("could not release the lock")}
	report(logger.NewConsole("treerig", &stdout, &stderr), noEnv, ended)
	if got := stderr.String(); !strings.Contains(got, "ERROR") || !strings.Contains(got, "could not release the lock") {
		t.Fatalf("stderr = %q, want an error that also names the release failure", got)
	}

	stderr.Reset()
	caused := clierr.Ended("The development session ended.", 130)
	caused.Cause = errors.New("the lock was lost")
	report(logger.NewConsole("treerig", &stdout, &stderr), noEnv, caused)
	if got := stderr.String(); !strings.Contains(got, "ERROR") || !strings.Contains(got, "caused by: the lock was lost") {
		t.Fatalf("stderr = %q, want an error that names its cause", got)
	}
}
