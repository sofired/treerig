package commands_test

// What `list` shows beyond a row's state (#204): the URL column and when it is
// filled, the footers a terminal gets, `--refresh`, and the repositories other
// than the one the command runs in.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/commands/commandstest"
	"github.com/sofired/treerig/internal/overview/card"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/worktree"
)

// routedCard is a worktree's card as an `up` leaves it: api at the host root,
// mail under its subdomain.
func routedCard(w *commandstest.World, ref worktree.Ref, domain, root string) {
	host := ref.Slug + "." + domain
	w.WriteRoutedCard(ref, "acme", domain, root, "main",
		card.IndexService{Name: "api", URL: "http://" + host},
		card.IndexService{Name: "mail", URL: "http://mail." + host})
}

func TestListShowsURLsOnlyWhileTheyLoad(t *testing.T) {
	ctx := context.Background()
	registered := func(t *testing.T) (*commandstest.World, *commandstest.TestContext) {
		t.Helper()
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api", "mail")
		routedCard(w, ref, acme, w.Root)
		return w, w.Context("", acme, nil)
	}
	row := func(tc *commandstest.TestContext) string {
		t.Helper()
		_, printed := tc.Logger.Snapshot()
		if len(printed) != 2 {
			t.Fatalf("printed = %q, want a header and a row", printed)
		}
		return printed[1]
	}

	t.Run("names the first running service's URL, and how many others there are", func(t *testing.T) {
		w, tc := registered(t)
		w.Docker.AddRouter(true)
		if _, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := row(tc); !strings.Contains(got, "http://feature-login.acme.localhost +1 more ") {
			t.Fatalf("row = %q, want api's URL and +1 more", got)
		}
		if lines, _ := tc.Logger.Snapshot(); len(lines) != 0 {
			t.Fatalf("stderr = %q, want nothing off a terminal", lines)
		}
	})

	t.Run("shows only the URL of a service that runs", func(t *testing.T) {
		w, tc := registered(t)
		w.Docker.AddRouter(true)
		w.Docker.SetStatus(w.Docker.Find("container", map[string]string{"com.docker.compose.service": "api"})[0], "exited")
		report, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := row(tc); !strings.Contains(got, "http://mail.feature-login.acme.localhost  ") || strings.Contains(got, "more") {
			t.Fatalf("row = %q, want only mail's URL", got)
		}
		if urls := report.Entries[0].URLs; len(urls) != 1 || urls[0].Service != "mail" {
			t.Fatalf("urls = %+v", urls)
		}
	})

	t.Run("shows - for a worktree that is stopped, whatever the router does", func(t *testing.T) {
		w, tc := registered(t)
		w.Docker.AddRouter(true)
		for _, id := range w.Docker.Find("container", nil) {
			w.Docker.SetStatus(id, "exited")
		}
		if _, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := row(tc); !strings.HasPrefix(got, "*  feature-login  stopped  -  ") {
			t.Fatalf("row = %q", got)
		}
		if lines, _ := tc.Logger.Snapshot(); len(lines) != 0 {
			t.Fatalf("stderr = %q: nothing is waiting on the router", lines)
		}
	})

	t.Run("shows - and says why on stderr while the router is stopped", func(t *testing.T) {
		for name, router := range map[string]func(*commandstest.World){
			"no router container": func(*commandstest.World) {},
			"an exited one":       func(w *commandstest.World) { w.Docker.AddRouter(false) },
		} {
			t.Run(name, func(t *testing.T) {
				w, tc := registered(t)
				router(w)
				report, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if got := row(tc); !strings.HasPrefix(got, "*  feature-login  running  -  ") {
					t.Fatalf("row = %q", got)
				}
				lines, _ := tc.Logger.Snapshot()
				if !slices.Equal(lines, []string{"info: The router is stopped, so no URL loads. Run 'treerig up' to start it."}) {
					t.Fatalf("stderr = %q", lines)
				}
				if report.Router == nil || report.Router.State != commands.RouterStopped || len(report.Entries[0].URLs) != 0 {
					t.Fatalf("report = %+v", report)
				}
			})
		}
	})

	t.Run("shows no URL of a router that serves another port", func(t *testing.T) {
		w, tc := registered(t)
		w.Docker.AddRouterOnPort(true, 8080)
		if _, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := row(tc); !strings.HasPrefix(got, "*  feature-login  running  -  ") {
			t.Fatalf("row = %q", got)
		}
		lines, _ := tc.Logger.Snapshot()
		if !slices.Equal(lines, []string{"info: The router runs on port 8080, not 80. Run 'treerig router up'."}) {
			t.Fatalf("stderr = %q", lines)
		}
	})

	t.Run("drops a URL the card gives for another router's port", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		w.WriteRoutedCard(ref, "acme", acme, w.Root, "main", card.IndexService{Name: "api", URL: "http://feature-login.acme.localhost:8080"})
		w.Docker.AddRouter(true)
		tc := w.Context("", acme, nil)
		if _, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := row(tc); !strings.HasPrefix(got, "*  feature-login  running  -  ") {
			t.Fatalf("row = %q", got)
		}
	})

	t.Run("keeps a card's URL from driving the terminal", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		w.WriteRoutedCard(ref, "acme\u001b[2K", acme, w.Root, "main", card.IndexService{Name: "api", URL: "http://feature-login.acme.localhost"})
		w.Docker.AddRouter(true)
		tc := w.Context("", acme, nil)
		if _, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{All: true}); err != nil {
			t.Fatal(err)
		}
		if table := strings.Join(tc.Logger.Printed, "\n"); strings.ContainsRune(table, '\u001b') || !strings.Contains(table, "acme�[2K") {
			t.Fatalf("table = %q", table)
		}
	})
}

