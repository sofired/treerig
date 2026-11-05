package commands_test

// What `status` shows beyond the services' states (#203): the URL column and
// when it is filled, the router's state, host routes and shared services as
// rows, the worktree argument, and what is said when there is nothing to list.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/commands/commandstest"
	"github.com/sofired/treerig/internal/router"
	"github.com/sofired/treerig/internal/worktree"
)

// dockerDown is what Docker prints when no daemon answers.
const dockerDown = "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"

// statusJSON runs `status --json` and decodes the one document it prints.
func statusJSON(t *testing.T, tc *commandstest.TestContext, worktreeName string) (commands.StatusReport, error) {
	t.Helper()
	_, err := commands.StatusWithContext(context.Background(), tc.CommandContext, commands.StatusOptions{JSON: true, Worktree: worktreeName})
	_, printed := tc.Logger.Snapshot()
	if len(printed) != 1 {
		t.Fatalf("stdout = %q, want one document", printed)
	}
	var report commands.StatusReport
	if decodeErr := json.Unmarshal([]byte(printed[0]), &report); decodeErr != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%q", decodeErr, printed)
	}
	return report, err
}

func TestStatusShowsURLsOnlyWhileTheyLoad(t *testing.T) {
	ctx := context.Background()
	registered := func(t *testing.T) (*commandstest.World, *commandstest.TestContext) {
		t.Helper()
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api", "mail")
		return w, w.Context("", acme, nil)
	}

	t.Run("fills the URL of a running service while the router runs", func(t *testing.T) {
		w, tc := registered(t)
		w.Docker.AddRouter(true)
		if _, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.StatusOptions{}); err != nil {
			t.Fatal(err)
		}
		lines, printed := tc.Logger.Snapshot()
		want := []string{
			"feature-login.acme.localhost   running",
			"  SERVICE  STATE    URL",
			"  api      running  http://feature-login.acme.localhost",
			"  mail     running  http://mail.feature-login.acme.localhost",
		}
		if !slices.Equal(printed, want) {
			t.Fatalf("printed = %q", printed)
		}
		if len(lines) != 0 {
			t.Fatalf("stderr = %q, want nothing", lines)
		}
	})

	t.Run("shows - for a service that does not run, and for a service with no route", func(t *testing.T) {
		w, tc := registered(t)
		w.Docker.AddRouter(true)
		w.Docker.Services = []string{"api", "mail", "worker"}
		w.Docker.SetStatus(w.Docker.Find("container", map[string]string{"com.docker.compose.service": "mail"})[0], "exited")
		if _, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.StatusOptions{}); err != nil {
			t.Fatal(err)
		}
		_, printed := tc.Logger.Snapshot()
		want := []string{
			"feature-login.acme.localhost   partly running",
			"  SERVICE  STATE            URL",
			"  api      running          http://feature-login.acme.localhost",
			"  mail     exited (code 0)  -",
			"  worker   not created      -",
		}
		if !slices.Equal(printed, want) {
			t.Fatalf("printed = %q", printed)
		}
	})

	t.Run("shows - everywhere and says so on stderr while the router is stopped", func(t *testing.T) {
		for name, router := range map[string]func(*commandstest.World){
			"no router container": func(*commandstest.World) {},
			"an exited one":       func(w *commandstest.World) { w.Docker.AddRouter(false) },
		} {
			t.Run(name, func(t *testing.T) {
				w, tc := registered(t)
				router(w)
				if _, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.StatusOptions{}); err != nil {
					t.Fatal(err)
				}
				lines, printed := tc.Logger.Snapshot()
				want := []string{
					"feature-login.acme.localhost   running",
					"  SERVICE  STATE    URL",
					"  api      running  -",
					"  mail     running  -",
				}
				if !slices.Equal(printed, want) {
					t.Fatalf("printed = %q", printed)
				}
				if !slices.Equal(lines, []string{"info: The router is stopped, so no URL loads. Run 'treerig up' to start it."}) {
					t.Fatalf("stderr = %q", lines)
				}
			})
		}
	})

	t.Run("does not talk about the router when nothing is routed", func(t *testing.T) {
		_, tc := registered(t)
		tc.Loaded.Config.Service = nil
		if _, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.StatusOptions{}); err != nil {
			t.Fatal(err)
		}
		if lines, _ := tc.Logger.Snapshot(); len(lines) != 0 {
			t.Fatalf("stderr = %q", lines)
		}
	})

	t.Run("probes nothing over HTTP", func(t *testing.T) {
		w, tc := registered(t)
		w.Docker.AddRouter(true)
		if _, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.StatusOptions{}); err != nil {
			t.Fatal(err)
		}
		for _, spec := range w.Runner.Calls() {
			if spec.Command != "docker" && spec.Command != "git" {
				t.Errorf("ran %s %v", spec.Command, spec.Args)
			}
		}
	})
}

