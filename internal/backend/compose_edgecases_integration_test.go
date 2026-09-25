//go:build integration

// Edge cases of the Compose backend against a real Docker daemon:
// compose_up_integration_test.go proves the up/status/down/inventory/reap
// round trip itself; this file proves what happens around its edges — a
// Compose file and its override both gone before `down`, a nested project
// directory whose relative bind mount must resolve against the Compose
// file's own directory rather than the config root, and reaping one
// worktree while a sibling in the same repository is left alone.
package backend

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/run/osrun"
	"github.com/sofired/berth/internal/worktree"
)

// copyFile copies src to dst, creating dst's parent directories, so a test
// can mutate a copy of a fixture without touching the shared testdata one.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// scratchComposeFixture copies the compose fixture's Compose file and build
// context into a fresh directory, so a test can delete the Compose file
// itself without disturbing the shared testdata copy other tests read.
func scratchComposeFixture(t *testing.T, fixture string) string {
	t.Helper()
	dir := t.TempDir()
	copyFile(t, filepath.Join(fixture, "compose.yaml"), filepath.Join(dir, "compose.yaml"))
	copyFile(t, filepath.Join(fixture, "builder", "Dockerfile"), filepath.Join(dir, "builder", "Dockerfile"))
	return dir
}

// newEdgeCaseRuntime is a Compose runtime configured the way this file's
// subtests need it, against composeFile under projectRoot.
func newEdgeCaseRuntime(composeFile, projectRoot, controlHome string) *Compose {
	return NewCompose(Options{
		ComposeFile: composeFile, ProjectRoot: projectRoot, ConfigFile: filepath.Join(projectRoot, "berth.toml"),
		ProjectName: "berth-test", Domain: "test.localhost", ControlHome: controlHome,
		LabelNamespace: composeNamespace, Runner: osrun.New(), Logger: logger.Silent{},
		RouterNetwork: "berth-router-unused", WaitTimeoutSeconds: intPtr(60), Environ: os.Environ(),
	})
}

// bringUpWorktree creates ref's worktree home under controlHome the way the
// command layer does before `up`, and fails the test on any error.
func bringUpWorktree(t *testing.T, controlHome string, ref worktree.Ref) {
	t.Helper()
	if err := files.SecureWorktreeDirectories(layout.WorktreeLayout(controlHome, ref.RepositoryID, ref.WorktreeID)); err != nil {
		t.Fatalf("SecureWorktreeDirectories(%s): %v", ref.Slug, err)
	}
}

