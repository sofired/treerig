package commands_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/commands/commandstest"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/router"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/worktree"
)

// sharedRoute is the route file of mailpit in the world's repository, read
// back: its text, or "" when there is none.
func (w *sharedWorld) sharedRoute(t *testing.T, tc *commandstest.TestContext, name string) string {
	t.Helper()
	data, err := os.ReadFile(router.SharedFragmentPath(w.RouterState, w.repository(t, tc), name))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// recordOf is the world's shared record for name.
func (w *sharedWorld) recordOf(t *testing.T, tc *commandstest.TestContext, name string) registry.SharedRecord {
	t.Helper()
	record, err := tc.Registry.Store(w.repository(t, tc)).FindShared(name)
	if err != nil || record == nil {
		t.Fatalf("record of %s = %v, %v", name, record, err)
	}
	return *record
}

// sharedUpRouted runs `shared up` with the world's router and index doubles.
func (w *sharedWorld) sharedUpRouted(tc *commandstest.TestContext, options commands.SharedUpOptions) error {
	return commands.SharedUpWithContext(context.Background(), tc.CommandContext, w.router, w.index, options)
}

func TestSharedServicesAreRouted(t *testing.T) {
	t.Run("up writes the route file for a routed port, from the record, under the shared host", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		record := w.recordOf(t, tc, "mailpit")
		if record.Routed != 8025 || record.Host() != "mailpit.shared.acme.localhost" {
			t.Fatalf("record = %+v", record)
		}
		route := w.sharedRoute(t, tc, "mailpit")
		name := layout.SharedProjectName(w.repository(t, tc)) + "_shared_mailpit"
		for _, want := range []string{
			`"` + name + `": {`,
			"Host(`mailpit.shared.acme.localhost`)",
			"http://berth-loopback:" + itoa(record.Ports[8025]),
		} {
			if !strings.Contains(route, want) {
				t.Errorf("route file lacks %q:\n%s", want, route)
			}
		}
		if strings.Contains(route, "http://berth-loopback:"+itoa(record.Ports[1025])) {
			t.Errorf("the raw port is routed:\n%s", route)
		}
	})

	t.Run("names the file shared.<repository>.<name>.yaml, which no service sweep sees", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		repository := result.Registration.RepositoryID
		if _, err := os.Stat(filepath.Join(w.RouterState.DynamicDir, "shared."+repository+".mailpit.yaml")); err != nil {
			t.Fatal(err)
		}
		listed, err := router.ListFragments(w.RouterState)
		if err != nil {
			t.Fatal(err)
		}
		for _, fragment := range listed {
			if strings.HasPrefix(filepath.Base(fragment.Path), "shared.") {
				t.Errorf("the service sweep lists %s", fragment.Path)
			}
		}
		if err := router.RemoveWorktreeFragments(w.RouterState, repository, result.Registration.WorktreeID); err != nil {
			t.Fatal(err)
		}
		if w.sharedRoute(t, tc, "mailpit") == "" {
			t.Error("a worktree's route sweep removed the shared route file")
		}
	})

	t.Run("a service with no routed port gets none", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		tc.Loaded.Config.Shared.Service[0].Port = nil
		tc.Loaded.Config.Shared.Service[0].Ports = []int{1025}
		w.mustUp(t, tc)
		if record := w.recordOf(t, tc, "mailpit"); record.Routed != 0 || record.Host() != "" {
			t.Errorf("record = %+v", record)
		}
		if route := w.sharedRoute(t, tc, "mailpit"); route != "" {
			t.Errorf("route file:\n%s", route)
		}
	})

	t.Run("a repeated up repairs a deleted or stale file without touching the container", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		path := router.SharedFragmentPath(w.RouterState, w.repository(t, tc), "mailpit")
		want := w.sharedRoute(t, tc, "mailpit")
		for name, damage := range map[string]func() error{
			"deleted": func() error { return os.Remove(path) },
			"stale": func() error {
				return os.WriteFile(path, []byte(strings.ReplaceAll(want, itoa(w.recordOf(t, tc, "mailpit").Ports[8025]), "29999")), 0o644)
			},
		} {
			if err := damage(); err != nil {
				t.Fatal(err)
			}
			ups := w.sharedUps(t, tc)
			w.mustUp(t, tc)
			if got := w.sharedRoute(t, tc, "mailpit"); got != want {
				t.Errorf("%s: route file:\n%s\nwant:\n%s", name, got, want)
			}
			if w.sharedUps(t, tc) != ups {
				t.Errorf("%s: the running instance was started again", name)
			}
		}
	})

	t.Run("is written only for a record this control home holds", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		repository := result.Registration.RepositoryID
		// Another control home started it: this one has no record, and a file
		// it did not write is neither created nor removed here.
		if err := os.Remove(layout.SharedLayout(w.ControlHome, repository).RecordPath("mailpit")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(router.SharedFragmentPath(w.RouterState, repository, "mailpit")); err != nil {
			t.Fatal(err)
		}
		w.mustUp(t, tc)
		if route := w.sharedRoute(t, tc, "mailpit"); route != "" {
			t.Errorf("a route file was written without a record:\n%s", route)
		}
		other := router.SharedFragmentPath(w.RouterState, repository, "mailpit")
		if err := router.WriteSharedFragment(w.RouterState, repository, "mailpit", "acme.localhost", 31000); err != nil {
			t.Fatal(err)
		}
		w.mustUp(t, tc)
		if _, err := os.Stat(other); err != nil {
			t.Errorf("a file with no record here was removed: %v", err)
		}
	})

	t.Run("a raw port that becomes the web UI is adopted by a running instance", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		tc.Loaded.Config.Shared.Service[0].Port = nil
		tc.Loaded.Config.Shared.Service[0].Ports = []int{8025, 1025}
		w.mustUp(t, tc)
		if route := w.sharedRoute(t, tc, "mailpit"); route != "" {
			t.Fatalf("route file before the port is declared:\n%s", route)
		}
		port := 8025
		tc.Loaded.Config.Shared.Service[0].Port = &port
		tc.Loaded.Config.Shared.Service[0].Ports = []int{1025}
		ups := w.sharedUps(t, tc)
		w.mustUp(t, tc)
		if w.recordOf(t, tc, "mailpit").Routed != 8025 || w.sharedRoute(t, tc, "mailpit") == "" {
			t.Errorf("record = %+v, route:\n%s", w.recordOf(t, tc, "mailpit"), w.sharedRoute(t, tc, "mailpit"))
		}
		if w.sharedUps(t, tc) != ups {
			t.Error("the running instance was restarted to route it")
		}
	})

	t.Run("a port that is not pinned waits for a recreate", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		tc.Loaded.Config.Shared.Service[0].Port = nil
		tc.Loaded.Config.Shared.Service[0].Ports = []int{1025}
		w.mustUp(t, tc)
		port := 8025
		tc.Loaded.Config.Shared.Service[0].Port = &port
		w.mustUp(t, tc)
		if record := w.recordOf(t, tc, "mailpit"); record.Routed != 0 || record.Ports[8025] != 0 {
			t.Errorf("a port the container does not publish was pinned: %+v", record)
		}
		if err := w.sharedUpRouted(tc, commands.SharedUpOptions{Names: []string{"mailpit"}, Recreate: true}); err != nil {
			t.Fatal(err)
		}
		if w.recordOf(t, tc, "mailpit").Routed != 8025 || w.sharedRoute(t, tc, "mailpit") == "" {
			t.Errorf("record = %+v", w.recordOf(t, tc, "mailpit"))
		}
	})
}

