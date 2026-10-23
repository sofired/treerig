package commands_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/commands/commandstest"
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/procid"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/shared"
	"github.com/sofired/treerig/internal/worktree"
)

// twoServices declares a second shared service, cache, beside mailpit, each
// with a named volume of its own and cache with an anonymous one besides, both
// used by api, and returns the context.
func twoServices(w *sharedWorld, root string) *commandstest.TestContext {
	w.Docker.SetSharedService("cache", map[string]any{
		"image": "busybox",
		"volumes": []any{
			map[string]any{"type": "volume", "source": "cachevol", "target": "/cache"},
			map[string]any{"type": "volume", "target": "/scratch"},
		},
	})
	tc := w.context(root)
	tc.Loaded.Config.Shared.Service = append(tc.Loaded.Config.Shared.Service, configShared("cache", 6379, "api"))
	return tc
}

// volumesOf are the names of the labelled shared volumes and the anonymous
// ones, by listing every volume in the daemon.
func (w *sharedWorld) volumesOf() []string {
	return w.Docker.Find("volume", map[string]string{})
}

// confirming is an Env whose terminal answers phrase, writing the question to
// the buffer it returns; before, when set, runs as the answer is read.
func confirming(phrase string, before func()) (*commands.Env, *bytes.Buffer) {
	question := &bytes.Buffer{}
	return &commands.Env{
		CommandName: "treerig", StdinIsTTY: true, Stderr: question,
		Stdin: &answerReader{answer: phrase + "\n", before: before},
	}, question
}

// answerReader hands out one answer, running before first.
type answerReader struct {
	answer string
	before func()
	read   bool
}

// Read runs the hook once and then returns the answer.
func (a *answerReader) Read(p []byte) (int, error) {
	if a.read {
		return 0, io.EOF
	}
	a.read = true
	if a.before != nil {
		a.before()
	}
	return copy(p, a.answer), nil
}