func TestListFootersForATerminal(t *testing.T) {
	ctx := context.Background()
	// A repository with two worktrees whose directories are gone, one that runs.
	gone := func(t *testing.T, count int) (*commandstest.World, *commandstest.TestContext) {
		t.Helper()
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		for _, name := range []string{"old-spike", "old-idea"}[:count] {
			root := w.Worktree(name, "feature/"+name)
			ref := w.Register(root, acme, name, brand.LabelNamespace, "api")
			// What `up` left, so the directory it was in can be told gone.
			w.WriteIndexCard(ref.RepositoryID, ref.WorktreeID, "acme", acme, name, name+"."+acme, root, "feature/"+name)
			if err := os.RemoveAll(root); err != nil {
				t.Fatal(err)
			}
		}
		tc := w.Context("", acme, nil)
		tc.StderrIsTTY = true
		return w, tc
	}

	t.Run("count the directories that are gone, singular and plural, and name where the rest is", func(t *testing.T) {
		for count, want := range map[int]string{
			1: "info: 1 directory is gone: 'treerig worktree remove old-spike' removes its worktree.",
			2: "info: 2 directories are gone: 'treerig worktree remove <slug>' removes one worktree.",
		} {
			w, tc := gone(t, count)
			w.Docker.AddRouter(true)
			if _, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{}); err != nil {
				t.Fatal(err)
			}
			lines, _ := tc.Logger.Snapshot()
			wantLines := []string{want, "info: Every repository: 'treerig list --all', or http://treerig.localhost"}
			if !slices.Equal(lines, wantLines) {
				t.Fatalf("%d gone: stderr = %q, want %q", count, lines, wantLines)
			}
		}
	})

	t.Run("name the router's port in the overview's address when it is not 80", func(t *testing.T) {
		w, tc := gone(t, 0)
		tc.RouterPort = 8080
		w.Docker.AddRouterOnPort(true, 8080)
		if _, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{}); err != nil {
			t.Fatal(err)
		}
		lines, _ := tc.Logger.Snapshot()
		if !slices.Equal(lines, []string{"info: Every repository: 'treerig list --all', or http://treerig.localhost:8080"}) {
			t.Fatalf("stderr = %q", lines)
		}
	})

	t.Run("leave out the overview's address while the router is not running", func(t *testing.T) {
		_, tc := gone(t, 0)
		if _, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{}); err != nil {
			t.Fatal(err)
		}
		lines, _ := tc.Logger.Snapshot()
		// The notice that no URL loads is not a footer: nothing runs here that would show one.
		if slices.ContainsFunc(lines, func(line string) bool { return strings.Contains(line, "Every repository") }) {
			t.Fatalf("stderr = %q", lines)
		}
	})

	t.Run("do not point at --all when the listing is every repository", func(t *testing.T) {
		w, tc := gone(t, 1)
		w.Docker.AddRouter(true)
		if _, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{All: true}); err != nil {
			t.Fatal(err)
		}
		lines, _ := tc.Logger.Snapshot()
		// A slug names a worktree only within its domain: the host names it anywhere.
		if !slices.Equal(lines, []string{"info: 1 directory is gone: 'treerig worktree remove old-spike.acme.localhost' removes its worktree."}) {
			t.Fatalf("stderr = %q", lines)
		}
	})

	t.Run("are not printed off a terminal, or with --json", func(t *testing.T) {
		w, tc := gone(t, 2)
		w.Docker.AddRouter(true)
		tc.StderrIsTTY = false
		if _, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{}); err != nil {
			t.Fatal(err)
		}
		if lines, _ := tc.Logger.Snapshot(); len(lines) != 0 {
			t.Fatalf("stderr = %q off a terminal", lines)
		}
		tc = w.Context("", acme, nil)
		tc.StderrIsTTY = true
		if _, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{JSON: true}); err != nil {
			t.Fatal(err)
		}
		if lines, _ := tc.Logger.Snapshot(); len(lines) != 0 {
			t.Fatalf("stderr = %q with --json", lines)
		}
	})
}

