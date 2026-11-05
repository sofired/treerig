package commands_test

import (
	"testing"

	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/worktree"
)

// TestFormatTable pins the exact column alignment against literal expected
// lines, using fixed, short strings so the widths are deterministic, unlike
// a worktree's real (variable-length) temp-directory root.
func TestFormatTable(t *testing.T) {
	entries := []commands.ListEntry{
		{Slug: "main", Project: "myapp", State: worktree.StateRunning, Root: "/home/sam/src/myapp", Current: true,
			URLs: []commands.ListURL{{Service: "web", URL: "http://main.myapp.localhost"}}},
		{Slug: "feature-x", Project: "myapp", State: worktree.StateStopped, Root: "/home/sam/src/myapp.feature-x", URLs: []commands.ListURL{}},
		{Slug: "old-spike", Project: "myapp", State: worktree.StateDirectoryMissing, Root: "/home/sam/src/myapp.old-spike"},
	}
	check := func(t *testing.T, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %d lines, want %d: %q", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("line %d:\n got  %q\n want %q", i, got[i], want[i])
			}
		}
	}

	t.Run("marks the current worktree, and shows - for a worktree with no URL", func(t *testing.T) {
		check(t, commands.FormatTable(entries, false), []string{
			"   WORKTREE   STATE              URL                          DIRECTORY",
			"*  main       running            http://main.myapp.localhost  /home/sam/src/myapp",
			"   feature-x  stopped            -                            /home/sam/src/myapp.feature-x",
			"   old-spike  directory missing  -                            /home/sam/src/myapp.old-spike",
		})
	})

	t.Run("puts the repository first when the listing spans repositories", func(t *testing.T) {
		check(t, commands.FormatTable(entries[:2], true), []string{
			"   REPOSITORY  WORKTREE   STATE    URL                          DIRECTORY",
			"*  myapp       main       running  http://main.myapp.localhost  /home/sam/src/myapp",
			"   myapp       feature-x  stopped  -                            /home/sam/src/myapp.feature-x",
		})
	})

	t.Run("names the first URL and counts the others", func(t *testing.T) {
		many := []commands.ListEntry{{Slug: "main", State: worktree.StateRunning, Root: "/src/main", URLs: []commands.ListURL{
			{Service: "web", URL: "http://main.myapp.localhost"},
			{Service: "api", URL: "http://api.main.myapp.localhost"},
			{Service: "mail", URL: "http://mail.main.myapp.localhost"},
		}}}
		check(t, commands.FormatTable(many, false), []string{
			"   WORKTREE  STATE    URL                                  DIRECTORY",
			"   main      running  http://main.myapp.localhost +2 more  /src/main",
		})
	})

	t.Run("a missing directory prints -, and nothing the clone chose can drive the terminal", func(t *testing.T) {
		check(t, commands.FormatTable([]commands.ListEntry{
			{Slug: "gone", Project: "x\x1b[2Jy", State: worktree.StateUnknown},
		}, true), []string{
			"   REPOSITORY  WORKTREE  STATE    URL  DIRECTORY",
			"   x\ufffd[2Jy      gone      unknown  -    -",
		})
	})

	t.Run("empty is a sentence, not a table", func(t *testing.T) {
		if got := commands.FormatTable(nil, false); len(got) != 1 || got[0] != commands.EmptyTable(false) {
			t.Fatalf("got %+v", got)
		}
		if got := commands.FormatTable(nil, true); len(got) != 1 || got[0] != commands.EmptyTable(true) {
			t.Fatalf("got %+v", got)
		}
	})
}