func TestSharedUpEnsuresTheRouterForARoutedUI(t *testing.T) {
	t.Run("shared up ensures the router, writes the route and refreshes the index", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		ensures := w.router.Ensures()
		reasons := len(w.index.Reasons())
		if err := w.sharedUpRouted(tc, commands.SharedUpOptions{}); err != nil {
			t.Fatal(err)
		}
		if w.router.Ensures() != ensures+1 {
			t.Errorf("ensures = %d, want %d", w.router.Ensures(), ensures+1)
		}
		if got := w.index.Reasons(); len(got) != reasons+1 || got[len(got)-1] != "shared up" {
			t.Errorf("index reasons = %v", got)
		}
		if w.sharedRoute(t, tc, "mailpit") == "" {
			t.Error("no route file")
		}
	})

	t.Run("a service with no web UI starts no router", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		tc.Loaded.Config.Shared.Service[0].Port = nil
		tc.Loaded.Config.Shared.Service[0].Ports = []int{1025}
		w.mustUp(t, tc)
		ensures := w.router.Ensures()
		if err := w.sharedUpRouted(tc, commands.SharedUpOptions{}); err != nil {
			t.Fatal(err)
		}
		if w.router.Ensures() != ensures {
			t.Errorf("the router was ensured for a service with nothing to route")
		}
	})

	t.Run("a router that will not start stops the command before anything changes", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		if err := w.sharedDown(tc, commands.SharedDownOptions{}); err != nil {
			t.Fatal(err)
		}
		w.router.FailEnsure(os.ErrDeadlineExceeded)
		if err := w.sharedUpRouted(tc, commands.SharedUpOptions{}); err == nil {
			t.Fatal("shared up succeeded without a router")
		}
		if w.mailpit(t, tc) != "" {
			t.Error("a container was started")
		}
	})
}