func TestStatusJSONCarriesURLsAndTheRouter(t *testing.T) {
	registered := func(t *testing.T) (*commandstest.World, *commandstest.TestContext) {
		t.Helper()
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api", "mail")
		return w, w.Context("", acme, nil)
	}

	t.Run("gives each service its url and the router its state", func(t *testing.T) {
		w, tc := registered(t)
		w.Docker.AddRouter(true)
		report, err := statusJSON(t, tc, "")
		if err != nil {
			t.Fatal(err)
		}
		if report.Router == nil || report.Router.State != commands.RouterRunning || report.DockerError != "" {
			t.Fatalf("router = %+v, dockerError = %q", report.Router, report.DockerError)
		}
		if len(report.Services) != 2 || report.Services[0].URL != "http://feature-login.acme.localhost" || report.Services[1].URL != "http://mail.feature-login.acme.localhost" {
			t.Fatalf("services = %+v", report.Services)
		}
	})

	t.Run("leaves url out while the router is stopped, and still prints the document", func(t *testing.T) {
		_, tc := registered(t)
		report, err := statusJSON(t, tc, "")
		if err != nil {
			t.Fatal(err)
		}
		if report.Router == nil || report.Router.State != commands.RouterStopped {
			t.Fatalf("router = %+v", report.Router)
		}
		for _, service := range report.Services {
			if service.URL != "" {
				t.Fatalf("service %+v has a url", service)
			}
		}
		_, printed := tc.Logger.Snapshot()
		if strings.Contains(printed[0], `"url"`) {
			t.Fatalf("document = %s", printed[0])
		}
		if lines, _ := tc.Logger.Snapshot(); !slices.Contains(lines, "info: The router is stopped, so no URL loads. Run 'treerig up' to start it.") {
			t.Fatalf("the notice stays on stderr with --json: %q", lines)
		}
	})

	t.Run("reports the router unknown when only it cannot be read", func(t *testing.T) {
		w, tc := registered(t)
		w.Docker.FailCall("treerig-router", dockerDown, 1, -1)
		report, err := statusJSON(t, tc, "")
		if err != nil {
			t.Fatalf("a router that cannot be read does not fail status: %v", err)
		}
		if report.Router == nil || report.Router.State != commands.RouterUnknown {
			t.Fatalf("router = %+v", report.Router)
		}
		if lines, _ := tc.Logger.Snapshot(); !slices.Contains(lines, "info: The router's state couldn't be read, so no URL is shown. Run 'treerig doctor' to see why.") {
			t.Fatalf("stderr = %q", lines)
		}
	})

	t.Run("prints the document with dockerError, and fails, when Docker did not answer", func(t *testing.T) {
		w, tc := registered(t)
		w.Docker.FailCall("compose", dockerDown, 1, -1)
		report, err := statusJSON(t, tc, "")
		if err == nil || !strings.Contains(err.Error(), "Docker is not available") {
			t.Fatalf("err = %v", err)
		}
		if report.State == nil || *report.State != worktree.StateUnknown || !strings.HasPrefix(report.DockerError, "Docker is not available: ") {
			t.Fatalf("report = %+v", report)
		}
		if report.Router == nil || report.Router.State != commands.RouterUnknown || len(report.Services) != 0 {
			t.Fatalf("report = %+v", report)
		}
	})

	t.Run("says Docker did not answer once in the text form, after the header", func(t *testing.T) {
		w, tc := registered(t)
		w.Docker.FailCall("compose", dockerDown, 1, -1)
		_, err := commands.StatusWithContext(context.Background(), tc.CommandContext, commands.StatusOptions{})
		if err == nil || !strings.Contains(err.Error(), "Docker is not available") {
			t.Fatalf("err = %v", err)
		}
		lines, printed := tc.Logger.Snapshot()
		if !slices.Equal(printed, []string{"feature-login.acme.localhost   unknown"}) || len(lines) != 0 {
			t.Fatalf("printed = %q, stderr = %q", printed, lines)
		}
	})
}

