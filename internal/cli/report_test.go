package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/logger"
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
	console := logger.NewConsole("berth", &stdout, &stderr)
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
