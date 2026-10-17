package brand_test

// Tests for the parts that apply to CLIName.
//
// Not covered here:
//   - "names an example config that is actually in the tree": a filesystem
//     check on BRAND.exampleFile, nothing to do with CLIName.
//   - "strips a script extension from a launcher name": CLIName takes
//     argv[0] directly, and a Go binary never carries a launcher script
//     extension for it to strip.

import (
	"testing"

	"github.com/sofired/treerig/internal/brand"
)

func TestCLIName(t *testing.T) {
	t.Run("follows the installed binary or symlink name", func(t *testing.T) {
		if got := brand.CLIName("/usr/local/bin/duke"); got != "duke" {
			t.Errorf("CLIName(%q) = %q, want %q", "/usr/local/bin/duke", got, "duke")
		}
		if got := brand.CLIName("/opt/tools/wt"); got != "wt" {
			t.Errorf("CLIName(%q) = %q, want %q", "/opt/tools/wt", got, "wt")
		}
	})

	t.Run("falls back to the product name when running the source directly", func(t *testing.T) {
		// An empty argv[0] leaves no meaningful name. A binary built
		// without -o and run as `./main` carries the same "no meaningful
		// name" case — see brand.go's own doc comment.
		if got := brand.CLIName(""); got != brand.Name {
			t.Errorf("CLIName(%q) = %q, want %q", "", got, brand.Name)
		}
		if got := brand.CLIName("main"); got != brand.Name {
			t.Errorf("CLIName(%q) = %q, want %q", "main", got, brand.Name)
		}
		if got := brand.CLIName("./main"); got != brand.Name {
			t.Errorf("CLIName(%q) = %q, want %q", "./main", got, brand.Name)
		}
	})
}

// TestSharedStorageNames pins the names a repository's shared services leave
// on disk and in Docker (docs/design/routing.md, decisions 16, 17 and 22).
// Changing one orphans every running shared service of every user, so it is
// a migration, not a rename.
func TestSharedStorageNames(t *testing.T) {
	t.Run("labels under the default namespace", func(t *testing.T) {
		for name, got := range map[string]string{
			"dev.treerig.shared":             brand.SharedLabel(brand.LabelNamespace),
			"dev.treerig.shared-definition":  brand.SharedDefinitionLabel(brand.LabelNamespace),
			"dev.treerig.shared-defined-by":  brand.SharedDefinedByLabel(brand.LabelNamespace),
			"dev.treerig.shared-use.":        brand.SharedUseLabelPrefix(brand.LabelNamespace),
			"dev.treerig.shared-use.mailpit": brand.SharedUseLabel(brand.LabelNamespace, "mailpit"),
		} {
			if got != name {
				t.Errorf("got %q, want %q", got, name)
			}
		}
	})

	t.Run("labels follow a project's own namespace", func(t *testing.T) {
		if got := brand.SharedLabel("dev.example"); got != "dev.example.shared" {
			t.Errorf("SharedLabel = %q", got)
		}
		if got := brand.SharedUseLabel("dev.example", "db"); got != "dev.example.shared-use.db" {
			t.Errorf("SharedUseLabel = %q", got)
		}
	})

	t.Run("no shared label is the worktree label or starts with it", func(t *testing.T) {
		worktree := brand.WorktreeLabel(brand.LabelNamespace)
		for _, label := range []string{
			brand.SharedLabel(brand.LabelNamespace), brand.SharedDefinitionLabel(brand.LabelNamespace),
			brand.SharedDefinedByLabel(brand.LabelNamespace), brand.SharedUseLabel(brand.LabelNamespace, "x"),
		} {
			if label == worktree {
				t.Errorf("%q is the worktree label", label)
			}
		}
	})

	t.Run("the project prefix", func(t *testing.T) {
		if brand.SharedProjectPrefix != "treerig-shared-" {
			t.Errorf("SharedProjectPrefix = %q", brand.SharedProjectPrefix)
		}
	})
}

// TestFormatLabel pins the label that carries a resource's state format
// version (docs/design/state.md). Every resource treerig creates has it, so
// changing the name is a migration, not a rename.
func TestFormatLabel(t *testing.T) {
	t.Run("the default namespace", func(t *testing.T) {
		if got := brand.FormatLabel(brand.LabelNamespace); got != "dev.treerig.format" {
			t.Errorf("FormatLabel = %q", got)
		}
	})

	t.Run("follows a project's own namespace", func(t *testing.T) {
		if got := brand.FormatLabel("dev.example"); got != "dev.example.format" {
			t.Errorf("FormatLabel = %q", got)
		}
	})
}

// TestEnvNameAndRepository pins the literal values the rest of the tree
// derives from: hook scripts and the release signing identity depend on them.
func TestEnvNameAndRepository(t *testing.T) {
	t.Run("EnvName prefixes a suffix with the environment prefix", func(t *testing.T) {
		for suffix, want := range map[string]string{
			"STAGE":        "TREERIG_STAGE",
			"WORKTREE_ID":  "TREERIG_WORKTREE_ID",
			"SERVICE_URL_": "TREERIG_SERVICE_URL_",
			"":             "TREERIG_",
		} {
			if got := brand.EnvName(suffix); got != want {
				t.Errorf("EnvName(%q) = %q, want %q", suffix, got, want)
			}
		}
	})

	t.Run("the repository is the GitHub owner and name", func(t *testing.T) {
		if brand.Repository != "sofired/treerig" {
			t.Errorf("Repository = %q", brand.Repository)
		}
	})
}
