package router_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/router"
)

func TestReadState(t *testing.T) {
	t.Run("reads nothing from a router that never started", func(t *testing.T) {
		state := newState(registrytest.TempDir(t))
		got, found, err := router.ReadState(state)
		if err != nil || found || got != (router.State{}) {
			t.Fatalf("ReadState = %+v, %v, %v", got, found, err)
		}
	})

	t.Run("reads the ports a start stored", func(t *testing.T) {
		w := newWorld(t, 80, "")
		got, found, err := router.ReadState(w.State)
		if err != nil || !found {
			t.Fatalf("ReadState = %+v, %v, %v", got, found, err)
		}
		if want := (router.State{Version: 1, IndexPort: seedIndexPort, PingPort: seedPingPort}); got != want {
			t.Fatalf("state = %+v, want %+v", got, want)
		}
	})

	t.Run("refuses a file that is not JSON, or of another version, telling the user to remove it", func(t *testing.T) {
		for name, contents := range map[string]string{"garbage": "{nope", "version 0": `{"version":0,"indexPort":30500}`, "empty": ""} {
			w := newWorld(t, 80, "")
			if err := os.WriteFile(w.State.StateFile, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, err := router.ReadState(w.State)
			if err == nil || !strings.Contains(err.Error(), "Remove it and run this again") {
				t.Errorf("%s: got %v", name, err)
			}
		}
	})

	t.Run("refuses a symlinked state file rather than following it out of the state directory", func(t *testing.T) {
		w := newWorld(t, 80, "")
		outside := filepath.Join(registrytest.TempDir(t), "elsewhere.json")
		if err := os.WriteFile(outside, []byte(`{"version":1,"indexPort":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(w.State.StateFile); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, w.State.StateFile); err != nil {
			t.Fatal(err)
		}
		if _, _, err := router.ReadState(w.State); err == nil {
			t.Fatal("expected a symlinked state file to be refused")
		}
	})

	t.Run("refuses a state file that is a directory", func(t *testing.T) {
		w := newWorld(t, 80, "")
		if err := os.Remove(w.State.StateFile); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(w.State.StateFile, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := router.ReadState(w.State); err == nil || !strings.Contains(err.Error(), "state.json") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestStateReserved(t *testing.T) {
	t.Run("lists the index and ping ports a worktree allocation must skip", func(t *testing.T) {
		got := router.State{IndexPort: 30500, PingPort: 30501}.Reserved()
		if !slices.Equal(got, []int{30500, 30501}) {
			t.Fatalf("Reserved = %v", got)
		}
	})

	t.Run("leaves out a port that was never allocated", func(t *testing.T) {
		if got := (router.State{IndexPort: 30500}).Reserved(); !slices.Equal(got, []int{30500}) {
			t.Fatalf("Reserved = %v", got)
		}
		if got := (router.State{}).Reserved(); len(got) != 0 {
			t.Fatalf("Reserved = %v", got)
		}
	})
}
