package backend

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/worktree"
)

const ns = brand.LabelNamespace

var ref = worktree.Ref{
	RepositoryID: "a08b4892-5111-49f1-a47a-5ab988c7f676",
	WorktreeID:   "2b41d9d4-ed88-49c1-be9c-5439bdf6a431",
	Slug:         "feature-login",
}

var project = layout.ComposeProjectName(ref.RepositoryID, ref.WorktreeID)

var composeModel = map[string]any{
	"name": project,
	"services": map[string]any{
		"api":     map[string]any{"image": "busybox", "expose": []any{"8080/tcp"}},
		"worker":  map[string]any{"image": "busybox", "ports": []any{map[string]any{"mode": "ingress", "target": 9090, "protocol": "tcp"}}},
		"quiet":   map[string]any{"image": "busybox", "networks": map[string]any{"backend": nil}},
		"builder": map[string]any{"build": map[string]any{"context": "/work/acme/docker/builder"}, "expose": []any{"7000"}},
	},
	"volumes": map[string]any{
		"data":   map[string]any{"name": project + "_data"},
		"shared": map[string]any{"name": "shared-external", "external": true},
	},
	"networks": map[string]any{
		"backend": map[string]any{"name": project + "_backend"},
		"corp":    map[string]any{"name": "corp", "external": true},
	},
}

// dockerFake answers every docker and compose invocation the runtime makes,
// with knobs for the failure modes; the port of the TypeScript DockerFake.
type dockerFake struct {
	mu          sync.Mutex
	calls       []run.Spec
	psLines     []map[string]any
	psOutput    *string
	resources   map[string][]string
	labels      map[string]map[string]string
	uninspected map[string]bool
}

func newFake() *dockerFake {
	return &dockerFake{
		resources:   map[string][]string{"ps": nil, "volume": nil, "network": nil, "image": nil},
		labels:      map[string]map[string]string{},
		uninspected: map[string]bool{},
	}
}

// own labels a resource as this worktree's; composeLabels false is an image
// from Compose's bake builder, which stamps only our build.labels.
func (f *dockerFake) own(id, service string, composeLabels bool) {
	labels := map[string]string{
		ns + ".managed":         "true",
		ns + ".repository-uuid": ref.RepositoryID,
		ns + ".worktree-uuid":   ref.WorktreeID,
	}
	if composeLabels {
		labels[ComposeProjectLabel] = project
		labels[ComposeServiceLabel] = service
	}
	f.labels[id] = labels
}

var quoted = regexp.MustCompile(`"([^"]+)"`)

func (f *dockerFake) Run(_ context.Context, spec run.Spec) (run.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, spec)
	f.mu.Unlock()
	ok := func(stdout string) (run.Result, error) { return run.Finish(spec, run.Result{Stdout: stdout}) }
	args := spec.Args
	if len(args) > 0 && args[0] == "compose" {
		for _, verb := range []string{"config", "up", "down", "ps"} {
			if slices.Contains(args, verb) {
				switch verb {
				case "config":
					encoded, _ := json.Marshal(composeModel)
					return ok(string(encoded))
				case "ps":
					if f.psOutput != nil {
						return ok(*f.psOutput)
					}
					var lines []string
					for _, line := range f.psLines {
						encoded, _ := json.Marshal(line)
						lines = append(lines, string(encoded))
					}
					return ok(strings.Join(lines, "\n"))
				}
				return ok("")
			}
		}
		return ok("")
	}
	if slices.Contains(args, "--filter") {
		kind := args[0]
		return ok(strings.Join(f.resources[kind], "\n"))
	}
	if index := slices.Index(args, "--format"); index >= 0 {
		id := args[len(args)-1]
		if f.uninspected[id] {
			return run.Finish(spec, run.Result{Stderr: "no such object", ExitCode: 1})
		}
		labels := f.labels[id]
		var values []string
		for _, match := range quoted.FindAllStringSubmatch(args[index+1], -1) {
			values = append(values, labels[match[1]])
		}
		return ok(strings.Join(values, "\t"))
	}
	return ok("")
}

func (f *dockerFake) Which(string) bool { return true }

func (f *dockerFake) composeCalls(verb string) []run.Spec {
	var matching []run.Spec
	for _, call := range f.calls {
		if len(call.Args) > 0 && call.Args[0] == "compose" && slices.Contains(call.Args, verb) {
			matching = append(matching, call)
		}
	}
	return matching
}