// blockingListeners never answers: each probe waits for its context to end,
// and the most that waited at once is kept.
type blockingListeners struct {
	mu      sync.Mutex
	current int
	peak    int
}

// Answers is ports.ListenerProbe's Answers.
func (b *blockingListeners) Answers(ctx context.Context, _ string, _ int) bool {
	b.mu.Lock()
	b.current++
	b.peak = max(b.peak, b.current)
	b.mu.Unlock()
	<-ctx.Done()
	b.mu.Lock()
	b.current--
	b.mu.Unlock()
	return false
}

func TestStatusHostRouteProbesAreBounded(t *testing.T) {
	rw := newRouteWorld(t)
	// Two more than may be tried at once, and every one silent until the
	// shared budget ends: all of them are then not listening, and the number
	// in flight never passed the bound.
	names := make([]string, 10)
	for i := range names {
		names[i] = "r" + strconv.Itoa(i)
		rw.mustAdd(t, commands.RouteAddOptions{Name: names[i], Port: 5100 + i, Subdomain: names[i]})
	}
	probe := &blockingListeners{}
	rw.tc.Listeners = probe
	rw.tc.HostRouteBudget = 200 * time.Millisecond

	if _, err := commands.StatusWithContext(context.Background(), rw.tc.CommandContext, commands.StatusOptions{}); err != nil {
		t.Fatal(err)
	}
	if probe.peak != 8 {
		t.Fatalf("%d connects were in flight at once, want the bound of 8", probe.peak)
	}
	_, printed := rw.tc.Logger.Snapshot()
	for _, name := range names {
		if !slices.ContainsFunc(printed, func(line string) bool {
			return strings.HasPrefix(line, "  "+name+" (host route)") && strings.Contains(line, "not listening")
		}) {
			t.Errorf("no not-listening row for %s in %q", name, printed)
		}
	}
}

// A command cancelled while the router and the ports are being asked ends with
// the cancellation, and prints nothing that a cancelled probe made up.
func TestStatusStopsWhenCancelled(t *testing.T) {
	rw := newRouteWorld(t)
	rw.mustAdd(t, commands.RouteAddOptions{Name: "web", Port: 5173, Subdomain: "app"})
	ctx, cancel := context.WithCancel(context.Background())
	rw.tc.Listeners = cancelling{cancel: cancel}
	_, before := rw.tc.Logger.Snapshot()

	_, err := commands.StatusWithContext(ctx, rw.tc.CommandContext, commands.StatusOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, after := rw.tc.Logger.Snapshot(); len(after) != len(before) {
		t.Fatalf("a cancelled status printed %q", after[len(before):])
	}
}

// cancelling cancels the command the first time it is asked.
type cancelling struct{ cancel context.CancelFunc }

// Answers is ports.ListenerProbe's Answers.
func (c cancelling) Answers(context.Context, string, int) bool {
	c.cancel()
	return false
}

func TestStatusOfAWorktreeThatIsNotRegistered(t *testing.T) {
	t.Run("names it and says what to run, on stderr, exiting 0", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		if _, err := commands.StatusWithContext(context.Background(), tc.CommandContext, commands.StatusOptions{}); err != nil {
			t.Fatal(err)
		}
		lines, printed := tc.Logger.Snapshot()
		if !slices.Equal(lines, []string{"info: worktree is not registered. Run 'treerig up' to create its environment."}) || len(printed) != 0 {
			t.Fatalf("stderr = %q, stdout = %q", lines, printed)
		}
	})
}