func TestSharedDownRefusesNamesThatAreNotSharedServices(t *testing.T) {
	setup := func(t *testing.T) (*sharedWorld, *commandstest.TestContext, string) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		return w, tc, result.Registration.RepositoryID
	}

	t.Run("a path in place of a name touches nothing outside the shared directory", func(t *testing.T) {
		w, tc, repository := setup(t)
		dir := layout.SharedLayout(w.ControlHome, repository)
		// What "../x" would reach if it were joined onto the records directory.
		outside := filepath.Join(dir.RepositoryDir, "x.json")
		if err := os.WriteFile(outside, []byte("precious"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"../x", "../../" + filepath.Base(w.ControlHome), "a/b", "x.json", ".", ""} {
			err := w.sharedDown(tc, commands.SharedDownOptions{Names: []string{name}, Volumes: true, Yes: true})
			if err == nil || !strings.Contains(err.Error(), "is not a shared service name") {
				t.Errorf("%q: err = %v", name, err)
			}
		}
		if data, err := os.ReadFile(outside); err != nil || string(data) != "precious" {
			t.Fatalf("the file outside was touched: %q, %v", data, err)
		}
		if len(w.sharedResources()["container"]) != 1 {
			t.Fatal("something was removed")
		}
	})

	t.Run("the registry refuses a bad name on every path to a record", func(t *testing.T) {
		w, tc, repository := setup(t)
		store := tc.Registry.Store(repository)
		if _, err := store.FindShared("../x"); err == nil {
			t.Error("FindShared accepted ../x")
		}
		if err := store.RemoveSharedRecord(context.Background(), registry.Writer{UserHome: w.UserHome}, "../x"); err == nil {
			t.Error("RemoveSharedRecord accepted ../x")
		}
		if _, err := store.SyncSharedPorts(context.Background(), registry.Writer{UserHome: w.UserHome}, registry.SharedPortPlan{Name: "../x", Ports: []int{1}, Range: tc.CommandContext.PortRange}); err == nil {
			t.Error("SyncSharedPorts accepted ../x")
		}
	})

	t.Run("a typo is refused, naming what is running and declared, instead of succeeding silently", func(t *testing.T) {
		w, tc, _ := setup(t)
		err := w.sharedDown(tc, commands.SharedDownOptions{Names: []string{"mailpt"}})
		if err == nil || !strings.Contains(err.Error(), "No shared service called mailpt. Running: mailpit. Declared: mailpit.") {
			t.Fatalf("err = %v", err)
		}
		if len(w.sharedResources()["container"]) != 1 {
			t.Fatal("something was removed")
		}
		if err := w.sharedDown(tc, commands.SharedDownOptions{Names: []string{"mailpt"}, Volumes: true, Yes: true}); err == nil {
			t.Fatal("--volumes with a typo succeeded")
		}
	})

	t.Run("a declared service that is not running is known: there is simply nothing to stop", func(t *testing.T) {
		w, tc, _ := setup(t)
		if err := w.sharedDown(tc, commands.SharedDownOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := w.sharedDown(tc, commands.SharedDownOptions{Names: []string{"mailpit"}}); err != nil {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a service that only has a record is known, and its ports are released with --volumes", func(t *testing.T) {
		w, tc, repository := setup(t)
		tc.Loaded.Config.Shared.Service = nil
		if err := w.sharedDown(tc, commands.SharedDownOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := w.sharedDown(tc, commands.SharedDownOptions{Names: []string{"mailpit"}, Volumes: true, Yes: true}); err != nil {
			t.Fatal(err)
		}
		if record, _ := tc.Registry.Store(repository).FindShared("mailpit"); record != nil {
			t.Fatalf("record = %+v", record)
		}
	})
}

func TestSharedDownVolumesScope(t *testing.T) {
	// Both services run; the cache container is then removed on its own, so
	// its volumes are unmounted and belong to no container at all.
	setup := func(t *testing.T) (*sharedWorld, *commandstest.TestContext, string) {
		w := newSharedWorld(t)
		tc := twoServices(w, "")
		result := w.mustUp(t, tc)
		return w, tc, result.Registration.RepositoryID
	}
	named := func(w *sharedWorld, kind, suffix string) string {
		for _, id := range w.Docker.Find(kind, map[string]string{}) {
			if strings.HasSuffix(id, suffix) {
				return id
			}
		}
		return ""
	}

	t.Run("a named service loses only the volumes its container mounts, named and anonymous", func(t *testing.T) {
		w, tc, repository := setup(t)
		stack := sharedStackFor(tc, repository)
		cache := w.Docker.ContainerNamed(w.project(t, tc) + "-cache-1")
		// The sibling is gone from Docker's containers, its volumes left unmounted.
		if err := stack.Reap(context.Background(), commands.SharedResourceForTest("container", cache)); err != nil {
			t.Fatal(err)
		}
		cachevol := named(w, "volume", "_cachevol")
		mailbox := named(w, "volume", "_mailbox")
		if cachevol == "" || mailbox == "" {
			t.Fatalf("volumes = %v", w.volumesOf())
		}

		if err := w.sharedDown(tc, commands.SharedDownOptions{Names: []string{"mailpit"}, Volumes: true, Yes: true}); err != nil {
			t.Fatal(err)
		}
		if w.Docker.Exists("volume", mailbox) {
			t.Error("mailpit's own volume survived")
		}
		if !w.Docker.Exists("volume", cachevol) {
			t.Error("a sibling's unmounted volume was removed by a named down")
		}
		for _, id := range w.volumesOf() {
			if strings.HasPrefix(id, "anon") && !w.Docker.Exists("volume", id) {
				t.Errorf("anonymous volume %s vanished", id)
			}
		}
		// Only mailpit's ports are released.
		store := tc.Registry.Store(repository)
		if record, _ := store.FindShared("mailpit"); record != nil {
			t.Error("mailpit's pinned ports were kept")
		}
		if record, _ := store.FindShared("cache"); record == nil {
			t.Error("the sibling's pinned ports were released")
		}
	})

	t.Run("without names every volume of the stack goes, mounted or not, and the anonymous ones its containers mounted", func(t *testing.T) {
		w, tc, repository := setup(t)
		stack := sharedStackFor(tc, repository)
		cache := w.Docker.ContainerNamed(w.project(t, tc) + "-cache-1")
		if err := stack.Reap(context.Background(), commands.SharedResourceForTest("container", cache)); err != nil {
			t.Fatal(err)
		}
		// An anonymous volume of the sibling's, now unmounted, is no longer provably ours.
		before := w.volumesOf()
		if err := w.sharedDown(tc, commands.SharedDownOptions{Volumes: true, Yes: true}); err != nil {
			t.Fatal(err)
		}
		left := w.volumesOf()
		for _, id := range left {
			if strings.Contains(id, "mailbox") || strings.Contains(id, "cachevol") {
				t.Errorf("volume %s survived", id)
			}
		}
		if len(before) == 0 {
			t.Fatal("no volumes to begin with")
		}
		// The mailpit container mounted no anonymous volume, and the cache's is unmounted: it stays.
		for _, id := range left {
			if !strings.HasPrefix(id, "anon") {
				t.Errorf("a non-anonymous volume survived: %s", id)
			}
		}
	})

	t.Run("an anonymous volume of a container that goes is removed, and the prompt lists it", func(t *testing.T) {
		w, tc, _ := setup(t)
		anonymous := ""
		for _, id := range w.volumesOf() {
			if strings.HasPrefix(id, "anon") {
				anonymous = id
			}
		}
		if anonymous == "" {
			t.Fatal("the cache has no anonymous volume")
		}
		env, question := confirming("remove acme shared services", nil)
		if err := commands.SharedDownWithContext(context.Background(), tc.CommandContext, env, commands.SharedDownOptions{Volumes: true}); err != nil {
			t.Fatalf("shared down --volumes: %v", err)
		}
		if w.Docker.Exists("volume", anonymous) {
			t.Error("the anonymous volume survived")
		}
		for _, want := range []string{"containers:", "networks:", "volumes:", "_mailbox", "_cachevol", anonymous + " (anonymous)", "pinned loopback ports (", "Type this exact phrase"} {
			if !strings.Contains(question.String(), want) {
				t.Errorf("the prompt lacks %q:\n%s", want, question)
			}
		}
		// Names, not counts.
		if strings.Contains(question.String(), "3 volume") {
			t.Errorf("the prompt counts volumes:\n%s", question)
		}
	})

	t.Run("a plain down never touches a volume, anonymous included", func(t *testing.T) {
		w, tc, _ := setup(t)
		before := w.volumesOf()
		if err := w.sharedDown(tc, commands.SharedDownOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := w.volumesOf(); !slices.Equal(got, before) {
			t.Fatalf("volumes = %v, were %v", got, before)
		}
	})
}

func TestSharedDownRefusesWhenThePlanGrewWhileYouDecided(t *testing.T) {
	w := newSharedWorld(t)
	tc := w.context("")
	result := w.mustUp(t, tc)
	var extra string
	env, question := confirming("remove acme shared services", func() {
		// Another shared volume appears while the prompt is up.
		labels := map[string]string{
			brand.ManagedLabel(brand.LabelNamespace): "true", brand.RepositoryLabel(brand.LabelNamespace): result.Registration.RepositoryID,
			brand.SharedLabel(brand.LabelNamespace): "true", "com.docker.compose.project": w.project(t, tc),
			brand.FormatLabel(brand.LabelNamespace): "1",
		}
		extra = w.Docker.Add("volume", labels)
	})
	before := len(w.sharedResources()["container"])
	err := commands.SharedDownWithContext(context.Background(), tc.CommandContext, env, commands.SharedDownOptions{Volumes: true})
	if err == nil || !strings.Contains(err.Error(), "What would be removed changed while you were deciding") || !strings.Contains(err.Error(), extra) {
		t.Fatalf("err = %v\n%s", err, question)
	}
	if len(w.sharedResources()["container"]) != before || !w.Docker.Exists("volume", extra) {
		t.Fatal("something was removed although the plan had changed")
	}
	// Run again: the plan includes it now.
	if err := w.sharedDown(tc, commands.SharedDownOptions{Volumes: true, Yes: true}); err != nil {
		t.Fatal(err)
	}
	if w.Docker.Exists("volume", extra) {
		t.Error("the second run left it")
	}
}

func TestSharedDownOnARepositoryNeverBroughtUp(t *testing.T) {
	w := commandstest.New(t, "feature/login", acme)
	tc := w.Context("", acme, nil)
	port := 8025
	tc.Loaded.Config.Shared = &config.Shared{File: "compose.shared.yaml", Service: []config.SharedService{{Name: "mailpit", Port: &port, UsedBy: []string{"api"}}}}

	t.Run("prints the empty line and succeeds, the namespace named", func(t *testing.T) {
		if err := commands.SharedDownWithContext(context.Background(), tc.CommandContext, &commands.Env{CommandName: "treerig"}, commands.SharedDownOptions{}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(tc.Logger.Lines, "\n"), "No shared services of this repository to stop (Docker label namespace dev.treerig).") {
			t.Fatalf("lines = %v", tc.Logger.Lines)
		}
	})

	t.Run("checks --label-namespace first, whatever the repository", func(t *testing.T) {
		err := commands.SharedDownWithContext(context.Background(), tc.CommandContext, &commands.Env{CommandName: "treerig"}, commands.SharedDownOptions{LabelNamespace: "nope"})
		if err == nil || !strings.Contains(err.Error(), "--label-namespace must look like a reverse domain") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("shared status lists the declared services as not created, exit 0", func(t *testing.T) {
		tc.Logger.Lines, tc.Logger.Printed = nil, nil
		if err := commands.SharedStatusWithContext(context.Background(), tc.CommandContext, commands.OutputOptions{JSON: true}); err != nil {
			t.Fatal(err)
		}
		_, printed := tc.Logger.Snapshot()
		var report commands.SharedStatusReport
		if err := json.Unmarshal([]byte(strings.Join(printed, "\n")), &report); err != nil || len(report.Shared) != 1 {
			t.Fatalf("report = %+v, %v\n%v", report, err, printed)
		}
		if got := report.Shared[0]; got.Name != "mailpit" || got.State != worktree.NotCreated || got.Definition != "unknown" || got.Connected || len(got.Addrs) != 0 {
			t.Errorf("entry = %+v", got)
		}
	})
}

func TestSharedUpRepairsBeforeItRefusesADifferingDefinition(t *testing.T) {
	w := newSharedWorld(t)
	first := w.context("")
	one := w.mustUp(t, first)
	second := w.context(w.Worktree("second", "feature/two"))
	w.mustUp(t, second)
	id := w.mailpit(t, first)
	network := w.Docker.WorktreeDefaultNetwork(refOf(one))
	if err := w.Docker.Detach(id, network); err != nil {
		t.Fatal(err)
	}
	w.Docker.SetSharedService("mailpit", mailpitDefinition("axllent/mailpit:v2"))

	err := w.sharedUp(second, commands.SharedUpOptions{})
	if err == nil || !strings.Contains(err.Error(), "Not replacing a running shared service") ||
		!strings.Contains(err.Error(), "mailpit is running with a different definition than this worktree's (started by feature-login.acme.localhost)") ||
		!strings.Contains(err.Error(), "'treerig shared up --recreate mailpit'") {
		t.Fatalf("shared up: %v", err)
	}
	if got := w.Docker.Aliases(id, network); len(got) != 1 {
		t.Errorf("the repair did not run: aliases %v", got)
	}
	if w.mailpit(t, first) != id || w.sharedUps(t, first) != 1 {
		t.Error("the differing instance was replaced")
	}
	if strings.Contains(lines(second), "is already running with this definition") {
		t.Errorf("a differing instance was called current:\n%s", lines(second))
	}
}

func TestSharedUpRecreateNeedsAName(t *testing.T) {
	w := newSharedWorld(t)
	tc := twoServices(w, "")
	w.mustUp(t, tc)
	before := w.mailpit(t, tc)
	err := w.sharedUp(tc, commands.SharedUpOptions{Recreate: true})
	if err == nil || !strings.Contains(err.Error(), "needs the name of the one to replace") || !strings.Contains(err.Error(), "Running: mailpit, cache.") {
		t.Fatalf("err = %v", err)
	}
	if w.mailpit(t, tc) != before {
		t.Fatal("something was replaced")
	}
}

func TestPinnedPortHeldByAHostProcess(t *testing.T) {
	t.Run("up names the service, the port, the way to find the holder and the way out, and starts nothing", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		// Pin the ports first, as a previous run would have left them.
		w.mustUp(t, tc)
		repository := w.repository(t, tc)
		record, _ := tc.Registry.Store(repository).FindShared("mailpit")
		if err := w.sharedDown(tc, commands.SharedDownOptions{}); err != nil {
			t.Fatal(err)
		}
		w.Probe.Occupy(record.Ports[1025])
		ups := w.sharedUps(t, tc)

		_, err := w.up(t, tc)
		want := []string{"Cannot start shared service mailpit", "127.0.0.1:" + itoa(record.Ports[1025]), "container port 1025", "find it with:", "treerig shared up --reallocate-port mailpit:1025"}
		for _, part := range want {
			if err == nil || !strings.Contains(err.Error(), part) {
				t.Fatalf("up: %v, want %q", err, part)
			}
		}
		if w.sharedUps(t, tc) != ups {
			t.Error("compose up ran for the shared project")
		}
		// The way out works.
		w.Probe.Vacate(record.Ports[1025])
		if err := w.sharedUp(tc, commands.SharedUpOptions{ReallocatePorts: []string{"mailpit:1025"}}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a bind failure after the probe passed is mapped to the same refusal, with Docker's error as its cause", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.Docker.BeforeComposeUp(func(project string) {
			if strings.HasPrefix(project, brand.SharedProjectPrefix) {
				// Something takes every pinned port between the probe and the start.
				for port := tc.CommandContext.PortRange.Low; port <= tc.CommandContext.PortRange.High; port++ {
					w.Probe.Occupy(port)
				}
			}
		})
		w.Docker.FailCall("--no-recreate", "Error response from daemon: driver failed programming external connectivity: Bind for 127.0.0.1:30001 failed: port is already allocated", 1, 1)
		_, err := w.up(t, tc)
		if err == nil || !strings.Contains(err.Error(), "Cannot start shared service mailpit") || !strings.Contains(err.Error(), "--reallocate-port mailpit:") {
			t.Fatalf("up: %v", err)
		}
		var ce interface{ Unwrap() error }
		if !asUnwrapper(err, &ce) || !strings.Contains(ce.Unwrap().Error(), "port is already allocated") {
			t.Errorf("Docker's error is not the cause: %v", err)
		}
	})
}

// asUnwrapper reports whether err has a cause.
func asUnwrapper(err error, target *interface{ Unwrap() error }) bool {
	u, ok := err.(interface{ Unwrap() error })
	if ok {
		*target = u
	}
	return ok && u.Unwrap() != nil
}

func TestAStartThatFailedWithAContainerUpStillConnectsTheOthers(t *testing.T) {
	w := newSharedWorld(t)
	first := w.context("")
	one := w.mustUp(t, first)
	second := w.context(w.Worktree("second", "feature/two"))

	// The instance is lost, then the second worktree's start creates a
	// container and fails afterwards (a health check, say).
	if err := sharedStackFor(first, one.Registration.RepositoryID).Reap(context.Background(), commands.SharedResourceForTest("container", w.mailpit(t, first))); err != nil {
		t.Fatal(err)
	}
	w.Docker.FailCall("--no-recreate", "container for service mailpit is unhealthy", 1, 1)
	w.Docker.BeforeComposeUp(func(project string) {
		if !strings.HasPrefix(project, brand.SharedProjectPrefix) {
			return
		}
		labels := map[string]string{
			brand.ManagedLabel(brand.LabelNamespace): "true", brand.RepositoryLabel(brand.LabelNamespace): one.Registration.RepositoryID,
			brand.SharedLabel(brand.LabelNamespace): "true", "com.docker.compose.project": project, "com.docker.compose.service": "mailpit",
			brand.FormatLabel(brand.LabelNamespace): "1",
		}
		created := w.Docker.Add("container", labels)
		w.Docker.SetName(created, project+"-mailpit-1")
	})
	_, err := w.up(t, second)
	if err == nil || !strings.Contains(err.Error(), "unhealthy") {
		t.Fatalf("up: %v", err)
	}
	id := w.mailpit(t, first)
	if id == "" {
		t.Fatal("no container")
	}
	network := w.Docker.WorktreeDefaultNetwork(refOf(one))
	if got := w.Docker.Aliases(id, network); len(got) != 1 {
		t.Fatalf("the first worktree was not reconnected to the container that is up: %v", got)
	}
}

func TestAStartingServiceIsSaidToBeStarting(t *testing.T) {
	w := newSharedWorld(t)
	first := w.context("")
	w.mustUp(t, first)
	w.Docker.SetHealth(w.mailpit(t, first), "starting")
	second := w.context(w.Worktree("second", "feature/two"))
	w.mustUp(t, second)
	if !strings.Contains(lines(second), "Shared service mailpit is still starting") {
		t.Fatalf("output:\n%s", lines(second))
	}
	second.Logger.Lines, second.Logger.Printed = nil, nil
	if _, err := commands.StatusWithContext(context.Background(), second.CommandContext, commands.OutputOptions{JSON: true}); err != nil {
		t.Fatal(err)
	}
	_, printed := second.Logger.Snapshot()
	var report commands.StatusReport
	if err := json.Unmarshal([]byte(strings.Join(printed, "\n")), &report); err != nil || len(report.Shared) != 1 || report.Shared[0].State != "starting" {
		t.Fatalf("report = %+v, %v", report, err)
	}
}

func TestStatusUsesTheServiceWordsAndSurvivesOneBadContainer(t *testing.T) {
	ctx := context.Background()
	entries := func(t *testing.T, tc *commandstest.TestContext) []commands.SharedStatusEntry {
		t.Helper()
		tc.Logger.Lines, tc.Logger.Printed = nil, nil
		if err := commands.SharedStatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{JSON: true}); err != nil {
			t.Fatal(err)
		}
		_, printed := tc.Logger.Snapshot()
		var report commands.SharedStatusReport
		if err := json.Unmarshal([]byte(strings.Join(printed, "\n")), &report); err != nil {
			t.Fatalf("not JSON: %v\n%v", err, printed)
		}
		return report.Shared
	}

	t.Run("an unhealthy container is unhealthy, a restarting one restarting", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		id := w.mailpit(t, tc)
		w.Docker.SetHealth(id, "unhealthy")
		if got := entries(t, tc)[0]; got.State != worktree.Unhealthy || got.Detail != "" {
			t.Errorf("unhealthy: %+v", got)
		}
		w.Docker.SetHealth(id, "healthy")
		w.Docker.SetStatus(id, "restarting")
		if got := entries(t, tc)[0]; got.State != worktree.Restarting || got.Detail != "" {
			t.Errorf("restarting: %+v", got)
		}
	})

	t.Run("one container in a state treerig does not know is an entry with state unknown, and the rest are reported", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := twoServices(w, "")
		w.mustUp(t, tc)
		w.Docker.SetStatus(w.mailpit(t, tc), "levitating")
		got := entries(t, tc)
		if len(got) != 2 || got[0].State != worktree.Unknown || !strings.Contains(got[0].Detail, "levitating") || got[1].State != worktree.Running {
			t.Fatalf("entries = %+v", got)
		}
		// And the text form of `status` still prints.
		tc.Logger.Lines, tc.Logger.Printed = nil, nil
		if _, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{}); err != nil {
			t.Fatalf("status: %v", err)
		}
	})

	t.Run("a container that cannot be proven the stack's is a warning, not a failure", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		labels := commandstest.Ownership(refOf(result), brand.LabelNamespace)
		labels[brand.SharedLabel(brand.LabelNamespace)] = "true"
		labels["com.docker.compose.project"] = w.project(t, tc)
		w.Docker.Add("container", labels)
		if got := entries(t, tc); len(got) != 1 || got[0].State != "running" {
			t.Fatalf("entries = %+v", got)
		}
		if !strings.Contains(lines(tc), "claims to be a shared service's and was left out") {
			t.Errorf("output:\n%s", lines(tc))
		}
	})

	t.Run("an unreadable record leaves the addresses out with a warning, and a bad shared file makes the definition unknown with a warning", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		record := layout.SharedLayout(w.ControlHome, result.Registration.RepositoryID).RecordPath("mailpit")
		if err := os.WriteFile(record, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		w.Docker.FailCall("config --format json", "yaml: line 3: mapping values are not allowed", 1, 1)
		got := entries(t, tc)[0]
		if len(got.Addrs) != 0 || got.Definition != "unknown" || got.State != "running" {
			t.Errorf("entry = %+v", got)
		}
		out := lines(tc)
		if !strings.Contains(out, "Could not read the record of mailpit's pinned ports") || !strings.Contains(out, "every definition is reported unknown") {
			t.Errorf("output:\n%s", out)
		}
	})
}