func TestSharedDownRemovesTheRoute(t *testing.T) {
	t.Run("shared down removes the route file and keeps the record", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		if err := w.sharedDown(tc, commands.SharedDownOptions{}); err != nil {
			t.Fatal(err)
		}
		if route := w.sharedRoute(t, tc, "mailpit"); route != "" {
			t.Errorf("route file after down:\n%s", route)
		}
		if w.recordOf(t, tc, "mailpit").Routed != 8025 {
			t.Error("the record lost its routed port")
		}
	})

	t.Run("a named down removes only that service's route", func(t *testing.T) {
		w := newSharedWorld(t)
		w.Docker.SetSharedService("minio", map[string]any{"image": "minio/minio:latest"})
		tc := w.context("")
		minioPort := 9001
		tc.Loaded.Config.Shared.Service = append(tc.Loaded.Config.Shared.Service, config.SharedService{Name: "minio", Port: &minioPort, UsedBy: []string{"api"}})
		w.mustUp(t, tc)
		if err := w.sharedDown(tc, commands.SharedDownOptions{Names: []string{"minio"}}); err != nil {
			t.Fatal(err)
		}
		if w.sharedRoute(t, tc, "minio") != "" || w.sharedRoute(t, tc, "mailpit") == "" {
			t.Errorf("minio route %q, mailpit route %q", w.sharedRoute(t, tc, "minio"), w.sharedRoute(t, tc, "mailpit"))
		}
	})

	t.Run("down --volumes removes the route before the record", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		if err := w.sharedDown(tc, commands.SharedDownOptions{Volumes: true, Yes: true}); err != nil {
			t.Fatal(err)
		}
		if w.sharedRoute(t, tc, "mailpit") != "" {
			t.Error("route file survived down --volumes")
		}
		if record, _ := tc.Registry.Store(result.Registration.RepositoryID).FindShared("mailpit"); record != nil {
			t.Error("record survived down --volumes")
		}
	})

	t.Run("works from a checkout with no berth.toml", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		if err := w.sharedDown(downgrade(tc), commands.SharedDownOptions{}); err != nil {
			t.Fatal(err)
		}
		if w.sharedRoute(t, tc, "mailpit") != "" {
			t.Error("route file survived a down without a config")
		}
	})

	t.Run("another repository's route file of the same name stays", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		other := "99999999-9999-4999-8999-999999999999"
		if err := router.WriteSharedFragment(w.RouterState, other, "mailpit", "acme.localhost", 31000); err != nil {
			t.Fatal(err)
		}
		if err := w.sharedDown(tc, commands.SharedDownOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(router.SharedFragmentPath(w.RouterState, other, "mailpit")); err != nil {
			t.Errorf("the other repository's route file was removed: %v", err)
		}
	})
}

