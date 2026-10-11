package commands_test

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/commands/commandstest"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/shared"
	"github.com/sofired/berth/internal/worktree"
)

// sharedWorld is a world whose berth.toml declares a mailpit shared service
// that the worktree's api service uses, over a daemon that creates what
// `compose up` would.
type sharedWorld struct {
	*commandstest.World
	router *commandstest.FakeRouter
	index  *commandstest.FakeIndex
}

// newSharedWorld is a world with one worktree, branch feature/login.
func newSharedWorld(t *testing.T) *sharedWorld {
	t.Helper()
	w := commandstest.New(t, "feature/login", acme)
	w.Docker.Realistic()
	w.Docker.SetSharedService("mailpit", map[string]any{
		"image":   "axllent/mailpit:v1.31.3",
		"volumes": []any{map[string]any{"type": "volume", "source": "mailbox", "target": "/data"}},
	})
	return &sharedWorld{World: w, router: commandstest.NewFakeRouter(80), index: &commandstest.FakeIndex{}}
}

// context is a CommandContext for the worktree at root ("" for the first),
// configured with a mailpit shared service used by api.
func (w *sharedWorld) context(root string) *commandstest.TestContext {
	return w.contextNamespace(root, nil)
}

// contextNamespace is context for a project that labels its resources under namespace.
func (w *sharedWorld) contextNamespace(root string, namespace *string) *commandstest.TestContext {
	tc := w.Context(root, acme, namespace)
	givePorts(tc)
	port := 8025
	tc.Loaded.Config.Shared = &config.Shared{
		File:    "compose.shared.yaml",
		Service: []config.SharedService{{Name: "mailpit", Port: &port, Ports: []int{1025}, UsedBy: []string{"api"}}},
	}
	return tc
}

// up runs `up` in tc.
func (w *sharedWorld) up(t *testing.T, tc *commandstest.TestContext) (*commands.UpResult, error) {
	t.Helper()
	return up(t, tc, w.router, w.index, commands.UpOptions{})
}

// mustUp runs `up` in tc and fails the test on error.
func (w *sharedWorld) mustUp(t *testing.T, tc *commandstest.TestContext) *commands.UpResult {
	t.Helper()
	result, err := w.up(t, tc)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	return result
}

// project is the shared Compose project of the world's repository.
func (w *sharedWorld) project(t *testing.T, tc *commandstest.TestContext) string {
	t.Helper()
	return layout.SharedProjectName(w.repository(t, tc))
}

// repository is the world's repository ID.
func (w *sharedWorld) repository(t *testing.T, tc *commandstest.TestContext) string {
	t.Helper()
	id, present, err := tc.Registry.Identities.RepositoryID(context.Background(), tc.Registry.Identities.Root)
	if err != nil || !present {
		t.Fatalf("RepositoryID: %q, %v, %v", id, present, err)
	}
	return id
}

// mailpit is the shared mailpit container's ID, or "".
func (w *sharedWorld) mailpit(t *testing.T, tc *commandstest.TestContext) string {
	t.Helper()
	return w.Docker.ContainerNamed(w.project(t, tc) + "-mailpit-1")
}

// sharedUps counts the `compose up` calls made for the shared project.
func (w *sharedWorld) sharedUps(t *testing.T, tc *commandstest.TestContext) int {
	t.Helper()
	n := 0
	project := w.project(t, tc)
	for _, call := range w.Docker.Calls() {
		if strings.Contains(call, "--project-name "+project) && strings.Contains(call, " up ") {
			n++
		}
	}
	return n
}

// lines are the messages the logger has printed, flattened.
func lines(tc *commandstest.TestContext) string {
	out, errs := tc.Logger.Snapshot()
	return strings.Join(append(out, errs...), "\n")
}

