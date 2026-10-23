package worktreestate_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/run/fakerun"
	"github.com/sofired/treerig/internal/worktree"
	"github.com/sofired/treerig/internal/worktreestate"
)

const (
	repoA = "11111111-1111-4111-8111-111111111111"
	wtA   = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	wtB   = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

// line is one `docker ps --format` row.
func line(repo, wt, service, state, status string) string {
	return strings.Join([]string{state, status, repo, wt, service, "", "id-" + service}, "\t")
}

func TestParseInventory(t *testing.T) {
	parse := func(t *testing.T, lines ...string) []worktreestate.Container {
		t.Helper()
		containers, err := worktreestate.ParseInventory(strings.Join(lines, "\n"))
		if err != nil {
			t.Fatalf("ParseInventory: %v", err)
		}
		return containers
	}

	t.Run("reads Docker's wordings into service states", func(t *testing.T) {
		for _, c := range []struct {
			name, state, status string
			want                worktree.ServiceStatus
		}{
			{"a running container", "running", "Up 3 minutes", worktree.ServiceStatus{State: worktree.Running}},
			{"a healthy one", "running", "Up 3 minutes (healthy)", worktree.ServiceStatus{State: worktree.Running}},
			{"one still passing its first health check", "running", "Up 3 seconds (health: starting)", worktree.ServiceStatus{State: worktree.Starting}},
			{"one failing its health check", "running", "Up 5 seconds (unhealthy)", worktree.ServiceStatus{State: worktree.Unhealthy}},
			{"the health wording in any case", "running", "Up 5 seconds (Unhealthy)", worktree.ServiceStatus{State: worktree.Unhealthy}},
			{"a container that was created and never started", "created", "Created", worktree.ServiceStatus{State: worktree.Stopped}},
			{"a restarting one", "restarting", "Restarting (1) 5 seconds ago", worktree.ServiceStatus{State: worktree.Restarting}},
			{"one that exited cleanly", "exited", "Exited (0) 2 minutes ago", worktree.ServiceStatus{State: worktree.Exited}},
			{"one that exited with an error", "exited", "Exited (3) About an hour ago", worktree.ServiceStatus{State: worktree.Exited, ExitCode: 3}},
			{"one SIGKILL ended, before Inventory asks Docker why", "exited", "Exited (137) 3 days ago", worktree.ServiceStatus{State: worktree.Unknown, Detail: worktree.CauseNotRead}},
			{"one SIGTERM ended", "exited", "Exited (143) 3 days ago", worktree.ServiceStatus{State: worktree.Stopped}},
			{"one that exited with 139", "exited", "Exited (139) 3 days ago", worktree.ServiceStatus{State: worktree.Exited, ExitCode: 139}},
			{"a paused one", "paused", "Up 5 minutes (Paused)", worktree.ServiceStatus{State: worktree.Stopped}},
			{"a dead one", "dead", "Dead", worktree.ServiceStatus{State: worktree.Stopped}},
			{"one being removed", "removing", "Removal In Progress", worktree.ServiceStatus{State: worktree.Stopped}},
			{"an exit with no code to read", "exited", "Exited", worktree.ServiceStatus{State: worktree.Unknown, Detail: "exited"}},
			{"a state Docker adds later", "frozen", "Frozen", worktree.ServiceStatus{State: worktree.Unknown, Detail: "frozen"}},
		} {
			t.Run(c.name, func(t *testing.T) {
				got := parse(t, line(repoA, wtA, "api", c.state, c.status))[0]
				if got.Status().State != c.want.State || got.ExitCode != c.want.ExitCode || got.Detail != c.want.Detail {
					t.Fatalf("got %+v, want %+v", got, c.want)
				}
			})
		}
	})

	t.Run("marks a container available only when running and not unhealthy or still starting", func(t *testing.T) {
		parsed := parse(t,
			line(repoA, wtA, "api", "running", "Up 3 seconds"),
			line(repoA, wtA, "mail", "running", "Up 3 seconds (health: starting)"),
			line(repoA, wtB, "api", "exited", "Exited (0) 2 minutes ago"),
			line(repoA, wtB, "mail", "running", "Up 5 seconds (unhealthy)"),
			line(repoA, wtB, "db", "running", "Up 5 seconds"),
		)
		got := make([][2]any, len(parsed))
		for i, c := range parsed {
			got[i] = [2]any{c.Service, c.Available()}
		}
		want := [][2]any{{"api", true}, {"mail", false}, {"api", false}, {"mail", false}, {"db", true}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("reads a shared container, which has no worktree label", func(t *testing.T) {
		parsed := parse(t, strings.Join([]string{"running", "Up 1 minute", repoA, "", "mailpit", "true", "m1"}, "\t"))
		if len(parsed) != 1 || !parsed[0].Shared || parsed[0].WorktreeID != "" || parsed[0].Service != "mailpit" {
			t.Fatalf("parsed = %+v", parsed)
		}
	})

	t.Run("rejects a line that does not have exactly six fields", func(t *testing.T) {
		for _, bad := range []string{
			"running\tUp\tonly-three",
			// The right number of fields, without the ID.
			strings.Join([]string{"running", "Up", repoA, wtA, "api", "", ""}, "\t"),
			// A tab inside a status would shift every field right, and the row
			// would read as another state rather than being refused.
			"running\tUp 2 seconds\t(healthy)\tr\tw\tapi\t",
			// A worktree container without a worktree, and a shared one that names one.
			strings.Join([]string{"running", "Up", repoA, "", "api", "", "x"}, "\t"),
			strings.Join([]string{"running", "Up", repoA, wtA, "api", "true", "x"}, "\t"),
		} {
			_, err := worktreestate.ParseInventory(bad)
			if err == nil || !strings.Contains(err.Error(), "unrecognized container inventory line") {
				t.Errorf("%q: error = %v", bad, err)
			}
		}
	})

	// Pinned because Inventory.Containers relies on the nil-vs-empty-slice
	// distinction: nil means Docker could not be asked at all.
	t.Run("never returns a nil slice on success, even for empty output", func(t *testing.T) {
		containers, err := worktreestate.ParseInventory("")
		if err != nil || containers == nil || len(containers) != 0 {
			t.Fatalf("ParseInventory(\"\") = %#v, %v", containers, err)
		}
	})
}

// dockerFake answers `docker ps` per namespace.
type dockerFake struct {
	mu       sync.Mutex
	calls    []run.Spec
	byFilter map[string]string
	exit     int
	stderr   string
	timeout  bool
	start    error
	// inspect are `container inspect` documents by container ID; an ID not
	// here is a container that cannot be inspected.
	inspect map[string]string
}

// handler records the call and answers it for the namespace it filters on.
func (d *dockerFake) handler(_ context.Context, spec run.Spec) (run.Result, error) {
	d.mu.Lock()
	d.calls = append(d.calls, spec)
	d.mu.Unlock()
	if d.start != nil {
		return run.Result{}, d.start
	}
	if len(spec.Args) >= 3 && spec.Args[0] == "container" && spec.Args[1] == "inspect" {
		doc, found := d.inspect[spec.Args[2]]
		if !found {
			return run.Result{ExitCode: 1, Stderr: "Error: No such container: " + spec.Args[2]}, nil
		}
		return run.Result{Stdout: "[" + doc + "]"}, nil
	}
	return run.Result{Stdout: d.byFilter[spec.Args[3]], ExitCode: d.exit, Stderr: d.stderr, TimedOut: d.timeout}, nil
}

// runner is a fakerun.Runner answering with d.
func (d *dockerFake) runner() run.Runner { return &fakerun.Runner{Handler: d.handler} }

func TestInventory(t *testing.T) {
	ctx := context.Background()
	namespaces := []string{"dev.treerig", "dev.other"}

	t.Run("asks Docker once per namespace and merges the answers in namespace order", func(t *testing.T) {
		d := &dockerFake{byFilter: map[string]string{
			"label=dev.treerig.managed=true": line(repoA, wtA, "api", "running", "Up 1 minute"),
			"label=dev.other.managed=true":   line(repoA, wtB, "web", "exited", "Exited (2) 1 minute ago"),
		}}
		got, err := worktreestate.New(worktreestate.Options{Runner: d.runner()}).Inventory(ctx, namespaces)
		if err != nil || !got.Answered || got.Error != "" {
			t.Fatalf("Inventory = %+v, %v", got, err)
		}
		if len(got.Containers) != 2 || got.Containers[0].Service != "api" || got.Containers[1].ExitCode != 2 {
			t.Fatalf("containers = %+v", got.Containers)
		}
		if len(d.calls) != 2 {
			t.Fatalf("docker was asked %d times, want one per namespace", len(d.calls))
		}
		for _, call := range d.calls {
			if call.Args[0] != "ps" || call.Args[1] != "--all" {
				t.Fatalf("call = %v, want docker ps --all", call.Args)
			}
		}
	})

	t.Run("asks Docker only about the containers that exited 137, and reads the answer", func(t *testing.T) {
		document := func(oom bool) string {
			return fmt.Sprintf(`{"Id":"x","Name":"/x","State":{"Status":"exited","ExitCode":137,"OOMKilled":%v},"Config":{"Labels":{}}}`, oom)
		}
		d := &dockerFake{
			byFilter: map[string]string{"label=dev.treerig.managed=true": strings.Join([]string{
				line(repoA, wtA, "stopped", "exited", "Exited (137) 1 hour ago"),
				line(repoA, wtA, "oom", "exited", "Exited (137) 1 hour ago"),
				line(repoA, wtA, "unread", "exited", "Exited (137) 1 hour ago"),
				line(repoA, wtA, "terminated", "exited", "Exited (143) 1 hour ago"),
				line(repoA, wtA, "crashed", "exited", "Exited (7) 1 hour ago"),
				line(repoA, wtA, "web", "running", "Up 1 hour"),
			}, "\n")},
			inspect: map[string]string{"id-stopped": document(false), "id-oom": document(true)},
		}
		got, err := worktreestate.New(worktreestate.Options{Runner: d.runner()}).Inventory(ctx, namespaces[:1])
		if err != nil || !got.Answered {
			t.Fatalf("Inventory = %+v, %v", got, err)
		}
		texts := map[string]string{}
		for _, container := range got.Containers {
			texts[container.Service] = container.Status().Text() + "|" + container.Detail
		}
		want := map[string]string{
			"stopped": "stopped|", "oom": "exited (code 137)|", "unread": "unknown|exited 137, cause not read",
			"terminated": "stopped|", "crashed": "exited (code 7)|", "web": "running|",
		}
		if !reflect.DeepEqual(texts, want) {
			t.Fatalf("got %v, want %v", texts, want)
		}
		inspects := 0
		for _, call := range d.calls {
			if len(call.Args) > 1 && call.Args[1] == "inspect" {
				inspects++
			}
		}
		if inspects != 3 {
			t.Fatalf("docker inspect ran %d times, want once for each of the three 137 exits", inspects)
		}
	})

	t.Run("answers with an empty, non-nil inventory when Docker names no containers", func(t *testing.T) {
		got, err := worktreestate.New(worktreestate.Options{Runner: (&dockerFake{}).runner()}).Inventory(ctx, namespaces)
		if err != nil || !got.Answered || got.Containers == nil || len(got.Containers) != 0 {
			t.Fatalf("Inventory = %#v, %v", got, err)
		}
	})

	t.Run("says why it could not answer, as a reason and not an error", func(t *testing.T) {
		for _, c := range []struct {
			name string
			d    *dockerFake
			want string
		}{
			{"a failing docker ps", &dockerFake{exit: 1, stderr: "boom\n"}, "boom"},
			{"a docker ps that says nothing", &dockerFake{exit: 3}, "`docker ps` exited 3 without saying why"},
			{"a timeout", &dockerFake{timeout: true}, "`docker ps` didn't answer within 30s"},
			{"a docker that will not start", &dockerFake{start: &run.StartError{ExitCode: 127, Detail: "ENOENT"}}, "Docker is not available"},
			{"a line it cannot read", &dockerFake{byFilter: map[string]string{
				"label=dev.treerig.managed=true": "running\tUp\tonly-three",
				"label=dev.other.managed=true":   "running\tUp\tonly-three",
			}}, "unrecognized container inventory line"},
		} {
			t.Run(c.name, func(t *testing.T) {
				got, err := worktreestate.New(worktreestate.Options{Runner: c.d.runner()}).Inventory(ctx, namespaces)
				if err != nil || got.Answered || got.Containers != nil || !strings.Contains(got.Error, c.want) {
					t.Fatalf("Inventory = %+v, %v; want a reason containing %q", got, err, c.want)
				}
			})
		}
	})

	t.Run("lets a cancelled run through instead of calling it Docker not answering", func(t *testing.T) {
		d := &dockerFake{start: context.Canceled}
		_, err := worktreestate.New(worktreestate.Options{Runner: d.runner()}).Inventory(ctx, namespaces)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the cancellation", err)
		}
	})

	t.Run("bounds each docker ps", func(t *testing.T) {
		d := &dockerFake{}
		if _, err := worktreestate.New(worktreestate.Options{Runner: d.runner(), InventoryTimeout: 7 * time.Second}).Inventory(ctx, namespaces[:1]); err != nil {
			t.Fatal(err)
		}
		if d.calls[0].Timeout != 7*time.Second {
			t.Fatalf("timeout = %v", d.calls[0].Timeout)
		}
	})
}
