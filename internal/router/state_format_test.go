package router_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/router"
)

// TestRouterStateFormat covers the version in router/state.json
// (docs/design/state.md): written by Ensure, a newer one refused by every
// reader with the upgrade instruction and left alone, a missing or unknown one
// unreadable like any other garbage.
func TestRouterStateFormat(t *testing.T) {
	t.Run("Ensure writes version 1", func(t *testing.T) {
		w := newWorldOpts(t, "", 80, nil)
		if err := os.Remove(w.State.StateFile); err != nil {
			t.Fatal(err)
		}
		stopped(t, w, router.ModeHost)
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(w.State.StateFile)
		if err != nil || !strings.HasPrefix(string(raw), "{\n  \"version\": 1,\n") {
			t.Fatalf("state = %q, %v", raw, err)
		}
	})

	t.Run("refuses a state file a newer berth wrote, naming the file and both versions, and leaves it alone", func(t *testing.T) {
		const newer = `{"version":2,"ports":{"index":30500},"indexPort":"not read by this build"}`
		w := newWorld(t, 80, "")
		if err := os.WriteFile(w.State.StateFile, []byte(newer), 0o600); err != nil {
			t.Fatal(err)
		}
		want := "Refusing to use the router state " + w.State.StateFile +
			": it is format 2, written by a newer berth, and this build reads format 1. Upgrade berth, then run this again."

		if _, _, err := router.ReadState(w.State); err == nil || err.Error() != want {
			t.Fatalf("ReadState: got %v\nwant %s", err, want)
		}
		stopped(t, w, router.ModeHost)
		if err := w.Router.Ensure(context.Background()); err == nil || err.Error() != want {
			t.Fatalf("Ensure: got %v", err)
		}
		if _, err := w.Router.CheckPorts(context.Background()); err == nil || err.Error() != want {
			t.Fatalf("CheckPorts: got %v", err)
		}
		if data, _ := os.ReadFile(w.State.StateFile); string(data) != newer {
			t.Fatalf("state rewritten to %q", data)
		}
	})

	t.Run("reads a file with no version as unreadable, not as a newer one", func(t *testing.T) {
		w := newWorld(t, 80, "")
		if err := os.WriteFile(w.State.StateFile, []byte(`{"indexPort":30500}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := router.ReadState(w.State)
		if err == nil || !strings.Contains(err.Error(), "is not readable. Remove it and run this again") || strings.Contains(err.Error(), "newer") {
			t.Fatalf("got %v", err)
		}
	})
}
