package commands_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/commands/commandstest"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/worktree"
)

// These tests are the commands that work without a treerig.toml (#200):
// `worktree list`, `worktree unlock`, `worktree remove` and, in internal/cli,
// the completion of worktree names. Each runs on a context whose Loaded is nil,
// as commands.ContextWithoutConfig builds it; the same commands with a
// treerig.toml are covered beside their other tests.

// twoRepositories is a world with a registered worktree in each of two
// repositories, and the slugs they are registered under.
func twoRepositories(t *testing.T) (w *commandstest.World, mine, theirs worktree.Ref) {
	t.Helper()
	w = commandstest.New(t, "feature/login", acme)
	widgets := w.Worktree("widgets", "main", filepath.Join(w.Base, "widgets-common"))
	mine = w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
	theirs = w.Register(widgets, "widgets.localhost", "main", "dev.widgets", "api")
	return w, mine, theirs
}

// noGitComplaint fails the test when lines hold a warning about Git or about
// a domain: what a command run outside a repository, or without a
// treerig.toml, must not say, whether from the removal itself or from the
// index refresh that follows it.
func noGitComplaint(t *testing.T, lines []string) {
	t.Helper()
	for _, line := range lines {
		if strings.HasPrefix(line, "warn:") && (strings.Contains(line, "Git") || strings.Contains(line, "registered as") || strings.Contains(line, brand.ConfigFile)) {
			t.Errorf("warned: %q", line)
		}
	}
}

// registerRecordingNothing is World.Register for a worktree whose registration
// records no label namespace, as one written by an older build did: its
// resources carry namespace all the same.
func registerRecordingNothing(t *testing.T, w *commandstest.World, root, domain, slug, namespace string) worktree.Ref {
	t.Helper()
	ref := w.Register(root, domain, slug, namespace, "api")
	writeRecord(t, w, &registry.WorktreeIdentity{RepositoryID: ref.RepositoryID, WorktreeID: ref.WorktreeID}, domain, slug, nil)
	return ref
}

// gitRefusesAt makes Git fail the way it does in a directory it does not trust
// (dubious ownership) for the top-level question about dir, and leaves every
// other call to the world's fake.
func gitRefusesAt(w *commandstest.World, dir string) {
	inner := w.Runner.Handler
	w.Runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
		if spec.Command == "git" && slices.Contains(spec.Args, "--show-toplevel") && slices.Contains(spec.Args, dir) {
			return run.Result{ExitCode: 128, Stderr: "fatal: detected dubious ownership in repository at '" + dir + "'\n"}, nil
		}
		return inner(ctx, spec)
	}
}