func TestHooksAndCommandsGetTheSharedURL(t *testing.T) {
	w := newSharedWorld(t)
	tc := w.context("")
	tc.Loaded.Config.Hooks = config.Hooks{Prepare: ns("./hooks/prepare")}
	tc.Loaded.Config.Commands = config.Commands{{Name: "show", Command: config.Command{Command: "./show", Lock: true, KillAfter: "10s"}}}
	w.mustUp(t, tc)
	envOf := func(suffix string) []string {
		for _, call := range w.Runner.Calls() {
			if strings.HasSuffix(call.Command, suffix) {
				return call.Env
			}
		}
		t.Fatalf("no %s call", suffix)
		return nil
	}
	const want = "BERTH_SHARED_URL_MAILPIT=http://mailpit.shared.acme.localhost"
	t.Run("up's hooks", func(t *testing.T) {
		if !slices.Contains(envOf("hooks/prepare"), want) {
			t.Errorf("%s missing", want)
		}
	})
	t.Run("berth run, with the router's port folded in", func(t *testing.T) {
		tc.CommandContext.RouterPort = 8080
		if err := commands.RunWithContext(context.Background(), tc.CommandContext, commands.RunOptions{Name: "show"}); err != nil {
			t.Fatal(err)
		}
		if env := envOf("/show"); !slices.Contains(env, want+":8080") {
			t.Errorf("env = %v", env)
		}
	})
	t.Run("a variable inherited from another worktree is dropped", func(t *testing.T) {
		tc.Environ = append(slices.Clone(tc.Environ), "BERTH_SHARED_URL_STALE=http://stale")
		tc.CommandContext.RouterPort = 80
		if err := commands.RunWithContext(context.Background(), tc.CommandContext, commands.RunOptions{Name: "show"}); err != nil {
			t.Fatal(err)
		}
		calls := w.Runner.Calls()
		if env := calls[len(calls)-1].Env; slices.Contains(env, "BERTH_SHARED_URL_STALE=http://stale") {
			t.Error("an inherited BERTH_SHARED_URL_* reached the command")
		}
	})
	t.Run("never in the environment Compose gets", func(t *testing.T) {
		for _, call := range w.Runner.Calls() {
			if call.Command != "docker" {
				continue
			}
			for _, entry := range call.Env {
				if strings.HasPrefix(entry, "BERTH_SHARED_URL_") {
					t.Fatalf("docker %v got %s", call.Args, entry)
				}
			}
		}
	})
}

func TestStatusReportsTheSharedURL(t *testing.T) {
	ctx := context.Background()
	t.Run("shared status --json carries url for a routed service only", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		tc.Logger.Lines, tc.Logger.Printed = nil, nil
		if err := commands.SharedStatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{JSON: true}); err != nil {
			t.Fatal(err)
		}
		_, printed := tc.Logger.Snapshot()
		var report struct {
			Shared []map[string]any `json:"shared"`
		}
		if err := json.Unmarshal([]byte(strings.Join(printed, "\n")), &report); err != nil {
			t.Fatal(err)
		}
		if report.Shared[0]["url"] != "http://mailpit.shared.acme.localhost" {
			t.Errorf("entry = %v", report.Shared[0])
		}
	})

	t.Run("the url key is absent when nothing is routed", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		tc.Loaded.Config.Shared.Service[0].Port = nil
		tc.Loaded.Config.Shared.Service[0].Ports = []int{1025}
		w.mustUp(t, tc)
		tc.Logger.Lines, tc.Logger.Printed = nil, nil
		if _, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{JSON: true}); err != nil {
			t.Fatal(err)
		}
		_, printed := tc.Logger.Snapshot()
		if strings.Contains(strings.Join(printed, "\n"), `"url"`) {
			t.Errorf("status:\n%s", strings.Join(printed, "\n"))
		}
	})

	t.Run("the router's port is folded in, and a repository never brought up has none", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		tc.CommandContext.RouterPort = 8080
		tc.Logger.Lines, tc.Logger.Printed = nil, nil
		if err := commands.SharedStatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{}); err != nil {
			t.Fatal(err)
		}
		_, printed := tc.Logger.Snapshot()
		if !slices.Contains(printed, "  url http://mailpit.shared.acme.localhost:8080") {
			t.Errorf("printed = %q", printed)
		}
	})
}

