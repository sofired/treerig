//go:build integration

package backend

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/run/osrun"
	"github.com/sofired/treerig/internal/worktree"
	"github.com/sofired/treerig/internal/worktreestate"
)

// statesNamespace is per file, so this test's sweep can never touch another
// suite's resources.
const statesNamespace = "dev.treerig-test-states"

// TestServiceStatesAgreeOnARealDaemon starts services that exit 0, exit 7, run
// unhealthy, run plain and are stopped with `compose stop`, and asserts that
// the two ways treerig reads a container's state, `docker compose ps` for one
// worktree (Compose.Status) and `docker ps` across worktrees
// (worktreestate.Inventory), give the same service states, and that the stop
// (an exit by SIGKILL) is stopped, not a crash.
func TestServiceStatesAgreeOnARealDaemon(t *testing.T) {
	requireDocker(t)
	composeSweep(t, statesNamespace+".managed=true")

	repositoryID, worktreeID := composeUUID(t), composeUUID(t)
	subject := worktree.Ref{RepositoryID: repositoryID, WorktreeID: worktreeID, Slug: "feature-states"}
	project := layout.ComposeProjectName(repositoryID, worktreeID)
	dir := t.TempDir()
	composeFile := filepath.Join(dir, "compose.yaml")
	labels := fmt.Sprintf(`    labels:
      %[1]s.managed: "true"
      %[1]s.repository-uuid: %[2]s
      %[1]s.worktree-uuid: %[3]s
`, statesNamespace, repositoryID, worktreeID)
	// No init on the service that is stopped: busybox as PID 1 ignores SIGTERM,
	// so a stop with a short grace period ends it by SIGKILL, exit 137.
	contents := "services:\n" +
		"  finished:\n    image: busybox:1.37.0\n    command: [\"true\"]\n" + labels +
		"  crashed:\n    image: busybox:1.37.0\n    command: [\"sh\", \"-c\", \"exit 7\"]\n" + labels +
		"  sick:\n    image: busybox:1.37.0\n    init: true\n    command: [\"sleep\", \"600\"]\n" +
		"    healthcheck:\n      test: [\"CMD\", \"false\"]\n      interval: 1s\n      retries: 1\n" + labels +
		"  plain:\n    image: busybox:1.37.0\n    init: true\n    command: [\"sleep\", \"600\"]\n" + labels +
		"  halted:\n    image: busybox:1.37.0\n    command: [\"sleep\", \"600\"]\n" + labels
	if err := os.WriteFile(composeFile, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "compose", "-p", project, "-f", composeFile, "down", "--volumes", "--remove-orphans", "-t", "1").Run()
		composeSweep(t, statesNamespace+".managed=true", statesNamespace+".repository-uuid="+repositoryID)
	})
	docker := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("docker", append([]string{"compose", "-p", project, "-f", composeFile}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("docker compose %v: %v\n%s", args, err, out)
		}
	}
	docker("up", "-d")
	docker("stop", "-t", "1", "halted")

	controlHome := t.TempDir()
	if err := files.SecureWorktreeDirectories(layout.WorktreeLayout(controlHome, repositoryID, worktreeID), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	runtime := NewCompose(Options{
		ComposeFile: composeFile, ProjectRoot: dir, ConfigFile: filepath.Join(dir, "treerig.toml"),
		ProjectName: "treerig-test", Domain: "test.localhost", ControlHome: controlHome,
		LabelNamespace: statesNamespace, Runner: osrun.New(), Logger: logger.Silent{},
		WaitTimeoutSeconds: intPtr(60), Environ: os.Environ(),
	})
	ctx := context.Background()
	want := map[string]string{
		"finished": "exited (code 0)", "crashed": "exited (code 7)", "sick": "unhealthy", "plain": "running", "halted": "stopped",
	}

	// Compose's view, the one status and doctor use.
	fromCompose := func() map[string]string {
		statuses, err := runtime.Status(ctx, subject)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		got := map[string]string{}
		for _, status := range statuses {
			got[status.Service] = status.Text()
		}
		return got
	}
	// docker ps's view, the one worktree list and the overview use.
	fromDockerPs := func() map[string]string {
		inventory, err := worktreestate.New(worktreestate.Options{Runner: osrun.New()}).Inventory(ctx, []string{statesNamespace})
		if err != nil || !inventory.Answered {
			t.Fatalf("Inventory = %+v, %v", inventory, err)
		}
		got := map[string]string{}
		for _, container := range inventory.Containers {
			if container.WorktreeID == worktreeID {
				got[container.Service] = container.Status().Text()
			}
		}
		return got
	}

	// The health check takes a moment to fail, and the short-lived services a
	// moment to exit.
	deadline := time.Now().Add(60 * time.Second)
	var compose, ps map[string]string
	for {
		compose, ps = fromCompose(), fromDockerPs()
		if equalStates(compose, want) && equalStates(ps, want) || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if !equalStates(compose, want) {
		t.Errorf("docker compose ps says %s, want %s", describeStates(compose), describeStates(want))
	}
	if !equalStates(ps, want) {
		t.Errorf("docker ps says %s, want %s", describeStates(ps), describeStates(want))
	}
}

// equalStates reports whether two service → state maps are the same.
func equalStates(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for service, state := range a {
		if b[service] != state {
			return false
		}
	}
	return true
}

// describeStates is a service → state map as one line, in service order.
func describeStates(states map[string]string) string {
	names := make([]string, 0, len(states))
	for service := range states {
		names = append(names, service)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, service := range names {
		parts[i] = service + "=" + states[service]
	}
	return strings.Join(parts, ", ")
}
