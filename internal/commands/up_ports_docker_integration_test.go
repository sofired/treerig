//go:build integration

package commands_test

// Pinned loopback ports (#119) on a real daemon: each routed service is
// published on its own 127.0.0.1 port, the port survives a restart, a port
// another program holds makes `up` refuse and --reallocate-port moves it, and
// `worktree remove` releases the worktree's ports. Every port comes from the
// test range (the walk fixture writes it to the scratch treerig root), never the
// default one a developer's own worktrees use.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
)

// loopbackGet is a GET straight to 127.0.0.1:port, bypassing the router.
func loopbackGet(port int) (int, string) {
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		return 0, err.Error()
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
}

// eventuallyOnLoopback waits for 127.0.0.1:port to answer with want.
func eventuallyOnLoopback(t *testing.T, port int, want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, body := loopbackGet(port)
		if status == http.StatusOK && body == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("127.0.0.1:%d answered %d %q, want %q", port, status, body, want)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// recordedPorts are the ports the registry holds for a worktree.
func (w *walk) recordedPorts(t *testing.T, result *commands.UpResult) map[string]int {
	t.Helper()
	record, err := registry.NewStore(w.controlHome, result.Registration.RepositoryID).Find(result.Registration.WorktreeID)
	if err != nil || record == nil {
		t.Fatalf("Find: %v, %v", record, err)
	}
	return record.Ports
}

func TestLoopbackPortsOnARealDaemon(t *testing.T) {
	w := newWalk(t)
	const host = "feature-walk.walk.localhost"

	result := w.up(t)
	port := w.recordedPorts(t, result)["api"]
	if !registrytest.PortRange.Contains(port) {
		t.Fatalf("api port %d is outside the test range %v", port, registrytest.PortRange)
	}
	eventuallyOnLoopback(t, port, "hello from api\n")
	// Published on loopback alone, not on every address.
	published, err := exec.Command("docker", "ps", "--filter", "publish="+strconv.Itoa(port), "--format", "{{.Ports}}").Output()
	if err != nil || !strings.Contains(string(published), "127.0.0.1:"+strconv.Itoa(port)+"->8080/tcp") || strings.Contains(string(published), "0.0.0.0") {
		t.Fatalf("docker publishes %q (%v)", published, err)
	}
	// The hostname through the router still works, and the URL says nothing of the port.
	w.eventually(t, host, func(status int, body string) bool { return status == http.StatusOK && body == "hello from api\n" })
	for _, entry := range result.URLs {
		if strings.Contains(entry.URL, strconv.Itoa(port)) {
			t.Fatalf("the URL %s names the loopback port", entry.URL)
		}
	}

	t.Run("the port is unchanged after the container restarts, and after down and up", func(t *testing.T) {
		containers := w.owned(t, result.Registration.WorktreeID, "ps", "-q")
		if len(containers) != 1 {
			t.Fatalf("containers = %v", containers)
		}
		if output, err := exec.Command("docker", "restart", containers[0]).CombinedOutput(); err != nil {
			t.Fatalf("docker restart: %v\n%s", err, output)
		}
		eventuallyOnLoopback(t, port, "hello from api\n")

		if err := commands.DownWithContext(context.Background(), w.context(t)); err != nil {
			t.Fatalf("down: %v", err)
		}
		again := w.up(t)
		if got := w.recordedPorts(t, again)["api"]; got != port {
			t.Fatalf("port after down and up = %d, was %d", got, port)
		}
		eventuallyOnLoopback(t, port, "hello from api\n")
	})

	t.Run("a second up leaves the override and the container alone", func(t *testing.T) {
		before := w.owned(t, result.Registration.WorktreeID, "ps", "-q")
		override := filepath.Join(layout.WorktreeLayout(w.controlHome, result.Registration.RepositoryID, result.Registration.WorktreeID).Home, "treerig.override.yaml")
		first, err := os.ReadFile(override)
		if err != nil {
			t.Fatal(err)
		}
		w.up(t)
		second, err := os.ReadFile(override)
		if err != nil || string(first) != string(second) {
			t.Fatalf("override changed (%v):\n%s\n---\n%s", err, first, second)
		}
		if after := w.owned(t, result.Registration.WorktreeID, "ps", "-q"); strings.Join(after, " ") != strings.Join(before, " ") {
			t.Fatalf("containers were recreated: %v then %v", before, after)
		}
	})

	t.Run("refuses a port another program took, names it, and --reallocate-port moves it", func(t *testing.T) {
		if err := commands.DownWithContext(context.Background(), w.context(t)); err != nil {
			t.Fatalf("down: %v", err)
		}
		squatter, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			t.Fatalf("taking %d: %v", port, err)
		}
		defer squatter.Close()

		cctx := w.context(t)
		_, err = commands.UpWithContext(context.Background(), cctx, commands.SharedRouter(w.env, w.svc, w.state),
			commands.BestEffortIndex(cctx), commands.UpOptions{})
		if err == nil || !strings.Contains(err.Error(), "127.0.0.1:"+strconv.Itoa(port)+" is held by another process") {
			t.Fatalf("up over a taken port: %v", err)
		}
		if running := w.owned(t, result.Registration.WorktreeID, "ps", "-q"); len(running) != 0 {
			t.Fatalf("the refused up started %v", running)
		}

		cctx = w.context(t)
		moved, err := commands.UpWithContext(context.Background(), cctx, commands.SharedRouter(w.env, w.svc, w.state),
			commands.BestEffortIndex(cctx), commands.UpOptions{ReallocatePorts: []string{"api"}})
		if err != nil {
			t.Fatalf("up --reallocate-port: %v", err)
		}
		next := w.recordedPorts(t, moved)["api"]
		if next == port || !registrytest.PortRange.Contains(next) {
			t.Fatalf("api moved to %d from %d", next, port)
		}
		eventuallyOnLoopback(t, next, "hello from api\n")

		// Its own container now holds the new port: moving it again is refused.
		cctx = w.context(t)
		_, err = commands.UpWithContext(context.Background(), cctx, commands.SharedRouter(w.env, w.svc, w.state),
			commands.BestEffortIndex(cctx), commands.UpOptions{ReallocatePorts: []string{"api"}})
		if err == nil || !strings.Contains(err.Error(), "held by this worktree's own container") {
			t.Fatalf("reallocating from under a running container: %v", err)
		}
		port = next
	})

	w.downAndRemove(t, result, host)
	if _, err := registry.NewStore(w.controlHome, result.Registration.RepositoryID).Find(result.Registration.WorktreeID); err != nil {
		t.Fatal(err)
	}
	if status, _ := loopbackGet(port); status != 0 {
		t.Fatalf("127.0.0.1:%d still answers after worktree remove", port)
	}
}