func TestUpStartsAndConnectsSharedServices(t *testing.T) {
	t.Run("the first up starts the shared service, pins its ports, and connects the worktree's network with the name as alias", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)

		id := w.mailpit(t, tc)
		if id == "" {
			t.Fatal("the shared service did not start")
		}
		repository := result.Registration.RepositoryID
		labels := w.Docker.Labels("container", id)
		if labels[brand.SharedLabel(brand.LabelNamespace)] != "true" || labels[brand.RepositoryLabel(brand.LabelNamespace)] != repository {
			t.Errorf("labels = %v", labels)
		}
		if labels[brand.SharedDefinedByLabel(brand.LabelNamespace)] != result.Registration.WorktreeID {
			t.Errorf("defined by %q, want the worktree %q", labels[brand.SharedDefinedByLabel(brand.LabelNamespace)], result.Registration.WorktreeID)
		}

		record, err := tc.Registry.Store(repository).FindShared("mailpit")
		if err != nil || record == nil || len(record.Ports) != 2 || record.Domain != acme {
			t.Fatalf("record = %+v, %v", record, err)
		}
		for container, host := range record.Ports {
			if got := w.Docker.Published(id, container); len(got) != 1 || got[0] != "127.0.0.1:"+itoa(host) {
				t.Errorf("container port %d publishes %v, record says %d", container, got, host)
			}
		}

		network := w.Docker.WorktreeDefaultNetwork(refOf(result))
		if network == "" {
			t.Fatal("the worktree has no default network")
		}
		if got := w.Docker.Aliases(id, network); len(got) != 1 || got[0] != "mailpit" {
			t.Errorf("aliases on the worktree's network = %v", got)
		}
		api := w.Docker.Find("container", map[string]string{brand.SharedUseLabel(brand.LabelNamespace, "mailpit"): "true"})
		if len(api) != 1 || w.Docker.Labels("container", api[0])["com.docker.compose.service"] != "api" {
			t.Errorf("the shared-use label is on %v", api)
		}
		if mail := w.Docker.Find("container", map[string]string{"com.docker.compose.service": "mail"}); len(mail) != 1 ||
			w.Docker.Labels("container", mail[0])[brand.SharedUseLabel(brand.LabelNamespace, "mailpit")] != "" {
			t.Errorf("a service that does not use mailpit is labelled as using it: %v", mail)
		}
	})

	t.Run("connects before the hooks run and after the worktree's own compose up", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		tc.Loaded.Config.Hooks = config.Hooks{Prepare: ns("./hooks/prepare")}
		inner := w.Runner.Handler
		problem, ran := "", false
		w.Runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
			if strings.HasSuffix(spec.Command, "hooks/prepare") {
				ran = true
				// At the moment the hook runs, the shared container must already be on
				// the worktree's network, and the worktree's own compose up must be done.
				id := w.mailpit(t, tc)
				if id == "" || len(w.Docker.Attachments(id)) != 2 {
					problem = "mailpit was not yet connected to the worktree's network"
				}
			}
			return inner(ctx, spec)
		}
		w.mustUp(t, tc)
		if !ran || problem != "" {
			t.Fatalf("hook ran = %v, problem: %s", ran, problem)
		}
	})
}

// itoa is strconv.Itoa.
func itoa(n int) string { return strconv.Itoa(n) }

// refOf is the worktree an up registered.
func refOf(result *commands.UpResult) worktree.Ref {
	r := result.Registration
	return worktree.Ref{RepositoryID: r.RepositoryID, WorktreeID: r.WorktreeID, Slug: r.Slug}
}

// mailpitDefinition is mailpit's entry in the shared file at a given image.
func mailpitDefinition(image string) map[string]any {
	return map[string]any{
		"image":   image,
		"volumes": []any{map[string]any{"type": "volume", "source": "mailbox", "target": "/data"}},
	}
}