func TestWorktreeListWithoutConfig(t *testing.T) {
	ctx := context.Background()

	t.Run("outside a repository lists every registered worktree, as --all does", func(t *testing.T) {
		w, _, _ := twoRepositories(t)
		tc := w.ContextWithoutConfig("")

		rows, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var hosts []string
		for _, row := range rows {
			hosts = append(hosts, row.Host)
			if row.Current {
				t.Errorf("%s is marked current outside a repository", row.Host)
			}
		}
		want := []string{"feature-login.acme.localhost", "main.widgets.localhost"}
		if !reflect.DeepEqual(hosts, want) {
			t.Fatalf("hosts = %v, want %v", hosts, want)
		}
		if lines := tc.Logger.Printed; len(lines) != 3 || !strings.HasPrefix(lines[0], "  HOST") {
			t.Fatalf("printed = %q, want a header and two rows", lines)
		}
		// The same answer --all gives.
		all, err := commands.ListWithContext(ctx, w.ContextWithoutConfig("").CommandContext, commands.ListOptions{All: true})
		if err != nil || !reflect.DeepEqual(all, rows) {
			t.Fatalf("--all = %+v, %v; want the rows above", all, err)
		}
	})

	t.Run("outside a repository --json prints the same rows, none current", func(t *testing.T) {
		w, _, _ := twoRepositories(t)
		tc := w.ContextWithoutConfig("")

		if _, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{JSON: true}); err != nil {
			t.Fatal(err)
		}
		var decoded []commands.ListedRegistration
		if len(tc.Logger.Printed) != 1 {
			t.Fatalf("printed = %q, want one JSON document", tc.Logger.Printed)
		}
		if err := json.Unmarshal([]byte(tc.Logger.Printed[0]), &decoded); err != nil || len(decoded) != 2 {
			t.Fatalf("decoded = %+v, %v", decoded, err)
		}
		for _, row := range decoded {
			if row.Current {
				t.Errorf("%s is current", row.Host)
			}
		}
	})

	t.Run("outside a repository with nothing registered says so for the machine", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.ContextWithoutConfig("")

		rows, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{})
		if err != nil || len(rows) != 0 {
			t.Fatalf("rows = %+v, %v", rows, err)
		}
		lines, printed := tc.Logger.Snapshot()
		if !slices.Equal(lines, []string{"info: No worktrees are registered on this machine."}) || len(printed) != 0 {
			t.Fatalf("lines = %q, printed = %q", lines, printed)
		}
	})

	t.Run("in a worktree whose treerig.toml is missing or unusable lists that repository's worktrees", func(t *testing.T) {
		w, _, _ := twoRepositories(t)
		tc := w.ContextWithoutConfig(w.Root)

		rows, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].Host != "feature-login.acme.localhost" || !rows[0].Current || rows[0].State != worktree.StateRunning {
			t.Fatalf("rows = %+v, want this repository's one worktree, current", rows)
		}
	})

	t.Run("inside a Git repository treerig does not know, with no or a broken treerig.toml, lists this repository's worktrees (none) and points at --all", func(t *testing.T) {
		w, _, _ := twoRepositories(t)
		fresh := w.Worktree("fresh", "main", filepath.Join(w.Base, "fresh-common"))
		tc := w.ContextWithoutConfig(fresh)

		rows, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{})
		if err != nil || len(rows) != 0 {
			t.Fatalf("rows = %+v, %v; want none: the other repositories' worktrees are not this one's", rows, err)
		}
		want := []string{
			"info: No worktrees are registered for this repository.",
			"info: Every repository: 'treerig worktree list --all'",
		}
		if lines, printed := tc.Logger.Snapshot(); !slices.Equal(lines, want) || len(printed) != 0 {
			t.Fatalf("lines = %q, printed = %q", lines, printed)
		}
	})

	t.Run("--all there lists every repository, and --json prints no hint", func(t *testing.T) {
		w, _, _ := twoRepositories(t)
		fresh := w.Worktree("fresh", "main", filepath.Join(w.Base, "fresh-common"))
		all, err := commands.ListWithContext(ctx, w.ContextWithoutConfig(fresh).CommandContext, commands.ListOptions{All: true})
		if err != nil || len(all) != 2 {
			t.Fatalf("rows = %+v, %v", all, err)
		}
		tc := w.ContextWithoutConfig(fresh)
		if _, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{JSON: true}); err != nil {
			t.Fatal(err)
		}
		if lines, printed := tc.Logger.Snapshot(); len(lines) != 0 || !slices.Equal(printed, []string{"[]"}) {
			t.Fatalf("lines = %q, printed = %q", lines, printed)
		}
	})

	t.Run("with a treerig.toml and no identity yet it says the same", func(t *testing.T) {
		w, _, _ := twoRepositories(t)
		fresh := w.Worktree("fresh", "main", filepath.Join(w.Base, "fresh-common"))
		tc := w.Context(fresh, acme, nil)

		rows, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{})
		if err != nil || len(rows) != 0 {
			t.Fatalf("rows = %+v, %v", rows, err)
		}
		want := []string{
			"info: No worktrees are registered for this repository.",
			"info: Every repository: 'treerig worktree list --all'",
		}
		if lines, _ := tc.Logger.Snapshot(); !slices.Equal(lines, want) {
			t.Fatalf("lines = %q", lines)
		}
	})
}

