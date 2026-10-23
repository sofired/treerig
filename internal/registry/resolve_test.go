package registry_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/checkoutindex/card"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
)

// Worktrees of the resolver's world. Every UUID is distinct so a wrong match
// is visible in the failure message.
const (
	repoOne = "11111111-1111-4111-8111-111111111111"
	repoTwo = "22222222-2222-4222-8222-222222222222"

	idApp     = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" // /repo/app, repo one
	idApp2    = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb" // /repo/app2, repo one: shares app's prefix
	idNested  = "cccccccc-cccc-4ccc-8ccc-cccccccccccc" // /repo/app/nested, repo two: inside app's directory
	idLinked  = "dddddddd-dddd-4ddd-8ddd-dddddddddddd" // recorded through a symbolic link
	idNoCard  = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee" // registered, never brought up
	idHome    = "ffffffff-ffff-4fff-8fff-ffffffffffff" // under the home directory
	idTwinOne = "01010101-0101-4101-8101-010101010101" // slug "twin" in repo one
	idTwinTwo = "02020202-0202-4202-8202-020202020202" // slug "twin" in repo two, another domain
	idLone    = "03030303-0303-4303-8303-030303030303" // slug "lone", repo two only
	idDupOne  = "04040404-0404-4404-8404-040404040404" // host dup.acme.localhost, claimed twice
	idDupTwo  = "05050505-0505-4505-8505-050505050505"

	widgetsDomain = "widgets.localhost"
)

// resolveWorld is a control home with registered worktrees, their index
// cards and directories on disk.
type resolveWorld struct {
	base, home, cwd string
	reg             *registry.Registry
	roots           map[string]string
}