func TestSharedPortsAvoidTheRouterState(t *testing.T) {
	t.Run("pins shared ports clear of the router's index and ping ports", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		// A four-port range holding the router's two ports leaves exactly the
		// other two for mailpit's two container ports, and no routed service
		// competes for them.
		tc.Loaded.Config.Service = nil
		tc.PortRange = layout.PortRange{Low: 30000, High: 30003}
		if err := os.MkdirAll(tc.RouterState.StateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		state := `{"version":1,"indexPort":30000,"pingPort":30001}`
		if err := os.WriteFile(tc.RouterState.StateFile, []byte(state), 0o600); err != nil {
			t.Fatal(err)
		}

		result := w.mustUp(t, tc)
		record, err := tc.Registry.Store(result.Registration.RepositoryID).FindShared("mailpit")
		if err != nil || record == nil {
			t.Fatalf("record = %+v, %v", record, err)
		}
		got := []int{}
		for _, port := range record.Ports {
			got = append(got, port)
		}
		slices.Sort(got)
		if !slices.Equal(got, []int{30002, 30003}) {
			t.Fatalf("shared ports = %v, want [30002 30003]: the router's 30000 and 30001 are reserved", got)
		}
	})

	t.Run("finds no port when the router's two hold the whole range", func(t *testing.T) {
		// Which free port a hash lands on varies with the repository, so the
		// test that fails without the reservation is the one with no free
		// port at all: the range is exactly the router's.
		w := newSharedWorld(t)
		tc := w.context("")
		tc.Loaded.Config.Service = nil
		tc.PortRange = layout.PortRange{Low: 30000, High: 30001}
		if err := os.MkdirAll(tc.RouterState.StateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		state := `{"version":1,"indexPort":30000,"pingPort":30001}`
		if err := os.WriteFile(tc.RouterState.StateFile, []byte(state), 0o600); err != nil {
			t.Fatal(err)
		}

		_, err := w.up(t, tc)
		wantErr(t, err, `No free port for mailpit:\d+ in 30000-30001`)
		if w.mailpit(t, tc) != "" {
			t.Error("the shared service started without ports")
		}
	})
}

func TestTwoWorktreesShareOneInstance(t *testing.T) {
	w := newSharedWorld(t)
	first := w.context("")
	one := w.mustUp(t, first)
	second := w.context(w.Worktree("second", "feature/two"))
	two := w.mustUp(t, second)

	id := w.mailpit(t, first)
	t.Run("the second up reuses the instance and calls nothing for it", func(t *testing.T) {
		if got := w.sharedUps(t, first); got != 1 {
			t.Fatalf("compose up ran %d times for the shared project", got)
		}
		if got := w.Docker.Find("container", map[string]string{brand.SharedLabel(brand.LabelNamespace): "true"}); len(got) != 1 || got[0] != id {
			t.Fatalf("shared containers = %v", got)
		}
	})

	t.Run("it is on each worktree's own network and no other", func(t *testing.T) {
		networkOne := w.Docker.WorktreeDefaultNetwork(refOf(one))
		networkTwo := w.Docker.WorktreeDefaultNetwork(refOf(two))
		if networkOne == "" || networkTwo == "" || networkOne == networkTwo {
			t.Fatalf("networks = %q, %q", networkOne, networkTwo)
		}
		attached := w.Docker.Attachments(id)
		for _, want := range []string{networkOne, networkTwo} {
			found := false
			for _, got := range attached {
				found = found || got == want
			}
			if !found {
				t.Errorf("not attached to %s (attached to %v)", want, attached)
			}
		}
		if len(attached) != 3 {
			t.Errorf("attached to %v: its own network and the two worktrees'", attached)
		}
	})

	t.Run("the worktrees' containers never share a network", func(t *testing.T) {
		for _, result := range []*commands.UpResult{one, two} {
			network := w.Docker.WorktreeDefaultNetwork(refOf(result))
			for _, other := range w.Docker.Find("container", map[string]string{brand.WorktreeLabel(brand.LabelNamespace): result.Registration.WorktreeID}) {
				if got := w.Docker.Attachments(other); len(got) != 1 || got[0] != network {
					t.Errorf("container %s is on %v, want only %s", other, got, network)
				}
			}
		}
	})

	t.Run("a repeated up changes nothing", func(t *testing.T) {
		w.mustUp(t, first)
		w.mustUp(t, second)
		if got := w.sharedUps(t, first); got != 1 {
			t.Fatalf("compose up ran %d times for the shared project", got)
		}
		if got := w.Docker.Attachments(id); len(got) != 3 {
			t.Fatalf("attachments = %v", got)
		}
	})
}

func TestUpFailsBeforeTouchingTheWorktreeWhenTheSharedServiceCannotStart(t *testing.T) {
	w := newSharedWorld(t)
	tc := w.context("")
	w.Docker.FailCall("--no-recreate", "pull access denied for axllent/mailpit", 1, -1)
	_, err := w.up(t, tc)
	if err == nil || !strings.Contains(err.Error(), "pull access denied") {
		t.Fatalf("up: %v", err)
	}
	if got := w.Docker.Find("container", map[string]string{brand.ManagedLabel(brand.LabelNamespace): "true"}); len(got) != 0 {
		t.Fatalf("containers were started: %v", got)
	}
	if w.Docker.Count("compose --project-name "+tc.Loaded.Config.Project.Name) != 0 {
		t.Error("the worktree's own compose was called")
	}
}

func TestSharedDefinitionDrift(t *testing.T) {
	setup := func(t *testing.T) (*sharedWorld, *commandstest.TestContext, *commandstest.TestContext) {
		w := newSharedWorld(t)
		first := w.context("")
		w.mustUp(t, first)
		second := w.context(w.Worktree("second", "feature/two"))
		return w, first, second
	}

	t.Run("up warns, names who started it, and never restarts or recreates it", func(t *testing.T) {
		w, first, second := setup(t)
		id := w.mailpit(t, first)
		w.Docker.SetSharedService("mailpit", mailpitDefinition("axllent/mailpit:v9.9.9"))
		w.mustUp(t, second)

		if got := w.mailpit(t, first); got != id {
			t.Fatalf("the container was recreated: %s -> %s", id, got)
		}
		if w.sharedUps(t, first) != 1 {
			t.Fatal("compose up ran for a running shared service")
		}
		out := lines(second)
		if !strings.Contains(out, "Shared service mailpit is running with a different definition than this worktree's (started by feature-login.acme.localhost)") ||
			!strings.Contains(out, "berth shared up --recreate mailpit") {
			t.Fatalf("no drift warning in:\n%s", out)
		}
		// It is connected all the same.
		if len(w.Docker.Attachments(id)) != 3 {
			t.Fatalf("attachments = %v", w.Docker.Attachments(id))
		}
	})

	t.Run("says a removed worktree when the one that started it is gone", func(t *testing.T) {
		w, first, second := setup(t)
		w.Docker.SetSharedService("mailpit", mailpitDefinition("axllent/mailpit:v9.9.9"))
		record, err := first.Registry.Store(w.repository(t, first)).Find(worktreeIDOf(t, first))
		if err != nil || record == nil {
			t.Fatal(err)
		}
		if _, err := first.Registry.Store(record.RepositoryID).Remove(context.Background(), record.WorktreeID, record.Slug, registry.Writer{UserHome: w.UserHome}); err != nil {
			t.Fatal(err)
		}
		w.mustUp(t, second)
		if !strings.Contains(lines(second), "(started by a removed worktree)") {
			t.Fatalf("output:\n%s", lines(second))
		}
	})

	t.Run("a running instance with the same definition is silent", func(t *testing.T) {
		w, _, second := setup(t)
		w.mustUp(t, second)
		if strings.Contains(lines(second), "different definition") {
			t.Fatalf("output:\n%s", lines(second))
		}
	})

	t.Run("a stopped instance is replaced and started from the caller's definition", func(t *testing.T) {
		w, first, second := setup(t)
		old := w.mailpit(t, first)
		w.Docker.SetStatus(old, "exited")
		w.Docker.SetSharedService("mailpit", mailpitDefinition("axllent/mailpit:v2"))
		w.mustUp(t, second)

		if w.Docker.Exists("container", old) {
			t.Fatal("the stopped container was kept")
		}
		id := w.mailpit(t, first)
		if id == "" || id == old {
			t.Fatalf("mailpit = %q (was %q)", id, old)
		}
		if got := w.Docker.Labels("container", id)[brand.SharedDefinedByLabel(brand.LabelNamespace)]; got != worktreeIDOf(t, second) {
			t.Errorf("defined by %q, want the second worktree", got)
		}
		if len(w.Docker.Find("volume", map[string]string{brand.SharedLabel(brand.LabelNamespace): "true"})) != 1 {
			t.Error("the volume did not survive")
		}
	})
}

// worktreeIDOf is the worktree ID tc's checkout has.
func worktreeIDOf(t *testing.T, tc *commandstest.TestContext) string {
	t.Helper()
	identity, err := tc.Registry.Identities.Inspect(context.Background(), tc.Registry.Identities.Root, registry.InspectOptions{})
	if err != nil || identity == nil {
		t.Fatalf("identity: %v, %v", identity, err)
	}
	return identity.WorktreeID
}

// sharedResources are the ids of everything carrying the shared label.
func (w *sharedWorld) sharedResources() map[string][]string {
	found := map[string][]string{}
	for _, kind := range []string{"container", "volume", "network"} {
		found[kind] = w.Docker.Find(kind, map[string]string{brand.SharedLabel(brand.LabelNamespace): "true"})
	}
	return found
}

// callIndex is the position of the first docker call containing every one of
// parts, or -1.
func callIndex(w *sharedWorld, after int, parts ...string) int {
	for i, call := range w.Docker.Calls() {
		if i < after {
			continue
		}
		matches := true
		for _, part := range parts {
			matches = matches && strings.Contains(call, part)
		}
		if matches {
			return i
		}
	}
	return -1
}

func TestDownAndRemoveWhileSharedServiceIsAttached(t *testing.T) {
	ctx := context.Background()

	t.Run("down disconnects the shared container first, and leaves it running and the worktree's network removed", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		id := w.mailpit(t, tc)
		network := w.Docker.WorktreeDefaultNetwork(refOf(result))
		before := len(w.Docker.Calls())

		if err := commands.DownWithContext(ctx, tc.CommandContext); err != nil {
			t.Fatalf("down: %v", err)
		}
		disconnect := callIndex(w, before, "network disconnect", network, id)
		composeDown := callIndex(w, before, "compose", " down ")
		if disconnect < 0 || composeDown < 0 || disconnect > composeDown {
			t.Fatalf("disconnect at %d, compose down at %d:\n%v", disconnect, composeDown, w.Docker.Calls()[before:])
		}
		if w.Docker.WorktreeDefaultNetwork(refOf(result)) != "" {
			t.Error("the worktree's network was kept")
		}
		if !w.Docker.Exists("container", id) || w.Docker.Labels("container", id)[brand.SharedLabel(brand.LabelNamespace)] != "true" {
			t.Fatal("the shared container went with the worktree")
		}
		if got := w.Docker.Attachments(id); len(got) != 1 {
			t.Errorf("attachments = %v, want only its own network", got)
		}
	})

	t.Run("worktree remove succeeds with it attached, leaving it and its data", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		id := w.mailpit(t, tc)
		resources := w.sharedResources()

		removed, err := remove(t, tc, approved(commands.RemoveOptions{}))
		if err != nil {
			t.Fatalf("remove: %v", err)
		}
		if removed.NetworkKept {
			t.Fatal("the worktree's network was kept")
		}
		if left := w.Docker.Owned(refOf(result), brand.LabelNamespace); len(left) != 0 {
			t.Fatalf("the worktree left behind %v", left)
		}
		for kind, ids := range resources {
			for _, resource := range ids {
				if !w.Docker.Exists(kind, resource) {
					t.Errorf("shared %s %s was removed by worktree remove", kind, resource)
				}
			}
		}
		if !w.Docker.Exists("container", id) {
			t.Fatal("the shared container is gone")
		}
		// The disconnect came before the first reap.
		disconnect := callIndex(w, 0, "network disconnect", id)
		firstReap := callIndex(w, 0, "rm ")
		if disconnect < 0 || (firstReap >= 0 && disconnect > firstReap) {
			t.Errorf("disconnect at %d, first reap at %d", disconnect, firstReap)
		}
		// The pinned ports are not the worktree's to release.
		if record, _ := tc.Registry.Store(result.Registration.RepositoryID).FindShared("mailpit"); record == nil {
			t.Error("worktree remove released the shared ports")
		}
	})

	t.Run("worktree remove of one worktree leaves the other's attachment alone", func(t *testing.T) {
		w := newSharedWorld(t)
		first := w.context("")
		w.mustUp(t, first)
		second := w.context(w.Worktree("second", "feature/two"))
		two := w.mustUp(t, second)
		id := w.mailpit(t, first)

		if _, err := remove(t, first, approved(commands.RemoveOptions{})); err != nil {
			t.Fatal(err)
		}
		attached := w.Docker.Attachments(id)
		if len(attached) != 2 {
			t.Fatalf("attachments = %v, want its own and the second worktree's", attached)
		}
		network := w.Docker.WorktreeDefaultNetwork(refOf(two))
		if attached[0] != network && attached[1] != network {
			t.Errorf("the second worktree's network %s is gone from %v", network, attached)
		}
	})

	t.Run("a shared container that is already gone is no obstacle to down", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		stale := w.mailpit(t, tc)
		w.Docker.FailCall("network disconnect", "Error response from daemon: No such container: "+stale, 1, -1)
		if err := commands.DownWithContext(ctx, tc.CommandContext); err != nil {
			t.Fatalf("down: %v", err)
		}
	})

	t.Run("a network that fails proof is reported and never disconnected", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		id := w.mailpit(t, tc)
		network := w.Docker.WorktreeDefaultNetwork(refOf(result))
		// Its labels stop proving it is this worktree's.
		labels := w.Docker.Labels("network", network)
		labels[brand.WorktreeLabel(brand.LabelNamespace)] = "someone-else"
		before := len(w.Docker.Calls())

		_ = commands.DownWithContext(ctx, tc.CommandContext)
		if callIndex(w, before, "network disconnect", network) >= 0 {
			t.Fatal("a network that failed proof was disconnected")
		}
		found := false
		for _, a := range w.Docker.Attachments(id) {
			found = found || a == network
		}
		if !found {
			t.Fatal("the attachment to the unproven network was removed")
		}
	})
}

