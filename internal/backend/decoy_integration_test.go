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
	"time"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/docker"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/run/osrun"
	"github.com/sofired/treerig/internal/worktree"
)

// Decoys on a real daemon: resources that carry some of a worktree's labels
// but cannot be proven its own. Removal must refuse every one before it
// touches anything, and must not even see one labelled under another namespace.

const decoyNamespace = "dev.treerig-test-decoy"

// requireDocker skips the test when no Docker daemon is reachable, unless
// TREERIG_TEST_REQUIRE_DOCKER=1 says the suite must fail loudly instead.
// TREERIG_TEST_SKIP_DOCKER=1 skips it even when a daemon is reachable, so CI
// can leave every Docker test to its own job.
func requireDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("TREERIG_TEST_SKIP_DOCKER") == "1" {
		t.Skip("Docker tests are skipped (TREERIG_TEST_SKIP_DOCKER=1)")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("TREERIG_TEST_REQUIRE_DOCKER") == "1" {
			t.Fatalf("docker is required (TREERIG_TEST_REQUIRE_DOCKER=1) but unavailable: %v", err)
		}
		t.Skip("docker is not available")
	}
}

// uuid generates a random RFC 4122 v4 UUID, so each run's decoy resources
// get IDs a sibling run cannot collide with.
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

// labelArgs turns a label map into repeated `--label name=value` flags for a
// docker command line.
func labelArgs(labels map[string]string) []string {
	var args []string
	for name, value := range labels {
		args = append(args, "--label", name+"="+value)
	}
	return args
}

// dockerRun runs `docker` with args against the real daemon and returns its
// trimmed combined output, failing the test if the command errors.
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
			brand.FormatLabel(decoyNamespace):     "1",
			ComposeProjectLabel:                   project,
		}
	}
	runtime := NewCompose(Options{
		ComposeFile: "/nonexistent/compose.yaml", ProjectRoot: "/nonexistent", ConfigFile: "/nonexistent/treerig.toml",
		ProjectName: "decoy", Domain: "decoy.localhost", ControlHome: t.TempDir(),
		LabelNamespace: decoyNamespace, Runner: osrun.New(), Logger: logger.Silent{}, Environ: os.Environ(),
	})
	volume := func(t *testing.T, labels map[string]string) string {
		name := "treerig-decoy-" + uuid(t)[:8]
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
	t.Run("refuses a container of a newer format, naming it and saying to upgrade", func(t *testing.T) {
		labels := ownership()
		labels[brand.FormatLabel(decoyNamespace)] = "2"
		id := container(t, labels)
		_, err := runtime.Inventory(ctx, ref)
		if err == nil || !strings.Contains(err.Error(), "Refusing to touch the container "+id[:12]+": it is format 2, written by a newer treerig") ||
			!strings.Contains(err.Error(), "Upgrade treerig") {
			t.Fatalf("inventory = %v", err)
		}
		if out, _ := exec.Command("docker", "inspect", id).CombinedOutput(); strings.Contains(string(out), "No such object") {
			t.Fatalf("%s was removed", id)
		}
	})
	t.Run("the pre-start listing finds a container of a newer format on a real daemon, and only that", func(t *testing.T) {
		labels := ownership()
		labels[brand.FormatLabel(decoyNamespace)] = "2"
		newer := container(t, labels)
		current := container(t, ownership())
		found, err := docker.FindNewerFormats(ctx, osrun.New(), []string{
			brand.ManagedLabel(decoyNamespace) + "=true", brand.WorktreeLabel(decoyNamespace) + "=" + ref.WorktreeID,
		}, brand.FormatLabel(decoyNamespace), 1, true, time.Minute)
		if err != nil || len(found) != 1 || found[0].Kind != "container" || found[0].Found != "2" || !strings.HasPrefix(newer, found[0].ID) {
			t.Fatalf("found = %+v, err = %v (newer %s, current %s)", found, err, newer, current)
		}
	})
	t.Run("the pre-start check finds an image of a newer format on a real daemon", func(t *testing.T) {
		labels := ownership()
		labels[brand.FormatLabel(decoyNamespace)] = "2"
		base := container(t, ownership())
		name := "treerig-decoy-image-" + uuid(t)[:8]
		args := []string{"commit"}
		for label, value := range labels {
			args = append(args, "--change", "LABEL "+label+"="+value)
		}
		dockerRun(t, append(args, base, name)...)
		t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "--force", name).Run() })
		found, err := docker.FindNewerFormats(ctx, osrun.New(), []string{
			brand.ManagedLabel(decoyNamespace) + "=true", brand.WorktreeLabel(decoyNamespace) + "=" + ref.WorktreeID,
		}, brand.FormatLabel(decoyNamespace), 1, true, time.Minute)
		if err != nil || len(found) != 1 || found[0].Kind != "image" || found[0].Found != "2" {
			t.Fatalf("found = %+v, err = %v", found, err)
		}
	})
	t.Run("proves a volume by the ownership labels alone, with no format label", func(t *testing.T) {
		labels := ownership()
		delete(labels, brand.FormatLabel(decoyNamespace))
		mine := volume(t, labels)
		owned, err := runtime.Inventory(ctx, ref)
		if err != nil || len(owned) != 1 || owned[0].ID != mine {
			t.Fatalf("owned = %v, err = %v", owned, err)
		}
	})
	t.Run("refuses a container with every ownership label but no format label", func(t *testing.T) {
		labels := ownership()
		delete(labels, brand.FormatLabel(decoyNamespace))
		refuses(t, container(t, labels))
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