func TestStatusTellsNotUsedHereFromNotConnected(t *testing.T) {
	ctx := context.Background()
	w := newSharedWorld(t)
	first := w.context("")
	w.mustUp(t, first)
	secondRoot := w.Worktree("second", "feature/two")
	second := w.context(secondRoot)
	w.mustUp(t, second)
	id := w.mailpit(t, first)
	text := func(tc *commandstest.TestContext) string {
		tc.Logger.Lines, tc.Logger.Printed = nil, nil
		if err := commands.SharedStatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{}); err != nil {
			t.Fatal(err)
		}
		_, printed := tc.Logger.Snapshot()
		return strings.Join(printed, "\n")
	}

	if got := text(second); !strings.Contains(got, "connected") || strings.Contains(got, "not connected") || strings.Contains(got, "not used here") {
		t.Errorf("connected:\n%s", got)
	}
	// Not connected: a running consumer, on a network the container left.
	if err := w.Docker.Detach(id, w.Docker.WorktreeDefaultNetwork(refOf(w.mustUpResult(t, second)))); err != nil {
		t.Fatal(err)
	}
	if got := text(second); !strings.Contains(got, "not connected") {
		t.Errorf("not connected:\n%s", got)
	}
	// Not used here: no service of this worktree that uses it is running.
	if err := commands.DownWithContext(ctx, second.CommandContext); err != nil {
		t.Fatal(err)
	}
	if got := text(second); !strings.Contains(got, "not used here") || strings.Contains(got, "not connected") {
		t.Errorf("not used here:\n%s", got)
	}
	var report commands.SharedStatusReport
	second.Logger.Lines, second.Logger.Printed = nil, nil
	if err := commands.SharedStatusWithContext(ctx, second.CommandContext, commands.OutputOptions{JSON: true}); err != nil {
		t.Fatal(err)
	}
	_, printed := second.Logger.Snapshot()
	if err := json.Unmarshal([]byte(strings.Join(printed, "\n")), &report); err != nil || report.Shared[0].Connected {
		t.Errorf("connected is true for a worktree with nothing running: %+v, %v", report, err)
	}
}

