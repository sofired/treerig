package router_test

// Go-only, per #55: no TypeScript `it` to mirror — ConnectNetwork and
// DisconnectNetwork are new in the Go build. Exercised end to end (a real
// daemon) by router_integration_test.go's
// TestRouterReconnectsLabelledWorktreeNetworks; these are the unit-level
// cases for the tolerance regexes themselves, found by review to have no
// coverage of their own — the integration test only ever sees a real
// daemon's success and "nothing to disconnect" answers, never a genuine
// failure or the other spelling of either tolerated refusal.

import (
	"context"
	"strings"
	"testing"
)

// networkCalls is every `network connect`/`disconnect` call so far, as raw argv.
func (w *testWorld) networkCalls(verb string) [][]string {
	var result [][]string
	for _, c := range w.Fake.snapshotCalls() {
		if len(c.args) > 1 && c.args[0] == "network" && c.args[1] == verb {
			result = append(result, c.args)
		}
	}
	return result
}

func TestRouterServiceConnectNetwork(t *testing.T) {
	t.Run("connects the router to network", func(t *testing.T) {
		w := newWorld(t, 80, "")
		if err := w.Router.ConnectNetwork(context.Background(), "some-network"); err != nil {
			t.Fatal(err)
		}
		calls := w.networkCalls("connect")
		if len(calls) != 1 || calls[0][2] != "some-network" || calls[0][3] != "router-1" {
			t.Fatalf("connect calls = %v", calls)
		}
	})
	t.Run("tolerates an already-attached pair: 'already exists in network'", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.mu.Lock()
		w.Fake.connectExitCode, w.Fake.connectStderr = 1,
			"Error response from daemon: endpoint with name berth-router already exists in network some-network"
		w.Fake.mu.Unlock()
		if err := w.Router.ConnectNetwork(context.Background(), "some-network"); err != nil {
			t.Fatalf("err = %v, want the already-attached refusal tolerated", err)
		}
	})
	t.Run("tolerates an already-attached pair: 'is already connected to network'", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.mu.Lock()
		w.Fake.connectExitCode, w.Fake.connectStderr = 1,
			"Error response from daemon: container already-attached is already connected to network some-network"
		w.Fake.mu.Unlock()
		if err := w.Router.ConnectNetwork(context.Background(), "some-network"); err != nil {
			t.Fatalf("err = %v, want the already-attached refusal tolerated", err)
		}
	})
	t.Run("fails on a genuine connect refusal", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.mu.Lock()
		w.Fake.connectExitCode, w.Fake.connectStderr = 1, "Error response from daemon: network some-network not found"
		w.Fake.mu.Unlock()
		err := w.Router.ConnectNetwork(context.Background(), "some-network")
		if err == nil || !strings.Contains(err.Error(), "some-network not found") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("does nothing when the router has no container", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.mu.Lock()
		w.Fake.ids = map[string]string{}
		w.Fake.mu.Unlock()
		if err := w.Router.ConnectNetwork(context.Background(), "some-network"); err != nil {
			t.Fatal(err)
		}
		if calls := w.networkCalls("connect"); len(calls) != 0 {
			t.Fatalf("expected no connect call when the router is not running, got %v", calls)
		}
	})
}

// Found in review, round 2 of #55: a labelled network Ensure's own
// reconnectWorktreeNetworks lists can already be gone by the time the
// matching connect call actually runs — a concurrent `down` or `worktree
// remove` tearing it down in between, since RoutedByLabel persists on the
// network until its removal actually finishes. ConnectNetwork itself must
// still fail loudly on "network not found" ("fails on a genuine connect
// refusal" above pins that), since up.go's own direct call for a
// worktree's newly created network has no such excuse; only the reconnect
// loop, reached here through Ensure, tolerates it.
func TestReconnectWorktreeNetworksTolerantOfAConcurrentTeardown(t *testing.T) {
	t.Run("does not fail Ensure when a labelled network is gone by the time it tries to reconnect it", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.mu.Lock()
		w.Fake.networkLs = "gone-worktree-network"
		w.Fake.connectExitCode, w.Fake.connectStderr = 1, "Error response from daemon: network gone-worktree-network not found"
		w.Fake.mu.Unlock()

		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatalf("err = %v, want a gone labelled network tolerated during reconnect", err)
		}
	})
}

func TestRouterServiceDisconnectNetwork(t *testing.T) {
	t.Run("disconnects the router from network", func(t *testing.T) {
		w := newWorld(t, 80, "")
		if err := w.Router.DisconnectNetwork(context.Background(), "some-network"); err != nil {
			t.Fatal(err)
		}
		calls := w.networkCalls("disconnect")
		if len(calls) != 1 || calls[0][2] != "some-network" || calls[0][3] != "router-1" {
			t.Fatalf("disconnect calls = %v", calls)
		}
	})
	t.Run("tolerates nothing to disconnect: 'is not connected to network'", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.mu.Lock()
		w.Fake.disconnectExitCode, w.Fake.disconnectStderr = 1,
			"Error response from daemon: container router-1 is not connected to network some-network"
		w.Fake.mu.Unlock()
		if err := w.Router.DisconnectNetwork(context.Background(), "some-network"); err != nil {
			t.Fatalf("err = %v, want tolerated", err)
		}
	})
	t.Run("tolerates nothing to disconnect: 'is not connected to any network'", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.mu.Lock()
		w.Fake.disconnectExitCode, w.Fake.disconnectStderr = 1,
			"Error response from daemon: container router-1 is not connected to any network"
		w.Fake.mu.Unlock()
		if err := w.Router.DisconnectNetwork(context.Background(), "some-network"); err != nil {
			t.Fatalf("err = %v, want tolerated", err)
		}
	})
	t.Run("tolerates nothing to disconnect: 'no such network'", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.mu.Lock()
		w.Fake.disconnectExitCode, w.Fake.disconnectStderr = 1, "Error response from daemon: no such network: some-network"
		w.Fake.mu.Unlock()
		if err := w.Router.DisconnectNetwork(context.Background(), "some-network"); err != nil {
			t.Fatalf("err = %v, want tolerated", err)
		}
	})
	t.Run("tolerates nothing to disconnect: 'no such container'", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.mu.Lock()
		w.Fake.disconnectExitCode, w.Fake.disconnectStderr = 1, "Error response from daemon: no such container: router-1"
		w.Fake.mu.Unlock()
		if err := w.Router.DisconnectNetwork(context.Background(), "some-network"); err != nil {
			t.Fatalf("err = %v, want tolerated", err)
		}
	})
	t.Run("fails on a genuine disconnect refusal", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.mu.Lock()
		w.Fake.disconnectExitCode, w.Fake.disconnectStderr = 1, "Error response from daemon: permission denied"
		w.Fake.mu.Unlock()
		err := w.Router.DisconnectNetwork(context.Background(), "some-network")
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("does nothing when the router has no container", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.mu.Lock()
		w.Fake.ids = map[string]string{}
		w.Fake.mu.Unlock()
		if err := w.Router.DisconnectNetwork(context.Background(), "some-network"); err != nil {
			t.Fatal(err)
		}
		if calls := w.networkCalls("disconnect"); len(calls) != 0 {
			t.Fatalf("expected no disconnect call when the router is not running, got %v", calls)
		}
	})
}