// newResolveWorld registers the worktrees above. The directories exist, so
// symbolic links and relative paths resolve for real.
func newResolveWorld(t *testing.T) *resolveWorld {
	t.Helper()
	base := registrytest.TempDir(t)
	w := &resolveWorld{base: base, home: filepath.Join(base, "home"), roots: map[string]string{}}
	controlHome := filepath.Join(base, "control")
	for _, dir := range []string{"repo/app/src", "repo/app/nested/pkg", "repo/app2/src", "real/linked", "home/work/tree"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(base, "real", "linked"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	w.cwd = filepath.Join(base, "repo", "app", "src")

	add := func(repo, id, slug, domain, root string) {
		t.Helper()
		writeMinimalRecord(t, controlHome, repo, id, slug, domain, "complete")
		if root == "" {
			return
		}
		w.roots[id] = root
		if err := os.MkdirAll(layout.WorktreeLayout(controlHome, repo, id).Home, 0o700); err != nil {
			t.Fatal(err)
		}
		err := card.WriteIndexCard(controlHome, repo, id, card.IndexCard{
			Project: "p", Domain: domain, Slug: slug, Host: slug + "." + domain, Root: root, Branch: "main",
			LabelNamespace: "dev.treerig", UpdatedAt: "2026-01-01T00:00:00.000Z",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	add(repoOne, idApp, "app", acmeDomain, filepath.Join(base, "repo", "app"))
	add(repoOne, idApp2, "app2", acmeDomain, filepath.Join(base, "repo", "app2"))
	add(repoTwo, idNested, "nested", widgetsDomain, filepath.Join(base, "repo", "app", "nested"))
	// Recorded through the link, as an older card or a hand-built one might be.
	add(repoTwo, idLinked, "linked", widgetsDomain, filepath.Join(base, "link"))
	add(repoTwo, idNoCard, "nocard", widgetsDomain, "")
	add(repoTwo, idHome, "homed", widgetsDomain, filepath.Join(base, "home", "work", "tree"))
	add(repoOne, idTwinOne, "twin", acmeDomain, "")
	add(repoTwo, idTwinTwo, "twin", widgetsDomain, "")
	add(repoTwo, idLone, "lone", widgetsDomain, "")
	add(repoOne, idDupOne, "dup", acmeDomain, "")
	add(repoTwo, idDupTwo, "dup", acmeDomain, "")

	w.reg = registry.New(registry.Options{ControlHome: controlHome, Root: filepath.Join(base, "somewhere")})
	return w
}

// options is the NameOptions of a caller run from the world's working
// directory, with its home directory.
func (w *resolveWorld) options() registry.NameOptions {
	return registry.NameOptions{Cwd: w.cwd, Home: w.home}
}

// scopeOf is a *string for NameOptions.RepositoryID.
func scopeOf(id string) *string { return &id }

func TestResolve(t *testing.T) {
	ctx := context.Background()
	w := newResolveWorld(t)
	root := func(parts ...string) string { return filepath.Join(append([]string{w.base}, parts...)...) }

	type tc struct {
		title string
		name  string
		with  func(*registry.NameOptions)
		want  string // the UUID resolved; empty when an error is wanted
		fail  string // substring of the error
		none  bool   // errors.Is(err, registry.ErrNoWorktree)
	}
	within := func(o *registry.NameOptions) { o.WithinDirectory = true }
	inRepo := func(id string) func(*registry.NameOptions) {
		return func(o *registry.NameOptions) { o.RepositoryID = scopeOf(id) }
	}
	outside := inRepo("")

	cases := []tc{
		// A UUID.
		{title: "a UUID names that worktree, in any repository", name: idLone, want: idLone},
		{title: "a UUID is read in either case", name: strings.ToUpper(idLone), want: idLone},
		{title: "a UUID that is not registered is not found", name: "99999999-9999-4999-8999-999999999999", fail: "No worktree is registered as 99999999", none: true},
		{title: "a UUID is never read as a slug, even when --domain is given", name: idLone, with: func(o *registry.NameOptions) { o.Domain = acmeDomain }, want: idLone},
		// A host.
		{title: "an exact host names that worktree", name: "lone." + widgetsDomain, want: idLone},
		{title: "a host wins over every other repository's claim on the same slug", name: "twin." + widgetsDomain, want: idTwinTwo, with: inRepo(repoOne)},
		{title: "a host claimed by two worktrees lists both with their UUIDs", name: "dup." + acmeDomain, fail: "'dup.acme.localhost' names more than one worktree: dup.acme.localhost (" + idDupOne + "), dup.acme.localhost (" + idDupTwo + ")"},
		{title: "a host is read without regard to case", name: "LONE." + strings.ToUpper(widgetsDomain), want: idLone},
		{title: "a host that is not registered is not found", name: "lone.acme.localhost", fail: "No worktree is registered as lone.acme.localhost.", none: true},
		// A slug.
		{title: "a slug unique on the machine, from outside a repository", name: "lone", with: outside, want: idLone},
		{title: "a slug unique on the machine, from another repository", name: "lone", with: inRepo(repoOne), want: idLone},
		{title: "a slug the current repository has wins over another repository's", name: "twin", with: inRepo(repoOne), want: idTwinOne},
		{title: "the same slug from the other repository", name: "twin", with: inRepo(repoTwo), want: idTwinTwo},
		{title: "a slug two repositories share is ambiguous outside a repository, listing hosts and UUIDs", name: "twin", with: outside,
			fail: "'twin' names more than one worktree: twin.acme.localhost (" + idTwinOne + "), twin.widgets.localhost (" + idTwinTwo + "). Name one by its host or its UUID, or add --domain."},
		{title: "a slug two repositories share is ambiguous from a third", name: "twin", with: inRepo("33333333-3333-4333-8333-333333333333"), fail: "names more than one worktree"},
		{title: "--domain narrows a shared slug", name: "twin", with: func(o *registry.NameOptions) { o.Domain = widgetsDomain; o.RepositoryID = scopeOf("") }, want: idTwinTwo},
		{title: "--domain with no worktree under it says so", name: "twin", with: func(o *registry.NameOptions) { o.Domain = "nowhere.localhost" }, fail: "No worktree is registered as twin under nowhere.localhost.", none: true},
		{title: "--domain does not narrow a host or a UUID", name: "lone." + widgetsDomain, with: func(o *registry.NameOptions) { o.Domain = acmeDomain }, want: idLone},
		{title: "an unknown name is not found", name: "nope", fail: "No worktree is registered as nope.", none: true},
		{title: "a slug is never read as a directory, even when one is called that", name: "src", fail: "No worktree is registered as src.", none: true},
		// A path.
		{title: "an absolute path to a root", name: root("repo", "app2"), want: idApp2},
		{title: "a root with a trailing slash", name: root("repo", "app2") + "/", want: idApp2},
		{title: "a root with dot segments", name: root("repo", "x", "..", "app2"), want: idApp2},
		{title: "'.' is the working directory", name: ".", with: func(o *registry.NameOptions) { o.Cwd = root("repo", "app2") }, want: idApp2},
		{title: "'..' is its parent", name: "..", with: func(o *registry.NameOptions) { o.Cwd = root("repo", "app", "nested", "pkg") }, want: idNested},
		{title: "'./x' reads from the working directory", name: "./nested", with: func(o *registry.NameOptions) { o.Cwd = root("repo", "app") }, want: idNested},
		{title: "'../x' reads from the working directory", name: "../app2", want: idApp2, with: func(o *registry.NameOptions) { o.Cwd = root("repo", "app") }},
		{title: "'~' is the home directory itself, which is inside a worktree's parent, not a root", name: "~", fail: "No worktree is registered with its directory at " + w.home + ".", none: true},
		{title: "'~/x' expands from the home value passed in", name: "~/work/tree", want: idHome},
		{title: "'~user' is refused, not expanded", name: "~root/x", fail: "'~root/x' names another user's home, which treerig does not expand. Write the full path, or '~/<path>' for your own."},
		{title: "'~' with no home known is refused", name: "~/work/tree", with: func(o *registry.NameOptions) { o.Home = "" }, fail: "Cannot expand '~/work/tree': your home directory is not known."},
		{title: "a path through a symbolic link finds the root recorded as the real directory", name: root("link"), want: idLinked},
		{title: "a path through the real directory finds the root recorded as a link", name: root("real", "linked"), want: idLinked},
		{title: "a path matching no recorded root is not found", name: root("real"), fail: "No worktree is registered with its directory at " + root("real") + ". (A worktree that was never brought up has no directory on record; name it by its host or UUID.)", none: true},
		{title: "a worktree with no card has no directory to match", name: root("elsewhere"), fail: "No worktree is registered with its directory at", none: true},
		// Nesting and whole-component matching.
		{title: "a directory nested in another worktree resolves to the inner one", name: root("repo", "app", "nested"), want: idNested},
		{title: "the outer worktree's own root is still the outer one", name: root("repo", "app"), want: idApp},
		{title: "a path inside the inner worktree is inside the inner one, for a read-only caller", name: root("repo", "app", "nested", "pkg"), with: within, want: idNested},
		{title: "a path inside the outer worktree but not the inner is the outer one, for a read-only caller", name: root("repo", "app", "src"), with: within, want: idApp},
		{title: "a sibling that shares a prefix is never inside: /repo/app2/src is not under /repo/app", name: root("repo", "app2", "src"), with: within, want: idApp2},
		{title: "a path beside a root whose name only starts with it matches nothing", name: root("repo", "app3"), with: within, fail: "No worktree is registered with a directory containing " + root("repo", "app3"), none: true},
		// Inside a directory, not at its root.
		{title: "a destructive caller refuses a path inside a directory, naming the root", name: root("repo", "app", "src"), with: func(o *registry.NameOptions) { o.Cwd = w.base },
			fail: "'" + root("repo", "app", "src") + "' is inside the directory of app." + acmeDomain + " (" + idApp + "), not its root. Name the worktree by its host, its UUID or its root directory, " + root("repo", "app") + "."},
		{title: "from inside the worktree, '.' is told to run the command with no name", name: ".", with: func(o *registry.NameOptions) { o.Cwd = root("repo", "app", "src") },
			fail: "'.' is inside the directory of app." + acmeDomain + " (" + idApp + "), not its root. Run the command with no name from here, or name the worktree by its host, its UUID or its root directory, " + root("repo", "app") + "."},
		{title: "a destructive caller refuses a path inside the inner worktree, naming the inner root", name: root("repo", "app", "nested", "pkg"), fail: "inside the directory of nested." + widgetsDomain},
		{title: "a destructive caller accepts a root", name: root("repo", "app", "nested"), want: idNested},
		{title: "a read-only caller accepts a root", name: root("repo", "app", "nested"), with: within, want: idNested},
		{title: "a read-only caller with no worktree around the path says it contains none", name: root("real"), with: within, fail: "No worktree is registered with a directory containing " + root("real") + ".", none: true},
	}
	for _, c := range cases {
		t.Run(c.title, func(t *testing.T) {
			opts := w.options()
			if c.with != nil {
				c.with(&opts)
			}
			got, err := w.reg.Resolve(ctx, c.name, opts)
			if c.fail != "" {
				if err == nil {
					t.Fatalf("Resolve(%q) = %+v, want an error containing %q", c.name, got, c.fail)
				}
				if !strings.Contains(err.Error(), c.fail) {
					t.Fatalf("Resolve(%q) error = %q, want it to contain %q", c.name, err, c.fail)
				}
				if errors.Is(err, registry.ErrNoWorktree) != c.none {
					t.Fatalf("errors.Is(err, ErrNoWorktree) = %v, want %v for %q", !c.none, c.none, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%q): %v", c.name, err)
			}
			if got == nil || got.Record.WorktreeID != c.want {
				t.Fatalf("Resolve(%q) = %+v, want %s", c.name, got, c.want)
			}
		})
	}

	t.Run("a path to a directory that was deleted still finds the root recorded there", func(t *testing.T) {
		gone := root("repo", "app2")
		if err := os.RemoveAll(gone); err != nil {
			t.Fatal(err)
		}
		got, err := w.reg.Resolve(ctx, gone, w.options())
		if err != nil || got == nil || got.Record.WorktreeID != idApp2 {
			t.Fatalf("got %+v, %v", got, err)
		}
	})

	t.Run("an empty name is the caller's mistake, not a lookup", func(t *testing.T) {
		if _, err := w.reg.Resolve(ctx, "", w.options()); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("a relative path with no working directory is refused", func(t *testing.T) {
		if _, err := w.reg.Resolve(ctx, "./x", registry.NameOptions{}); err == nil || !strings.Contains(err.Error(), "Cwd") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestResolveTwoRecordsOneDirectory(t *testing.T) {
	ctx := context.Background()
	base := registrytest.TempDir(t)
	controlHome := filepath.Join(base, "control")
	shared := filepath.Join(base, "dir")
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, rec := range []struct{ repo, id, slug string }{{repoOne, idApp, "first"}, {repoTwo, idApp2, "second"}} {
		writeMinimalRecord(t, controlHome, rec.repo, rec.id, rec.slug, acmeDomain, "complete")
		if err := os.MkdirAll(layout.WorktreeLayout(controlHome, rec.repo, rec.id).Home, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := card.WriteIndexCard(controlHome, rec.repo, rec.id, card.IndexCard{
			Project: "p", Domain: acmeDomain, Slug: rec.slug, Host: rec.slug + "." + acmeDomain, Root: shared,
			LabelNamespace: "dev.treerig", UpdatedAt: "2026-01-01T00:00:00.000Z",
		}); err != nil {
			t.Fatal(err)
		}
	}
	reg := registry.New(registry.Options{ControlHome: controlHome, Root: base})
	_, err := reg.Resolve(ctx, shared, registry.NameOptions{})
	if err == nil || !strings.Contains(err.Error(), "first.acme.localhost ("+idApp+"), second.acme.localhost ("+idApp2+")") {
		t.Fatalf("expected both worktrees listed, got %v", err)
	}
}

func TestResolveAsksGitForTheRepositoryOnlyWhenItMatters(t *testing.T) {
	ctx := context.Background()

	t.Run("a slug the machine has once never asks Git", func(t *testing.T) {
		w := newResolveWorld(t)
		// This registry's root is no repository at all; asking Git would fail.
		got, err := w.reg.Resolve(ctx, "lone", w.options())
		if err != nil || got == nil || got.Record.WorktreeID != idLone {
			t.Fatalf("got %+v, %v", got, err)
		}
	})

	t.Run("a shared slug asks Git, and prefers the repository it is in", func(t *testing.T) {
		f := newRegistryFixture(t, "feature/login", acmeDomain)
		identity, err := registrytest.Register(ctx, f.controlHome, f.registry.Identities, f.root, acmeDomain, "feature-login", nil, "complete")
		if err != nil {
			t.Fatal(err)
		}
		writeMinimalRecord(t, f.controlHome, repoB, wtB, "feature-login", "widgets.localhost", "complete")

		got, err := f.registry.Resolve(ctx, "feature-login", registry.NameOptions{})
		if err != nil || got == nil || got.Record.WorktreeID != identity.WorktreeID {
			t.Fatalf("got %+v, %v; want this repository's own worktree", got, err)
		}
	})

	t.Run("a shared slug outside every repository is ambiguous", func(t *testing.T) {
		f := newRegistryFixture(t, "feature/login", acmeDomain)
		writeMinimalRecord(t, f.controlHome, repoA, wtA, "feature-login", acmeDomain, "complete")
		writeMinimalRecord(t, f.controlHome, repoB, wtB, "feature-login", "widgets.localhost", "complete")
		outside := registry.New(registry.Options{
			ControlHome: f.controlHome, Root: filepath.Join(f.base, "not-a-repository"), Runner: f.runner, Logger: f.log,
		})

		_, err := outside.Resolve(ctx, "feature-login", registry.NameOptions{})
		if err == nil || !strings.Contains(err.Error(), "names more than one worktree") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestNameCandidates(t *testing.T) {
	ctx := context.Background()
	w := newResolveWorld(t)

	names := func(opts registry.NameOptions) map[string]string {
		t.Helper()
		got, err := w.reg.NameCandidates(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		byID := map[string]string{}
		for _, c := range got {
			byID[c.Record.WorktreeID] = c.Name
		}
		return byID
	}

	t.Run("a slug that names its worktree alone is offered as the slug", func(t *testing.T) {
		got := names(registry.NameOptions{RepositoryID: scopeOf("")})
		if got[idLone] != "lone" || got[idApp] != "app" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("a slug two repositories share is offered as its host outside a repository, and every offered name resolves back", func(t *testing.T) {
		opts := registry.NameOptions{RepositoryID: scopeOf("")}
		got := names(opts)
		if got[idTwinOne] != "twin."+acmeDomain || got[idTwinTwo] != "twin."+widgetsDomain {
			t.Fatalf("got %v", got)
		}
		for id, name := range got {
			if id == idDupOne || id == idDupTwo {
				continue
			}
			resolved, err := w.reg.Resolve(ctx, name, registry.NameOptions{RepositoryID: scopeOf("")})
			if err != nil || resolved.Record.WorktreeID != id {
				t.Fatalf("offered %q for %s, which resolves to %+v, %v", name, id, resolved, err)
			}
		}
	})

	t.Run("the current repository's own worktree keeps the slug, the other is offered by host", func(t *testing.T) {
		got := names(registry.NameOptions{RepositoryID: scopeOf(repoOne)})
		if got[idTwinOne] != "twin" || got[idTwinTwo] != "twin."+widgetsDomain {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("a host two worktrees claim is offered by UUID", func(t *testing.T) {
		got := names(registry.NameOptions{RepositoryID: scopeOf("")})
		if got[idDupOne] != idDupOne || got[idDupTwo] != idDupTwo {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("--domain leaves out the other domains and narrows a shared slug", func(t *testing.T) {
		got := names(registry.NameOptions{Domain: widgetsDomain, RepositoryID: scopeOf("")})
		if _, ok := got[idApp]; ok {
			t.Fatalf("an acme worktree was offered under --domain %s: %v", widgetsDomain, got)
		}
		if got[idTwinTwo] != "twin" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("each carries the directory its card recorded, and is sorted by slug", func(t *testing.T) {
		got, err := w.reg.NameCandidates(ctx, registry.NameOptions{RepositoryID: scopeOf("")})
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i < len(got); i++ {
			if got[i-1].Record.Slug > got[i].Record.Slug {
				t.Fatalf("not sorted by slug: %v then %v", got[i-1].Record.Slug, got[i].Record.Slug)
			}
		}
		for _, c := range got {
			if want := w.roots[c.Record.WorktreeID]; c.Root != want {
				t.Fatalf("%s: root %q, want %q", c.Record.Slug, c.Root, want)
			}
		}
	})

	t.Run("an empty registry offers nothing", func(t *testing.T) {
		empty := registry.New(registry.Options{ControlHome: filepath.Join(registrytest.TempDir(t), "control"), Root: w.base})
		got, err := empty.NameCandidates(ctx, registry.NameOptions{})
		if err != nil || len(got) != 0 {
			t.Fatalf("got %v, %v", got, err)
		}
	})
}

func TestIsPathName(t *testing.T) {
	for _, c := range []struct {
		name string
		want bool
	}{
		{".", true}, {"..", true}, {"/", true}, {"/repo/app", true}, {"./app", true}, {"../app", true}, {"~", true}, {"~/app", true}, {"~user", true},
		{"app", false}, {"app/src", false}, {"...", false}, {".hidden", false}, {"..x", false}, {"feature-x.acme.localhost", false},
		{idApp, false}, {"", false}, {"a~", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := registry.IsPathName(c.name); got != c.want {
				t.Fatalf("IsPathName(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

func TestIsUUID(t *testing.T) {
	for _, c := range []struct {
		name string
		want bool
	}{
		{idApp, true}, {strings.ToUpper(idApp), true}, {"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", true},
		{"", false}, {"app", false}, {idApp + "0", false}, {strings.ReplaceAll(idApp, "-", ""), false}, {"gggggggg-gggg-4ggg-8ggg-gggggggggggg", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := registry.IsUUID(c.name); got != c.want {
				t.Fatalf("IsUUID(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

// registerWithCard writes a complete registration and an index card that
// records root, the way a finished `up` leaves them.
func registerWithCard(t *testing.T, controlHome, repo, id, slug, domain, root string) {
	t.Helper()
	writeMinimalRecord(t, controlHome, repo, id, slug, domain, "complete")
	if err := os.MkdirAll(layout.WorktreeLayout(controlHome, repo, id).Home, 0o700); err != nil {
		t.Fatal(err)
	}
	err := card.WriteIndexCard(controlHome, repo, id, card.IndexCard{
		Project: "p", Domain: domain, Slug: slug, Host: slug + "." + domain, Root: root, Branch: "main",
		LabelNamespace: "dev.treerig", UpdatedAt: "2026-01-01T00:00:00.000Z",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestResolvedCarriesTheRecordedRoot(t *testing.T) {
	ctx := context.Background()
	w := newResolveWorld(t)
	for _, c := range []struct {
		name, id, root string
	}{
		{idApp, idApp, w.roots[idApp]},
		{"app", idApp, w.roots[idApp]},
		{"app." + acmeDomain, idApp, w.roots[idApp]},
		{w.roots[idApp], idApp, w.roots[idApp]},
		{"nocard", idNoCard, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := w.reg.Resolve(ctx, c.name, registry.NameOptions{RepositoryID: scopeOf("")})
			if err != nil || got.Record.WorktreeID != c.id || got.Root != c.root {
				t.Fatalf("got %+v, %v; want %s at %q", got, err, c.id, c.root)
			}
		})
	}
}

func TestResolveNamesADirectoryByFileIdentity(t *testing.T) {
	ctx := context.Background()
	base := registrytest.TempDir(t)
	controlHome := filepath.Join(base, "control")
	if err := os.MkdirAll(filepath.Join(base, "Repo", "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "repo")); err != nil {
		t.Skip("the filesystem tells Repo and repo apart, so there is no second spelling to name")
	}
	registerWithCard(t, controlHome, repoOne, idApp, "app", acmeDomain, filepath.Join(base, "Repo"))
	reg := registry.New(registry.Options{ControlHome: controlHome, Root: base})

	got, err := reg.Resolve(ctx, filepath.Join(base, "repo"), registry.NameOptions{})
	if err != nil || got.Record.WorktreeID != idApp {
		t.Fatalf("the root spelled in the other case: %+v, %v", got, err)
	}
	_, err = reg.Resolve(ctx, filepath.Join(base, "repo", "src"), registry.NameOptions{})
	if err == nil || !strings.Contains(err.Error(), "not its root") {
		t.Fatalf("a directory inside it, spelled in the other case, is inside it: %v", err)
	}
}

func TestResolveFailsWhereSkippingCouldNameTheWrongWorktree(t *testing.T) {
	ctx := context.Background()
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory, so nothing here is unreadable")
	}

	t.Run("a recorded directory that cannot be resolved fails the lookup, naming the worktree and the way out", func(t *testing.T) {
		base := registrytest.TempDir(t)
		controlHome := filepath.Join(base, "control")
		locked := filepath.Join(base, "locked")
		if err := os.MkdirAll(filepath.Join(locked, "wt"), 0o700); err != nil {
			t.Fatal(err)
		}
		registerWithCard(t, controlHome, repoOne, idApp, "app", acmeDomain, filepath.Join(locked, "wt"))
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		reg := registry.New(registry.Options{ControlHome: controlHome, Root: base})

		_, err := reg.Resolve(ctx, base, registry.NameOptions{})
		if err == nil || !strings.Contains(err.Error(), "Could not resolve the directory of app.acme.localhost. Name the worktree by its host or its UUID instead") {
			t.Fatalf("got %v", err)
		}
		if errors.Is(err, registry.ErrNoWorktree) {
			t.Fatal("a lookup that could not finish is not 'no such worktree'")
		}
	})

	t.Run("a card that exists and cannot be read fails the lookup the same way", func(t *testing.T) {
		base := registrytest.TempDir(t)
		controlHome := filepath.Join(base, "control")
		registerWithCard(t, controlHome, repoOne, idApp, "app", acmeDomain, base)
		cardFile := card.IndexCardPath(controlHome, repoOne, idApp)
		if err := os.Chmod(cardFile, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(cardFile, 0o600) })
		reg := registry.New(registry.Options{ControlHome: controlHome, Root: base})

		_, err := reg.Resolve(ctx, base, registry.NameOptions{})
		if err == nil || !strings.Contains(err.Error(), "Could not read the index card of app.acme.localhost. Name the worktree by its host or its UUID instead") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestResolveSkipsACardThatDoesNotParse(t *testing.T) {
	ctx := context.Background()
	base := registrytest.TempDir(t)
	controlHome := filepath.Join(base, "control")
	outer, inner := filepath.Join(base, "outer"), filepath.Join(base, "outer", "inner")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	registerWithCard(t, controlHome, repoOne, idApp, "outer", acmeDomain, outer)
	registerWithCard(t, controlHome, repoOne, idApp2, "inner", acmeDomain, inner)
	if err := os.WriteFile(card.IndexCardPath(controlHome, repoOne, idApp2), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := registry.New(registry.Options{ControlHome: controlHome, Root: base})

	// The inner worktree has no card to match, so a read-only caller's path
	// inside it is attributed to the outer one; a destructive caller is refused.
	got, err := reg.Resolve(ctx, inner, registry.NameOptions{WithinDirectory: true})
	if err != nil || got.Record.WorktreeID != idApp {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := reg.Resolve(ctx, inner, registry.NameOptions{}); err == nil || !strings.Contains(err.Error(), "not its root") {
		t.Fatalf("a destructive caller must not be handed the outer worktree: %v", err)
	}
}

func TestNameCandidatesNeverOfferWhatResolveRefuses(t *testing.T) {
	ctx := context.Background()
	w := newResolveWorld(t)
	controlHome := filepath.Join(w.base, "control")
	// A branch named like a UUID makes a slug that reads as a UUID.
	const uuidSlug = "abababab-abab-4aba-8aba-abababababab"
	writeMinimalRecord(t, controlHome, repoOne, "07070707-0707-4707-8707-070707070707", uuidSlug, acmeDomain, "complete")

	candidates, err := w.reg.NameCandidates(ctx, registry.NameOptions{RepositoryID: scopeOf("")})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range candidates {
		if c.Record.Slug != uuidSlug {
			continue
		}
		found = true
		if c.Name != uuidSlug+"."+acmeDomain {
			t.Fatalf("offered %q, want the host", c.Name)
		}
		got, err := w.reg.Resolve(ctx, c.Name, registry.NameOptions{})
		if err != nil || got.Record.WorktreeID != c.Record.WorktreeID {
			t.Fatalf("%q resolves to %+v, %v", c.Name, got, err)
		}
	}
	if !found {
		t.Fatal("the worktree was not offered at all")
	}
}

func TestResolveExpandsTildeFromTheRegistrysOwnHomeWhenNoneIsGiven(t *testing.T) {
	w := newResolveWorld(t)
	reg := registry.New(registry.Options{ControlHome: filepath.Join(w.base, "control"), Root: w.base, UserHome: w.home})
	got, err := reg.Resolve(context.Background(), "~/work/tree", registry.NameOptions{})
	if err != nil || got.Record.WorktreeID != idHome {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestResolveIgnoresARecordedRootThatCannotNameADirectory(t *testing.T) {
	ctx := context.Background()
	for _, root := range []string{".", "/", "relative/dir", "//"} {
		t.Run("root "+root, func(t *testing.T) {
			base := registrytest.TempDir(t)
			controlHome := filepath.Join(base, "control")
			registerWithCard(t, controlHome, repoOne, idApp, "app", acmeDomain, root)
			reg := registry.New(registry.Options{ControlHome: controlHome, Root: base})

			// Neither the working directory ("." would match it) nor anything
			// under "/" belongs to the worktree.
			for _, name := range []string{base, "."} {
				_, err := reg.Resolve(ctx, name, registry.NameOptions{Cwd: base, WithinDirectory: true})
				if !errors.Is(err, registry.ErrNoWorktree) {
					t.Fatalf("Resolve(%q) = %v, want no worktree", name, err)
				}
			}
			got, err := reg.Resolve(ctx, "app", registry.NameOptions{})
			if err != nil || got.Root != "" {
				t.Fatalf("the worktree is still named by its slug, with no root: %+v, %v", got, err)
			}
			candidates, err := reg.NameCandidates(ctx, registry.NameOptions{})
			if err != nil || len(candidates) != 1 || candidates[0].Root != "" {
				t.Fatalf("candidates = %+v, %v; want one with no root", candidates, err)
			}
		})
	}
}

func TestResolveCountsARootBehindARegularFileAsGone(t *testing.T) {
	ctx := context.Background()
	base := registrytest.TempDir(t)
	controlHome := filepath.Join(base, "control")
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The worktree lived at file/wt, and file has since become a regular file.
	registerWithCard(t, controlHome, repoOne, idApp, "app", acmeDomain, filepath.Join(file, "wt"))
	other := filepath.Join(base, "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	registerWithCard(t, controlHome, repoOne, idApp2, "other", acmeDomain, other)
	reg := registry.New(registry.Options{ControlHome: controlHome, Root: base})

	got, err := reg.Resolve(ctx, other, registry.NameOptions{})
	if err != nil || got.Record.WorktreeID != idApp2 {
		t.Fatalf("an unrelated lookup must not fail: %+v, %v", got, err)
	}
	got, err = reg.Resolve(ctx, filepath.Join(file, "wt"), registry.NameOptions{})
	if err != nil || got.Record.WorktreeID != idApp {
		t.Fatalf("the gone directory is still named by its recorded path: %+v, %v", got, err)
	}
	if _, err := reg.Resolve(ctx, filepath.Join(file, "wt", "deeper"), registry.NameOptions{}); err == nil || !strings.Contains(err.Error(), "not its root") {
		t.Fatalf("got %v", err)
	}
}
