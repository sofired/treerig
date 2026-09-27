package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/router"
)

// TestPublishedPort is Router.PublishedPort (#79): the host port a running
// router publishes for its own port 80, read off its container.
func TestPublishedPort(t *testing.T) {
	t.Run("reports the port a running router publishes", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		port, running, err := w.Router.PublishedPort(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !running || port != 8080 {
			t.Fatalf("got port %d, running %v; want 8080, true", port, running)
		}
	})

	t.Run("reports nothing running when the router has no container", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		w.Fake.mu.Lock()
		w.Fake.ids = map[string]string{}
		w.Fake.mu.Unlock()
		port, running, err := w.Router.PublishedPort(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if running || port != 0 {
			t.Fatalf("got port %d, running %v; want 0, false", port, running)
		}
	})
}

// TestEnsureNotesAPortChange is #79's notice: a router recreated on a new
// port says so once, since URLs already open still name the old one.
func TestEnsureNotesAPortChange(t *testing.T) {
	const moved = "The shared router moved from port 80 to 8080; URLs already open, the index page included, still use the old port."

	t.Run("says the router moved when a running router published another port", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		old := routerDoc(w.State.DynamicDir, w.Port)
		old.NetworkSettings.Ports = map[string][]router.InspectPortBinding{"80/tcp": {{HostIP: "127.0.0.1", HostPort: "80"}}}
		w.Fake.setDoc("router-1", old)
		w.Fake.mu.Lock()
		w.Fake.onRecreate = w.setHealthy // the recreate lands it on 8080
		w.Fake.mu.Unlock()

		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		lines, _ := w.Logger.Snapshot()
		if !hasLine(lines, "info: "+moved) {
			t.Fatalf("expected the move to be announced, got %v", lines)
		}
	})

	t.Run("says nothing about ports when the router was not running before", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		taken(t, w)
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

	t.Run("says nothing about ports when the router already published this one", func(t *testing.T) {
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

// TestStartFailureNamesTheSettingsFile is the start-failure message after
// #79: it names settings.toml beside BERTH_ROUTER_PORT, and, for a port
// below 1024, the caveat that a rootless Docker daemon could never publish it.
func TestStartFailureNamesTheSettingsFile(t *testing.T) {
	t.Run("names the settings file, and the rootless caveat, for a port below 1024 the daemon refused", func(t *testing.T) {
		w := newWorldOpts(t, "", 80, func(o *router.Options) { o.SettingsPath = "/berth/settings.toml" })
		taken(t, w)
		w.Fake.upExitCode = 1
		w.Fake.upStderr = "Error response from daemon: driver failed programming external connectivity: bind: permission denied"
		err := w.Router.Ensure(context.Background())
		if err == nil {
			t.Fatal("expected the start to fail")
		}
		for _, want := range []string{
			"Make sure 127.0.0.1:80 is free, or choose another port with BERTH_ROUTER_PORT or [router] port in /berth/settings.toml.",
			"If your Docker daemon runs rootless, it cannot publish a port below 1024 at all; pick one above 1023.",
			"bind: permission denied",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q lacks %q", err, want)
			}
		}
	})

	t.Run("recognises RootlessKit's own privileged-port refusal", func(t *testing.T) {
		w := newWorld(t, 80, "")
		taken(t, w)
		w.Fake.upExitCode = 1
		w.Fake.upStderr = "Error response from daemon: cannot expose privileged port 80, you might need to add \"net.ipv4.ip_unprivileged_port_start=0\" (currently 1024) to /etc/sysctl.conf"
		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "If your Docker daemon runs rootless, it cannot publish a port below 1024 at all; pick one above 1023.") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("leaves the rootless caveat out of a busy port below 1024: that is somebody else's server", func(t *testing.T) {
		w := newWorld(t, 80, "")
		taken(t, w)
		w.Fake.upExitCode = 1
		w.Fake.upStderr = "bind: address already in use"
		err := w.Router.Ensure(context.Background())
		if err == nil {
			t.Fatal("expected the start to fail")
		}
		if strings.Contains(err.Error(), "rootless") {
			t.Fatalf("error %q gives rootless advice for a busy port", err)
		}
		if !strings.Contains(err.Error(), "or [router] port in settings.toml.") {
			t.Fatalf("error %q does not name the settings file", err)
		}
	})

	t.Run("leaves the rootless caveat out for a port above 1023, and names the file generically without a path", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		taken(t, w)
		w.Fake.upExitCode = 1
		err := w.Router.Ensure(context.Background())
		if err == nil {
			t.Fatal("expected the start to fail")
		}
		if !strings.Contains(err.Error(), "or [router] port in settings.toml.") {
			t.Fatalf("error %q does not name the settings file", err)
		}
		if strings.Contains(err.Error(), "below 1024") {
			t.Fatalf("error %q mentions the rootless limit for a port above 1023", err)
		}
	})
}

// hasLine reports whether lines holds exactly want.
func hasLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}
