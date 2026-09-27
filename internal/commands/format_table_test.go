package commands_test

import (
	"testing"

	"github.com/sofired/berth/internal/commands"
)

// TestFormatTable pins the exact column alignment against literal expected
// lines, using fixed, short strings so the widths are deterministic, unlike
// a worktree's real (variable-length) temp-directory root.
func TestFormatTable(t *testing.T) {
	branch := func(s string) *string { return &s }
	root := func(s string) *string { return &s }

	rows := []commands.ListedRegistration{
		{Slug: "feature-login", Domain: "acme.localhost", Host: "feature-login.acme.localhost", State: commands.StateReady, Branch: branch("feature/login"), Root: root("/repo/worktree"), Current: true},
		{Slug: "feature-other", Domain: "acme.localhost", Host: "feature-other.acme.localhost", State: commands.StateReady, Branch: branch("feature/other"), Root: root("/repo/other"), Current: false},
	}

	got := commands.FormatTable(rows, false)
	want := []string{
		"  HOST                          STATE  BRANCH         ROOT",
		"* feature-login.acme.localhost  ready  feature/login  /repo/worktree",
		"  feature-other.acme.localhost  ready  feature/other  /repo/other",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d:\n got  %q\n want %q", i, got[i], want[i])
		}
	}

	t.Run("a missing root or branch prints -", func(t *testing.T) {
		got := commands.FormatTable([]commands.ListedRegistration{
			{Slug: "gone", Domain: "acme.localhost", Host: "gone.acme.localhost", State: commands.StateMissing},
		}, false)
		want := []string{
			"  HOST                 STATE    BRANCH  ROOT",
			"  gone.acme.localhost  missing  -       -",
		}
		if len(got) != len(want) {
			t.Fatalf("got %+v", got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("line %d:\n got  %q\n want %q", i, got[i], want[i])
			}
		}
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
