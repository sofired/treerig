package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/router"
)

// TestRunningPort is Router.RunningPort (#79): the loopback port the running
// router serves, read off its container's label so it works in both modes.
func TestRunningPort(t *testing.T) {
	t.Run("reports the port a running host-mode router serves, though it publishes nothing", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		port, running, err := w.Router.RunningPort(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !running || port != 8080 {
			t.Fatalf("got port %d, running %v; want 8080, true", port, running)
		}
	})

	t.Run("reports the port of a bridge-mode router the same way", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		w.setHealthy(router.ModeBridge)
		port, running, err := w.Router.RunningPort(context.Background())
		if err != nil || !running || port != 8080 {
			t.Fatalf("got port %d, running %v, err %v; want 8080, true", port, running, err)
		}
	})

	t.Run("reports an unknown port for a container that carries no port label", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		doc := routerDoc(router.ModeHost, 8080)
		delete(doc.Config.Labels, router.RouterPortLabel)
		w.Fake.setDoc("router-1", doc)
		port, running, err := w.Router.RunningPort(context.Background())
		if err != nil || !running || port != 0 {
			t.Fatalf("got port %d, running %v, err %v; want 0, true", port, running, err)
		}
	})

	t.Run("reports nothing running when the router has no container", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		w.Fake.setIDs(map[string]string{})
		port, running, err := w.Router.RunningPort(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if running || port != 0 {
			t.Fatalf("got port %d, running %v; want 0, false", port, running)
		}
	})
}

// TestRunning is Router.Running: the mode and port labels of the container.
func TestRunning(t *testing.T) {
	t.Run("reads the mode and port the container was started with", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		w.setHealthy(router.ModeBridge)
		running, err := w.Router.Running(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if want := (router.Running{Present: true, Up: true, Mode: router.ModeBridge, Port: 8080}); running != want {
			t.Fatalf("Running = %+v, want %+v", running, want)
		}
	})

	t.Run("calls a stopped container present but not up", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		doc := routerDoc(router.ModeHost, 8080)
		doc.State.Status = "exited"
		w.Fake.setDoc("router-1", doc)
		running, err := w.Router.Running(context.Background())
		if err != nil || !running.Present || running.Up {
			t.Fatalf("Running = %+v, %v", running, err)
		}
	})

	t.Run("counts a container it cannot inspect as running with an unknown mode", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		w.Fake.deleteDoc("router-1")
		running, err := w.Router.Running(context.Background())
		if err != nil || !running.Present || !running.Up || running.Mode != "" || running.Port != 0 {
			t.Fatalf("Running = %+v, %v", running, err)
		}
	})
}

// TestEnsureNotesAPortChange is #79's notice: a router recreated on a new
// port says so once, since URLs already open still name the old one.
func TestEnsureNotesAPortChange(t *testing.T) {
	const moved = "The shared router moved from port 80 to 8080; URLs already open, the index page included, still use the old port."

	t.Run("says the router moved when a running router served another port", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		w.Fake.setDoc("router-1", routerDoc(router.ModeHost, 80))

		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		lines, _ := w.Logger.Snapshot()
		if !hasLine(lines, "info: "+moved) {
			t.Fatalf("expected the move to be announced, got %v", lines)
		}
		if count := strings.Count(strings.Join(lines, "\n"), "moved from port"); count != 1 {
			t.Fatalf("the move was announced %d times", count)
		}
	})

	t.Run("says nothing about ports when the router was not running before", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		stopped(t, w, router.ModeHost)
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		lines, _ := w.Logger.Snapshot()
		for _, line := range lines {
			if strings.Contains(line, "moved from port") {
				t.Fatalf("unexpected port notice: %v", lines)
			}
		}
	})

	t.Run("says nothing about ports when a stopped router carried another port", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		doc := routerDoc(router.ModeHost, 80)
		doc.State.Status = "exited"
		w.Fake.setDoc("router-1", doc)
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		lines, _ := w.Logger.Snapshot()
		for _, line := range lines {
			if strings.Contains(line, "moved from port") {
				t.Fatalf("unexpected port notice for a router that served nothing: %v", lines)
			}
		}
	})

	t.Run("says nothing about ports when the router already served this one", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		lines, _ := w.Logger.Snapshot()
		for _, line := range lines {
			if strings.Contains(line, "moved from port") {
				t.Fatalf("unexpected port notice: %v", lines)
			}
		}
	})
}
