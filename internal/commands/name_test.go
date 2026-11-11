package commands_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/commands/commandstest"
	"github.com/sofired/treerig/internal/worktree"
)

// namedWorld is two repositories whose worktrees both have the slug "main":
// this repository's, and widgets', each with an index card that records its
// directory.
func namedWorld(t *testing.T) (w *commandstest.World, mine, theirs worktree.Ref, theirRoot string) {
	t.Helper()
	w = commandstest.New(t, "feature/login", acme)
	theirRoot = w.Worktree("widgets", "main", filepath.Join(w.Base, "widgets-common"))
	mine = w.Register("", acme, "main", brand.LabelNamespace, "api")
	theirs = w.Register(theirRoot, "widgets.localhost", "main", "dev.widgets", "api")
	w.WriteIndexCard(mine.RepositoryID, mine.WorktreeID, "acme", acme, "main", "main."+acme, w.Root, "feature/login")
	w.WriteIndexCard(theirs.RepositoryID, theirs.WorktreeID, "widgets", "widgets.localhost", "main", "main.widgets.localhost", theirRoot, "main")
	return w, mine, theirs, theirRoot
}

func TestNamingAWorktreeToRemoveIt(t *testing.T) {
	t.Run("a host names the worktree, from outside a repository", func(t *testing.T) {
		w, mine, theirs, _ := namedWorld(t)
		result, err := remove(t, w.ContextWithoutConfig(""), approved(commands.RemoveOptions{Identifier: "main.widgets.localhost"}))
		if err != nil || !result.Registration {
			t.Fatalf("result = %+v, %v", result, err)
		}
		if left := w.Docker.Owned(theirs, "dev.widgets"); len(left) != 0 {
			t.Fatalf("left behind: %v", left)
		}
		if left := w.Docker.Owned(mine, brand.LabelNamespace); len(left) != 2 {
			t.Fatalf("the other worktree lost resources: %v", left)
		}
	})

	t.Run("an absolute path to its root names the worktree", func(t *testing.T) {
		w, mine, theirs, theirRoot := namedWorld(t)
		result, err := remove(t, w.ContextWithoutConfig(""), approved(commands.RemoveOptions{Identifier: theirRoot}))
		if err != nil || !result.Registration || result.Target.WorktreeID != theirs.WorktreeID {
			t.Fatalf("result = %+v, %v", result, err)
		}
		if left := w.Docker.Owned(mine, brand.LabelNamespace); len(left) != 2 {
			t.Fatalf("the other worktree lost resources: %v", left)
		}
	})

	t.Run("'.' names the worktree the command runs in, and a relative path its sibling", func(t *testing.T) {
		w, _, theirs, theirRoot := namedWorld(t)
		result, err := remove(t, w.ContextWithoutConfig(theirRoot), approved(commands.RemoveOptions{Identifier: "."}))
		if err != nil || result.Target.WorktreeID != theirs.WorktreeID {
			t.Fatalf("result = %+v, %v", result, err)
		}
	})

	t.Run("'../x' names a sibling directory's worktree,", func(t *testing.T) {
		w, mine, _, theirRoot := namedWorld(t)
		rel, err := filepath.Rel(theirRoot, w.Root)
		if err != nil {
			t.Fatal(err)
		}
		result, err := remove(t, w.ContextWithoutConfig(theirRoot), approved(commands.RemoveOptions{Identifier: rel}))
		if err != nil || result.Target.WorktreeID != mine.WorktreeID {
			t.Fatalf("result = %+v, %v", result, err)
		}
	})

	t.Run("a path inside the directory, not its root, is refused and nothing is removed", func(t *testing.T) {
		w, _, theirs, theirRoot := namedWorld(t)
		inside := filepath.Join(theirRoot, "src")
		if err := os.MkdirAll(inside, 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := remove(t, w.ContextWithoutConfig(""), approved(commands.RemoveOptions{Identifier: inside}))
		wantErr(t, err, `is inside the directory of main\.widgets\.localhost \(`+theirs.WorktreeID+`\), not its root\.`)
		if left := w.Docker.Owned(theirs, "dev.widgets"); len(left) != 2 {
			t.Fatalf("something was removed: %v", left)
		}
	})

	t.Run("a slug two repositories share is this repository's own from inside it", func(t *testing.T) {
		w, mine, theirs, _ := namedWorld(t)
		result, err := remove(t, w.ContextWithoutConfig(w.Root), approved(commands.RemoveOptions{Identifier: "main"}))
		if err != nil || result.Target.WorktreeID != mine.WorktreeID {
			t.Fatalf("result = %+v, %v", result, err)
		}
		if left := w.Docker.Owned(theirs, "dev.widgets"); len(left) != 2 {
			t.Fatalf("the other repository's worktree lost resources: %v", left)
		}
	})

	t.Run("a slug two repositories share is refused from outside either, listing hosts and UUIDs, and nothing is removed", func(t *testing.T) {
		w, mine, theirs, _ := namedWorld(t)
		_, err := remove(t, w.ContextWithoutConfig(""), approved(commands.RemoveOptions{Identifier: "main"}))
		wantErr(t, err, `^'main' names more than one worktree: main\.`+strings.ReplaceAll(acme, ".", `\.`)+` \(`+mine.WorktreeID+`\), main\.widgets\.localhost \(`+theirs.WorktreeID+`\)\. Name one by its host or its UUID, or add --domain\.$`)
		if len(w.Docker.Owned(mine, brand.LabelNamespace)) != 2 || len(w.Docker.Owned(theirs, "dev.widgets")) != 2 {
			t.Fatal("something was removed")
		}
	})

	t.Run("--domain narrows a shared slug", func(t *testing.T) {
		w, _, theirs, _ := namedWorld(t)
		result, err := remove(t, w.ContextWithoutConfig(""), approved(commands.RemoveOptions{Identifier: "main", Domain: "widgets.localhost"}))
		if err != nil || result.Target.WorktreeID != theirs.WorktreeID {
			t.Fatalf("result = %+v, %v", result, err)
		}
	})

	t.Run("a bare word is never read as a directory, even when one is called that", func(t *testing.T) {
		w, _, theirs, theirRoot := namedWorld(t)
		// The directory the worktree lives in is called "widgets"; the word names no worktree.
		cwd := filepath.Dir(theirRoot)
		_, err := remove(t, w.ContextWithoutConfig(cwd), approved(commands.RemoveOptions{Identifier: filepath.Base(theirRoot)}))
		wantErr(t, err, `^No worktree is registered as `+filepath.Base(theirRoot)+`\.$`)
		if left := w.Docker.Owned(theirs, "dev.widgets"); len(left) != 2 {
			t.Fatalf("something was removed: %v", left)
		}
	})
}

func TestNamingAWorktreeToUnlockIt(t *testing.T) {
	t.Run("a host names the worktree", func(t *testing.T) {
		w, _, theirs, _ := namedWorld(t)
		killDeadHolder(t, w.ControlHome, theirs)
		result, err := unlock(t, w.ContextWithoutConfig(""), commands.UnlockOptions{Identifier: "main.widgets.localhost"})
		if err != nil || len(result.Cleared) != 2 {
			t.Fatalf("result = %+v, %v", result, err)
		}
	})

	t.Run("a path to its root names the worktree, and a path inside it is refused", func(t *testing.T) {
		w, _, theirs, theirRoot := namedWorld(t)
		killDeadHolder(t, w.ControlHome, theirs)
		inside := filepath.Join(theirRoot, "src")
		if err := os.MkdirAll(inside, 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := unlock(t, w.ContextWithoutConfig(""), commands.UnlockOptions{Identifier: inside})
		wantErr(t, err, `is inside the directory of main\.widgets\.localhost`)
		result, err := unlock(t, w.ContextWithoutConfig(""), commands.UnlockOptions{Identifier: theirRoot})
		if err != nil || len(result.Cleared) != 2 {
			t.Fatalf("result = %+v, %v", result, err)
		}
	})

	t.Run("an ambiguous slug lists hosts and UUIDs", func(t *testing.T) {
		w, mine, theirs, _ := namedWorld(t)
		_, err := unlock(t, w.ContextWithoutConfig(""), commands.UnlockOptions{Identifier: "main"})
		wantErr(t, err, `names more than one worktree: .*`+mine.WorktreeID+`.*`+theirs.WorktreeID)
	})

	t.Run("a UUID nothing registered still falls back to the locks it left", func(t *testing.T) {
		w, _, _, _ := namedWorld(t)
		_, err := unlock(t, w.ContextWithoutConfig(""), commands.UnlockOptions{Identifier: "ffffffff-ffff-4fff-8fff-ffffffffffff"})
		wantErr(t, err, `^No worktree is registered as ffffffff-ffff-4fff-8fff-ffffffffffff, and no lock is held for a worktree of that UUID\.$`)
	})
}
