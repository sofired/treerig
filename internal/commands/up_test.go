package commands_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/commands/commandstest"
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/overview/card"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/router"
	"github.com/sofired/treerig/internal/run"
)

// up runs UpWithContext against tc's CommandContext, the shared entry point
// every subtest in this file calls.
func up(t *testing.T, tc *commandstest.TestContext, router commands.Router, index commands.Index, options commands.UpOptions) (*commands.UpResult, error) {
	t.Helper()
	return commands.UpWithContext(context.Background(), tc.CommandContext, router, index, options)
}

// givePorts sets an explicit port on every [service.<name>] the world's default
// config declares: World's fake Compose file exposes none, and a [service.<name>]
// with neither is exactly the "declares no port" refusal — real behavior
// this package's other Up tests are not about.
func givePorts(tc *commandstest.TestContext) {
	port := 8080
	for i := range tc.Loaded.Config.Service {
		tc.Loaded.Config.Service[i].Port = &port
	}
}

func TestUpWithContext(t *testing.T) {
	t.Run("registers, starts services, runs prepare and seed the first time, and prints the host and URLs", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		router := commandstest.NewFakeRouter(80)
		index := &commandstest.FakeIndex{}

		result, err := up(t, tc, router, index, commands.UpOptions{})
		if err != nil {
			t.Fatalf("Up: %v", err)
		}
		if result.Registration.Slug != "feature-login" || result.Registration.Domain != acme {
			t.Fatalf("unexpected registration: %+v", result.Registration)
		}
		if router.Ensures() != 1 {
			t.Fatalf("expected the router to be ensured once, got %d", router.Ensures())
		}
		if len(result.Hooks) != 0 {
			t.Fatalf("expected no hooks configured, got %v", result.Hooks)
		}
		if len(result.URLs) != 2 {
			t.Fatalf("expected two service URLs, got %+v", result.URLs)
		}

		state, err := tc.Registry.Store(result.Registration.RepositoryID).Initialization(result.Registration.WorktreeID, result.Registration.Slug)
		if err != nil || state != registry.Complete {
			t.Fatalf("expected completed initialization, got %v, %v", state, err)
		}

		if got := index.Reasons(); len(got) != 1 || got[0] != "up" {
			t.Fatalf("expected one index refresh reason [up], got %v", got)
		}

		lines, _ := tc.Logger.Snapshot()
		found := false
		for _, line := range lines {
			if strings.Contains(line, "feature-login.acme.localhost is up.") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected an 'is up' line, got %v", lines)
		}

		upCalls := 0
		for _, line := range w.Docker.Calls() {
			if strings.HasPrefix(line, "compose ") && strings.Contains(line, " up ") {
				upCalls++
			}
		}
		if upCalls != 1 {
			t.Fatalf("expected exactly one compose up call, got %d in %v", upCalls, w.Docker.Calls())
		}

		got, err := card.ReadIndexCard(w.ControlHome, result.Registration.RepositoryID, result.Registration.WorktreeID)
		if err != nil {
			t.Fatalf("ReadIndexCard: %v", err)
		}
		if got == nil || got.Project != tc.Loaded.Config.Project.Name || got.Slug != "feature-login" || got.Domain != acme {
			t.Fatalf("unexpected index card: %+v", got)
		}
	})

	t.Run("calls off an interrupted removal: the worktree stops reading as removing once it is up again", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		first := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		paths := layout.WorktreeLayout(w.ControlHome, first.RepositoryID, first.WorktreeID)
		if err := os.WriteFile(paths.RemovalPath, []byte(registry.RemovalMarkerContents()), 0o600); err != nil {
			t.Fatal(err)
		}
		tc := w.Context("", acme, nil)
		givePorts(tc)

		if _, err := up(t, tc, commandstest.NewFakeRouter(80), &commandstest.FakeIndex{}, commands.UpOptions{}); err != nil {
			t.Fatalf("Up: %v", err)
		}
		if removing, err := tc.Registry.Store(first.RepositoryID).Removing(first.WorktreeID); err != nil || removing {
			t.Fatalf("Removing = %v, %v; the marker should be gone", removing, err)
		}
	})

	t.Run("refuses a removal marker of a newer format rather than clearing it", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		first := w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		paths := layout.WorktreeLayout(w.ControlHome, first.RepositoryID, first.WorktreeID)
		if err := os.WriteFile(paths.RemovalPath, []byte("format 2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		tc := w.Context("", acme, nil)
		givePorts(tc)

		_, err := up(t, tc, commandstest.NewFakeRouter(80), &commandstest.FakeIndex{}, commands.UpOptions{})
		if err == nil || !strings.Contains(err.Error(), "written by a newer treerig") {
			t.Fatalf("err = %v", err)
		}
		if data, _ := os.ReadFile(paths.RemovalPath); string(data) != "format 2\n" {
			t.Fatalf("the newer marker was changed: %q", data)
		}
	})

	t.Run("runs only prepare on a later up, since seed only runs the first time", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		tc.Loaded.Config.Hooks = config.Hooks{Prepare: ns("./hooks/prepare"), Seed: ns("./hooks/seed")}
		router := commandstest.NewFakeRouter(80)
		index := &commandstest.FakeIndex{}
		// The environment each hook ran with, by stage.
		hookEnv := map[string][]string{}
		fallback := w.Runner.Handler
		w.Runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
			if strings.HasSuffix(spec.Label, " hook") {
				hookEnv[spec.Label] = spec.Env
			}
			return fallback(ctx, spec)
		}

		first, err := up(t, tc, router, index, commands.UpOptions{})
		if err != nil {
			t.Fatalf("first Up: %v", err)
		}
		if want := []config.HookStage{config.Prepare, config.Seed}; !slices.Equal(first.Hooks, want) {
			t.Fatalf("first Up hooks = %v, want %v", first.Hooks, want)
		}
		for _, label := range []string{"prepare hook", "seed hook"} {
			registration, err := tc.Registry.Current(context.Background())
			if err != nil || registration == nil {
				t.Fatalf("registration: %v, %v", registration, err)
			}
			want := layout.ComposeProjectName(registration.RepositoryID, registration.WorktreeID)
			if !slices.Contains(hookEnv[label], "COMPOSE_PROJECT_NAME="+want) {
				t.Errorf("%s ran without COMPOSE_PROJECT_NAME=%s: %v", label, want, hookEnv[label])
			}
		}

		second, err := up(t, tc, router, index, commands.UpOptions{})
		if err != nil {
			t.Fatalf("second Up: %v", err)
		}
		if want := []config.HookStage{config.Prepare}; !slices.Equal(second.Hooks, want) {
			t.Fatalf("second Up hooks = %v, want %v", second.Hooks, want)
		}
	})

	t.Run("writes each route file to treerig-loopback at the port recorded in the registration", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		fakeRouter := commandstest.NewFakeRouter(80)

		result, err := up(t, tc, fakeRouter, &commandstest.FakeIndex{}, commands.UpOptions{})
		if err != nil {
			t.Fatalf("Up: %v", err)
		}
		record, err := tc.Registry.Store(result.Registration.RepositoryID).Find(result.Registration.WorktreeID)
		if err != nil || record == nil {
			t.Fatalf("record: %v, %v", record, err)
		}
		if len(record.Ports) != 2 {
			t.Fatalf("recorded ports = %v, want one per routed service", record.Ports)
		}
		for service, port := range record.Ports {
			path := router.FragmentPath(tc.RouterState, record.RepositoryID, record.WorktreeID, service)
			doc, err := router.ReadFragment(tc.RouterState, path)
			if err != nil {
				t.Fatalf("%s: %v", service, err)
			}
			if got := doc.TargetPort(); got != port {
				t.Errorf("%s's route file targets port %d, the registration records %d", service, got, port)
			}
			for _, server := range doc.HTTP.Services {
				if want := "http://treerig-loopback:" + strconv.Itoa(port); server.LoadBalancer.Servers[0].URL != want {
					t.Errorf("%s's target = %q, want %q", service, server.LoadBalancer.Servers[0].URL, want)
				}
			}
		}
	})

	t.Run("removes the route files of services no longer declared, and keeps the others", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		fakeRouter := commandstest.NewFakeRouter(80)

		first, err := up(t, tc, fakeRouter, &commandstest.FakeIndex{}, commands.UpOptions{})
		if err != nil {
			t.Fatalf("first Up: %v", err)
		}
		kept, dropped := tc.Loaded.Config.Service[0].Name, tc.Loaded.Config.Service[1].Name
		tc.Loaded.Config.Service = tc.Loaded.Config.Service[:1]
		if _, err := up(t, tc, fakeRouter, &commandstest.FakeIndex{}, commands.UpOptions{}); err != nil {
			t.Fatalf("second Up: %v", err)
		}

		repo, wt := first.Registration.RepositoryID, first.Registration.WorktreeID
		if _, err := os.Stat(router.FragmentPath(tc.RouterState, repo, wt, kept)); err != nil {
			t.Errorf("the declared service's route file should remain: %v", err)
		}
		if _, err := os.Stat(router.FragmentPath(tc.RouterState, repo, wt, dropped)); !os.IsNotExist(err) {
			t.Errorf("the dropped service's route file should be gone, stat err = %v", err)
		}
	})

	t.Run("removes every route file when a later up declares no routes at all", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		fakeRouter := commandstest.NewFakeRouter(80)

		if _, err := up(t, tc, fakeRouter, &commandstest.FakeIndex{}, commands.UpOptions{}); err != nil {
			t.Fatalf("first Up: %v", err)
		}
		tc.Loaded.Config.Service = nil
		if _, err := up(t, tc, fakeRouter, &commandstest.FakeIndex{}, commands.UpOptions{}); err != nil {
			t.Fatalf("second Up (no routes): %v", err)
		}
		entries, err := os.ReadDir(tc.RouterState.DynamicDir)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("expected no route files left, got %v", entries)
		}
		for _, call := range w.Docker.Calls() {
			if strings.HasPrefix(call, "network connect ") || strings.HasPrefix(call, "network disconnect ") {
				t.Fatalf("up must not connect or disconnect networks, saw %q", call)
			}
		}
	})

	t.Run("keeps a still-declared service's route file in place on a re-up instead of removing and rewriting it", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		fakeRouter := commandstest.NewFakeRouter(80)

		first, err := up(t, tc, fakeRouter, &commandstest.FakeIndex{}, commands.UpOptions{})
		if err != nil {
			t.Fatalf("first Up: %v", err)
		}
		path := router.FragmentPath(tc.RouterState, first.Registration.RepositoryID, first.Registration.WorktreeID, tc.Loaded.Config.Service[0].Name)
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		// Observed from inside the second up, once its compose up has run
		// and before it returns: the file must exist at every point.
		missing := false
		fakeRouter.BeforeEnsure(func() {
			if _, err := os.Stat(path); err != nil {
				missing = true
			}
		})
		if _, err := up(t, tc, fakeRouter, &commandstest.FakeIndex{}, commands.UpOptions{}); err != nil {
			t.Fatalf("second Up: %v", err)
		}
		after, err := os.Stat(path)
		if err != nil || missing {
			t.Fatalf("route file missing during or after the re-up: %v (missing %v)", err, missing)
		}
		if after.Size() != before.Size() {
			t.Errorf("route file changed size across a re-up with the same ports: %d -> %d", before.Size(), after.Size())
		}
	})

	t.Run("allocates worktree ports clear of the router's index and ping ports", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		// A four-port range holding the router's two ports leaves exactly the
		// other two for the two routed services.
		tc.PortRange = layout.PortRange{Low: 30000, High: 30003}
		if err := os.MkdirAll(tc.RouterState.StateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		state := `{"version":1,"indexPort":30000,"pingPort":30001}`
		if err := os.WriteFile(tc.RouterState.StateFile, []byte(state), 0o600); err != nil {
			t.Fatal(err)
		}

		result, err := up(t, tc, commandstest.NewFakeRouter(80), &commandstest.FakeIndex{}, commands.UpOptions{})
		if err != nil {
			t.Fatalf("Up: %v", err)
		}
		record, err := tc.Registry.Store(result.Registration.RepositoryID).Find(result.Registration.WorktreeID)
		if err != nil || record == nil {
			t.Fatalf("record: %v, %v", record, err)
		}
		got := []int{}
		for _, port := range record.Ports {
			got = append(got, port)
		}
		slices.Sort(got)
		if !slices.Equal(got, []int{30002, 30003}) {
			t.Fatalf("service ports = %v, want [30002 30003]: the router's 30000 and 30001 are reserved", got)
		}
	})

	t.Run("propagates a hook failure and never completes initialization", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		tc.Loaded.Config.Hooks = config.Hooks{Prepare: ns("./hooks/prepare")}
		originalHandler := w.Runner.Handler
		w.Runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
			if spec.Command == "/repo/hooks/prepare" || strings.HasSuffix(spec.Command, "/hooks/prepare") {
				return run.Result{ExitCode: 3}, nil
			}
			return originalHandler(ctx, spec)
		}
		router := commandstest.NewFakeRouter(80)
		index := &commandstest.FakeIndex{}

		_, err := up(t, tc, router, index, commands.UpOptions{})
		wantErr(t, err, "The prepare hook failed \\(exit 3\\)")

		registered, findErr := tc.Registry.Current(context.Background())
		if findErr != nil || registered == nil {
			t.Fatalf("expected the registration to still exist, got %v, %v", registered, findErr)
		}
		state, err := tc.Registry.Store(registered.RepositoryID).Initialization(registered.WorktreeID, registered.Slug)
		if err != nil || state != registry.Pending {
			t.Fatalf("expected initialization to remain pending after a hook failure, got %v, %v", state, err)
		}
		if len(index.Reasons()) != 0 {
			t.Fatalf("expected no index refresh after a failed up, got %v", index.Reasons())
		}
	})

	t.Run("fails a hung hook as a timeout, not a failure, and releases the worktree's lock", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		limit := "10m"
		tc.Loaded.Config.Hooks = config.Hooks{Prepare: ns("./hooks/prepare"), Timeout: &config.HookTimeouts{Prepare: &limit}}
		originalHandler := w.Runner.Handler
		hung := true
		var limits []time.Duration
		w.Runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
			if strings.HasSuffix(spec.Command, "/hooks/prepare") {
				limits = append(limits, spec.Timeout)
				if hung {
					// What the real runner reports once the limit passes and
					// the hook's process group is stopped.
					return run.Result{ExitCode: 1, TimedOut: true}, nil
				}
				return run.Result{}, nil
			}
			return originalHandler(ctx, spec)
		}
		router := commandstest.NewFakeRouter(80)
		index := &commandstest.FakeIndex{}

		_, err := up(t, tc, router, index, commands.UpOptions{})
		wantErr(t, err, "^The prepare hook timed out after 10m and was terminated\\.$")
		if code := clierr.Code(err); code != 124 {
			t.Fatalf("exit code = %d, want 124, apart from a hook's own failure", code)
		}
		if !slices.Equal(limits, []time.Duration{10 * time.Minute}) {
			t.Fatalf("the hook ran with limits %v, want [10m]", limits)
		}
		registered, findErr := tc.Registry.Current(context.Background())
		if findErr != nil || registered == nil {
			t.Fatalf("expected the registration to still exist, got %v, %v", registered, findErr)
		}
		state, stateErr := tc.Registry.Store(registered.RepositoryID).Initialization(registered.WorktreeID, registered.Slug)
		if stateErr != nil || state != registry.Pending {
			t.Fatalf("expected initialization to remain pending after a timeout, got %v, %v", state, stateErr)
		}

		// The operation lock was released on the way out: the next up takes
		// it at once instead of waiting out its 25 minutes.
		hung = false
		retry, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := commands.UpWithContext(retry, tc.CommandContext, router, index, commands.UpOptions{}); err != nil {
			t.Fatalf("the retry after a timeout: %v", err)
		}
	})

	t.Run("refuses to relabel a worktree that still owns resources under its old namespace", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register(w.Root, acme, "feature-login", "dev.predecessor", "api", "mail")
		tc := w.Context("", acme, nil) // config's own namespace defaults to brand.LabelNamespace ("dev.treerig")
		givePorts(tc)
		router := commandstest.NewFakeRouter(80)
		index := &commandstest.FakeIndex{}

		_, err := up(t, tc, router, index, commands.UpOptions{})
		wantErr(t, err, "labelled under dev\\.predecessor, but treerig\\.toml now says dev\\.treerig")
		if router.Ensures() != 0 {
			t.Fatalf("expected the router never to be ensured once relabelling is refused, got %d", router.Ensures())
		}
		if len(w.Docker.Owned(ref, "dev.predecessor")) == 0 {
			t.Fatalf("expected the old namespace's resources to remain untouched")
		}
	})

	t.Run("refuses when the registration was removed while waiting for the router", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		router := commandstest.NewFakeRouter(80)
		router.BeforeEnsure(func() {
			identity, err := tc.Registry.Identities.Inspect(context.Background(), tc.Registry.Identities.Root, registry.InspectOptions{})
			if err != nil || identity == nil {
				t.Fatalf("inspect: %v, %v", identity, err)
			}
			if _, err := tc.Registry.Store(identity.RepositoryID).Remove(context.Background(), identity.WorktreeID, "feature-login", registry.Writer{Logger: tc.Logger, Invocation: "treerig", UserHome: tc.UserHome}); err != nil {
				t.Fatalf("remove: %v", err)
			}
		})
		index := &commandstest.FakeIndex{}

		_, err := up(t, tc, router, index, commands.UpOptions{})
		wantErr(t, err, "was removed while 'treerig up' was waiting for the shared router")
	})

	t.Run("refuses when the registration was relabelled while waiting for the router", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		router := commandstest.NewFakeRouter(80)
		relabelled := false
		router.BeforeEnsure(func() {
			if relabelled {
				return
			}
			relabelled = true
			other := registry.New(registry.Options{
				ControlHome: tc.ControlHome, Root: tc.Loaded.Root, Domain: acme,
				LabelNamespace: "dev.other", Runner: tc.Runner, Logger: tc.Logger,
				UserHome: tc.UserHome,
			})
			if _, err := other.Ensure(context.Background(), ""); err != nil {
				t.Fatalf("relabel: %v", err)
			}
		})
		index := &commandstest.FakeIndex{}

		_, err := up(t, tc, router, index, commands.UpOptions{})
		wantErr(t, err, "was re-labelled under dev.other while 'treerig up' was waiting for the shared router")
	})

	t.Run("refuses when the checkout's identity markers changed while waiting for the router", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		router := commandstest.NewFakeRouter(80)
		router.BeforeEnsure(func() {
			identity, err := tc.Registry.Identities.Inspect(context.Background(), tc.Registry.Identities.Root, registry.InspectOptions{})
			if err != nil || identity == nil {
				t.Fatalf("inspect: %v, %v", identity, err)
			}
			// Overwritten with a different, still well-formed UUID: exactly
			// what a concurrent process re-minting the marker would leave, and
			// distinct from the "registration was removed" case above, which
			// never touches the marker itself.
			marker := filepath.Join(identity.GitDir, brand.MarkerDir, "worktree-id")
			if err := os.WriteFile(marker, []byte(registry.IdentityMarkerContents("11111111-1111-4111-8111-111111111111")), 0o600); err != nil {
				t.Fatalf("rewrite worktree marker: %v", err)
			}
		})
		index := &commandstest.FakeIndex{}

		_, err := up(t, tc, router, index, commands.UpOptions{})
		wantErr(t, err, "This checkout's treerig markers changed or were removed while 'treerig up' was waiting for the shared router")
	})

	// #19: the base loader accepts a control character in [service.index]
	// label or purpose — down, worktree remove, status and the rest load
	// the same file and proceed — but `up` itself is one of the few
	// callers that refuses one, and it
	// must refuse before making any Docker call.
	t.Run("refuses a control character in a service index label or purpose, before ensuring the router", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		givePorts(tc)
		tc.Loaded.Config.Service[0].Index = &config.ServiceIndex{Label: "a\tb"}
		router := commandstest.NewFakeRouter(80)
		index := &commandstest.FakeIndex{}

		_, err := up(t, tc, router, index, commands.UpOptions{})
		want := tc.Loaded.File + " is not a valid treerig.toml:\n  service.api.index.label: must not contain control characters"
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if router.Ensures() != 0 {
			t.Fatalf("router.Ensures() = %d, want 0: up must refuse before touching Docker", router.Ensures())
		}
	})
}
