package worktree_test

import (
	"testing"

	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/worktree"
)

func TestEnvironment(t *testing.T) {
	ref := worktree.Ref{RepositoryID: "repo-a", WorktreeID: "wt-1", Slug: "feature-login"}
	source := worktree.EnvironmentSource{
		ProjectName: "acme", Domain: "acme.localhost", ControlHome: "/control",
		ConfigFile: "/work/acme/treerig.toml", ProjectRoot: "/work/acme",
	}

	t.Run("lists its keys in a fixed order, ending with the Compose project", func(t *testing.T) {
		var keys []string
		for _, pair := range worktree.Environment(source, ref) {
			keys = append(keys, pair[0])
		}
		want := []string{"PROJECT", "REPOSITORY_ID", "WORKTREE_ID", "SLUG", "HOST", "WORKTREE_HOME", "CONFIG", "ROOT", "COMPOSE_PROJECT"}
		if len(keys) != len(want) {
			t.Fatalf("keys = %v, want %v", keys, want)
		}
		for i := range want {
			if keys[i] != want[i] {
				t.Fatalf("keys = %v, want %v", keys, want)
			}
		}
	})

	t.Run("carries the worktree's Compose project name, as TREERIG_COMPOSE_PROJECT", func(t *testing.T) {
		want := layout.ComposeProjectName("repo-a", "wt-1")
		found := false
		for _, pair := range worktree.Environment(source, ref) {
			if pair[0] == worktree.ComposeProjectKey {
				found = pair[1] == want
			}
		}
		if !found {
			t.Fatalf("no %s=%s in the environment", worktree.ComposeProjectKey, want)
		}
		if worktree.ComposeProjectKey != "COMPOSE_PROJECT" {
			t.Fatalf("ComposeProjectKey = %q", worktree.ComposeProjectKey)
		}
		variables := worktree.Variables(worktree.Environment(source, ref))
		if last := variables[len(variables)-1]; last != "TREERIG_COMPOSE_PROJECT="+want {
			t.Fatalf("last variable = %q", last)
		}
	})
}