func TestDecoysNeitherInventoryListsTheOthers(t *testing.T) {
	w := newSharedWorld(t)
	tc := w.context("")
	result := w.mustUp(t, tc)
	ref := refOf(result)
	runtime := tc.Runtime(brand.LabelNamespace)
	stack := sharedStackFor(tc, result.Registration.RepositoryID)

	t.Run("the worktree's inventory lists no shared resource, though the shared container is on its network", func(t *testing.T) {
		if len(w.Docker.Attachments(w.mailpit(t, tc))) != 2 {
			t.Fatal("the shared container is not attached to the worktree's network")
		}
		owned, err := runtime.Inventory(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		shared := w.sharedResources()
		for _, resource := range owned {
			for _, ids := range shared {
				for _, id := range ids {
					if resource.ID == id {
						t.Errorf("the worktree's inventory lists the shared %s %s", resource.Kind, id)
					}
				}
			}
		}
		if len(owned) == 0 {
			t.Fatal("the worktree's own inventory is empty")
		}
	})

	t.Run("the shared inventory lists no worktree resource, though a worktree container is on its network", func(t *testing.T) {
		inventory, err := stack.Inventory(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, resource := range inventory {
			if w.Docker.Labels(resource.Kind, resource.ID)[brand.WorktreeLabel(brand.LabelNamespace)] != "" {
				t.Errorf("the shared inventory lists %s", resource)
			}
		}
		if len(inventory) != 3 {
			t.Errorf("inventory = %v", inventory)
		}
	})

	t.Run("reaping a worktree's own resources never removes a shared one", func(t *testing.T) {
		owned, err := runtime.Inventory(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		before := w.sharedResources()
		// Disconnected first, as worktree remove does; then every resource is reaped.
		if err := commands.DisconnectSharedForTest(context.Background(), tc.CommandContext, ref, brand.LabelNamespace, brand.LabelNamespace); err != nil {
			t.Fatal(err)
		}
		for _, resource := range owned {
			if err := runtime.Reap(context.Background(), ref, resource); err != nil {
				t.Fatalf("reap %v: %v", resource, err)
			}
		}
		for kind, ids := range before {
			for _, id := range ids {
				if !w.Docker.Exists(kind, id) {
					t.Errorf("shared %s %s was reaped", kind, id)
				}
			}
		}
	})
}

// sharedStackFor is the repository's shared stack as tc builds it.
func sharedStackFor(tc *commandstest.TestContext, repositoryID string) *shared.Stack {
	return commands.SharedStackForTest(tc.CommandContext, repositoryID)
}

func TestUpRefusesAWorktreeThatCannotTakeTheSharedServices(t *testing.T) {
	cases := []struct {
		name  string
		setup func(w *sharedWorld, tc *commandstest.TestContext)
		want  string
	}{
		{"a used-by service the Compose file does not define", func(w *sharedWorld, tc *commandstest.TestContext) {
			tc.Loaded.Config.Shared.Service[0].UsedBy = []string{"api", "ghost"}
		}, "[shared.service.mailpit] lists ghost in used-by, which is not defined in"},
		{"a shared name that is also a Compose service", func(w *sharedWorld, tc *commandstest.TestContext) {
			w.Docker.Services = append(w.Docker.Services, "mailpit")
		}, "[shared.service.mailpit] has the same name as a service in"},
		{"a used-by service on the host network", func(w *sharedWorld, tc *commandstest.TestContext) {
			w.Docker.SetServiceConfig("api", "network_mode", "host")
		}, "Service api in"},
		{"a used-by service sharing another service's network namespace", func(w *sharedWorld, tc *commandstest.TestContext) {
			w.Docker.SetServiceConfig("api", "network_mode", "service:mail")
		}, "network_mode: service:mail, so the shared service mailpit cannot be connected"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newSharedWorld(t)
			tc := w.context("")
			c.setup(w, tc)
			_, err := w.up(t, tc)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("up: %v, want %q", err, c.want)
			}
			if got := w.Docker.Find("container", map[string]string{brand.ManagedLabel(brand.LabelNamespace): "true"}); len(got) != 0 {
				t.Errorf("something started: %v", got)
			}
			if w.sharedUps(t, tc) != 0 {
				t.Error("the shared project was started")
			}
		})
	}

	t.Run("accepts a used-by service that only an inactive profile defines", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		tc.Loaded.Config.Shared.Service[0].UsedBy = []string{"api", "jobs"}
		// `compose config` hides jobs until its profile is on, and shows it with --profile '*'.
		w.Docker.FailCall("config", "", 0, 0)
		inner := w.Runner.Handler
		w.Runner.Handler = profileAware(inner, "jobs")
		if _, err := w.up(t, tc); err != nil {
			t.Fatalf("up: %v", err)
		}
	})
}