func TestStatusOfANamedWorktree(t *testing.T) {
	ctx := context.Background()
	// theirs is widgets' worktree, whose treerig.toml is in its own directory.
	named := func(t *testing.T) (w *commandstest.World, tc *commandstest.TestContext, theirRoot string) {
		t.Helper()
		w, _, _, theirRoot = namedWorld(t)
		toml := "[project]\nname = \"widgets\"\ndomain = \"widgets.localhost\"\nlabel-namespace = \"dev.widgets\"\n\n[compose]\nfile = \"compose.yaml\"\n\n[service.api]\n"
		if err := os.WriteFile(filepath.Join(theirRoot, brand.ConfigFile), []byte(toml), 0o600); err != nil {
			t.Fatal(err)
		}
		w.Docker.AddRouter(true)
		return w, w.ContextWithoutConfig(""), theirRoot
	}

	t.Run("reads another repository's worktree from its own directory, from outside any repository", func(t *testing.T) {
		_, tc, _ := named(t)
		if _, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.StatusOptions{Worktree: "main.widgets.localhost"}); err != nil {
			t.Fatal(err)
		}
		lines, printed := tc.Logger.Snapshot()
		// widgets' worktree started api only; the Compose file declares mail too.
		want := []string{
			"main.widgets.localhost   partly running",
			"  SERVICE  STATE        URL",
			"  api      running      http://main.widgets.localhost",
			"  mail     not created  -",
		}
		if !slices.Equal(printed, want) || len(lines) != 0 {
			t.Fatalf("printed = %q, stderr = %q", printed, lines)
		}
	})

	t.Run("accepts a path inside its directory, since status only reads", func(t *testing.T) {
		_, tc, theirRoot := named(t)
		inside := filepath.Join(theirRoot, "src")
		if err := os.MkdirAll(inside, 0o700); err != nil {
			t.Fatal(err)
		}
		report, err := statusJSON(t, tc, inside)
		if err != nil || report.Host == nil || *report.Host != "main.widgets.localhost" {
			t.Fatalf("report = %+v, %v", report, err)
		}
	})

	t.Run("uses the namespace the registration recorded, not this directory's", func(t *testing.T) {
		_, tc, _ := named(t)
		report, err := statusJSON(t, tc, "main.widgets.localhost")
		if err != nil {
			t.Fatal(err)
		}
		// The containers are labelled dev.widgets, and this directory's
		// treerig.toml would say another namespace; a lookup under that one
		// would have found none.
		if len(report.Services) == 0 || report.Services[0].State != worktree.Running.JSON() {
			t.Fatalf("report = %+v", report)
		}
	})

	t.Run("says the directory is missing and stops, exiting 0", func(t *testing.T) {
		_, tc, theirRoot := named(t)
		if err := os.RemoveAll(theirRoot); err != nil {
			t.Fatal(err)
		}
		if _, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.StatusOptions{Worktree: "main.widgets.localhost"}); err != nil {
			t.Fatalf("a missing directory is a state status reports: %v", err)
		}
		lines, printed := tc.Logger.Snapshot()
		if !slices.Equal(printed, []string{"main.widgets.localhost   directory missing"}) {
			t.Fatalf("printed = %q", printed)
		}
		want := "info: The directory of main.widgets.localhost is missing, so its services can't be listed. If a drive is not connected, connect it. " +
			"Otherwise run 'treerig worktree remove main.widgets.localhost'. Directory: " + theirRoot
		if len(lines) != 1 || lines[0] != want {
			t.Fatalf("stderr = %q", lines)
		}
	})

	t.Run("prints the JSON document for a missing directory too", func(t *testing.T) {
		_, tc, theirRoot := named(t)
		if err := os.RemoveAll(theirRoot); err != nil {
			t.Fatal(err)
		}
		report, err := statusJSON(t, tc, "main.widgets.localhost")
		if err != nil || report.State == nil || *report.State != worktree.StateDirectoryMissing || len(report.Services) != 0 || report.Router != nil {
			t.Fatalf("report = %+v, %v", report, err)
		}
	})

	t.Run("fails when the directory is there and its treerig.toml is not", func(t *testing.T) {
		_, tc, theirRoot := named(t)
		if err := os.Remove(filepath.Join(theirRoot, brand.ConfigFile)); err != nil {
			t.Fatal(err)
		}
		_, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.StatusOptions{Worktree: "main.widgets.localhost"})
		if err == nil || !strings.Contains(err.Error(), "No treerig.toml at ") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("refuses a name that is none, as every command that takes a worktree does", func(t *testing.T) {
		_, tc, _ := named(t)
		_, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.StatusOptions{Worktree: "nowhere"})
		if err == nil || !strings.Contains(err.Error(), "No worktree is registered as nowhere") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a worktree no up has recorded a directory for says so", func(t *testing.T) {
		w, _, _, _ := namedWorld(t)
		w.Register(w.Worktree("fresh", "fresh"), "fresh.localhost", "fresh", brand.LabelNamespace)
		tc := w.ContextWithoutConfig("")
		_, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.StatusOptions{Worktree: "fresh.fresh.localhost"})
		if err != nil {
			t.Fatal(err)
		}
		lines, printed := tc.Logger.Snapshot()
		if len(printed) != 1 || !strings.HasPrefix(printed[0], "fresh.fresh.localhost   ") {
			t.Fatalf("printed = %q", printed)
		}
		if len(lines) != 1 || !strings.Contains(lines[0], "has no record of where fresh.fresh.localhost lives") {
			t.Fatalf("stderr = %q", lines)
		}
	})
}

