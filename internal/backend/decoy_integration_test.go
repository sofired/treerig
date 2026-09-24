//go:build integration

package backend

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/run/osrun"
	"github.com/sofired/berth/internal/worktree"
)

// Decoys on a real daemon: resources that carry some of a worktree's labels
// but cannot be proven its own. Removal must refuse every one before it
// touches anything, and must not even see one labelled under another namespace.

const decoyNamespace = "dev.berth-test-decoy"

func requireDocker(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("BERTH_TEST_REQUIRE_DOCKER") == "1" {
			t.Fatalf("docker is required (BERTH_TEST_REQUIRE_DOCKER=1) but unavailable: %v", err)
		}
		t.Skip("docker is not available")
	}
}

func uuid(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func labelArgs(labels map[string]string) []string {
	var args []string
	for name, value := range labels {
		args = append(args, "--label", name+"="+value)
	}
	return args
}

func dockerRun(t *testing.T, args ...string) string {
	t.Helper()
	output, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestDecoysOnARealDaemon(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	ref := worktree.Ref{RepositoryID: uuid(t), WorktreeID: uuid(t), Slug: "decoy"}
	project := layout.ComposeProjectName(ref.RepositoryID, ref.WorktreeID)
	ownership := func() map[string]string {
		return map[string]string{
			brand.ManagedLabel(decoyNamespace):    "true",
			brand.RepositoryLabel(decoyNamespace): ref.RepositoryID,
			brand.WorktreeLabel(decoyNamespace):   ref.WorktreeID,
			ComposeProjectLabel:                   project,
		}
	}
	runtime := NewCompose(Options{
		ComposeFile: "/nonexistent/compose.yaml", ProjectRoot: "/nonexistent", ConfigFile: "/nonexistent/berth.toml",
		ProjectName: "decoy", Domain: "decoy.localhost", ControlHome: t.TempDir(),
		LabelNamespace: decoyNamespace, Runner: osrun.New(), Logger: logger.Silent{}, Environ: os.Environ(),
	})
	volume := func(t *testing.T, labels map[string]string) string {
		name := "berth-decoy-" + uuid(t)[:8]
		dockerRun(t, append(append([]string{"volume", "create"}, labelArgs(labels)...), name)...)
		t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "--force", name).Run() })
		return name
	}
	container := func(t *testing.T, labels map[string]string) string {
		id := dockerRun(t, append(append([]string{"create"}, labelArgs(labels)...), "busybox:1.37.0", "true")...)
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "--force", id).Run() })
		return id
	}
	refuses := func(t *testing.T, id string) {
		t.Helper()
		_, err := runtime.Inventory(ctx, ref)
		if err == nil || !strings.Contains(err.Error(), "Refusing to touch") {
			t.Fatalf("inventory = %v, want a refusal", err)
		}
		if out, _ := exec.Command("docker", "inspect", id).CombinedOutput(); strings.Contains(string(out), "No such object") {
			t.Fatalf("%s was removed", id)
		}
	}
	if err := exec.Command("docker", "image", "inspect", "busybox:1.37.0").Run(); err != nil {
		dockerRun(t, "pull", "busybox:1.37.0")
	}

	t.Run("refuses a volume with the right owner but another project's label", func(t *testing.T) {
		labels := ownership()
		labels[ComposeProjectLabel] = "somebody-else"
		refuses(t, volume(t, labels))
	})
	t.Run("refuses a volume in this project whose owner is another worktree", func(t *testing.T) {
		labels := ownership()
		labels[brand.WorktreeLabel(decoyNamespace)] = uuid(t)
		refuses(t, volume(t, labels))
	})
	t.Run("refuses a container with no Compose project label: only images may lack one", func(t *testing.T) {
		labels := ownership()
		delete(labels, ComposeProjectLabel)
		refuses(t, container(t, labels))
	})
	t.Run("refuses a volume in this project that carries none of the ownership labels", func(t *testing.T) {
		refuses(t, volume(t, map[string]string{ComposeProjectLabel: project}))
	})
	t.Run("does not see a resource labelled under another namespace at all", func(t *testing.T) {
		labels := map[string]string{
			brand.ManagedLabel("dev.somebody"):    "true",
			brand.RepositoryLabel("dev.somebody"): ref.RepositoryID,
			brand.WorktreeLabel("dev.somebody"):   ref.WorktreeID,
		}
		volume(t, labels)
		owned, err := runtime.Inventory(ctx, ref)
		if err != nil || len(owned) != 0 {
			t.Fatalf("owned = %v, err = %v", owned, err)
		}
	})
	t.Run("inventories and reaps what is provably its own", func(t *testing.T) {
		mine := volume(t, ownership())
		owned, err := runtime.Inventory(ctx, ref)
		if err != nil || len(owned) != 1 || owned[0].ID != mine {
			t.Fatalf("owned = %v, err = %v", owned, err)
		}
		if err := runtime.Reap(ctx, ref, owned[0]); err != nil {
			t.Fatal(err)
		}
		if out, _ := exec.Command("docker", "volume", "inspect", mine).CombinedOutput(); !strings.Contains(string(out), "no such volume") {
			t.Fatalf("%s survived: %s", mine, out)
		}
	})
}