func TestListRefresh(t *testing.T) {
	ctx := context.Background()

	t.Run("needs --all inside a repository, and says so before it reads anything", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		tc := w.Context("", acme, nil)
		_, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{Refresh: true})
		if err == nil || err.Error() != "--refresh rebuilds the overview, which lists every repository: add --all." {
			t.Fatalf("err = %v", err)
		}
		if len(tc.Logger.Printed) != 0 {
			t.Fatalf("printed = %q", tc.Logger.Printed)
		}
		if _, statErr := os.Stat(w.RouterState.IndexFile); !os.IsNotExist(statErr) {
			t.Fatalf("the page was written: %v", statErr)
		}
	})

	t.Run("rebuilds the page with --all, and from outside a repository without it", func(t *testing.T) {
		for name, options := range map[string]commands.ListOptions{
			"--all":   {All: true, Refresh: true},
			"outside": {Refresh: true},
		} {
			t.Run(name, func(t *testing.T) {
				w := commandstest.New(t, "feature/login", acme)
				w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
				tc := w.Context("", acme, nil)
				if name == "outside" {
					tc = w.ContextWithoutConfig("")
				}
				if _, err := commands.ListWithContext(ctx, tc.CommandContext, options); err != nil {
					t.Fatal(err)
				}
				page, err := os.ReadFile(w.RouterState.IndexFile)
				if err != nil || !strings.Contains(string(page), "feature-login.acme.localhost") {
					t.Fatalf("page = %q, %v", page, err)
				}
				if len(tc.Logger.Printed) != 2 {
					t.Fatalf("printed = %q, want the table too", tc.Logger.Printed)
				}
			})
		}
	})

	t.Run("writes nothing and takes no lock without it", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		for _, options := range []commands.ListOptions{{}, {All: true}, {JSON: true}} {
			tc := w.Context("", acme, nil)
			if _, err := commands.ListWithContext(ctx, tc.CommandContext, options); err != nil {
				t.Fatal(err)
			}
		}
		if entries, err := os.ReadDir(w.RouterState.StateDir); err == nil {
			t.Fatalf("the router's state directory was made: %v", entries)
		}
	})
}

func TestListOtherRepositories(t *testing.T) {
	ctx := context.Background()

	t.Run("asks Git about each repository, so a directory gone from another one is told", func(t *testing.T) {
		w, _, theirs := twoRepositories(t)
		widgets := filepath.Join(w.Base, "widgets")
		w.WriteRoutedCard(theirs, "widgets", "widgets.localhost", widgets, "main")
		if err := os.RemoveAll(widgets); err != nil {
			t.Fatal(err)
		}
		tc := w.ContextWithoutConfig("")
		report, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		states := map[string]worktree.State{}
		for _, entry := range report.Entries {
			states[entry.Slug] = entry.State
		}
		if states["main"] != worktree.StateDirectoryMissing || states["feature-login"] != worktree.StateRunning {
			t.Fatalf("states = %v", states)
		}
	})

	t.Run("shows a repository Git does not answer for as unknown, with the cause said once", func(t *testing.T) {
		w, _, theirs := twoRepositories(t)
		widgets := filepath.Join(w.Base, "widgets")
		w.WriteRoutedCard(theirs, "widgets", "widgets.localhost", widgets, "main")
		inner := w.Runner.Handler
		w.Runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
			for i, arg := range spec.Args {
				if arg == "-C" && i+1 < len(spec.Args) && spec.Args[i+1] == widgets && slices.Contains(spec.Args, "worktree") {
					return run.Result{ExitCode: 128, Stderr: "fatal: not a git repository\n"}, nil
				}
			}
			return inner(ctx, spec)
		}
		tc := w.ContextWithoutConfig("")
		report, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		states := map[string]worktree.State{}
		for _, entry := range report.Entries {
			states[entry.Slug] = entry.State
		}
		if states["main"] != worktree.StateUnknown || states["feature-login"] != worktree.StateRunning {
			t.Fatalf("states = %v", states)
		}
		lines, _ := tc.Logger.Snapshot()
		warned := 0
		for _, line := range lines {
			if strings.HasPrefix(line, "warn: Git could not list the worktrees of 1 repository") {
				warned++
			}
			if strings.Contains(line, "main.widgets.localhost is unknown") {
				t.Errorf("the cause was repeated for the row: %q", line)
			}
		}
		if warned != 1 {
			t.Fatalf("warned %d times, want once: %q", warned, lines)
		}
	})
}

func TestListWithNothingToList(t *testing.T) {
	ctx := context.Background()

	t.Run("asks Docker nothing, so it needs no Docker to say so, and leaves the router out of the document", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Docker.FailCall("ps", dockerDown, 1, -1)
		tc := w.ContextWithoutConfig("")
		report, err := commands.ListWithContext(ctx, tc.CommandContext, commands.ListOptions{JSON: true})
		if err != nil {
			t.Fatal(err)
		}
		if report.Router != nil || !report.DockerReachable || report.Entries == nil || len(report.Entries) != 0 {
			t.Fatalf("report = %+v", report)
		}
		if calls := w.Docker.Calls(); slices.ContainsFunc(calls, func(call string) bool { return strings.HasPrefix(call, "ps ") }) {
			t.Fatalf("docker was asked to list containers: %v", calls)
		}
	})
}