func TestStatusShowsNoURLForARouterThatIsNotThisRootsOrPort(t *testing.T) {
	registered := func(t *testing.T) (*commandstest.World, *commandstest.TestContext) {
		t.Helper()
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api", "mail")
		return w, w.Context("", acme, nil)
	}
	noURLs := func(t *testing.T, tc *commandstest.TestContext) {
		t.Helper()
		_, printed := tc.Logger.Snapshot()
		for _, line := range printed[2:] {
			if !strings.HasSuffix(line, "  -") {
				t.Errorf("row %q has a URL", line)
			}
		}
	}

	t.Run("a router on another port than this root's", func(t *testing.T) {
		w, tc := registered(t)
		w.Docker.AddRouterOnPort(true, 8080)
		if _, err := commands.StatusWithContext(context.Background(), tc.CommandContext, commands.StatusOptions{}); err != nil {
			t.Fatal(err)
		}
		noURLs(t, tc)
		if lines, _ := tc.Logger.Snapshot(); !slices.Equal(lines, []string{"info: The router runs on port 8080, not 80. Run 'treerig router up'."}) {
			t.Fatalf("stderr = %q", lines)
		}
	})

	t.Run("the JSON says which port it runs on", func(t *testing.T) {
		w, tc := registered(t)
		w.Docker.AddRouterOnPort(true, 8080)
		report, err := statusJSON(t, tc, "")
		if err != nil || report.Router == nil || *report.Router != (commands.StatusRouter{State: commands.RouterRunning, Port: 8080}) {
			t.Fatalf("router = %+v, %v", report.Router, err)
		}
		for _, service := range report.Services {
			if service.URL != "" {
				t.Fatalf("service %+v has a url", service)
			}
		}
	})

	t.Run("a router another treerig root started", func(t *testing.T) {
		w, tc := registered(t)
		w.Docker.SetLabel(w.Docker.AddRouter(true), router.RouterHomeLabel, "/home/other/.treerig")
		report, err := statusJSON(t, tc, "")
		if err != nil || report.Router == nil || report.Router.State != commands.RouterRunning || !strings.Contains(report.Router.Detail, "/home/other/.treerig") {
			t.Fatalf("router = %+v, %v", report.Router, err)
		}
		lines, _ := tc.Logger.Snapshot()
		if !slices.Contains(lines, "info: The router belongs to a different treerig root, so no URL is shown. Run 'treerig router down', then 'treerig up'.") {
			t.Fatalf("stderr = %q", lines)
		}
	})

	t.Run("a router whose container cannot be inspected is unknown, not up", func(t *testing.T) {
		w, tc := registered(t)
		id := w.Docker.AddRouter(true)
		w.Docker.FailCall("inspect "+id, "no such object", 1, -1)
		report, err := statusJSON(t, tc, "")
		if err != nil || report.Router == nil || report.Router.State != commands.RouterUnknown || !strings.Contains(report.Router.Detail, "could not be inspected") {
			t.Fatalf("router = %+v, %v", report.Router, err)
		}
		for _, service := range report.Services {
			if service.URL != "" {
				t.Fatalf("service %+v has a url", service)
			}
		}
	})
}

func TestStatusReadsComposeWithoutInterpolation(t *testing.T) {
	w := commandstest.New(t, "feature/login", acme)
	w.Register("", acme, "feature-login", brand.LabelNamespace, "api", "mail")
	tc := w.Context("", acme, nil)
	if _, err := commands.StatusWithContext(context.Background(), tc.CommandContext, commands.StatusOptions{}); err != nil {
		t.Fatal(err)
	}
	configs := 0
	for _, call := range w.Docker.Calls() {
		if strings.Contains(call, " config ") || strings.HasSuffix(call, " config") {
			configs++
			if !strings.Contains(call, "--no-interpolate") {
				t.Errorf("status read the Compose file with interpolation: %s", call)
			}
		}
	}
	if configs == 0 {
		t.Fatal("status never read the Compose file")
	}
}