func (f *dockerFake) removals() bool {
	for _, call := range f.calls {
		if (len(call.Args) > 0 && call.Args[0] == "rm") || (len(call.Args) > 1 && call.Args[1] == "rm") {
			return true
		}
	}
	return false
}

func newRuntime(t *testing.T, fake *dockerFake) (*Compose, string) {
	t.Helper()
	controlHome := t.TempDir()
	return NewCompose(Options{
		ComposeFile:    "/work/acme/docker/compose.yaml",
		ProjectRoot:    "/work/acme",
		ConfigFile:     "/work/acme/berth.toml",
		ProjectName:    "acme",
		Domain:         "acme.localhost",
		ControlHome:    controlHome,
		LabelNamespace: ns,
		Runner:         fake,
		Logger:         logger.Silent{},
		Environ:        []string{"PATH=/bin", "COMPOSE_FILE=from-the-shell", "COMPOSE_PATH_SEPARATOR=:", "COMPOSE_ENV_FILES=x", "COMPOSE_IGNORE_ORPHANS=1"},
	}), controlHome
}

// ensureHome creates a worktree's home directory, as the command layer does
// before `up`; the runtime itself never creates it.
func ensureHome(t *testing.T, controlHome string) {
	t.Helper()
	if err := os.MkdirAll(layout.WorktreeLayout(controlHome, ref.RepositoryID, ref.WorktreeID).Home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
}

func environ(spec run.Spec) map[string]string {
	values := map[string]string{}
	for _, entry := range spec.Env {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = value
	}
	return values
}

func TestDownStatus(t *testing.T) {
	ctx := context.Background()
	t.Run("down acts on the project name alone — no model, no files, no override — and leaves named volumes to reap", func(t *testing.T) {
		fake := newFake()
		rt, _ := newRuntime(t, fake)
		if err := rt.Down(ctx, ref); err != nil {
			t.Fatal(err)
		}
		downs := fake.composeCalls("down")
		if len(downs) != 1 || !reflect.DeepEqual(downs[0].Args, []string{"compose", "--project-name", project, "down", "--remove-orphans"}) {
			t.Fatalf("down calls = %+v", downs)
		}
		if len(fake.composeCalls("config")) != 0 {
			t.Fatal("down read the model")
		}
	})
	t.Run("hands Compose the same BERTH_* vocabulary hooks receive, plus the project name", func(t *testing.T) {
		fake := newFake()
		rt, controlHome := newRuntime(t, fake)
		ensureHome(t, controlHome)
		if err := rt.Up(ctx, ref, nil); err != nil {
			t.Fatal(err)
		}
		got := environ(fake.composeCalls("up")[0])
		want := map[string]string{
			"BERTH_PROJECT":        "acme",
			"BERTH_REPOSITORY_ID":  ref.RepositoryID,
			"BERTH_WORKTREE_ID":    ref.WorktreeID,
			"BERTH_SLUG":           "feature-login",
			"BERTH_HOST":           "feature-login.acme.localhost",
			"BERTH_WORKTREE_HOME":  layout.WorktreeLayout(controlHome, ref.RepositoryID, ref.WorktreeID).Home,
			"BERTH_CONFIG":         "/work/acme/berth.toml",
			"BERTH_ROOT":           "/work/acme",
			"COMPOSE_PROJECT_NAME": project,
			"COMPOSE_PROGRESS":     "plain",
		}
		for name, value := range want {
			if got[name] != value {
				t.Errorf("%s = %q, want %q", name, got[name], value)
			}
		}
		if _, ok := got["BERTH_STAGE"]; ok {
			t.Error("BERTH_STAGE reached Compose")
		}
	})
	t.Run("keeps the shell from redirecting Compose to other files", func(t *testing.T) {
		fake := newFake()
		rt, controlHome := newRuntime(t, fake)
		ensureHome(t, controlHome)
		if err := rt.Up(ctx, ref, nil); err != nil {
			t.Fatal(err)
		}
		got := environ(fake.composeCalls("up")[0])
		for _, name := range []string{"COMPOSE_FILE", "COMPOSE_PATH_SEPARATOR", "COMPOSE_ENV_FILES", "COMPOSE_IGNORE_ORPHANS"} {
			if _, ok := got[name]; ok {
				t.Errorf("%s reached Compose", name)
			}
		}
	})
	t.Run("status maps Compose states and reports declared services that have no container as absent", func(t *testing.T) {
		fake := newFake()
		fake.psLines = []map[string]any{
			{"Service": "api", "State": "running", "Health": "healthy"},
			{"Service": "worker", "State": "running", "Health": "starting"},
			{"Service": "noise", "State": "exited", "Health": ""},
		}
		rt, _ := newRuntime(t, fake)
		got, err := rt.Status(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		want := []worktree.ServiceStatus{
			{Service: "api", State: worktree.Running},
			{Service: "builder", State: worktree.Absent},
			{Service: "quiet", State: worktree.Absent},
			{Service: "worker", State: worktree.Starting},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v", got)
		}
		if ps := fake.composeCalls("ps"); slices.Contains(ps[0].Args, "-f") {
			t.Fatal("ps was given files")
		}
		if config := fake.composeCalls("config"); len(config) != 1 || !slices.Contains(config[0].Args, "--project-directory") {
			t.Fatalf("config calls = %+v", config)
		}
	})
	t.Run("status accepts the single-array form older Compose releases print", func(t *testing.T) {
		fake := newFake()
		output := `[{"Service":"api","State":"running"},{"Service":"worker","State":"exited"}]`
		fake.psOutput = &output
		rt, _ := newRuntime(t, fake)
		got, err := rt.Status(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		states := map[string]worktree.ServiceState{}
		for _, status := range got {
			states[status.Service] = status.State
		}
		if states["api"] != worktree.Running || states["worker"] != worktree.Stopped {
			t.Fatalf("states = %v", states)
		}
	})
	t.Run("status treats an empty array or null — older Compose with no containers — as everything absent", func(t *testing.T) {
		for _, output := range []string{"[]", "[]\n", "null"} {
			fake := newFake()
			fake.psOutput = &output
			rt, _ := newRuntime(t, fake)
			got, err := rt.Status(ctx, ref)
			if err != nil {
				t.Fatal(err)
			}
			for _, status := range got {
				if status.State != worktree.Absent {
					t.Fatalf("%q: %+v", output, got)
				}
			}
		}
	})
	t.Run("status refuses to report anything absent when the ps output is unreadable, even partially", func(t *testing.T) {
		for _, output := range []string{
			"NAME   IMAGE   COMMAND\nberth-x-api-1   busybox   \"sleep\"",
			"{\"Service\":\"api\",\"State\":\"running\"}\n{\"Service\":\"worker\",\"State\":\"runn",
			`[{"Service":"api","State":"running"},{"nope":true}]`,
		} {
			fake := newFake()
			fake.psOutput = &output
			rt, _ := newRuntime(t, fake)
			_, err := rt.Status(ctx, ref)
			if err == nil || !strings.Contains(err.Error(), "Run `docker compose version` and report it") {
				t.Fatalf("%q: %v", output, err)
			}
		}
	})
	t.Run("status surfaces an unhealthy running container without inventing a state", func(t *testing.T) {
		fake := newFake()
		fake.psLines = []map[string]any{{"Service": "api", "State": "running", "Health": "unhealthy"}, {"Service": "worker", "State": "exited"}}
		rt, _ := newRuntime(t, fake)
		got, err := rt.Status(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		if got[0] != (worktree.ServiceStatus{Service: "api", State: worktree.Running, Detail: "unhealthy"}) ||
			got[3] != (worktree.ServiceStatus{Service: "worker", State: worktree.Stopped}) {
			t.Fatalf("got %+v", got)
		}
	})
}

func populated() *dockerFake {
	fake := newFake()
	fake.resources = map[string][]string{"ps": {"c1", "c2"}, "volume": {"v1"}, "network": {"n1"}, "image": {"i1"}}
	fake.own("c1", "api", true)
	fake.own("c2", "worker", true)
	fake.own("v1", "", true)
	fake.own("n1", "", true)
	fake.own("i1", "", true)
	return fake
}

func TestInventoryReap(t *testing.T) {
	ctx := context.Background()
	t.Run("returns every owned resource in removal order, each verified with a single inspect", func(t *testing.T) {
		fake := populated()
		rt, _ := newRuntime(t, fake)
		owned, err := rt.Inventory(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		want := []worktree.OwnedResource{
			{Kind: "container", ID: "c1", Service: "api"},
			{Kind: "container", ID: "c2", Service: "worker"},
			{Kind: "volume", ID: "v1"},
			{Kind: "network", ID: "n1"},
			{Kind: "image", ID: "i1"},
		}
		if !reflect.DeepEqual(owned, want) {
			t.Fatalf("owned = %+v", owned)
		}
		inspects := 0
		for _, call := range fake.calls {
			if slices.Contains(call.Args, "--format") {
				inspects++
			}
		}
		if inspects != 5 || fake.removals() {
			t.Fatalf("inspects = %d, removals = %v", inspects, fake.removals())
		}
	})
	t.Run("lists images by ownership only, never by project label", func(t *testing.T) {
		fake := populated()
		rt, _ := newRuntime(t, fake)
		if _, err := rt.Inventory(ctx, ref); err != nil {
			t.Fatal(err)
		}
		var lists [][]string
		for _, call := range fake.calls {
			if len(call.Args) > 1 && call.Args[0] == "image" && call.Args[1] == "ls" {
				lists = append(lists, call.Args)
			}
		}
		filters := 0
		if len(lists) == 1 {
			for _, arg := range lists[0] {
				if arg == "--filter" {
					filters++
				}
			}
		}
		if len(lists) != 1 || filters != 3 {
			t.Fatalf("image lists = %v", lists)
		}
	})
	t.Run("refuses the whole inventory when one resource belongs to another worktree, removing nothing", func(t *testing.T) {
		fake := populated()
		fake.labels["v1"][ns+".worktree-uuid"] = "ffffffff-ffff-4fff-8fff-ffffffffffff"
		rt, _ := newRuntime(t, fake)
		_, err := rt.Inventory(ctx, ref)
		if err == nil || err.Error() != "Refusing to touch volume v1: its ownership labels do not match this worktree." || fake.removals() {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("refuses a resource whose labels cannot be inspected", func(t *testing.T) {
		fake := populated()
		fake.uninspected["n1"] = true
		rt, _ := newRuntime(t, fake)
		_, err := rt.Inventory(ctx, ref)
		if err == nil || err.Error() != "Refusing to touch network n1: its labels could not be inspected." {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("finds a resource with the right owner but the wrong project label, and then refuses it", func(t *testing.T) {
		fake := populated()
		fake.resources["ps"] = []string{"c1", "stray"}
		fake.own("stray", "api", true)
		fake.labels["stray"][ComposeProjectLabel] = "somebody-else"
		rt, _ := newRuntime(t, fake)
		_, err := rt.Inventory(ctx, ref)
		if err == nil || !strings.Contains(err.Error(), "Refusing to touch container stray") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("accepts and reaps an image the bake builder left without Compose's own labels", func(t *testing.T) {
		fake := populated()
		fake.own("i1", "", false)
		rt, _ := newRuntime(t, fake)
		owned, err := rt.Inventory(ctx, ref)
		if err != nil || !slices.Contains(owned, worktree.OwnedResource{Kind: "image", ID: "i1"}) {
			t.Fatalf("owned = %+v, err = %v", owned, err)
		}
		if err := rt.Reap(ctx, ref, worktree.OwnedResource{Kind: "image", ID: "i1"}); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, call := range fake.calls {
			found = found || strings.Join(call.Args, " ") == "image rm i1"
		}
		if !found {
			t.Fatal("image i1 was not removed")
		}
	})
	t.Run("still refuses an image whose Compose project label names another project", func(t *testing.T) {
		fake := populated()
		fake.labels["i1"][ComposeProjectLabel] = "somebody-else"
		rt, _ := newRuntime(t, fake)
		_, err := rt.Inventory(ctx, ref)
		if err == nil || err.Error() != "Refusing to touch image i1: its ownership labels do not match this worktree." {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("refuses a volume without a Compose project label: only images may lack one", func(t *testing.T) {
		fake := populated()
		fake.own("v1", "", false)
		rt, _ := newRuntime(t, fake)
		_, err := rt.Inventory(ctx, ref)
		if err == nil || err.Error() != "Refusing to touch volume v1: its ownership labels do not match this worktree." {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("reap re-verifies and then removes with the right verb for each kind", func(t *testing.T) {
		fake := populated()
		rt, _ := newRuntime(t, fake)
		for _, resource := range []worktree.OwnedResource{{Kind: "container", ID: "c1"}, {Kind: "volume", ID: "v1"}, {Kind: "network", ID: "n1"}, {Kind: "image", ID: "i1"}} {
			if err := rt.Reap(ctx, ref, resource); err != nil {
				t.Fatal(err)
			}
		}
		var removals []string
		for _, call := range fake.calls {
			if !slices.Contains(call.Args, "--format") && !slices.Contains(call.Args, "--filter") && call.Args[0] != "compose" {
				removals = append(removals, strings.Join(call.Args, " "))
			}
		}
		want := []string{"rm --force c1", "volume rm v1", "network rm n1", "image rm i1"}
		if !reflect.DeepEqual(removals, want) {
			t.Fatalf("removals = %v", removals)
		}
	})
	t.Run("reap refuses a resource that no longer verifies, even if inventory once returned it", func(t *testing.T) {
		fake := populated()
		rt, _ := newRuntime(t, fake)
		fake.labels["c1"][ns+".managed"] = "false"
		err := rt.Reap(ctx, ref, worktree.OwnedResource{Kind: "container", ID: "c1"})
		if err == nil || !strings.Contains(err.Error(), "ownership labels do not match") || fake.removals() {
			t.Fatalf("err = %v", err)
		}
	})
}

// isRemovalCall matches Reap's own removal args ("rm --force <id>",
// "volume rm <id>", "network rm <id>", "image rm <id>") the same way
// TestInventoryReap's own "removals" subtest reads f.calls: everything that
// is not a compose call, an inspect (--format) or a list (--filter).
func isRemovalCall(call run.Spec) bool {
	return len(call.Args) > 0 && call.Args[0] != "compose" &&
		!slices.Contains(call.Args, "--format") && !slices.Contains(call.Args, "--filter")
}

// TestComposeTimeouts is #14: every bounded call — compose config, compose
// ps, and Inventory's own ps/ls/inspect — carries Timeouts.Probe (30s by
// default, matching row 12's router precedent), Reap's removal carries
// Timeouts.Remove (a minute by default), and `up`/`down` carry neither,
// deliberately, since both stay long-running by design (see compose.go's
// own timeouts comment).
func TestComposeTimeouts(t *testing.T) {
	ctx := context.Background()
	t.Run("bounds compose config, compose ps and Inventory's own probes to Timeouts.Probe, and Reap's removal to Timeouts.Remove", func(t *testing.T) {
		fake := populated()
		controlHome := t.TempDir()
		rt := NewCompose(Options{
			ComposeFile: "/work/acme/docker/compose.yaml", ProjectRoot: "/work/acme",
			ConfigFile: "/work/acme/berth.toml", ProjectName: "acme", Domain: "acme.localhost",
			ControlHome: controlHome, LabelNamespace: ns, Runner: fake, Logger: logger.Silent{},
			Timeouts: Timeouts{Probe: 7 * time.Second, Remove: 11 * time.Second},
		})
		if _, err := rt.Status(ctx, ref); err != nil {
			t.Fatal(err)
		}
		owned, err := rt.Inventory(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		if err := rt.Reap(ctx, ref, owned[0]); err != nil {
			t.Fatal(err)
		}

		var sawRemoval bool
		for _, call := range fake.calls {
			if isRemovalCall(call) {
				sawRemoval = true
				if call.Timeout != 11*time.Second {
					t.Errorf("removal %v: Timeout = %s, want 11s", call.Args, call.Timeout)
				}
				continue
			}
			if call.Timeout != 7*time.Second {
				t.Errorf("call %v: Timeout = %s, want 7s", call.Args, call.Timeout)
			}
		}
		if !sawRemoval {
			t.Fatal("Reap made no removal call")
		}
	})

	t.Run("defaults to 30s for probes and a minute for removal when Options.Timeouts is unset", func(t *testing.T) {
		fake := populated()
		rt, _ := newRuntime(t, fake)
		if _, err := rt.Status(ctx, ref); err != nil {
			t.Fatal(err)
		}
		owned, err := rt.Inventory(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		if err := rt.Reap(ctx, ref, owned[0]); err != nil {
			t.Fatal(err)
		}
		for _, call := range fake.calls {
			want := dockerProbeTimeout
			if isRemovalCall(call) {
				want = removeTimeout
			}
			if call.Timeout != want {
				t.Errorf("call %v: Timeout = %s, want %s", call.Args, call.Timeout, want)
			}
		}
	})

	t.Run("up and down carry no timeout of their own — long-running by design", func(t *testing.T) {
		fake := newFake()
		rt, controlHome := newRuntime(t, fake)
		ensureHome(t, controlHome)
		if err := rt.Up(ctx, ref, nil); err != nil {
			t.Fatal(err)
		}
		if err := rt.Down(ctx, ref); err != nil {
			t.Fatal(err)
		}
		for _, verb := range []string{"up", "down"} {
			for _, call := range fake.composeCalls(verb) {
				if call.Timeout != 0 {
					t.Errorf("%s: Timeout = %s, want 0 (unbounded)", verb, call.Timeout)
				}
			}
		}
	})
}
