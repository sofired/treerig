package commands_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/commands/commandstest"
	"github.com/sofired/treerig/internal/overview/card"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/worktree"
)

const acmeDomain = "acme.localhost"

// register is registrytest.Register against a world's context, standing in
// for what `up` (not part of this package) would have registered.
func register(t *testing.T, w *commandstest.World, root, domain, slug string, initialization string) *registry.WorktreeIdentity {
	t.Helper()
	identities := registry.NewIdentityService(root, w.Runner, nil)
	identity, err := registrytest.Register(context.Background(), w.ControlHome, identities, root, domain, slug, nil, initialization)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return identity
}

func TestList(t *testing.T) {
	ctx := context.Background()

	t.Run("lists this repository with state and directory, marking the current worktree", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		other := w.Worktree("other", "feature/other")
		register(t, w, w.Root, acmeDomain, "feature-login", "complete")
		register(t, w, other, acmeDomain, "feature-other", "complete")

		cctx := w.Context(w.Root, acmeDomain, nil)
		rowsReport, err := commands.ListWithContext(ctx, cctx.CommandContext, commands.ListOptions{})
		if err != nil {
			t.Fatalf("ListWithContext: %v", err)
		}
		rows := rowsReport.Entries

		if len(rows) != 2 {
			t.Fatalf("expected 2 rows, got %+v", rows)
		}
		if rows[0].Host != "feature-login.acme.localhost" || rows[0].State != worktree.StateStopped ||
			rows[0].Branch == nil || *rows[0].Branch != "feature/login" ||
			rows[0].Root != w.Root || !rows[0].Current {
			t.Fatalf("unexpected row 0: %+v", rows[0])
		}
		if rows[1].Host != "feature-other.acme.localhost" || rows[1].State != worktree.StateStopped ||
			rows[1].Branch == nil || *rows[1].Branch != "feature/other" ||
			rows[1].Root != other || rows[1].Current {
			t.Fatalf("unexpected row 1: %+v", rows[1])
		}

		lines := cctx.Logger.Printed
		if len(lines) != 3 {
			t.Fatalf("expected a header and 2 rows, got %+v", lines)
		}
		if !strings.HasPrefix(lines[0], "   WORKTREE") {
			t.Fatalf("unexpected header: %q", lines[0])
		}
		if !strings.HasPrefix(lines[1], "*  feature-login") || !strings.HasSuffix(lines[1], w.Root) {
			t.Fatalf("expected the current row marked with '*', and ending in its directory: %q", lines[1])
		}
		if !strings.HasPrefix(lines[2], "   feature-other") || !strings.HasSuffix(lines[2], other) {
			t.Fatalf("expected the other row unmarked, and ending in its directory: %q", lines[2])
		}
	})

	t.Run("tells a worktree whose directory is gone, one never brought up, and one brought down apart, with the last-seen branch", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		gone := w.Worktree("gone", "feature/gone")
		half := w.Worktree("half", "feature/half")
		register(t, w, w.Root, acmeDomain, "feature-login", "complete")
		goneIdentity := register(t, w, gone, acmeDomain, "feature-gone", "complete")
		register(t, w, half, acmeDomain, "feature-half", "pending") // registered, never brought up

		// `up` would have written a card while `gone` was still live; that
		// card is what lets a row whose directory is gone still say where it
		// was last seen.
		w.WriteIndexCard(goneIdentity.RepositoryID, goneIdentity.WorktreeID, "acme", acmeDomain, "feature-gone",
			"feature-gone."+acmeDomain, gone, "feature/gone")
		// `rm -rf gone`: Git still lists it, now as prunable.
		if err := os.RemoveAll(gone); err != nil {
			t.Fatal(err)
		}

		cctx := w.Context(w.Root, acmeDomain, nil)
		rowsReport, err := commands.ListWithContext(ctx, cctx.CommandContext, commands.ListOptions{})
		if err != nil {
			t.Fatalf("ListWithContext: %v", err)
		}
		rows := rowsReport.Entries

		got := map[string][2]string{}
		for _, row := range rows {
			branch := "-"
			if row.Branch != nil {
				branch = *row.Branch
			}
			got[row.Slug] = [2]string{row.State.String(), branch}
		}
		want := map[string][2]string{
			"feature-gone":  {"directory missing", "feature/gone"},
			"feature-half":  {"not started", "feature/half"},
			"feature-login": {"stopped", "feature/login"},
		}
		for slug, expect := range want {
			if got[slug] != expect {
				t.Fatalf("slug %s: got %v, want %v (all: %+v)", slug, got[slug], expect, rows)
			}
		}
	})

	t.Run("calls a worktree Git no longer lists gone when the directory it was last seen in is, and unknown when nothing recorded one", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		pruned := w.Worktree("pruned", "feature/pruned")
		unrecorded := w.Worktree("unrecorded", "feature/unrecorded")
		register(t, w, w.Root, acmeDomain, "feature-login", "complete")
		prunedIdentity := register(t, w, pruned, acmeDomain, "feature-pruned", "complete")
		register(t, w, unrecorded, acmeDomain, "feature-unrecorded", "complete")
		w.WriteIndexCard(prunedIdentity.RepositoryID, prunedIdentity.WorktreeID, "acme", acmeDomain, "feature-pruned",
			"feature-pruned."+acmeDomain, pruned, "feature/pruned")
		// `git worktree remove` for both: Git forgets them and their directories.
		for _, root := range []string{pruned, unrecorded} {
			if err := os.RemoveAll(root); err != nil {
				t.Fatal(err)
			}
		}
		w.Git.SetActive([]string{w.Root})

		cctx := w.Context(w.Root, acmeDomain, nil)
		rowsReport, err := commands.ListWithContext(ctx, cctx.CommandContext, commands.ListOptions{})
		if err != nil {
			t.Fatalf("ListWithContext: %v", err)
		}
		rows := rowsReport.Entries
		got := map[string]commands.ListEntry{}
		for _, row := range rows {
			got[row.Slug] = row
		}
		if got["feature-pruned"].State != worktree.StateDirectoryMissing {
			t.Fatalf("pruned: %+v", got["feature-pruned"])
		}
		if row := got["feature-unrecorded"]; row.State != worktree.StateUnknown || row.Hint != "Git does not list it and no directory was recorded for it: run 'treerig worktree remove feature-unrecorded' if it is gone." {
			t.Fatalf("unrecorded: %+v", row)
		}
		hinted := false
		for _, line := range cctx.Logger.Lines {
			hinted = hinted || strings.Contains(line, "feature-unrecorded.acme.localhost is unknown. Git does not list it")
		}
		if !hinted {
			t.Fatalf("the cause of an unknown row is on stderr: %+v", cctx.Logger.Lines)
		}
	})

	t.Run("shows what the containers say, with one docker ps for the whole listing", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		w.Docker.Services = []string{"api", "worker"}
		other := w.Worktree("other", "feature/other")
		third := w.Worktree("third", "feature/third")
		login := register(t, w, w.Root, acmeDomain, "feature-login", "complete")
		otherIdentity := register(t, w, other, acmeDomain, "feature-other", "complete")
		thirdIdentity := register(t, w, third, acmeDomain, "feature-third", "complete")
		ref := func(identity *registry.WorktreeIdentity, slug string) worktree.Ref {
			return worktree.Ref{RepositoryID: identity.RepositoryID, WorktreeID: identity.WorktreeID, Slug: slug}
		}
		w.Docker.Up(ref(login, "feature-login"), brand.LabelNamespace, "api", "worker")
		crashed := w.Docker.Up(ref(otherIdentity, "feature-other"), brand.LabelNamespace, "api", "worker")
		w.Docker.SetStatus(crashed[1], "exited")
		w.Docker.SetExitCode(crashed[1], 3)
		starting := w.Docker.Up(ref(thirdIdentity, "feature-third"), brand.LabelNamespace, "api")
		w.Docker.SetHealth(starting[0], "starting")
		for _, slug := range []string{"feature-login", "feature-other", "feature-third"} {
			identity := map[string]*registry.WorktreeIdentity{"feature-login": login, "feature-other": otherIdentity, "feature-third": thirdIdentity}[slug]
			w.WriteIndexCard(identity.RepositoryID, identity.WorktreeID, "acme", acmeDomain, slug, slug+"."+acmeDomain, w.Root, "main")
		}

		cctx := w.Context(w.Root, acmeDomain, nil)
		rowsReport, err := commands.ListWithContext(ctx, cctx.CommandContext, commands.ListOptions{})
		if err != nil {
			t.Fatalf("ListWithContext: %v", err)
		}
		rows := rowsReport.Entries
		got := map[string]worktree.State{}
		for _, row := range rows {
			got[row.Slug] = row.State
		}
		want := map[string]worktree.State{
			"feature-login": worktree.StateRunning,
			"feature-other": worktree.StatePartlyRunning,
			"feature-third": worktree.StateStarting,
		}
		for slug, state := range want {
			if got[slug] != state {
				t.Fatalf("%s = %q, want %q", slug, got[slug], state)
			}
		}
		psCalls := 0
		for _, call := range w.Docker.Calls() {
			if strings.HasPrefix(call, "ps ") {
				psCalls++
			}
		}
		if psCalls != 1 {
			t.Fatalf("docker ps ran %d times, want once for the one label namespace; calls: %v", psCalls, w.Docker.Calls())
		}
	})

	t.Run("lists every worktree as unknown, fails with the cause said once, when Docker does not answer", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		register(t, w, w.Root, acmeDomain, "feature-login", "complete")
		register(t, w, w.Worktree("other", "feature/other"), acmeDomain, "feature-other", "complete")
		w.Docker.FailCall("ps", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock", 1, -1)

		cctx := w.Context(w.Root, acmeDomain, nil)
		rowsReport, err := commands.ListWithContext(ctx, cctx.CommandContext, commands.ListOptions{})
		if err == nil || !strings.HasPrefix(err.Error(), "Docker is not available: ") || clierr.Code(err) != 1 {
			t.Fatalf("err = %v, want Docker's cause, exit 1", err)
		}
		rows := rowsReport.Entries
		if len(rows) != 2 || rows[0].State != worktree.StateUnknown || rows[1].State != worktree.StateUnknown ||
			!strings.HasPrefix(rows[0].Hint, "Docker is not available: ") {
			t.Fatalf("rows = %+v", rows)
		}
		if len(cctx.Logger.Printed) != 3 || !strings.Contains(cctx.Logger.Printed[1], "unknown") {
			t.Fatalf("the table is still printed: %+v", cctx.Logger.Printed)
		}
		// The cause is the error, once: not a warning, and not a line per row.
		for _, line := range cctx.Logger.Lines {
			if strings.Contains(line, "Docker") {
				t.Fatalf("the cause was repeated on stderr: %+v", cctx.Logger.Lines)
			}
		}
	})

	t.Run("reports a worktree whose removal was interrupted as removing", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		identity := register(t, w, w.Root, acmeDomain, "feature-login", "complete")
		store := registry.NewStore(w.ControlHome, identity.RepositoryID)
		marker := filepath.Join(w.ControlHome, "repositories", identity.RepositoryID, "worktrees", identity.WorktreeID+".removing")
		if err := os.WriteFile(marker, []byte(registry.RemovalMarkerContents()), 0o600); err != nil {
			t.Fatal(err)
		}
		if removing, err := store.Removing(identity.WorktreeID); err != nil || !removing {
			t.Fatalf("Removing = %v, %v", removing, err)
		}
		cctx := w.Context(w.Root, acmeDomain, nil)
		rowsReport, err := commands.ListWithContext(ctx, cctx.CommandContext, commands.ListOptions{JSON: true})
		if err != nil {
			t.Fatalf("ListWithContext: %v", err)
		}
		rows := rowsReport.Entries
		if len(rows) != 1 || rows[0].State != worktree.StateRemoving {
			t.Fatalf("rows = %+v", rows)
		}
		if printed := strings.Join(cctx.Logger.Printed, "\n"); !strings.Contains(printed, `"state": "removing"`) {
			t.Fatalf("printed = %s", printed)
		}
	})

	t.Run("reports a detached worktree's branch as empty, which is not the same as unknown", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		detached := w.Worktree("detached", "")
		register(t, w, detached, acmeDomain, "detached", "complete")

		cctx := w.Context(detached, acmeDomain, nil)
		report, err := commands.ListWithContext(ctx, cctx.CommandContext, commands.ListOptions{JSON: true})
		if err != nil {
			t.Fatalf("ListWithContext: %v", err)
		}
		rows := report.Entries
		if len(rows) != 1 || rows[0].Branch == nil || *rows[0].Branch != "" {
			t.Fatalf("expected a single detached row, got %+v", rows)
		}
		if printed := strings.Join(cctx.Logger.Printed, "\n"); !strings.Contains(printed, `"branch": ""`) {
			t.Fatalf("expected the document to say the branch is empty, got %s", printed)
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

		localReport, err := commands.ListWithContext(ctx, w.Context(w.Root, acmeDomain, nil).CommandContext, commands.ListOptions{})
		if err != nil {
			t.Fatalf("ListWithContext: %v", err)
		}
		local := localReport.Entries
		if len(local) != 1 || local[0].Host != "feature-login.acme.localhost" {
			t.Fatalf("expected only the local repository, got %+v", local)
		}

		everythingReport, err := commands.ListWithContext(ctx, w.Context(w.Root, acmeDomain, nil).CommandContext, commands.ListOptions{All: true})
		if err != nil {
			t.Fatalf("ListWithContext --all: %v", err)
		}
		everything := everythingReport.Entries
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
		forgottenReport, err := commands.ListWithContext(ctx, w.Context(w.Root, acmeDomain, nil).CommandContext, commands.ListOptions{All: true})
		if err != nil {
			t.Fatalf("ListWithContext --all: %v", err)
		}
		forgotten := forgottenReport.Entries
		if forgotten[0].Branch == nil || *forgotten[0].Branch != "feature/login" {
			t.Fatalf("expected the local row to still know its branch: %+v", forgotten[0])
		}
		if forgotten[1].Branch != nil {
			t.Fatalf("expected the other repository's branch to be forgotten, got %+v", forgotten[1])
		}
	})

	t.Run("--json prints the document", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		register(t, w, w.Root, acmeDomain, "feature-login", "complete")

		cctx := w.Context(w.Root, acmeDomain, nil)
		rowsReport, err := commands.ListWithContext(ctx, cctx.CommandContext, commands.ListOptions{JSON: true})
		if err != nil {
			t.Fatalf("ListWithContext: %v", err)
		}
		rows := rowsReport.Entries
		var parsed commands.ListReport
		if err := json.Unmarshal([]byte(strings.Join(cctx.Logger.Printed, "\n")), &parsed); err != nil {
			t.Fatalf("unmarshal printed JSON: %v", err)
		}
		if len(parsed.Entries) != len(rows) || parsed.Entries[0].Slug != rows[0].Slug || !parsed.Entries[0].Current {
			t.Fatalf("printed JSON does not match rows: printed=%+v rows=%+v", parsed, rows)
		}
	})

	t.Run("keeps a directory name from driving the terminal, in the table and in the JSON", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		// A clone chooses its own directory names: an escape sequence here
		// would move a real cursor, and a right-to-left override would
		// reverse the row.
		hostileName := "ho\u001b[2Kstile"
		hostile := w.Worktree(hostileName, "hostile")
		register(t, w, hostile, acmeDomain, "hostile", "complete")

		cctx := w.Context(hostile, acmeDomain, nil)
		if _, err := commands.ListWithContext(ctx, cctx.CommandContext, commands.ListOptions{}); err != nil {
			t.Fatalf("ListWithContext: %v", err)
		}
		table := strings.Join(cctx.Logger.Printed, "\n")
		if strings.ContainsRune(table, '\u001b') {
			t.Fatalf("expected the escape character to be filtered: %q", table)
		}
		if !strings.Contains(table, "\ufffd") {
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
		var parsed commands.ListReport
		if err := json.Unmarshal([]byte(printed), &parsed); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(parsed.Entries) != 1 || parsed.Entries[0].Root != hostile {
			t.Fatalf("expected the directory to round-trip exactly through JSON: %+v", parsed.Entries)
		}
	})

	t.Run("says so when nothing is registered", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acmeDomain)
		cctx := w.Context(w.Root, acmeDomain, nil)

		rowsReport, err := commands.ListWithContext(ctx, cctx.CommandContext, commands.ListOptions{})
		if err != nil {
			t.Fatalf("ListWithContext: %v", err)
		}
		rows := rowsReport.Entries
		if len(rows) != 0 {
			t.Fatalf("expected no rows, got %+v", rows)
		}
		// Nothing to pipe, so nothing on stdout: the sentence is a notice.
		if len(cctx.Logger.Printed) != 0 {
			t.Fatalf("expected nothing on stdout, got %+v", cctx.Logger.Printed)
		}
		found := false
		for _, line := range cctx.Logger.Lines {
			if line == "info: No worktrees are registered for this repository. Run 'treerig up' to register this worktree." {
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