// profileAware makes `compose --profile * config` report service as well, the
// way Compose does once every profile is on.
func profileAware(inner func(context.Context, run.Spec) (run.Result, error), service string) func(context.Context, run.Spec) (run.Result, error) {
	return func(ctx context.Context, spec run.Spec) (run.Result, error) {
		result, err := inner(ctx, spec)
		if err != nil || spec.Command != "docker" || !strings.Contains(strings.Join(spec.Args, " "), "--profile * config") {
			return result, err
		}
		var document map[string]any
		if json.Unmarshal([]byte(result.Stdout), &document) != nil {
			return result, err
		}
		services, _ := document["services"].(map[string]any)
		services[service] = map[string]any{"image": "busybox"}
		encoded, _ := json.Marshal(document)
		result.Stdout = string(encoded)
		return result, nil
	}
}

func TestSharedStartFailsClosed(t *testing.T) {
	t.Run("up fails rather than assume the shared container is absent when Docker cannot say", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.Docker.FailCall("ps -aq", "Error response from daemon: i/o timeout", 1, -1)
		_, err := w.up(t, tc)
		if err == nil {
			t.Fatal("up succeeded")
		}
		if w.sharedUps(t, tc) != 0 {
			t.Error("compose up ran although the state of the container was unknown")
		}
	})

	t.Run("up fails on a container state it does not know, and never recreates", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		w.Docker.SetStatus(w.mailpit(t, tc), "levitating")
		_, err := w.up(t, tc)
		if err == nil || !strings.Contains(err.Error(), "Could not tell what state") {
			t.Fatalf("up: %v", err)
		}
		if w.sharedUps(t, tc) != 1 {
			t.Error("compose up ran again")
		}
	})

	t.Run("an inspect that fails is not read as the container being gone", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		w.Docker.FailCall("container inspect", "Error response from daemon: boom", 1, -1)
		if _, err := w.up(t, tc); err == nil {
			t.Fatal("up succeeded")
		}
		if w.sharedUps(t, tc) != 1 {
			t.Error("compose up ran again")
		}
	})
}