func TestWorktreeRemoveWithoutConfig(t *testing.T) {
	ctx := context.Background()

	t.Run("outside a repository removes a worktree named by slug, under the namespace its registration records", func(t *testing.T) {
		w, _, theirs := twoRepositories(t)
		tc := w.ContextWithoutConfig("")

		result, err := remove(t, tc, approved(commands.RemoveOptions{Identifier: "main"}))
		if err != nil {
			t.Fatal(err)
		}
		if !result.Registration || !result.Home {
			t.Fatalf("result = %+v", result)
		}
		if got := kinds(result.Resources); !reflect.DeepEqual(got, []string{"container", "volume"}) {
			t.Fatalf("kinds = %v", got)
		}
		if left := w.Docker.Owned(theirs, "dev.widgets"); len(left) != 0 {
			t.Fatalf("left behind: %v", left)
		}
		lines, _ := tc.Logger.Snapshot()
		if last := lines[len(lines)-1]; last != "info: Removed main.widgets.localhost: 1 container, 1 volume, home directory, registration." {
			t.Fatalf("last line = %q\nall: %q", last, lines)
		}
		noGitComplaint(t, lines)
	})

	t.Run("outside a repository removes a worktree named by UUID, leaving the other repository's alone", func(t *testing.T) {
		w, mine, theirs := twoRepositories(t)

		if _, err := remove(t, w.ContextWithoutConfig(""), approved(commands.RemoveOptions{Identifier: theirs.WorktreeID})); err != nil {
			t.Fatal(err)
		}
		if left := w.Docker.Owned(mine, brand.LabelNamespace); len(left) != 2 {
			t.Fatalf("the other worktree lost resources: %v", left)
		}
		if found, err := w.ContextWithoutConfig("").Registry.Resolve(ctx, "feature-login", registry.NameOptions{}); err != nil || found == nil {
			t.Fatalf("the other worktree's registration: %+v, %v", found, err)
		}
	})

	t.Run("outside a repository takes the namespace from the index card when the registration records none", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		widgets := w.Worktree("widgets", "main", filepath.Join(w.Base, "widgets-common"))
		ref := registerRecordingNothing(t, w, widgets, "widgets.localhost", "main", brand.LabelNamespace)
		w.WriteIndexCard(ref.RepositoryID, ref.WorktreeID, "widgets", "widgets.localhost", "main", "main.widgets.localhost", widgets, "main")

		result, err := remove(t, w.ContextWithoutConfig(""), approved(commands.RemoveOptions{Identifier: "main"}))
		if err != nil || !result.Registration {
			t.Fatalf("result = %+v, %v", result, err)
		}
		if left := w.Docker.Owned(ref, brand.LabelNamespace); len(left) != 0 {
			t.Fatalf("left behind: %v", left)
		}
	})

	t.Run("outside a repository refuses, touching nothing, when neither the registration nor the card records the namespace", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		widgets := w.Worktree("widgets", "main", filepath.Join(w.Base, "widgets-common"))
		ref := registerRecordingNothing(t, w, widgets, "widgets.localhost", "main", "dev.oldns")

		_, err := remove(t, w.ContextWithoutConfig(""), approved(commands.RemoveOptions{Identifier: "main"}))
		wantErr(t, err, `^Cannot remove main\.widgets\.localhost from here: nothing on disk records which Docker labels its resources carry\. `+
			`Run 'treerig worktree remove main' from inside the repository that created it, or pass --label-namespace \(dev\.treerig unless that project set project\.label-namespace\)\.$`)
		if left := w.Docker.Owned(ref, "dev.oldns"); len(left) != 2 {
			t.Fatalf("something was removed: %v", left)
		}
		// --label-namespace is still the way out, until it is retired.
		if result, err := remove(t, w.ContextWithoutConfig(""), approved(commands.RemoveOptions{Identifier: "main", LabelNamespace: "dev.oldns"})); err != nil || !result.Registration {
			t.Fatalf("result = %+v, %v", result, err)
		}
	})

	t.Run("outside a repository, with no name, there is nothing to remove and it says how to name one", func(t *testing.T) {
		w, _, _ := twoRepositories(t)
		_, err := remove(t, w.ContextWithoutConfig(""), approved(commands.RemoveOptions{}))
		wantErr(t, err, `^There is no worktree to remove here: .* is not inside a Git worktree\. Name one with 'treerig worktree remove <worktree>'\.$`)
	})

	t.Run("outside a repository a name nobody registered is the same error as inside one", func(t *testing.T) {
		w, _, _ := twoRepositories(t)
		_, err := remove(t, w.ContextWithoutConfig(""), approved(commands.RemoveOptions{Identifier: "nope"}))
		wantErr(t, err, `^No worktree is registered as nope\.$`)
	})

	t.Run("in a worktree whose treerig.toml is unusable removes it under its registration's namespace, and the current one by default", func(t *testing.T) {
		w, mine, _ := twoRepositories(t)
		tc := w.ContextWithoutConfig(w.Root)

		result, err := remove(t, tc, approved(commands.RemoveOptions{}))
		if err != nil || !result.Registration || !result.Marker {
			t.Fatalf("result = %+v, %v", result, err)
		}
		if left := w.Docker.Owned(mine, brand.LabelNamespace); len(left) != 0 {
			t.Fatalf("left behind: %v", left)
		}
		lines, _ := tc.Logger.Snapshot()
		noGitComplaint(t, lines)
	})

	t.Run("in a worktree whose treerig.toml is unusable refuses, naming the file, when nothing else records the namespace", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		registerRecordingNothing(t, w, w.Root, acme, "feature-login", brand.LabelNamespace)

		_, err := remove(t, w.ContextWithoutConfig(w.Root), approved(commands.RemoveOptions{}))
		wantErr(t, err, `^Cannot remove feature-login\.acme\.localhost from this directory: its treerig\.toml could not be used to find out, and nothing on disk records which Docker labels its resources carry\. `+
			`Fix treerig\.toml, or pass --label-namespace \(dev\.treerig unless that project set project\.label-namespace\)\.$`)
	})
}

