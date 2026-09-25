package commands_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/checkoutindex/card"
	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/commands/commandstest"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/registry/registrytest"
)

const acmeDomain = "acme.localhost"

// register is registrytest.Register against a world's context, standing in
// for what `up` (not part of this package) would have registered.
func register(t *testing.T, w *commandstest.World, root, domain, slug string, initialization string) *registry.WorktreeIdentity {
	t.Helper()
	identities := registry.NewIdentityService(root, w.Runner)
	identity, err := registrytest.Register(context.Background(), w.ControlHome, identities, root, domain, slug, nil, initialization)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return identity
}

func TestWorktreeList(t *testing.T) {
	ctx := context.Background()

	t.Run("lists this repository with branch, root, and state, marking the current worktree", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		other := w.Worktree("other", "feature/other")
		register(t, w, w.Root, acmeDomain, "feature-login", "complete")
		register(t, w, other, acmeDomain, "feature-other", "complete")

		cctx := w.Context(w.Root, acmeDomain, nil)
		rows, err := commands.ListWithContext(ctx, cctx.CommandContext, commands.ListOptions{})
		if err != nil {
			t.Fatalf("ListWithContext: %v", err)
		}

		if len(rows) != 2 {
			t.Fatalf("expected 2 rows, got %+v", rows)
		}
		if rows[0].Host != "feature-login.acme.localhost" || rows[0].State != commands.StateReady ||
			rows[0].Branch == nil || *rows[0].Branch != "feature/login" ||
			rows[0].Root == nil || *rows[0].Root != w.Root || !rows[0].Current {
			t.Fatalf("unexpected row 0: %+v", rows[0])
		}
		if rows[1].Host != "feature-other.acme.localhost" || rows[1].State != commands.StateReady ||
			rows[1].Branch == nil || *rows[1].Branch != "feature/other" ||
			rows[1].Root == nil || *rows[1].Root != other || rows[1].Current {
			t.Fatalf("unexpected row 1: %+v", rows[1])
		}

		lines := cctx.Logger.Printed
		if len(lines) != 3 {
			t.Fatalf("expected a header and 2 rows, got %+v", lines)
		}
		if !strings.HasPrefix(lines[0], "  HOST") {
			t.Fatalf("unexpected header: %q", lines[0])
		}
		if !strings.HasPrefix(lines[1], "* feature-login.acme.localhost") {
			t.Fatalf("expected the current row marked with '*': %q", lines[1])
		}
		if !strings.HasPrefix(lines[2], "  feature-other.acme.localhost") {
			t.Fatalf("expected the other row unmarked: %q", lines[2])
		}
	})

	t.Run("reports a registration whose worktree Git no longer lists as missing (with its last-seen branch), and one whose first up never finished as pending", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		gone := w.Worktree("gone", "feature/gone")
		half := w.Worktree("half", "feature/half")
		register(t, w, w.Root, acmeDomain, "feature-login", "complete")
		goneIdentity := register(t, w, gone, acmeDomain, "feature-gone", "complete")
		register(t, w, half, acmeDomain, "feature-half", "pending") // registered, never brought up

		// `up` would have written a card while `gone` was still live; that
		// card is what lets a missing row still say where it was last seen.
		w.WriteIndexCard(goneIdentity.RepositoryID, goneIdentity.WorktreeID, "acme", acmeDomain, "feature-gone",
			"feature-gone."+acmeDomain, gone, "feature/gone")
		// Simulate `git worktree remove gone`.
		w.Git.SetActive(activeWithout(w.Git.Active(), gone))

		cctx := w.Context(w.Root, acmeDomain, nil)
		rows, err := commands.ListWithContext(ctx, cctx.CommandContext, commands.ListOptions{})
		if err != nil {
			t.Fatalf("ListWithContext: %v", err)
		}

		got := map[string][2]string{}
		for _, row := range rows {
			branch := "-"
			if row.Branch != nil {
				branch = *row.Branch
			}
			got[row.Slug] = [2]string{string(row.State), branch}
		}
		want := map[string][2]string{
			"feature-gone":  {"missing", "feature/gone"},
			"feature-half":  {"pending", "feature/half"},
			"feature-login": {"ready", "feature/login"},
		}
		for slug, expect := range want {
			if got[slug] != expect {
				t.Fatalf("slug %s: got %v, want %v (all: %+v)", slug, got[slug], expect, rows)
			}
		}
	})

	t.Run("says a detached checkout is detached, in the index's words", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		detached := w.Worktree("detached", "")
		register(t, w, detached, acmeDomain, "detached", "complete")

		cctx := w.Context(detached, acmeDomain, nil)
		rows, err := commands.ListWithContext(ctx, cctx.CommandContext, commands.ListOptions{})
		if err != nil {
			t.Fatalf("ListWithContext: %v", err)
		}
		if len(rows) != 1 || rows[0].Branch == nil || *rows[0].Branch != "" {
			t.Fatalf("expected a single detached row, got %+v", rows)
		}
		found := false
		for _, line := range cctx.Logger.Printed {
			if strings.Contains(line, "(detached)") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected the table to say (detached), got %+v", cctx.Logger.Printed)
		}
	})

	t.Run("--all spans repositories and domains, with last-seen detail for the others", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		widgetsCommon := filepath.Join(w.Base, "widgets-common")
		widgets := w.Worktree("widgets", "main", widgetsCommon)
		register(t, w, w.Root, acmeDomain, "feature-login", "complete")
		widgetsIdentity := register(t, w, widgets, "widgets.localhost", "main", "complete")
		w.WriteIndexCard(widgetsIdentity.RepositoryID, widgetsIdentity.WorktreeID, "widgets", "widgets.localhost", "main",
			"main.widgets.localhost", widgets, "main")

		local, err := commands.ListWithContext(ctx, w.Context(w.Root, acmeDomain, nil).CommandContext, commands.ListOptions{})
		if err != nil {
			t.Fatalf("ListWithContext: %v", err)
		}
		if len(local) != 1 || local[0].Host != "feature-login.acme.localhost" {
			t.Fatalf("expected only the local repository, got %+v", local)
		}

		everything, err := commands.ListWithContext(ctx, w.Context(w.Root, acmeDomain, nil).CommandContext, commands.ListOptions{All: true})
		if err != nil {
			t.Fatalf("ListWithContext --all: %v", err)
		}
		if len(everything) != 2 {
			t.Fatalf("expected both repositories, got %+v", everything)
		}
		if everything[0].Host != "feature-login.acme.localhost" || !everything[0].Current {
			t.Fatalf("unexpected row 0: %+v", everything[0])
		}
		if everything[1].Host != "main.widgets.localhost" || everything[1].Current {
			t.Fatalf("unexpected row 1: %+v", everything[1])
		}

		// With the other repository's card gone, nothing records its branch.
		cardPath := cardPathFor(w, widgetsIdentity.RepositoryID, widgetsIdentity.WorktreeID)
		removeFile(t, cardPath)
		forgotten, err := commands.ListWithContext(ctx, w.Context(w.Root, acmeDomain, nil).CommandContext, commands.ListOptions{All: true})
		if err != nil {
			t.Fatalf("ListWithContext --all: %v", err)
		}
		if forgotten[0].Branch == nil || *forgotten[0].Branch != "feature/login" {
			t.Fatalf("expected the local row to still know its branch: %+v", forgotten[0])
		}
		if forgotten[1].Branch != nil {
			t.Fatalf("expected the other repository's branch to be forgotten, got %+v", forgotten[1])
		}
	})

	t.Run("--json prints the rows", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		register(t, w, w.Root, acmeDomain, "feature-login", "complete")

		cctx := w.Context(w.Root, acmeDomain, nil)
		rows, err := commands.ListWithContext(ctx, cctx.CommandContext, commands.ListOptions{JSON: true})
		if err != nil {
			t.Fatalf("ListWithContext: %v", err)
		}
		var parsed []commands.ListedRegistration
		if err := json.Unmarshal([]byte(strings.Join(cctx.Logger.Printed, "\n")), &parsed); err != nil {
			t.Fatalf("unmarshal printed JSON: %v", err)
		}
		if len(parsed) != len(rows) || parsed[0].Slug != rows[0].Slug {
			t.Fatalf("printed JSON does not match rows: printed=%+v rows=%+v", parsed, rows)
		}
	})

	t.Run("keeps a branch name from driving the terminal, in the table and in the JSON", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		// A clone chooses its own branch names: an escape sequence here
		// would move a real cursor, and a right-to-left override would
		// reverse the row.
		hostileBranch := "feature/\u001b[2Kx"
		hostile := w.Worktree("hostile", hostileBranch)
		register(t, w, hostile, acmeDomain, "hostile", "complete")

		cctx := w.Context(hostile, acmeDomain, nil)
		if _, err := commands.ListWithContext(ctx, cctx.CommandContext, commands.ListOptions{}); err != nil {
			t.Fatalf("ListWithContext: %v", err)
		}
		table := strings.Join(cctx.Logger.Printed, "\n")
		if strings.ContainsRune(table, '\u001b') {
			t.Fatalf("expected the escape character to be filtered: %q", table)
		}
		if !strings.Contains(table, "�") {
			t.Fatalf("expected the replacement character in the table: %q", table)
		}

		jsonCtx := w.Context(hostile, acmeDomain, nil)
		if _, err := commands.ListWithContext(ctx, jsonCtx.CommandContext, commands.ListOptions{JSON: true}); err != nil {
			t.Fatalf("ListWithContext --json: %v", err)
		}
		printed := strings.Join(jsonCtx.Logger.Printed, "\n")
		if strings.ContainsRune(printed, '\u001b') {
			t.Fatalf("expected the escape character to be escaped in JSON: %q", printed)
		}
		if !strings.Contains(printed, `\u001b`) {
			t.Fatalf("expected the escape to survive as \\u001b: %q", printed)
		}
		var parsed []commands.ListedRegistration
		if err := json.Unmarshal([]byte(printed), &parsed); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(parsed) != 1 || parsed[0].Branch == nil || *parsed[0].Branch != hostileBranch {
			t.Fatalf("expected the branch to round-trip exactly through JSON: %+v", parsed)
		}
	})

	t.Run("says so when nothing is registered", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		cctx := w.Context(w.Root, acmeDomain, nil)

		rows, err := commands.ListWithContext(ctx, cctx.CommandContext, commands.ListOptions{})
		if err != nil {
			t.Fatalf("ListWithContext: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("expected no rows, got %+v", rows)
		}
		// Nothing to pipe, so nothing on stdout: the sentence is a notice.
		if len(cctx.Logger.Printed) != 0 {
			t.Fatalf("expected nothing on stdout, got %+v", cctx.Logger.Printed)
		}
		found := false
		for _, line := range cctx.Logger.Lines {
			if line == "info: No worktrees are registered for this repository." {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected the empty notice, got %+v", cctx.Logger.Lines)
		}
		if got := commands.FormatTable(nil, true); len(got) != 1 || got[0] != "No worktrees are registered on this machine." {
			t.Fatalf("unexpected FormatTable(nil, true): %+v", got)
		}
	})
}

// cardPathFor returns where a worktree's index card would live under the
// world's control home.
func cardPathFor(w *commandstest.World, repositoryID, worktreeID string) string {
	return card.IndexCardPath(w.ControlHome, repositoryID, worktreeID)
}

// removeFile deletes path, failing the test if it cannot.
func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
}

// activeWithout returns active with remove's root filtered out, for a test
// that wants "every registered worktree except the one just removed".
func activeWithout(active []string, remove string) []string {
	out := make([]string, 0, len(active))
	for _, root := range active {
		if root != remove {
			out = append(out, root)
		}
	}
	return out
}