func TestConcurrentFirstStartsAreTolerated(t *testing.T) {
	w := newSharedWorld(t)
	tc := w.context("")
	project := ""
	w.Docker.BeforeComposeUp(func(p string) {
		if !strings.HasPrefix(p, brand.SharedProjectPrefix) || project != "" {
			return
		}
		project = p
		// Another control home wins the race: its container exists by the time ours is created.
		labels := commandstest.Ownership(worktree.Ref{RepositoryID: w.repository(t, tc)}, brand.LabelNamespace)
		delete(labels, brand.WorktreeLabel(brand.LabelNamespace))
		labels[brand.SharedLabel(brand.LabelNamespace)] = "true"
		labels["com.docker.compose.project"] = p
		labels["com.docker.compose.service"] = "mailpit"
		id := w.Docker.Add("container", labels)
		w.Docker.SetName(id, p+"-mailpit-1")
	})
	w.Docker.FailCall("--no-recreate", `Error response from daemon: Conflict. The container name "/x-mailpit-1" is already in use by container "abc".`, 1, 1)
	if _, err := w.up(t, tc); err != nil {
		t.Fatalf("up: %v", err)
	}
	if !strings.Contains(lines(tc), "Shared service mailpit was started by another command; using it.") {
		t.Fatalf("output:\n%s", lines(tc))
	}
	// The winner's container is the one that is used; ours was never created.
	if got := w.Docker.Find("container", map[string]string{"com.docker.compose.service": "mailpit"}); len(got) != 1 {
		t.Fatalf("mailpit containers = %v", got)
	}
}