// mustUpResult is the result of an up that has already happened: a repeated up.
func (w *sharedWorld) mustUpResult(t *testing.T, tc *commandstest.TestContext) *commands.UpResult {
	t.Helper()
	return w.mustUp(t, tc)
}

func TestTheSweepWarnsAndSaysWhatIsLeft(t *testing.T) {
	w := newSharedWorld(t)
	first := w.context("")
	w.mustUp(t, first)
	secondRoot := w.Worktree("second", "feature/two")
	second := w.context(secondRoot)
	two := w.mustUp(t, second)

	markers := procid.Markers{Project: "x", WorktreeID: two.Registration.WorktreeID}
	op, _ := lock.WorktreeScopes(w.ControlHome, two.Registration.RepositoryID, two.Registration.WorktreeID, two.Registration.Slug, markers)
	holder := lock.New(op, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
	if _, err := holder.Acquire(context.Background(), "treerig up"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Release() })

	first.Logger.Lines, first.Logger.Printed = nil, nil
	if err := w.sharedUp(first, commands.SharedUpOptions{Names: []string{"mailpit"}, Recreate: true}); err != nil {
		t.Fatalf("shared up: %v (a skipped worktree is not a failure)", err)
	}
	var warns []string
	for _, line := range first.Logger.Lines {
		if strings.HasPrefix(line, "warn: ") {
			warns = append(warns, line)
		}
	}
	if len(warns) != 2 ||
		!strings.Contains(warns[0], "Skipped feature-two.acme.localhost: it is busy.") ||
		warns[1] != "warn: 1 worktree(s) were skipped and are not connected yet; run 'treerig shared up' again when they are idle." {
		t.Fatalf("warnings = %q", warns)
	}
}