func TestComposeBackendEdgeCasesOnARealDaemon(t *testing.T) {
	requireDocker(t)
	composeSweep(t, composeNamespace+".managed=true")
	ctx := context.Background()

	t.Run("`down` succeeds when the Compose file and its override are both gone, and named volumes survive", func(t *testing.T) {
		fixture := scratchComposeFixture(t, composeFixture(t))
		composeFile := filepath.Join(fixture, "compose.yaml")
		controlHome := t.TempDir()
		repositoryID, worktreeID := composeUUID(t), composeUUID(t)
		subject := worktree.Ref{RepositoryID: repositoryID, WorktreeID: worktreeID, Slug: "feature-broken"}
		project := layout.ComposeProjectName(subject.RepositoryID, subject.WorktreeID)
		t.Cleanup(func() {
			_ = exec.Command("docker", "compose", "-p", project, "down", "--volumes", "--rmi", "local", "--remove-orphans").Run()
			composeSweep(t, composeNamespace+".managed=true", composeNamespace+".repository-uuid="+repositoryID)
		})
		bringUpWorktree(t, controlHome, subject)

		runtime := newEdgeCaseRuntime(composeFile, fixture, controlHome)
		if err := runtime.Up(ctx, subject, []string{"api"}); err != nil {
			t.Fatalf("Up: %v", err)
		}

		overridePath := filepath.Join(layout.WorktreeLayout(controlHome, subject.RepositoryID, subject.WorktreeID).Home, brand.Name+".override.yaml")
		if err := os.Remove(overridePath); err != nil {
			t.Fatalf("removing the override: %v", err)
		}
		if err := os.Remove(composeFile); err != nil {
			t.Fatalf("removing the Compose file: %v", err)
		}

		if err := runtime.Down(ctx, subject); err != nil {
			t.Fatalf("Down with both the Compose file and the override gone: %v", err)
		}

		if got := dockerRun(t, "ps", "-aq", "--filter", "label=com.docker.compose.project="+project); got != "" {
			t.Fatalf("expected every container gone after down, found %q", got)
		}
		if !dockerExists("volume", project+"_data") {
			t.Fatal("expected the named volume to survive down")
		}
	})

	t.Run("a nested Compose file's relative bind mount resolves against that file's own directory, read back from inside a real container", func(t *testing.T) {
		fixture := composeNestedFixture(t)
		controlHome := t.TempDir()
		repositoryID, worktreeID := composeUUID(t), composeUUID(t)
		subject := worktree.Ref{RepositoryID: repositoryID, WorktreeID: worktreeID, Slug: "feature-nested"}
		project := layout.ComposeProjectName(subject.RepositoryID, subject.WorktreeID)
		t.Cleanup(func() {
			_ = exec.Command("docker", "compose", "-p", project, "down", "--volumes", "--rmi", "local", "--remove-orphans").Run()
			composeSweep(t, composeNamespace+".managed=true", composeNamespace+".repository-uuid="+repositoryID)
		})
		bringUpWorktree(t, controlHome, subject)

		runtime := newEdgeCaseRuntime(filepath.Join(fixture, "docker", "compose.yaml"), fixture, controlHome)
		// With the config root as the project directory instead of the
		// Compose file's own directory, the bind source would resolve to
		// <fixture>/www, which does not exist, and the fixture's
		// create_host_path: false would make Compose refuse rather than
		// invent it.
		if err := runtime.Up(ctx, subject, nil); err != nil {
			t.Fatalf("Up: %v", err)
		}

		webID := dockerRun(t, "compose", "-p", project, "ps", "-q", "web")
		if got := dockerRun(t, "exec", webID, "cat", "/www/index.html"); got != "served from docker/www" {
			t.Fatalf("index.html read back from the container = %q", got)
		}
	})

	t.Run("reaping one worktree leaves a sibling worktree's live resources in the same repository alone", func(t *testing.T) {
		fixture := composeFixture(t)
		controlHome := t.TempDir()
		repositoryID := composeUUID(t)
		subject := worktree.Ref{RepositoryID: repositoryID, WorktreeID: composeUUID(t), Slug: "feature-a"}
		sibling := worktree.Ref{RepositoryID: repositoryID, WorktreeID: composeUUID(t), Slug: "feature-b"}
		subjectProject := layout.ComposeProjectName(subject.RepositoryID, subject.WorktreeID)
		siblingProject := layout.ComposeProjectName(sibling.RepositoryID, sibling.WorktreeID)
		t.Cleanup(func() {
			for _, project := range []string{subjectProject, siblingProject} {
				_ = exec.Command("docker", "compose", "-p", project, "down", "--volumes", "--rmi", "local", "--remove-orphans").Run()
			}
			composeSweep(t, composeNamespace+".managed=true", composeNamespace+".repository-uuid="+repositoryID)
		})
		bringUpWorktree(t, controlHome, subject)
		bringUpWorktree(t, controlHome, sibling)

		runtime := newEdgeCaseRuntime(filepath.Join(fixture, "compose.yaml"), fixture, controlHome)
		// `api` mounts the named volume, so both worktrees own a container, a volume, and a network.
		if err := runtime.Up(ctx, subject, []string{"api"}); err != nil {
			t.Fatalf("Up(subject): %v", err)
		}
		if err := runtime.Up(ctx, sibling, []string{"api"}); err != nil {
			t.Fatalf("Up(sibling): %v", err)
		}

		if err := runtime.Down(ctx, subject); err != nil {
			t.Fatalf("Down(subject): %v", err)
		}
		owned, err := runtime.Inventory(ctx, subject)
		if err != nil {
			t.Fatalf("Inventory(subject): %v", err)
		}
		for _, resource := range owned {
			if err := runtime.Reap(ctx, subject, resource); err != nil {
				t.Fatalf("Reap(subject, %+v): %v", resource, err)
			}
		}

		remaining, err := runtime.Inventory(ctx, subject)
		if err != nil {
			t.Fatalf("Inventory(subject) after reap: %v", err)
		}
		if len(remaining) != 0 {
			t.Fatalf("expected nothing left of subject, got %+v", remaining)
		}

		// subject and sibling share a repository, and their project names
		// already differ only because the worktree-uuid label differs too:
		// the reaper's ownership check, not a name, is what kept the
		// sibling's still-running resources out of subject's inventory.
		siblingOwned, err := runtime.Inventory(ctx, sibling)
		if err != nil {
			t.Fatalf("Inventory(sibling): %v", err)
		}
		var kinds []string
		for _, resource := range siblingOwned {
			kinds = append(kinds, resource.Kind)
		}
		if want := []string{"container", "volume", "network"}; !slices.Equal(kinds, want) {
			t.Fatalf("sibling inventory kinds = %v, want %v", kinds, want)
		}
		if !dockerExists("container", siblingProject+"-api-1") {
			t.Fatal("expected the sibling's container to survive")
		}
		if !dockerExists("volume", siblingProject+"_data") {
			t.Fatal("expected the sibling's volume to survive")
		}
		labelFormat := fmt.Sprintf(`{{ index .Config.Labels "%s.worktree-uuid" }}`, composeNamespace)
		if got := dockerFormat(t, labelFormat, "inspect", siblingProject+"-api-1"); got != sibling.WorktreeID {
			t.Fatalf("sibling container's worktree-uuid label = %q, want %q: ownership labels, not names, are what protected it", got, sibling.WorktreeID)
		}
	})
}
