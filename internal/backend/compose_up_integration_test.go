//go:build integration

// Port of the "up" part of
// tests/integration/compose-runtime-docker.integration.test.ts against a
// real Docker daemon: the full up, status, down, inventory, reap round
// trip. The suite's other titles (a Compose file that goes missing before
// `down`, a nested project directory, sibling isolation, a project-labelled
// impostor) are left for a later work package — this one title is what
// proves Up itself, the new capability here, against a real daemon; the
// rest exercise Down/Inventory/Reap, already covered without Up. The
// endpoints() assertions are left out too: Endpoints is not part of this
// package yet (router territory, #39).
package backend

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/run/osrun"
	"github.com/sofired/berth/internal/worktree"
)

// composeNamespace is per file, so this suite's sweep can never touch
// another suite's resources, matching the TypeScript integration suite's own rule.
const composeNamespace = "dev.berth-test-compose"

func composeFixture(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../../tests/integration/fixtures/compose")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(abs, "compose.yaml")); err != nil {
		t.Skipf("compose fixture not found at %s: %v", abs, err)
	}
	return abs
}

func composeUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// composeListArgs are docker's own "list every id" verb per resource kind.
var composeListArgs = map[string][]string{
	"container": {"ps", "-aq"},
	"volume":    {"volume", "ls", "-q"},
	"network":   {"network", "ls", "-q"},
	"image":     {"image", "ls", "-q"},
}

// composeSweep removes every container, volume, network, and image carrying
// every one of labels, best effort.
func composeSweep(t *testing.T, labels ...string) {
	t.Helper()
	var filters []string
	for _, label := range labels {
		filters = append(filters, "--filter", "label="+label)
	}
	for _, kind := range []string{"container", "volume", "network", "image"} {
		args := append(append([]string{}, composeListArgs[kind]...), filters...)
		out, _ := exec.Command("docker", args...).CombinedOutput()
		for _, id := range strings.Fields(string(out)) {
			removeArgs := []string{kind, "rm"}
			if kind != "network" {
				removeArgs = append(removeArgs, "--force")
			}
			removeArgs = append(removeArgs, id)
			_ = exec.Command("docker", removeArgs...).Run()
		}
	}
}

func dockerExists(kind, name string) bool {
	return exec.Command("docker", kind, "inspect", name).Run() == nil
}

