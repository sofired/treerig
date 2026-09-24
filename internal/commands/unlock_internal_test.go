package commands

// Tests wrapPartialClearFailure directly, the way internal/router and
// internal/config test their own unexported pieces: given what a failed
// lock.Clear had already cleared before it failed (internal/lock's own
// clear_test.go, "reports what it already cleared when a later commit in
// the same batch fails", injects that failure for real, one directory
// per scope, and pins that lock.Clear itself preserves the partial result;
// this file pins what UnlockWithContext turns that into).

import (
	"errors"
	"strings"
	"testing"
)

func TestWrapPartialClearFailure(t *testing.T) {
	t.Run("returns the error unchanged when nothing was cleared", func(t *testing.T) {
		err := errors.New("could not mark the operation lock's record released: disk full")
		got := wrapPartialClearFailure(nil, err)
		if got != err {
			t.Fatalf("got %v, want the original error, unwrapped", got)
		}
	})

	t.Run("prefaces the error with what was already cleared, without repeating its own text", func(t *testing.T) {
		cleared := []ClearedLock{{Kind: "development", Description: "feature-login development session", Target: "/x/dev"}}
		err := errors.New("could not mark the operation lock's record released: disk full")
		got := wrapPartialClearFailure(cleared, err)
		want := "Already cleared the feature-login development session lock before this failed. Run this again to clear the rest."
		if got.Error() != want {
			t.Fatalf("got %q, want %q", got.Error(), want)
		}
		// err's own text belongs on report()'s "caused by" line (see
		// internal/cli's TestReportDoesNotDuplicateTheCause), not repeated
		// inside the message here.
		if strings.Contains(got.Error(), err.Error()) {
			t.Fatalf("got %q, want it not to repeat the cause's own text", got.Error())
		}
		if !errors.Is(got, err) {
			t.Fatalf("got %v, want it to still unwrap to the original error", got)
		}
	})

	t.Run("joins more than one already-cleared lock the way the success line does", func(t *testing.T) {
		cleared := []ClearedLock{
			{Kind: "development", Description: "feature-login development session"},
			{Kind: "operation", Description: "feature-login"},
		}
		err := errors.New("boom")
		got := wrapPartialClearFailure(cleared, err)
		want := "Already cleared the feature-login development session lock and the feature-login lock before this failed. Run this again to clear the rest."
		if got.Error() != want {
			t.Fatalf("got %q, want %q", got.Error(), want)
		}
		if !errors.Is(got, err) {
			t.Fatalf("got %v, want it to still unwrap to the original error", got)
		}
	})
}