func TestTheSweepGoesOnPastAnUnreadableRegistration(t *testing.T) {
	w := newSharedWorld(t)
	first := w.context("")
	one := w.mustUp(t, first)
	second := w.context(w.Worktree("second", "feature/two"))
	two := w.mustUp(t, second)
	record := layout.WorktreeLayout(w.ControlHome, two.Registration.RepositoryID, two.Registration.WorktreeID).RecordPath
	if err := os.WriteFile(record, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := w.sharedUp(first, commands.SharedUpOptions{Names: []string{"mailpit"}, Recreate: true})
	if err == nil || !strings.Contains(err.Error(), "Could not reconnect 1 worktree(s)") {
		t.Fatalf("shared up: %v", err)
	}
	if !strings.Contains(lines(first), "Could not read the registration of worktree "+two.Registration.WorktreeID) {
		t.Errorf("output:\n%s", lines(first))
	}
	if got := w.Docker.Aliases(w.mailpit(t, first), w.Docker.WorktreeDefaultNetwork(refOf(one))); len(got) != 1 {
		t.Errorf("the readable worktree was not reconnected: %v", got)
	}
}

func TestUnlockSharedHintKeepsTheFlag(t *testing.T) {
	w := commandstest.New(t, "feature/login", acme)
	tc := w.Context("", acme, nil)
	_, err := commands.UnlockSharedWithContext(context.Background(), tc.CommandContext, commands.UnlockOptions{Shared: true})
	if err == nil || !strings.Contains(err.Error(), "Name another with 'treerig worktree unlock --shared <worktree>'.") {
		t.Fatalf("err = %v", err)
	}
	if _, err := commands.UnlockWithContext(context.Background(), tc.CommandContext, commands.UnlockOptions{}); err == nil || strings.Contains(err.Error(), "--shared") {
		t.Fatalf("the plain hint changed: %v", err)
	}
}

func TestDoctorSaysWhereToRunUnlockWhenNothingIsRegistered(t *testing.T) {
	w := newSharedWorld(t)
	tc := w.context("")
	result := w.mustUp(t, tc)
	scope := lock.SharedScope(w.ControlHome, result.Registration.RepositoryID)
	lk := lock.New(scope, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
	if _, err := lk.Acquire(context.Background(), "treerig up"); err != nil {
		t.Fatal(err)
	}
	if err := lk.Release(); err != nil {
		t.Fatal(err)
	}
	mutateRecord(t, scope, func(rec map[string]any) {
		rec["pid"] = deadPID
		rec["machineId"] = "elsewhere"
		delete(rec, "releasedAt")
	})
	if _, err := remove(t, tc, approved(commands.RemoveOptions{})); err != nil {
		t.Fatal(err)
	}
	checks, err := commands.LockChecksForTest(w.ControlHome)
	if err != nil {
		t.Fatal(err)
	}
	var found *commands.Check
	for _, check := range checks {
		if check.ID == "locks.shared" {
			found = &check
		}
	}
	if found == nil || found.Subject != "shared services of repository "+result.Registration.RepositoryID ||
		!strings.Contains(found.Detail, "from a checkout of that repository") || !strings.Contains(found.Detail, scope.Target) {
		t.Fatalf("check = %+v", found)
	}
}

func TestADoctorListingFailureIsAFailureLine(t *testing.T) {
	w := newSharedWorld(t)
	tc := w.context("")
	w.mustUp(t, tc)
	// The statuses read passes; the orphan scan's listing fails. This depends
	// on the call count: the shared-labelled listing is the second one the
	// doctor section makes (statuses first, the orphan scan second), so a new
	// listing ahead of the orphan scan means changing the 2 below.
	w.Docker.FailCallAfter("label=dev.treerig.shared=true", 2, "Error response from daemon: i/o timeout", 1, -1)
	checks, err := commands.SharedDoctorForTest(context.Background(), tc.CommandContext)
	if err != nil {
		t.Fatal(err)
	}
	failed := false
	for _, check := range checks {
		failed = failed || (check.Status == commands.StatusFail && strings.Contains(check.Detail, "i/o timeout"))
	}
	if !failed {
		t.Fatalf("checks = %+v: a listing error was dropped", checks)
	}
}

func TestConfigValidateCountsSharedServices(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "treerig.toml")
	toml := "[project]\nname = \"acme\"\ndomain = \"acme.localhost\"\n[compose]\nfile = \"c.yaml\"\n[shared]\nfile = \"s.yaml\"\n[shared.service.mailpit]\n[shared.service.db]\n"
	if err := os.WriteFile(file, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	log := &logger.Recording{}
	env := &commands.Env{CommandName: "treerig", Logger: log, ConfigPath: file, Getenv: func(k string) (string, bool) {
		if k == "HOME" {
			return dir, true
		}
		return "", false
	}}
	if err := commands.ConfigValidate(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	_, printed := log.Snapshot()
	if len(printed) < 2 || printed[1] != "project acme · domain acme.localhost · 0 service(s) · 0 hook(s) · 2 shared service(s)" {
		t.Fatalf("printed = %q", printed)
	}
}

func TestDescribeAnonymousNamesTheServiceTheMountAndAShortID(t *testing.T) {
	long := strings.Repeat("ab12", 16)
	for _, c := range []struct {
		name   string
		volume shared.Resource
		want   string
	}{
		{"a generated 64-hex name is shortened", shared.Resource{ID: long, Service: "mailpit", Path: "/data"}, "mailpit's /data volume (ab12ab12…)"},
		{"a short name is kept whole", shared.Resource{ID: "anon01", Service: "cache", Path: "/scratch"}, "cache's /scratch volume (anon01)"},
		{"without a mount path or a service it still says what it is", shared.Resource{ID: long}, "anonymous volume (ab12ab12…)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := commands.DescribeAnonymousForTest(c.volume); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestAPlainSharedDownWarnsAboutAnonymousVolumes(t *testing.T) {
	w := newSharedWorld(t)
	tc := twoServices(w, "")
	w.mustUp(t, tc)
	tc.Logger.Lines, tc.Logger.Printed = nil, nil
	var anonymous string
	for _, id := range w.volumesOf() {
		if strings.HasPrefix(id, "anon") {
			anonymous = id
		}
	}
	if anonymous == "" {
		t.Fatal("no anonymous volume")
	}
	if err := w.sharedDown(tc, commands.SharedDownOptions{}); err != nil {
		t.Fatal(err)
	}
	out := lines(tc)
	if !strings.Contains(out, "warn: cache's /scratch volume ("+anonymous+") is anonymous") || !strings.Contains(out, "will not be reattached") ||
		!strings.Contains(out, "'treerig shared down --volumes' deletes these now") {
		t.Fatalf("output:\n%s", out)
	}
	// A down that removes nothing anonymous says nothing of the kind.
	w2 := newSharedWorld(t)
	tc2 := w2.context("")
	w2.mustUp(t, tc2)
	if err := w2.sharedDown(tc2, commands.SharedDownOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(lines(tc2), "anonymous") {
		t.Fatalf("output:\n%s", lines(tc2))
	}
}

// fallbackAfterActiveEndpoints is the whole message `up` ends with when Compose
// cannot recreate a network a shared container is attached to.
const fallbackAfterActiveEndpoints = "Compose cannot recreate this worktree's network while a shared service is attached to it. " +
	"Run 'treerig down' and then 'treerig up' again; the shared services keep running."

func TestUpWhenComposeCannotRecreateAnAttachedNetwork(t *testing.T) {
	t.Run("disconnects the shared service, runs compose up once and ends with the way out", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		network := w.Docker.WorktreeDefaultNetwork(refOf(result))
		id := w.mailpit(t, tc)
		w.Docker.FailCall(" up ", `network x_default has active endpoints (name:"mailpit")`, 1, -1)
		ups, disconnects := w.Docker.Count(" up "), w.Docker.Count("network disconnect")
		tc.Logger.Lines, tc.Logger.Printed = nil, nil

		_, err := w.up(t, tc)
		if err == nil || err.Error() != fallbackAfterActiveEndpoints {
			t.Fatalf("up: %v, want exactly the fallback", err)
		}
		cause := errors.Unwrap(err)
		if cause == nil || !strings.Contains(cause.Error(), "has active endpoints") {
			t.Errorf("the fallback lost Compose's error as its cause: %v", cause)
		}
		if got := w.Docker.Count(" up ") - ups; got != 1 {
			t.Errorf("compose up ran %d times, want exactly once", got)
		}
		if w.Docker.Count("network disconnect") == disconnects {
			t.Error("the shared container was not disconnected")
		}
		if got := w.Docker.Aliases(id, network); len(got) != 0 {
			t.Errorf("the shared container is still attached: %v", got)
		}
		if out := lines(tc); !strings.Contains(out, "Disconnected the shared services from this worktree's network.") {
			t.Errorf("output:\n%s", out)
		}
	})

	t.Run("does not say it disconnected when the disconnect failed", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		w.Docker.FailCall(" up ", `network x_default has active endpoints (name:"mailpit")`, 1, -1)
		w.Docker.FailCall("network disconnect", "Error response from daemon: boom", 1, -1)
		tc.Logger.Lines, tc.Logger.Printed = nil, nil

		_, err := w.up(t, tc)
		if err == nil || err.Error() != fallbackAfterActiveEndpoints {
			t.Fatalf("up: %v, want exactly the fallback", err)
		}
		out := lines(tc)
		if !strings.Contains(out, "Could not disconnect the shared services") || strings.Contains(out, "Disconnected the shared services") {
			t.Errorf("output:\n%s", out)
		}
	})

	t.Run("any other start failure is explained as it always was, with nothing disconnected", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		w.Docker.FailCall(" up ", "pull access denied for example/api", 1, -1)
		disconnects := w.Docker.Count("network disconnect")

		_, err := w.up(t, tc)
		if err == nil || !strings.Contains(err.Error(), "pull access denied for example/api") || strings.Contains(err.Error(), "treerig down") {
			t.Fatalf("up: %v", err)
		}
		if w.Docker.Count("network disconnect") != disconnects {
			t.Error("a shared container was disconnected for a failure that is not about the network")
		}
	})
}