func TestTwoRepositoriesCannotRouteTheSameSharedName(t *testing.T) {
	const other = "99999999-9999-4999-8999-999999999999"
	w := newSharedWorld(t)
	tc := w.context("")
	routed := 8025
	// Another repository of this control home already routes a mailpit on acme.localhost.
	if _, err := tc.Registry.Store(other).SyncSharedPorts(context.Background(), registry.Writer{Logger: tc.Logger, Invocation: "test", UserHome: tc.UserHome}, registry.SharedPortPlan{
		Name: "mailpit", Domain: "acme.localhost", Ports: []int{8025}, Routed: &routed,
		Range: registrytest.PortRange, Probe: tc.PortProbe,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := w.up(t, tc)
	if err == nil {
		t.Fatal("the second repository was allowed to route mailpit.shared.acme.localhost")
	}
	for _, want := range []string{"mailpit.shared.acme.localhost", "repository " + other} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
	if w.mailpit(t, tc) != "" {
		t.Error("a container was started before the conflict was found")
	}
	if w.sharedRoute(t, tc, "mailpit") != "" {
		t.Error("a route file was written for the refused service")
	}
}

func TestDoctorSharedRoute(t *testing.T) {
	t.Run("a running routed service with its file is ok and says where it routes", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		record := w.recordOf(t, tc, "mailpit")
		check := only(t, sharedChecks(t, tc, "shared.route"))
		want := "http://mailpit.shared.acme.localhost routes to 127.0.0.1:" + itoa(record.Ports[8025])
		if check.Status != commands.StatusOK || check.Instance != "mailpit" || check.Detail != want {
			t.Errorf("route = %+v, want %q", check, want)
		}
	})

	t.Run("a running routed service without its file is a warning that names the way out", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		if err := os.Remove(router.SharedFragmentPath(w.RouterState, w.repository(t, tc), "mailpit")); err != nil {
			t.Fatal(err)
		}
		check := only(t, sharedChecks(t, tc, "shared.route"))
		if check.Status != commands.StatusWarn || !strings.Contains(check.Detail, "has no route file") || !strings.Contains(check.Detail, "'berth shared up' writes it") {
			t.Errorf("route = %+v", check)
		}
	})

	t.Run("a service that is down has no route and no line about it", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		if err := w.sharedDown(tc, commands.SharedDownOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := sharedChecks(t, tc, "shared.route"); len(got) != 0 {
			t.Errorf("routes = %+v", got)
		}
	})

	t.Run("a service with no web UI has no line", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		tc.Loaded.Config.Shared.Service[0].Port = nil
		tc.Loaded.Config.Shared.Service[0].Ports = []int{1025}
		w.mustUp(t, tc)
		if got := sharedChecks(t, tc, "shared.route"); len(got) != 0 {
			t.Errorf("routes = %+v", got)
		}
	})
}

// watchDestructive calls observe, with the docker arguments, before every
// call that stops or removes something, so a test can assert what was already
// true at that moment rather than only at the end.
func (w *sharedWorld) watchDestructive(observe func(args []string)) {
	inner := w.Runner.Handler
	w.Runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
		if spec.Command == "docker" && len(spec.Args) > 0 {
			switch {
			case spec.Args[0] == "stop", spec.Args[0] == "rm",
				len(spec.Args) > 1 && (spec.Args[0] == "network" || spec.Args[0] == "volume") && spec.Args[1] == "rm":
				observe(spec.Args)
			}
		}
		return inner(ctx, spec)
	}
}

