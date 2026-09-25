package commands_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/commands/commandstest"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/worktree"
)

const acme = "acme.localhost"

// ns takes value's address, for the *string fields (LabelNamespace and the
// like) tests need a pointer literal for.
func ns(value string) *string { return &value }

// approved returns options with Yes set, standing in for the interactive
// confirmation prompt.
func approved(options commands.RemoveOptions) commands.RemoveOptions {
	options.Yes = true
	return options
}

// remove runs RemoveWithContext against tc's CommandContext with a bare
// "berth" Env, the shared entry point every subtest in this file calls.
func remove(t *testing.T, tc *commandstest.TestContext, options commands.RemoveOptions) (*commands.RemoveResult, error) {
	t.Helper()
	return commands.RemoveWithContext(context.Background(), tc.CommandContext, &commands.Env{CommandName: "berth"}, options)
}

// kinds returns resources' Kind values, in order, for asserting what a
// removal touched without comparing full OwnedResource values.
func kinds(resources []worktree.OwnedResource) []string {
	var out []string
	for _, resource := range resources {
		out = append(out, resource.Kind)
	}
	return out
}

// exists reports whether path is present on disk, failing the test on any
// error other than absence.
func exists(t *testing.T, path string) bool {
	t.Helper()
	found, err := files.PathExists(path)
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// wantErr fails the test unless err is non-nil and its message matches
// pattern.
func wantErr(t *testing.T, err error, pattern string) {
	t.Helper()
	if err == nil || !regexp.MustCompile(pattern).MatchString(err.Error()) {
		t.Fatalf("error = %v, want one matching %q", err, pattern)
	}
}

func TestWorktreeRemove(t *testing.T) {
	ctx := context.Background()

	t.Run("removes the current worktree: resources, home, registration, and identity marker, so the next up starts fresh", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api", "mail")
		paths := layout.WorktreeLayout(w.ControlHome, ref.RepositoryID, ref.WorktreeID)
		if !exists(t, paths.Home) {
			t.Fatal("no home before removal")
		}
		tc := w.Context("", acme, nil)

		result, err := remove(t, tc, approved(commands.RemoveOptions{}))
		if err != nil {
			t.Fatal(err)
		}

		// What stopping removed and what reap found, in inventory order.
		if got := kinds(result.Resources); !reflect.DeepEqual(got, []string{"container", "container", "volume"}) {
			t.Fatalf("kinds = %v", got)
		}
		if !result.Home || !result.Registration || !result.Marker {
			t.Fatalf("result = %+v", result)
		}
		if left := w.Docker.Owned(ref, brand.LabelNamespace); len(left) != 0 {
			t.Fatalf("left behind: %v", left)
		}
		if exists(t, paths.Home) || exists(t, paths.QuarantinePath) {
			t.Fatal("the home survived")
		}
		if current, _ := tc.Registry.Current(ctx); current != nil {
			t.Fatalf("still registered: %+v", current)
		}
		if identity, _ := tc.Registry.Identities.Inspect(ctx, w.Root, registry.InspectOptions{}); identity != nil {
			t.Fatalf("marker survived: %+v", identity)
		}
		lines, _ := tc.Logger.Snapshot()
		if last := lines[len(lines)-1]; last != "info: Removed feature-login.acme.localhost: 2 containers, 1 volume, home directory, registration, identity marker." {
			t.Fatalf("last line = %q", last)
		}
	})

	t.Run("reaps the local worktree through its registration's namespace, not the current config's", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", "dev.other", "api", "mail")
		// The config's own namespace defaults to brand.LabelNamespace
		// ("dev.berth"), distinct from the "dev.other" the worktree was
		// actually registered and labelled under.
		tc := w.Context("", acme, nil)

		result, err := remove(t, tc, approved(commands.RemoveOptions{}))
		if err != nil {
			t.Fatal(err)
		}
		if got := kinds(result.Resources); !reflect.DeepEqual(got, []string{"container", "container", "volume"}) {
			t.Fatalf("kinds = %v", got)
		}
		if left := w.Docker.Owned(ref, "dev.other"); len(left) != 0 {
			t.Fatalf("left behind under the registration's namespace: %v", left)
		}
	})

	// #19, row 25 in docs/port/divergences.md: `worktree remove` only tears
	// down what is already there, so — unlike `up` — it loads and proceeds
	// against a [service.index] label or purpose that carries a control
	// character.
	t.Run("proceeds when a service index label or purpose carries a control character", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		tc := w.Context("", acme, nil)
		tc.Loaded.Config.Service[0].Index = &config.ServiceIndex{Label: "a\tb", Purpose: "the \u202eAPI"}

		if _, err := remove(t, tc, approved(commands.RemoveOptions{})); err != nil {
			t.Fatalf("RemoveWithContext: %v", err)
		}
		if left := w.Docker.Owned(ref, brand.LabelNamespace); len(left) != 0 {
			t.Fatalf("left behind: %v", left)
		}
	})

	t.Run("requires the hostname as a confirmation phrase and touches nothing without it", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		tc := w.Context("", acme, nil)

		_, err := remove(t, tc, commands.RemoveOptions{})
		wantErr(t, err, regexp.QuoteMeta(`Interactive confirmation required: "feature-login.acme.localhost".`))
		if left := w.Docker.Owned(ref, brand.LabelNamespace); len(left) != 2 {
			t.Fatalf("owned = %v", left)
		}
		if current, _ := tc.Registry.Current(ctx); current == nil || current.Slug != "feature-login" {
			t.Fatalf("current = %+v", current)
		}
	})

	t.Run("stops before the prompt when a resource cannot be proven ours, and keeps the registration", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		// A volume with this worktree's ownership labels but another project's.
		labels := commandstest.Ownership(ref, brand.LabelNamespace)
		labels["com.docker.compose.project"] = "somebody-else"
		stray := w.Docker.Add("volume", labels)
		tc := w.Context("", acme, nil)
		paths := layout.WorktreeLayout(w.ControlHome, ref.RepositoryID, ref.WorktreeID)

		_, err := remove(t, tc, approved(commands.RemoveOptions{}))
		wantErr(t, err, regexp.QuoteMeta("Refusing to touch volume "+stray+": its ownership labels do not match this worktree."))

		if left := w.Docker.Owned(ref, brand.LabelNamespace); len(left) != 3 {
			t.Fatalf("owned = %v", left)
		}
		if !exists(t, paths.Home) {
			t.Fatal("the home was removed")
		}
		// Nothing was destroyed, so nothing is pending: the next `up` must not seed over intact data.
		state, err := tc.Registry.Store(ref.RepositoryID).Initialization(ref.WorktreeID, "feature-login")
		if err != nil || state != registry.Complete {
			t.Fatalf("initialization = %v, %v", state, err)
		}
	})

	t.Run("keeps the registration when a reap fails midway, so a retry can finish", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		// Verified when inventoried, and no longer ours when reaped: the volume's
		// fourth inspection (planned, current, owned, then the reap) sees another worktree.
		volume := w.Docker.Owned(ref, brand.LabelNamespace)[1]
		foreign := commandstest.Ownership(ref, brand.LabelNamespace)
		foreign[brand.WorktreeLabel(brand.LabelNamespace)] = "ffffffff-ffff-4fff-8fff-ffffffffffff"
		w.Docker.TamperAfter(volume, 3, foreign)
		tc := w.Context("", acme, nil)

		_, err := remove(t, tc, approved(commands.RemoveOptions{}))
		wantErr(t, err, regexp.QuoteMeta("Refusing to touch volume "+volume))

		current, _ := tc.Registry.Current(ctx)
		if current == nil || current.Slug != "feature-login" || current.LabelNamespace == nil || *current.LabelNamespace != "dev.berth" {
			t.Fatalf("current = %+v", current)
		}
		// Data may already be gone, so the record is pending again: an `up` now would seed, not resume.
		state, _ := tc.Registry.Store(ref.RepositoryID).Initialization(ref.WorktreeID, "feature-login")
		if state != registry.Pending {
			t.Fatalf("initialization = %v", state)
		}
		// The volume is someone else's now; put it back so the retry can finish.
		w.Docker.Labels("volume", volume)[brand.WorktreeLabel(brand.LabelNamespace)] = ref.WorktreeID
		w.Docker.TamperAfter(volume, 1<<30, nil)
		retry, err := remove(t, w.Context("", acme, nil), approved(commands.RemoveOptions{}))
		if err != nil || !retry.Registration || !retry.Marker {
			t.Fatalf("retry = %+v, %v", retry, err)
		}
	})

	t.Run("removes another worktree of this repository by slug, including its identity marker", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		other := w.Worktree("other", "feature/other")
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		otherRef := w.Register(other, acme, "feature-other", brand.LabelNamespace, "api")

		result, err := remove(t, w.Context("", acme, nil), approved(commands.RemoveOptions{Identifier: "feature-other"}))
		if err != nil {
			t.Fatal(err)
		}
		if result.Target.WorktreeID != otherRef.WorktreeID || !result.Marker {
			t.Fatalf("result = %+v", result)
		}
		otherContext := w.Context(other, acme, nil)
		if current, _ := otherContext.Registry.Current(ctx); current != nil {
			t.Fatalf("other still registered: %+v", current)
		}
		if identity, _ := otherContext.Registry.Identities.Inspect(ctx, other, registry.InspectOptions{}); identity != nil {
			t.Fatal("other's marker survived")
		}
		mine := w.Context("", acme, nil)
		if current, _ := mine.Registry.Current(ctx); current == nil || current.Slug != "feature-login" {
			t.Fatalf("this worktree was touched: %+v", current)
		}
		if listed, _ := mine.Registry.List(ctx); len(listed) != 1 {
			t.Fatalf("list = %+v", listed)
		}
	})

	t.Run("removes an orphaned registration whose Git worktree is gone, stopping its containers, with no marker to remove", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		gone := w.Worktree("gone", "feature/gone")
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		orphan := w.Register(gone, acme, "feature-gone", brand.LabelNamespace, "api")
		w.Git.Forget(gone)
		if err := os.RemoveAll(gone); err != nil {
			t.Fatal(err)
		}

		result, err := remove(t, w.Context("", acme, nil), approved(commands.RemoveOptions{Identifier: orphan.WorktreeID}))
		if err != nil {
			t.Fatal(err)
		}
		if !result.Registration || !result.Home || result.Marker {
			t.Fatalf("result = %+v", result)
		}
		if got := kinds(result.Resources); !reflect.DeepEqual(got, []string{"container", "volume"}) {
			t.Fatalf("kinds = %v", got)
		}
		if listed, _ := w.Context("", acme, nil).Registry.List(ctx); len(listed) != 1 {
			t.Fatalf("list = %+v", listed)
		}
	})

	t.Run("stops a target gracefully when it has containers, and skips the stop when it has none", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		downs := func() int {
			n := 0
			for _, call := range w.Docker.Calls() {
				if strings.HasPrefix(call, "compose ") && strings.HasSuffix(call, " down --remove-orphans") {
					n++
				}
			}
			return n
		}
		// Nothing left to stop: the container already went by hand.
		for _, id := range w.Docker.Owned(ref, brand.LabelNamespace) {
			if w.Docker.Labels("container", id) != nil {
				w.Docker.Handle(ctx, dockerCall("rm", "--force", id))
			}
		}
		if _, err := remove(t, w.Context("", acme, nil), approved(commands.RemoveOptions{})); err != nil {
			t.Fatal(err)
		}
		if downs() != 0 {
			t.Fatal("stopped a worktree with no containers")
		}

		other := w.Worktree("other", "feature/other")
		w.Register(other, acme, "feature-other", brand.LabelNamespace, "api")
		if _, err := remove(t, w.Context("", acme, nil), approved(commands.RemoveOptions{Identifier: "feature-other"})); err != nil {
			t.Fatal(err)
		}
		if downs() != 1 {
			t.Fatalf("downs = %d, want 1", downs())
		}
	})

	t.Run("finishes an interrupted removal that left a quarantine directory behind", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		paths := layout.WorktreeLayout(w.ControlHome, ref.RepositoryID, ref.WorktreeID)
		if err := os.RemoveAll(paths.Home); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(paths.QuarantinePath, 0o700); err != nil {
			t.Fatal(err)
		}

		result, err := remove(t, w.Context("", acme, nil), approved(commands.RemoveOptions{}))
		if err != nil || !result.Home || exists(t, paths.QuarantinePath) {
			t.Fatalf("result = %+v, %v", result, err)
		}
	})

	t.Run("explains a home it cannot delete, and finishes once the files are reclaimed", func(t *testing.T) {
		// Root ignores mode bits, so the provocation below cannot work as uid 0.
		if os.Getuid() == 0 {
			t.Skip("root ignores mode bits")
		}
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		paths := layout.WorktreeLayout(w.ControlHome, ref.RepositoryID, ref.WorktreeID)
		// A directory the owner cannot unlink from stands in for files another uid left behind.
		stubborn := filepath.Join(paths.Home, "data")
		if err := os.Mkdir(stubborn, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stubborn, "base"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(stubborn, 0); err != nil {
			t.Fatal(err)
		}
		reclaim := func() {
			for _, candidate := range []string{filepath.Join(paths.QuarantinePath, "data"), stubborn} {
				_ = os.Chmod(candidate, 0o700)
			}
		}
		t.Cleanup(reclaim)

		_, err := remove(t, w.Context("", acme, nil), approved(commands.RemoveOptions{}))
		owner := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
		wantErr(t, err, `(?s)owned by another user.*chown -R `+owner+` /d.*'berth worktree remove' again`)
		if current, _ := w.Context("", acme, nil).Registry.Current(ctx); current == nil {
			t.Fatal("the registration went before the home did")
		}
		reclaim()

		retry, err := remove(t, w.Context("", acme, nil), approved(commands.RemoveOptions{}))
		if err != nil || !retry.Home || !retry.Registration {
			t.Fatalf("retry = %+v, %v", retry, err)
		}
		if exists(t, paths.Home) || exists(t, paths.QuarantinePath) {
			t.Fatal("the home survived the retry")
		}
	})

	t.Run("refuses a foreign worktree that nothing on disk describes, and accepts --label-namespace for it", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		widgets := w.Worktree("widgets", "main", filepath.Join(w.Base, "widgets-common"))
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		// Registered by a tool version that recorded no namespace, and never brought up: no card either.
		identities := registry.NewIdentityService(widgets, w.Runner)
		identity, err := identities.Ensure(ctx, widgets)
		if err != nil {
			t.Fatal(err)
		}
		writeRecord(t, w, identity, "widgets.localhost", "main", nil)
		legacy := worktree.Ref{RepositoryID: identity.RepositoryID, WorktreeID: identity.WorktreeID, Slug: "main"}
		w.Docker.Up(legacy, "dev.legacy", "api")

		_, err = remove(t, w.Context("", acme, nil), approved(commands.RemoveOptions{Identifier: identity.WorktreeID}))
		wantErr(t, err, `(?s)Cannot remove main\.widgets\.localhost from this repository: it belongs to a different one.*--label-namespace \(dev\.berth unless`)
		_, err = remove(t, w.Context("", acme, nil), approved(commands.RemoveOptions{Identifier: identity.WorktreeID, LabelNamespace: "not a namespace"}))
		wantErr(t, err, "--label-namespace must look like a reverse domain")

		result, err := remove(t, w.Context("", acme, nil), approved(commands.RemoveOptions{Identifier: identity.WorktreeID, LabelNamespace: "dev.legacy"}))
		if err != nil || !result.Registration {
			t.Fatalf("result = %+v, %v", result, err)
		}
		if left := w.Docker.Owned(legacy, "dev.legacy"); len(left) != 0 {
			t.Fatalf("the legacy resources survived: %v", left)
		}
	})

	t.Run("refuses --label-namespace when the registration records a different one, rather than orphaning the data", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")

		_, err := remove(t, w.Context("", acme, nil), approved(commands.RemoveOptions{LabelNamespace: "dev.wrong"}))
		wantErr(t, err, regexp.QuoteMeta("feature-login.acme.localhost records its resources as labelled under dev.berth, not dev.wrong. Drop --label-namespace, or pass the recorded one."))
		if left := w.Docker.Owned(ref, brand.LabelNamespace); len(left) != 2 {
			t.Fatalf("owned = %v", left)
		}
		// The recorded one, spelled out, is fine.
		result, err := remove(t, w.Context("", acme, nil), approved(commands.RemoveOptions{LabelNamespace: "dev.berth"}))
		if err != nil || !result.Registration {
			t.Fatalf("result = %+v, %v", result, err)
		}
	})

	t.Run("does nothing, gracefully, when another removal finished while it waited for the lock", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		tc := w.Context("", acme, nil)
		// The rival wins after this call resolved its target but before it took
		// the lock: the pre-prompt inventory is the seam.
		w.Docker.BeforeFirstCall(func() {
			if _, err := remove(t, w.Context("", acme, nil), approved(commands.RemoveOptions{})); err != nil {
				t.Errorf("rival: %v", err)
			}
		})

		late, err := remove(t, tc, approved(commands.RemoveOptions{}))
		if err != nil {
			t.Fatal(err)
		}
		if !late.AlreadyGone || len(late.Resources) != 0 || late.Home || late.Registration || late.Marker {
			t.Fatalf("late = %+v", late)
		}
		lines, _ := tc.Logger.Snapshot()
		if !slices.Contains(lines, "info: feature-login.acme.localhost was already removed while 'berth worktree remove' was waiting; nothing left to do.") {
			t.Fatalf("lines = %v", lines)
		}
		for _, line := range lines {
			if strings.HasPrefix(line, "info: Removed ") {
				t.Fatalf("claimed a removal: %q", line)
			}
		}
	})

	t.Run("runs again when an up re-labelled the worktree while it waited for the lock", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		tc := w.Context("", acme, nil)
		w.Docker.BeforeFirstCall(func() {
			identity := &registry.WorktreeIdentity{RepositoryID: ref.RepositoryID, WorktreeID: ref.WorktreeID}
			writeRecord(t, w, identity, acme, "feature-login", ns("dev.other"))
		})

		_, err := remove(t, tc, approved(commands.RemoveOptions{}))
		wantErr(t, err, regexp.QuoteMeta("was re-labelled under dev.other while 'berth worktree remove' was waiting; run it again"))
		if left := w.Docker.Owned(ref, brand.LabelNamespace); len(left) != 2 {
			t.Fatalf("destroyed something: %v", left)
		}
	})

	t.Run("names what it could not find", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace)
		_, err := remove(t, w.Context("", acme, nil), approved(commands.RemoveOptions{Identifier: "nope"}))
		wantErr(t, err, regexp.QuoteMeta("No worktree is registered as nope."))
		_, err = remove(t, w.Context("", acme, nil), approved(commands.RemoveOptions{Identifier: "nope", Domain: "x.localhost"}))
		wantErr(t, err, regexp.QuoteMeta("No worktree is registered as nope under x.localhost."))
	})

	t.Run("refuses when this worktree was never registered", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		_, err := remove(t, w.Context("", acme, nil), approved(commands.RemoveOptions{}))
		wantErr(t, err, regexp.QuoteMeta("This worktree is not registered; nothing to remove. Name another with 'berth worktree remove <slug|uuid>'."))
	})
}

// writeRecord writes a registry record for identity directly to disk,
// bypassing Ensure, so a test can put a worktree in a state (a recorded
// domain or namespace, in particular) the normal registration path would
// never produce on its own.
func writeRecord(t *testing.T, w *commandstest.World, identity *registry.WorktreeIdentity, domain, slug string, namespace *string) {
	t.Helper()
	store := registry.NewStore(w.ControlHome, identity.RepositoryID)
	record := map[string]any{
		"version": 1, "repositoryId": identity.RepositoryID, "worktreeId": identity.WorktreeID,
		"slug": slug, "domain": domain, "initialization": "complete",
	}
	if namespace != nil {
		record["labelNamespace"] = *namespace
	}
	if err := os.MkdirAll(filepath.Dir(store.RecordPath(identity.WorktreeID)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := files.AtomicJSON(store.RecordPath(identity.WorktreeID), record); err != nil {
		t.Fatal(err)
	}
}

// dockerCall builds a run.Spec for a direct `docker` invocation, standing in
// for a container the caller removed by hand rather than through berth.
func dockerCall(args ...string) run.Spec { return run.Spec{Command: "docker", Args: args} }