func TestTwoWorktreesOfOneStackOnARealDaemon(t *testing.T) {
	first := newWalk(t)
	one := first.up(t)

	// A second checkout of the same repository, with the same files.
	second := filepath.Join(first.base, "checkout2")
	gitIn(t, first.checkout, "worktree", "add", "-b", "feature/two", second)
	for _, name := range []string{"treerig.toml", "compose.yaml"} {
		data, err := os.ReadFile(filepath.Join(first.checkout, name))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(second, name), string(data), 0o644)
	}
	if err := os.MkdirAll(filepath.Join(second, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(second, "hooks", "record"), "#!/bin/sh\n", 0o755)
	env := *first.env
	env.Cwd = second
	cctx, err := commands.Context(&env)
	if err != nil {
		t.Fatal(err)
	}
	two, err := commands.UpWithContext(context.Background(), cctx, commands.SharedRouter(&env, first.svc, first.state),
		commands.BestEffortIndex(cctx), commands.UpOptions{})
	if err != nil {
		t.Fatalf("second worktree's up: %v", err)
	}

	onePort, twoPort := first.recordedPorts(t, one)["api"], first.recordedPorts(t, two)["api"]
	if onePort == 0 || twoPort == 0 || onePort == twoPort {
		t.Fatalf("ports = %d and %d", onePort, twoPort)
	}
	eventuallyOnLoopback(t, onePort, "hello from api\n")
	eventuallyOnLoopback(t, twoPort, "hello from api\n")

	// Nothing in the registry claims a port twice.
	scan, err := registry.ScanRegistrations(first.controlHome)
	if err != nil || len(registry.PortConflicts(scan.Records)) != 0 {
		t.Fatalf("scan = %+v, %v", scan, err)
	}

	// Removing one releases only its ports.
	cctx2, err := commands.Context(&env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commands.RemoveWithContext(context.Background(), cctx2, &env, commands.RemoveOptions{Yes: true}); err != nil {
		t.Fatalf("remove second: %v", err)
	}
	if status, _ := loopbackGet(twoPort); status != 0 {
		t.Fatalf("127.0.0.1:%d still answers after its worktree was removed", twoPort)
	}
	eventuallyOnLoopback(t, onePort, "hello from api\n")
	first.downAndRemove(t, one, "feature-walk.walk.localhost")
}