func TestHooksAndCommandsGetThePinnedSharedAddresses(t *testing.T) {
	w := newSharedWorld(t)
	tc := w.context("")
	tc.Loaded.Config.Hooks = config.Hooks{Prepare: ns("./hooks/prepare")}
	tc.Loaded.Config.Commands = config.Commands{{Name: "show", Command: config.Command{Command: "./show", Lock: true, KillAfter: "10s"}}}
	result := w.mustUp(t, tc)
	record, err := tc.Registry.Store(result.Registration.RepositoryID).FindShared("mailpit")
	if err != nil || record == nil {
		t.Fatalf("record = %v, %v", record, err)
	}

	want := map[string]string{
		"BERTH_SHARED_ADDR_MAILPIT_1025": "127.0.0.1:" + itoa(record.Ports[1025]),
		"BERTH_SHARED_ADDR_MAILPIT_8025": "127.0.0.1:" + itoa(record.Ports[8025]),
	}
	envOf := func(suffix string) []string {
		for _, call := range w.Runner.Calls() {
			if strings.HasSuffix(call.Command, suffix) {
				return call.Env
			}
		}
		t.Fatalf("no %s call", suffix)
		return nil
	}
	check := func(t *testing.T, env []string) {
		t.Helper()
		for name, value := range want {
			if !slices.Contains(env, name+"="+value) {
				t.Errorf("%s=%s missing from the environment", name, value)
			}
		}
	}
	t.Run("up's hooks", func(t *testing.T) { check(t, envOf("hooks/prepare")) })
	t.Run("berth run", func(t *testing.T) {
		if err := commands.RunWithContext(context.Background(), tc.CommandContext, commands.RunOptions{Name: "show"}); err != nil {
			t.Fatal(err)
		}
		check(t, envOf("/show"))
	})
	t.Run("never in the environment Compose gets", func(t *testing.T) {
		for _, call := range w.Runner.Calls() {
			if call.Command != "docker" {
				continue
			}
			for _, entry := range call.Env {
				if strings.HasPrefix(entry, "BERTH_SHARED_") {
					t.Fatalf("docker %v got %s", call.Args, entry)
				}
			}
		}
	})
}