func TestSharedDownUnroutesBeforeItRemovesAnything(t *testing.T) {
	for _, volumes := range []bool{false, true} {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		repository := result.Registration.RepositoryID
		observed := 0
		w.watchDestructive(func(args []string) {
			observed++
			if route := w.sharedRoute(t, tc, "mailpit"); route != "" {
				t.Errorf("volumes=%v: docker %v ran while the route file still existed", volumes, args)
			}
			// The record is released last (decision 5): it is still there for every removal.
			if record, _ := tc.Registry.Store(repository).FindShared("mailpit"); record == nil {
				t.Errorf("volumes=%v: the record was gone before docker %v", volumes, args)
			}
		})
		if err := w.sharedDown(tc, commands.SharedDownOptions{Volumes: volumes, Yes: true}); err != nil {
			t.Fatal(err)
		}
		if observed == 0 {
			t.Fatalf("volumes=%v: nothing was observed", volumes)
		}
	}
}

func TestSharedDownRemovesARouteLeftByAContainerStoppedAnotherWay(t *testing.T) {
	w := newSharedWorld(t)
	tc := w.context("")
	w.mustUp(t, tc)
	// Removed behind berth's back: the route file stays, and the hostname would 502.
	if _, err := w.Docker.Handle(context.Background(), run.Spec{Command: "docker", Args: []string{"rm", "--force", w.mailpit(t, tc)}}); err != nil {
		t.Fatal(err)
	}
	if w.sharedRoute(t, tc, "mailpit") == "" {
		t.Fatal("no route file to leave behind")
	}
	if err := w.sharedDown(tc, commands.SharedDownOptions{}); err != nil {
		t.Fatal(err)
	}
	if w.sharedRoute(t, tc, "mailpit") != "" {
		t.Error("the route file of a service that was not running survived shared down")
	}
}

func TestADroppedPortStopsRoutingAReusedContainer(t *testing.T) {
	w := newSharedWorld(t)
	tc := w.context("")
	w.mustUp(t, tc)
	record := w.recordOf(t, tc, "mailpit")
	tc.Loaded.Config.Shared.Service[0].Port = nil
	tc.Loaded.Config.Shared.Service[0].Ports = []int{1025}
	ups := w.sharedUps(t, tc)
	w.mustUp(t, tc)
	after := w.recordOf(t, tc, "mailpit")
	if after.Routed != 0 || after.Ports[8025] != record.Ports[8025] {
		t.Errorf("record = %+v, was %+v: it should stop routing and keep the pin", after, record)
	}
	if w.sharedRoute(t, tc, "mailpit") != "" {
		t.Error("the route file survived the port being dropped")
	}
	if w.sharedUps(t, tc) != ups {
		t.Error("the running container was restarted")
	}
	// And declaring it again routes it again, with no restart.
	port := 8025
	tc.Loaded.Config.Shared.Service[0].Port = &port
	tc.Loaded.Config.Shared.Service[0].Ports = []int{1025}
	w.mustUp(t, tc)
	if w.recordOf(t, tc, "mailpit").Routed != 8025 || w.sharedRoute(t, tc, "mailpit") == "" {
		t.Error("declaring the port again did not route it")
	}
}

func TestAnUnpinnedPortIsSaidToNeedARecreate(t *testing.T) {
	w := newSharedWorld(t)
	tc := w.context("")
	tc.Loaded.Config.Shared.Service[0].Port = nil
	tc.Loaded.Config.Shared.Service[0].Ports = []int{1025}
	w.mustUp(t, tc)
	port := 8025
	tc.Loaded.Config.Shared.Service[0].Port = &port
	w.mustUp(t, tc)
	if !strings.Contains(lines(tc), "port 8025 is not pinned") || !strings.Contains(lines(tc), "'berth shared up --recreate mailpit'") {
		t.Errorf("output:\n%s", lines(tc))
	}
}

