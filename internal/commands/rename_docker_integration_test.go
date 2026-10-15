//go:build integration

package commands_test

// `worktree rename` on a real Docker daemon and a live test router (#11): the
// refusals a stopped container needs a daemon for, the route file a crash
// leaves behind, the data in a volume across the rename, and nothing of the
// old slug left after `worktree remove`.

import (
	"context"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/commands"
)

func TestRenameOnARealDaemon(t *testing.T) {
	w := newWalk(t)
	const oldHost = "feature-walk.walk.localhost"
	const newHost = "login.walk.localhost"
	ctx := context.Background()
	rename := func() (*commands.RenameResult, error) {
		return commands.RenameWithContext(ctx, w.context(t), commands.RenameOptions{Slug: "login"})
	}

	result := w.up(t)
	id := result.Registration.WorktreeID
	w.eventually(t, oldHost, func(status int, body string) bool { return status == http.StatusOK && body == "hello from api\n" })
	containers := w.owned(t, id, "ps", "-q")
	if len(containers) != 1 {
		t.Fatalf("containers = %v", containers)
	}
	if out, err := exec.Command("docker", "exec", containers[0], "sh", "-c", "echo keepme > /data/marker").CombinedOutput(); err != nil {
		t.Fatalf("writing the volume marker: %v\n%s", err, out)
	}
	ports := w.recordedPorts(t, result)

	// A running container is refused.
	if _, err := rename(); err == nil || !strings.Contains(err.Error(), "still has 1 container") {
		t.Fatalf("rename while running: %v", err)
	}
	// So is a stopped one: it still carries the old environment.
	if out, err := exec.Command("docker", "stop", containers[0]).CombinedOutput(); err != nil {
		t.Fatalf("docker stop: %v\n%s", err, out)
	}
	if _, err := rename(); err == nil || !strings.Contains(err.Error(), "still has 1 container") {
		t.Fatalf("rename while stopped: %v", err)
	}
	if len(w.owned(t, id, "ps", "-aq")) != 1 || len(w.routeFiles(t, id)) != 1 {
		t.Fatal("a refused rename changed something")
	}

	// A container removed by hand leaves its route file: a dead route.
	if out, err := exec.Command("docker", "rm", "--force", containers[0]).CombinedOutput(); err != nil {
		t.Fatalf("docker rm: %v\n%s", err, out)
	}
	if len(w.routeFiles(t, id)) != 1 {
		t.Fatal("expected the leftover route file")
	}
	if _, err := rename(); err != nil {
		t.Fatalf("rename: %v", err)
	}
	w.requireNoRouteFiles(t, id)
	w.eventually(t, oldHost, func(status int, _ string) bool { return status == http.StatusNotFound })
	w.eventually(t, newHost, func(status int, _ string) bool { return status == http.StatusNotFound })

	// `up` serves the new host, from the same volume and the same ports.
	w.up(t)
	w.eventually(t, newHost, func(status int, body string) bool { return status == http.StatusOK && body == "hello from api\n" })
	if status, _ := w.get(oldHost); status != http.StatusNotFound {
		t.Fatalf("the old host answers %d", status)
	}
	containers = w.owned(t, id, "ps", "-q")
	if len(containers) != 1 {
		t.Fatalf("containers after up = %v", containers)
	}
	if out, err := exec.Command("docker", "exec", containers[0], "cat", "/data/marker").CombinedOutput(); err != nil || string(out) != "keepme\n" {
		t.Fatalf("volume marker = %q, %v", out, err)
	}
	if after := w.recordedPorts(t, result); after["api"] != ports["api"] {
		t.Fatalf("ports = %v, were %v", after, ports)
	}

	// Nothing of the old slug is left once the worktree is removed.
	w.downAndRemove(t, result, newHost)
	found, _ := exec.Command("grep", "-rl", "feature-walk", w.base).CombinedOutput()
	for _, path := range strings.Fields(string(found)) {
		if !strings.Contains(path, "/.git/") {
			t.Errorf("the old slug survives in %s", path)
		}
	}
}