func TestNamedTargetsDoNotDependOnWhereTheyRunFrom(t *testing.T) {
	t.Run("remove <x> from a directory Git does not trust warns with Git's reason, takes the target as another repository's, and goes on", func(t *testing.T) {
		w, _, theirs := twoRepositories(t)
		cwd := w.Outside()
		gitRefusesAt(w, cwd)
		tc := w.ContextWithoutConfig(cwd)

		result, err := remove(t, tc, approved(commands.RemoveOptions{Identifier: "main"}))
		if err != nil || !result.Registration {
			t.Fatalf("result = %+v, %v", result, err)
		}
		if left := w.Docker.Owned(theirs, "dev.widgets"); len(left) != 0 {
			t.Fatalf("left behind: %v", left)
		}
		lines, _ := tc.Logger.Snapshot()
		warned := 0
		for _, line := range lines {
			if strings.HasPrefix(line, "warn:") && strings.Contains(line, "detected dubious ownership") && strings.Contains(line, "main") {
				warned++
			}
		}
		if warned != 1 {
			t.Fatalf("want one warning with Git's reason, lines = %q", lines)
		}
	})

	t.Run("remove <x> there still needs the namespace recorded, since the target cannot be taken for local", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := registerRecordingNothing(t, w, w.Root, acme, "feature-login", "dev.oldns")
		gitRefusesAt(w, w.Root)

		_, err := remove(t, w.ContextWithoutConfig(w.Root), approved(commands.RemoveOptions{Identifier: "feature-login"}))
		wantErr(t, err, `nothing on disk records which Docker labels its resources carry`)
		if left := w.Docker.Owned(ref, "dev.oldns"); len(left) != 2 {
			t.Fatalf("something was removed: %v", left)
		}
	})

	t.Run("remove with no name there returns Git's error", func(t *testing.T) {
		w, _, _ := twoRepositories(t)
		cwd := w.Outside()
		gitRefusesAt(w, cwd)
		_, err := remove(t, w.ContextWithoutConfig(cwd), approved(commands.RemoveOptions{}))
		wantErr(t, err, `detected dubious ownership`)
	})

	t.Run("unlock <x> there does not ask Git about the directory, and unlock with no name returns Git's error", func(t *testing.T) {
		w, _, theirs := twoRepositories(t)
		cwd := w.Outside()
		gitRefusesAt(w, cwd)
		tc := w.ContextWithoutConfig(cwd)

		if _, err := unlock(t, tc, commands.UnlockOptions{Identifier: theirs.WorktreeID}); err != nil {
			t.Fatal(err)
		}
		if lines, _ := tc.Logger.Snapshot(); strings.Contains(strings.Join(lines, "\n"), "warn:") {
			t.Fatalf("lines = %q", lines)
		}
		_, err := unlock(t, tc, commands.UnlockOptions{})
		wantErr(t, err, `detected dubious ownership`)
	})
}

func TestWorktreeUnlockWithoutConfig(t *testing.T) {
	t.Run("outside a repository finds a worktree by slug or UUID and clears its dead holder's locks", func(t *testing.T) {
		w, mine, theirs := twoRepositories(t)
		killDeadHolder(t, w.ControlHome, theirs)

		result, err := unlock(t, w.ContextWithoutConfig(""), commands.UnlockOptions{Identifier: "main"})
		if err != nil || len(result.Cleared) != 2 {
			t.Fatalf("result = %+v, %v; want both of its locks cleared", result, err)
		}

		// Nothing is held for the other repository's worktree, found by UUID.
		again, err := unlock(t, w.ContextWithoutConfig(""), commands.UnlockOptions{Identifier: mine.WorktreeID})
		if err != nil || len(again.Cleared) != 0 {
			t.Fatalf("result = %+v, %v", again, err)
		}
	})

	t.Run("outside a repository, with no name, there is no worktree to unlock and it says how to name one", func(t *testing.T) {
		w, _, _ := twoRepositories(t)
		_, err := unlock(t, w.ContextWithoutConfig(""), commands.UnlockOptions{})
		wantErr(t, err, `^There is no worktree to unlock here: .* is not inside a Git worktree\. Name one with 'treerig worktree unlock <worktree>'\.$`)
		_, err = unlock(t, w.ContextWithoutConfig(""), commands.UnlockOptions{Shared: true})
		wantErr(t, err, `Name one with 'treerig worktree unlock --shared <worktree>'\.$`)
	})

	t.Run("in a worktree whose treerig.toml is unusable unlocks that worktree by default", func(t *testing.T) {
		w, mine, _ := twoRepositories(t)
		killDeadHolder(t, w.ControlHome, mine)

		result, err := unlock(t, w.ContextWithoutConfig(w.Root), commands.UnlockOptions{})
		if err != nil || len(result.Cleared) != 2 {
			t.Fatalf("result = %+v, %v", result, err)
		}
	})
}