func TestStatusOfANamedWorktreeWhoseDirectoryCannotBeTrusted(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*commandstest.World, *commandstest.TestContext, string) {
		t.Helper()
		w, _, _, theirRoot := namedWorld(t)
		toml := "[project]\nname = \"widgets\"\ndomain = \"widgets.localhost\"\n\n[compose]\nfile = \"compose.yaml\"\n\n[service.api]\n"
		if err := os.WriteFile(filepath.Join(theirRoot, brand.ConfigFile), []byte(toml), 0o600); err != nil {
			t.Fatal(err)
		}
		return w, w.ContextWithoutConfig(""), theirRoot
	}

	t.Run("a directory that now belongs to another repository is not read", func(t *testing.T) {
		w, _, theirs, _ := namedWorld(t)
		// The card of widgets' worktree says it lives where this repository's
		// worktree does.
		w.WriteIndexCard(theirs.RepositoryID, theirs.WorktreeID, "widgets", "widgets.localhost", "main", "main.widgets.localhost", w.Root, "main")
		tc := w.ContextWithoutConfig("")
		report, err := statusJSON(t, tc, "main.widgets.localhost")
		if err != nil || report.State == nil || *report.State != worktree.StateUnknown || !strings.Contains(report.Hint, "belongs to another repository") || report.Router != nil {
			t.Fatalf("report = %+v, %v", report, err)
		}
		for _, call := range w.Docker.Calls() {
			if strings.Contains(call, " config ") {
				t.Fatalf("it handed the other repository's Compose file to Docker: %s", call)
			}
		}
	})

	t.Run("a parent that is empty may be a volume that is not mounted: unknown, not missing", func(t *testing.T) {
		w, _, theirs, _ := namedWorld(t)
		mountPoint := filepath.Join(w.Base, "volume")
		if err := os.Mkdir(mountPoint, 0o700); err != nil {
			t.Fatal(err)
		}
		w.WriteIndexCard(theirs.RepositoryID, theirs.WorktreeID, "widgets", "widgets.localhost", "main", "main.widgets.localhost", filepath.Join(mountPoint, "widgets"), "main")
		tc := w.ContextWithoutConfig("")
		report, err := statusJSON(t, tc, "main.widgets.localhost")
		if err != nil || report.State == nil || *report.State != worktree.StateUnknown || !strings.Contains(report.Hint, "is the volume mounted?") {
			t.Fatalf("report = %+v, %v", report, err)
		}
	})

	t.Run("a Compose file that is too large is refused before Compose sees it", func(t *testing.T) {
		w, tc, theirRoot := setup(t)
		if err := os.WriteFile(filepath.Join(theirRoot, "compose.yaml"), []byte(strings.Repeat("# padding\n", 1<<17)), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.StatusOptions{Worktree: "main.widgets.localhost"})
		if err == nil || !strings.Contains(err.Error(), "compose.yaml is larger than") {
			t.Fatalf("err = %v", err)
		}
		for _, call := range w.Docker.Calls() {
			if strings.Contains(call, " config ") {
				t.Fatalf("Docker was handed it: %s", call)
			}
		}
	})
}

func TestStatusRetriesComposeWithInterpolationWhenItRefusesWithout(t *testing.T) {
	w := commandstest.New(t, "feature/login", acme)
	w.Register("", acme, "feature-login", brand.LabelNamespace, "api", "mail")
	tc := w.Context("", acme, nil)
	w.Docker.FailCall("--no-interpolate", "services.api.ports contains an invalid type", 1, 1)

	if _, err := commands.StatusWithContext(context.Background(), tc.CommandContext, commands.StatusOptions{}); err != nil {
		t.Fatal(err)
	}
	var configs []string
	for _, call := range w.Docker.Calls() {
		if strings.Contains(call, " config ") || strings.HasSuffix(call, " config") {
			configs = append(configs, call)
		}
	}
	if len(configs) != 2 || !strings.Contains(configs[0], "--no-interpolate") || strings.Contains(configs[1], "--no-interpolate") {
		t.Fatalf("config calls = %q, want one with --no-interpolate and one without", configs)
	}
	if _, printed := tc.Logger.Snapshot(); len(printed) < 3 || printed[0] != "feature-login.acme.localhost   running" {
		t.Fatalf("printed = %q", printed)
	}
}