func TestAStartThatFailedAfterCreatingTheContainerStillRoutesIt(t *testing.T) {
	w := newSharedWorld(t)
	tc := w.context("")
	created := false
	w.Docker.BeforeComposeUp(func(p string) {
		if !strings.HasPrefix(p, brand.SharedProjectPrefix) || created {
			return
		}
		created = true
		labels := commandstest.Ownership(worktree.Ref{RepositoryID: w.repository(t, tc)}, brand.LabelNamespace)
		delete(labels, brand.WorktreeLabel(brand.LabelNamespace))
		labels[brand.SharedLabel(brand.LabelNamespace)] = "true"
		labels["com.docker.compose.project"] = p
		labels["com.docker.compose.service"] = "mailpit"
		id := w.Docker.Add("container", labels)
		w.Docker.SetName(id, p+"-mailpit-1")
	})
	w.Docker.FailCall("--no-recreate", "interrupted", 1, 1)
	if _, err := w.up(t, tc); err == nil {
		t.Fatal("up succeeded")
	}
	if w.sharedRoute(t, tc, "mailpit") == "" {
		t.Error("a service left running by an interrupted start has no route")
	}
}

func TestADoctorReportsARouteFileOfAServiceThatIsNotRunning(t *testing.T) {
	w := newSharedWorld(t)
	tc := w.context("")
	w.mustUp(t, tc)
	w.Docker.SetStatus(w.mailpit(t, tc), "exited")
	check := only(t, sharedChecks(t, tc, "shared.route"))
	if check.Status != commands.StatusWarn || !strings.Contains(check.Detail, "the route file is present but the service is not running") ||
		!strings.Contains(check.Detail, "answers 502") {
		t.Errorf("route = %+v", check)
	}
}

func TestStatusSaysAStoppedServicesURLIsNotRunning(t *testing.T) {
	w := newSharedWorld(t)
	tc := w.context("")
	w.mustUp(t, tc)
	if err := w.sharedDown(tc, commands.SharedDownOptions{}); err != nil {
		t.Fatal(err)
	}
	tc.Logger.Lines, tc.Logger.Printed = nil, nil
	if err := commands.SharedStatusWithContext(context.Background(), tc.CommandContext, commands.OutputOptions{}); err != nil {
		t.Fatal(err)
	}
	_, printed := tc.Logger.Snapshot()
	if !slices.Contains(printed, "  url http://mailpit.shared.acme.localhost (not running)") {
		t.Errorf("printed = %q", printed)
	}
}

func TestSharedDownDoesNotStripARouteThatAppearedWhileItDecided(t *testing.T) {
	w := newSharedWorld(t)
	tc := w.context("")
	w.mustUp(t, tc)
	repository := w.repository(t, tc)
	// The container and its network go away behind berth's back: the plan is
	// empty, and only a route file is left.
	remove := func(args ...string) {
		if _, err := w.Docker.Handle(context.Background(), run.Spec{Command: "docker", Args: args}); err != nil {
			t.Fatal(err)
		}
	}
	remove("rm", "--force", w.mailpit(t, tc))
	for _, network := range w.Docker.Find("network", map[string]string{brand.SharedLabel(brand.LabelNamespace): "true"}) {
		remove("network", "rm", network)
	}
	// Between the plan and the lock another `shared up` starts it again.
	inner := w.Runner.Handler
	started := false
	w.Runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
		if !started && spec.Command == "docker" && len(spec.Args) > 1 && spec.Args[0] == "network" && spec.Args[1] == "ls" {
			started = true
			p := layout.SharedProjectName(repository)
			labels := commandstest.Ownership(worktree.Ref{RepositoryID: repository}, brand.LabelNamespace)
			delete(labels, brand.WorktreeLabel(brand.LabelNamespace))
			labels[brand.SharedLabel(brand.LabelNamespace)] = "true"
			labels["com.docker.compose.project"] = p
			labels["com.docker.compose.service"] = "mailpit"
			id := w.Docker.Add("container", labels)
			w.Docker.SetName(id, p+"-mailpit-1")
		}
		return inner(ctx, spec)
	}
	err := w.sharedDown(tc, commands.SharedDownOptions{})
	if !started {
		t.Fatal("the hook never ran: the plan was not made through network ls")
	}
	if err == nil || !strings.Contains(err.Error(), "started while") {
		t.Fatalf("err = %v, want the refusal that something started", err)
	}
	if w.sharedRoute(t, tc, "mailpit") == "" {
		t.Error("the route of a service that started meanwhile was stripped")
	}
}