func dockerFormat(t *testing.T, format string, args ...string) string {
	t.Helper()
	full := append(append([]string{}, args...), "--format", format)
	out, err := exec.Command("docker", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", full, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestComposeRuntimeAgainstARealDaemon(t *testing.T) {
	requireDocker(t)
	fixture := composeFixture(t)
	composeFile := filepath.Join(fixture, "compose.yaml")
	composeSweep(t, composeNamespace+".managed=true")

	repositoryID, worktreeID := composeUUID(t), composeUUID(t)
	subject := worktree.Ref{RepositoryID: repositoryID, WorktreeID: worktreeID, Slug: "feature-subject"}
	project := layout.ComposeProjectName(subject.RepositoryID, subject.WorktreeID)
	controlHome := t.TempDir()
	t.Cleanup(func() {
		_ = exec.Command("docker", "compose", "-p", project, "down", "--volumes", "--rmi", "local", "--remove-orphans").Run()
		composeSweep(t, composeNamespace+".managed=true", composeNamespace+".repository-uuid="+repositoryID)
	})

	// The command layer creates the worktree home before `up`; the runtime refuses to.
	if err := files.SecureWorktreeDirectories(layout.WorktreeLayout(controlHome, subject.RepositoryID, subject.WorktreeID)); err != nil {
		t.Fatalf("SecureWorktreeDirectories: %v", err)
	}

	runtime := NewCompose(Options{
		ComposeFile: composeFile, ProjectRoot: fixture, ConfigFile: filepath.Join(fixture, "berth.toml"),
		ProjectName: "berth-test", Domain: "test.localhost", ControlHome: controlHome,
		LabelNamespace: composeNamespace, Runner: osrun.New(), Logger: logger.Silent{},
		RouterNetwork: "berth-router-unused", WaitTimeoutSeconds: 60, Environ: os.Environ(),
	})
	ctx := context.Background()

	if err := runtime.Up(ctx, subject, []string{"api", "worker", "builder"}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// Labels landed on the container, the volume, the default network, and
	// the built image — via the generated override.
	apiID := dockerFormat(t, "{{.ID}}", "compose", "-p", project, "ps", "-q", "api")
	if got := dockerFormat(t, fmt.Sprintf(`{{ index .Config.Labels "%s.managed" }} {{ index .Config.Labels "%s.worktree-uuid" }}`, composeNamespace, composeNamespace), "inspect", apiID); got != "true "+subject.WorktreeID {
		t.Fatalf("container labels = %q", got)
	}
	if got := dockerFormat(t, fmt.Sprintf(`{{ index .Labels "%s.worktree-uuid" }}`, composeNamespace), "volume", "inspect", project+"_data"); got != subject.WorktreeID {
		t.Fatalf("volume label = %q", got)
	}
	if got := dockerFormat(t, fmt.Sprintf(`{{ index .Labels "%s.worktree-uuid" }}`, composeNamespace), "network", "inspect", project+"_default"); got != subject.WorktreeID {
		t.Fatalf("network label = %q", got)
	}
	if got := dockerFormat(t, fmt.Sprintf(`{{ index .Config.Labels "%s.worktree-uuid" }}`, composeNamespace), "image", "inspect", project+"-builder"); got != subject.WorktreeID {
		t.Fatalf("image label = %q", got)
	}

	// The consumer's Compose file saw the BERTH_* environment.
	env := dockerFormat(t, "{{ range .Config.Env }}{{ println . }}{{ end }}", "inspect", apiID)
	for _, want := range []string{
		"SLUG=feature-subject",
		"HOST=feature-subject.test.localhost",
		"WORKTREE_HOME=" + layout.WorktreeLayout(controlHome, subject.RepositoryID, subject.WorktreeID).Home,
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("expected %q in container env:\n%s", want, env)
		}
	}

	statuses, err := runtime.Status(ctx, subject)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	for _, s := range statuses {
		if s.State != worktree.Running {
			t.Fatalf("expected every service running, got %+v", statuses)
		}
	}

	if err := runtime.Down(ctx, subject); err != nil {
		t.Fatalf("Down: %v", err)
	}
	statuses, err = runtime.Status(ctx, subject)
	if err != nil {
		t.Fatalf("Status after down: %v", err)
	}
	for _, s := range statuses {
		if s.State != worktree.Absent {
			t.Fatalf("expected every service absent after down, got %+v", statuses)
		}
	}
	// Down is reversible: the named volume and the built image are still there for reap to find.
	if !dockerExists("volume", project+"_data") {
		t.Fatal("expected the data volume to survive down")
	}
	if !dockerExists("image", project+"-builder") {
		t.Fatal("expected the built image to survive down")
	}

	owned, err := runtime.Inventory(ctx, subject)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	var kinds []string
	for _, resource := range owned {
		kinds = append(kinds, resource.Kind)
	}
	if want := []string{"volume", "image"}; !slices.Equal(kinds, want) {
		t.Fatalf("inventory kinds = %v, want %v", kinds, want)
	}
	for _, resource := range owned {
		if err := runtime.Reap(ctx, subject, resource); err != nil {
			t.Fatalf("Reap %+v: %v", resource, err)
		}
	}
	if dockerExists("volume", project+"_data") {
		t.Fatal("expected the data volume gone after reap")
	}
	if dockerExists("image", project+"-builder") {
		t.Fatal("expected the built image gone after reap")
	}
	remaining, err := runtime.Inventory(ctx, subject)
	if err != nil {
		t.Fatalf("Inventory after reap: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected nothing left, got %+v", remaining)
	}
}
