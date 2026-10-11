package commands_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/commands/commandstest"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/procid"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/shared"
)

// sharedUp runs `shared up` in tc.
func (w *sharedWorld) sharedUp(tc *commandstest.TestContext, options commands.SharedUpOptions) error {
	return commands.SharedUpWithContext(context.Background(), tc.CommandContext, options)
}

// sharedDown runs `shared down` in tc with a non-interactive Env.
func (w *sharedWorld) sharedDown(tc *commandstest.TestContext, options commands.SharedDownOptions) error {
	return commands.SharedDownWithContext(context.Background(), tc.CommandContext, &commands.Env{CommandName: "berth"}, options)
}

// downgrade is tc with no usable berth.toml, as `shared down` may run.
func downgrade(tc *commandstest.TestContext) *commandstest.TestContext {
	bare := *tc.CommandContext
	bare.Loaded = nil
	return &commandstest.TestContext{CommandContext: &bare, Logger: tc.Logger}
}

func TestSharedUp(t *testing.T) {
	t.Run("starts every declared service for a registered worktree and connects it", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		w.Docker.ClearFailures()
		// Stop the instance, as a reboot would; shared up starts it again from this definition.
		old := w.mailpit(t, tc)
		w.Docker.SetStatus(old, "exited")

		if err := w.sharedUp(tc, commands.SharedUpOptions{}); err != nil {
			t.Fatalf("shared up: %v", err)
		}
		id := w.mailpit(t, tc)
		if id == "" || id == old || w.Docker.Exists("container", old) {
			t.Fatalf("mailpit = %q (was %q)", id, old)
		}
		network := w.Docker.WorktreeDefaultNetwork(refOf(result))
		if got := w.Docker.Aliases(id, network); len(got) != 1 || got[0] != "mailpit" {
			t.Errorf("not reconnected: aliases %v", got)
		}
	})

	t.Run("refuses an unregistered worktree, an unknown name, and a config with no shared services", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		if err := w.sharedUp(tc, commands.SharedUpOptions{}); err == nil || !strings.Contains(err.Error(), "This worktree is not registered") {
			t.Fatalf("unregistered: %v", err)
		}
		w.mustUp(t, tc)
		if err := w.sharedUp(tc, commands.SharedUpOptions{Names: []string{"nope"}}); err == nil || !strings.Contains(err.Error(), "No shared service called nope. berth.toml declares mailpit.") {
			t.Fatalf("unknown name: %v", err)
		}
		tc.Loaded.Config.Shared = nil
		if err := w.sharedUp(tc, commands.SharedUpOptions{}); err == nil || !strings.Contains(err.Error(), "declares no shared services") {
			t.Fatalf("no shared services: %v", err)
		}
	})

	t.Run("on a running instance with the same definition changes nothing but still reconnects", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		id := w.mailpit(t, tc)
		network := w.Docker.WorktreeDefaultNetwork(refOf(result))
		// A daemon restart that dropped the attachment.
		if err := w.Docker.Detach(id, network); err != nil {
			t.Fatal(err)
		}

		if err := w.sharedUp(tc, commands.SharedUpOptions{}); err != nil {
			t.Fatalf("shared up: %v", err)
		}
		if w.sharedUps(t, tc) != 1 || w.mailpit(t, tc) != id {
			t.Fatal("a running instance was started again")
		}
		if got := w.Docker.Aliases(id, network); len(got) != 1 {
			t.Fatalf("aliases = %v: the worktree was not repaired", got)
		}
		if !strings.Contains(lines(tc), "Shared service mailpit is already running with this definition.") {
			t.Errorf("output:\n%s", lines(tc))
		}
	})

	t.Run("refuses a differing definition without --recreate, changing nothing", func(t *testing.T) {
		w := newSharedWorld(t)
		first := w.context("")
		w.mustUp(t, first)
		second := w.context(w.Worktree("second", "feature/two"))
		w.mustUp(t, second)
		id := w.mailpit(t, first)
		w.Docker.SetSharedService("mailpit", mailpitDefinition("axllent/mailpit:v2"))

		err := w.sharedUp(second, commands.SharedUpOptions{})
		if err == nil || !strings.Contains(err.Error(), "mailpit is running with a different definition than this worktree's (started by feature-login.acme.localhost)") ||
			!strings.Contains(err.Error(), "berth shared up --recreate mailpit") {
			t.Fatalf("shared up: %v", err)
		}
		if w.mailpit(t, first) != id || w.sharedUps(t, first) != 1 {
			t.Fatal("a refused shared up changed the instance")
		}
	})

	t.Run("--recreate replaces it, keeps its volume and reconnects every worktree", func(t *testing.T) {
		w := newSharedWorld(t)
		first := w.context("")
		one := w.mustUp(t, first)
		second := w.context(w.Worktree("second", "feature/two"))
		two := w.mustUp(t, second)
		old := w.mailpit(t, first)
		volumes := w.sharedResources()["volume"]
		w.Docker.SetSharedService("mailpit", mailpitDefinition("axllent/mailpit:v2"))

		if err := w.sharedUp(second, commands.SharedUpOptions{Names: []string{"mailpit"}, Recreate: true}); err != nil {
			t.Fatalf("shared up --recreate: %v", err)
		}
		id := w.mailpit(t, first)
		if id == "" || id == old {
			t.Fatalf("mailpit = %q (was %q)", id, old)
		}
		if got := w.sharedResources()["volume"]; len(got) != 1 || got[0] != volumes[0] {
			t.Errorf("volumes = %v, were %v", got, volumes)
		}
		for _, result := range []*commands.UpResult{one, two} {
			network := w.Docker.WorktreeDefaultNetwork(refOf(result))
			if got := w.Docker.Aliases(id, network); len(got) != 1 || got[0] != "mailpit" {
				t.Errorf("%s is not reconnected: aliases %v", result.Registration.Slug, got)
			}
		}
		if got := w.Docker.Labels("container", id)[brand.SharedDefinedByLabel(brand.LabelNamespace)]; got != two.Registration.WorktreeID {
			t.Errorf("defined by %q, want the second worktree", got)
		}
	})

	t.Run("--recreate replaces a running service even when its definition is the same, and reconnects", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		old := w.mailpit(t, tc)

		if err := w.sharedUp(tc, commands.SharedUpOptions{Names: []string{"mailpit"}, Recreate: true}); err != nil {
			t.Fatalf("shared up --recreate: %v", err)
		}
		id := w.mailpit(t, tc)
		if id == "" || id == old || w.Docker.Exists("container", old) {
			t.Fatalf("mailpit = %q (was %q)", id, old)
		}
		if got := w.Docker.Aliases(id, w.Docker.WorktreeDefaultNetwork(refOf(result))); len(got) != 1 {
			t.Errorf("not reconnected: %v", got)
		}
		if !strings.Contains(lines(tc), "Replacing shared service mailpit.") {
			t.Errorf("output:\n%s", lines(tc))
		}
	})

	t.Run("the pinned ports survive a recreate", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		store := tc.Registry.Store(result.Registration.RepositoryID)
		before, _ := store.FindShared("mailpit")
		w.Docker.SetSharedService("mailpit", mailpitDefinition("axllent/mailpit:v2"))
		if err := w.sharedUp(tc, commands.SharedUpOptions{Names: []string{"mailpit"}, Recreate: true}); err != nil {
			t.Fatal(err)
		}
		after, _ := store.FindShared("mailpit")
		if before.Ports[1025] != after.Ports[1025] || before.Ports[8025] != after.Ports[8025] {
			t.Fatalf("ports changed: %v -> %v", before.Ports, after.Ports)
		}
		for container, host := range after.Ports {
			if got := w.Docker.Published(w.mailpit(t, tc), container); len(got) != 1 || got[0] != "127.0.0.1:"+itoa(host) {
				t.Errorf("container port %d publishes %v, want %d", container, got, host)
			}
		}
	})
}

// TestTheSweepUsesTheStacksNamespaceForTheStackAndTheWorktreesForItsConsumers
// pins that a worktree registered under another Docker label namespace is
// still found and attached: the shared container carries the namespace of the
// command that started it, the worktree's containers its own.
func TestTheSweepUsesTheStacksNamespaceForTheStackAndTheWorktreesForItsConsumers(t *testing.T) {
	w := newSharedWorld(t)
	first := w.context("")
	w.mustUp(t, first)

	otherRoot := w.Worktree("other", "feature/other")
	other := w.Context(otherRoot, acme, ns("dev.other"))
	givePorts(other)
	otherResult := w.mustUp(t, other)
	// Its api container is a consumer of mailpit, labelled under its own namespace.
	for _, id := range w.Docker.Find("container", map[string]string{brand.WorktreeLabel("dev.other"): otherResult.Registration.WorktreeID}) {
		w.Docker.Labels("container", id)[brand.SharedUseLabel("dev.other", "mailpit")] = "true"
	}

	if err := w.sharedUp(first, commands.SharedUpOptions{}); err != nil {
		t.Fatalf("shared up: %v", err)
	}
	network := w.Docker.WorktreeDefaultNetwork(refOf(otherResult))
	if network == "" {
		t.Fatal("the other worktree has no network")
	}
	if got := w.Docker.Aliases(w.mailpit(t, first), network); len(got) != 1 || got[0] != "mailpit" {
		t.Fatalf("the worktree under dev.other was not attached: aliases %v\n%s", got, lines(first))
	}
}

// TestRemoveDisconnectsTheStackUnderTheCommandsNamespaceFromTheWorktreesOwnNetworks
// pins the namespace split of a removal: the shared container is found under
// the namespace of the command that removes, the networks it is detached from
// are proven under the removed worktree's. A shared container attached to a
// worktree labelled under another namespace must not keep its network alive.
func TestRemoveDisconnectsTheStackUnderTheCommandsNamespaceFromTheWorktreesOwnNetworks(t *testing.T) {
	w := newSharedWorld(t)
	first := w.context("")
	w.mustUp(t, first)
	stack := w.mailpit(t, first)

	other := w.Context(w.Worktree("other", "feature/other"), acme, ns("dev.other"))
	givePorts(other)
	otherResult := w.mustUp(t, other)
	network := w.Docker.WorktreeDefaultNetwork(refOf(otherResult))
	if network == "" {
		t.Fatal("the other worktree has no network")
	}
	// The stack started by the default-namespace command is attached to it, as
	// a sweep from that command would have done.
	w.Docker.Attach(stack, network, "mailpit")

	result, err := remove(t, first, approved(commands.RemoveOptions{Identifier: otherResult.Registration.WorktreeID}))
	if err != nil {
		t.Fatalf("remove: %v\n%s", err, lines(first))
	}
	if got := w.Docker.Aliases(stack, network); len(got) != 0 {
		t.Errorf("the shared container is still attached to the removed worktree's network: %v", got)
	}
	if w.Docker.Exists("network", network) {
		t.Errorf("the removed worktree's network survived: %v", result)
	}
	if !w.Docker.Exists("container", stack) {
		t.Error("the shared container was removed with the worktree")
	}
}

// TestRemoveOfAnotherRepositorysWorktreeFindsItsStackUnderTheRecordedNamespace
// pins the other half of the namespace split: a worktree of another
// repository is removed from a checkout whose berth.toml says nothing about
// that repository's stack, so the stack is read under the namespace the
// target recorded, and each side is still proven by its own labels.
func TestRemoveOfAnotherRepositorysWorktreeFindsItsStackUnderTheRecordedNamespace(t *testing.T) {
	w := newSharedWorld(t)
	here := w.context("")
	widgetsRoot := w.Worktree("widgets", "main", filepath.Join(w.Base, "widgets-common"))
	widgets := w.Context(widgetsRoot, "widgets.localhost", ns("dev.widgets"))
	givePorts(widgets)
	port := 8025
	widgets.Loaded.Config.Shared = &config.Shared{
		File:    "compose.shared.yaml",
		Service: []config.SharedService{{Name: "mailpit", Port: &port, Ports: []int{1025}, UsedBy: []string{"api"}}},
	}
	result := w.mustUp(t, widgets)
	stack := w.mailpit(t, widgets)
	network := w.Docker.WorktreeDefaultNetwork(refOf(result))
	if stack == "" || network == "" {
		t.Fatalf("stack %q, network %q", stack, network)
	}
	if got := w.Docker.Aliases(stack, network); len(got) != 1 {
		t.Fatalf("the stack was not connected by up: %v", got)
	}

	removed, err := remove(t, here, approved(commands.RemoveOptions{Identifier: result.Registration.WorktreeID}))
	if err != nil {
		t.Fatalf("remove: %v\n%s", err, lines(here))
	}
	if removed.NetworkKept {
		t.Errorf("the network was kept: the stack was not found under the recorded namespace\n%s", lines(here))
	}
	if got := w.Docker.Aliases(stack, network); len(got) != 0 {
		t.Errorf("the shared container is still attached: %v", got)
	}
	if w.Docker.Exists("network", network) {
		t.Error("the removed worktree's network survived")
	}
	if !w.Docker.Exists("container", stack) {
		t.Error("the shared container was removed with the worktree")
	}
	if out := lines(here); !strings.Contains(out, "shared services are still running (mailpit)") {
		t.Errorf("no hint that the stack still runs:\n%s", out)
	}
}

func TestReconnectSweepSkipsABusyWorktree(t *testing.T) {
	w := newSharedWorld(t)
	first := w.context("")
	w.mustUp(t, first)
	secondRoot := w.Worktree("second", "feature/two")
	second := w.context(secondRoot)
	two := w.mustUp(t, second)
	one := w.mustUp(t, first)
	w.Docker.SetSharedService("mailpit", mailpitDefinition("axllent/mailpit:v2"))

	// The second worktree is busy: something holds its operation lock.
	markers := procid.Markers{Project: "x", WorktreeID: two.Registration.WorktreeID}
	op, _ := lock.WorktreeScopes(w.ControlHome, two.Registration.RepositoryID, two.Registration.WorktreeID, two.Registration.Slug, markers)
	holder := lock.New(op, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
	if _, err := holder.Acquire(context.Background(), "berth up"); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	if err := w.sharedUp(first, commands.SharedUpOptions{Names: []string{"mailpit"}, Recreate: true}); err != nil {
		t.Fatalf("shared up --recreate: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("waited %v on a busy worktree", elapsed)
	}
	id := w.mailpit(t, first)
	networkOne := w.Docker.WorktreeDefaultNetwork(refOf(one))
	networkTwo := w.Docker.WorktreeDefaultNetwork(refOf(two))
	if got := w.Docker.Aliases(id, networkOne); len(got) != 1 {
		t.Errorf("the free worktree was not reconnected: %v", got)
	}
	if got := w.Docker.Aliases(id, networkTwo); len(got) != 0 {
		t.Errorf("the busy worktree was connected anyway: %v", got)
	}
	if out := lines(first); !strings.Contains(out, "Skipped feature-two.acme.localhost: it is busy.") || strings.Contains(out, "Waiting for the") {
		t.Errorf("output:\n%s", out)
	}

	// Free again: the next shared up repairs it, with nothing else changing.
	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}
	if err := w.sharedUp(first, commands.SharedUpOptions{}); err != nil {
		t.Fatalf("shared up: %v", err)
	}
	if got := w.Docker.Aliases(id, networkTwo); len(got) != 1 {
		t.Errorf("the repair did not reconnect the worktree: %v", got)
	}
	if w.mailpit(t, first) != id {
		t.Error("the repair recreated the instance")
	}
}

func TestSharedPortReallocation(t *testing.T) {
	t.Run("is refused while the container runs, proven by labels, and names the way out", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		store := tc.Registry.Store(result.Registration.RepositoryID)
		before, _ := store.FindShared("mailpit")

		err := w.sharedUp(tc, commands.SharedUpOptions{ReallocatePorts: []string{"mailpit:1025"}})
		if err == nil || !strings.Contains(err.Error(), "mailpit is running") || !strings.Contains(err.Error(), "berth shared down mailpit") {
			t.Fatalf("shared up: %v", err)
		}
		after, _ := store.FindShared("mailpit")
		if after.Ports[1025] != before.Ports[1025] {
			t.Fatal("the port moved from under a running container")
		}
	})

	t.Run("moves the port once the service is down, and says so", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		store := tc.Registry.Store(result.Registration.RepositoryID)
		before, _ := store.FindShared("mailpit")
		if err := w.sharedDown(tc, commands.SharedDownOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := w.sharedUp(tc, commands.SharedUpOptions{ReallocatePorts: []string{"mailpit:1025"}}); err != nil {
			t.Fatalf("shared up: %v", err)
		}
		after, _ := store.FindShared("mailpit")
		if after.Ports[1025] == before.Ports[1025] || after.Ports[8025] != before.Ports[8025] {
			t.Fatalf("ports %v -> %v", before.Ports, after.Ports)
		}
		if got := w.Docker.Published(w.mailpit(t, tc), 1025); len(got) != 1 || got[0] != "127.0.0.1:"+itoa(after.Ports[1025]) {
			t.Errorf("publishes %v", got)
		}
		if !strings.Contains(lines(tc), "mailpit's port 1025 moved from 127.0.0.1:"+itoa(before.Ports[1025])) {
			t.Errorf("output:\n%s", lines(tc))
		}
	})

	t.Run("rejects a value that is not <name>:<port> of a declared service", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		for value, want := range map[string]string{
			"mailpit":      "write it as <name>:<container port>",
			"mailpit:smtp": "write it as <name>:<container port>",
			"nope:1025":    "no such shared service",
			"mailpit:9999": "9999 is not one of its container ports. mailpit declares 8025, 1025.",
		} {
			if err := w.sharedUp(tc, commands.SharedUpOptions{ReallocatePorts: []string{value}}); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %v, want %q", value, err, want)
			}
		}
	})
}

func TestSharedDown(t *testing.T) {
	t.Run("stops the container and the stack's network, keeping the volume and the pinned ports", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		resources := w.sharedResources()

		if err := w.sharedDown(tc, commands.SharedDownOptions{}); err != nil {
			t.Fatalf("shared down: %v", err)
		}
		got := w.sharedResources()
		if len(got["container"]) != 0 || len(got["network"]) != 0 || len(got["volume"]) != 1 || got["volume"][0] != resources["volume"][0] {
			t.Fatalf("after: %v", got)
		}
		if record, _ := tc.Registry.Store(result.Registration.RepositoryID).FindShared("mailpit"); record == nil || len(record.Ports) != 2 {
			t.Fatalf("record = %+v", record)
		}
		if !strings.Contains(lines(tc), "1 worktree(s) use mailpit, which is no longer running; 'berth up' or 'berth shared up' starts it again.") {
			t.Errorf("output:\n%s", lines(tc))
		}
		// The worktree's own resources are untouched.
		if left := w.Docker.Owned(refOf(result), brand.LabelNamespace); len(left) == 0 {
			t.Fatal("shared down removed the worktree's resources")
		}
		// The container was stopped before it was removed.
		stop, remove := callIndex(w, 0, "stop "), callIndex(w, 0, "rm --force")
		if stop < 0 || remove < stop {
			t.Errorf("stop at %d, remove at %d", stop, remove)
		}
	})

	t.Run("removes the stack's network even when no container is left, such as after an interrupted stop", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		stack := sharedStackFor(tc, result.Registration.RepositoryID)
		if err := stack.Reap(context.Background(), shared.Resource{Kind: "container", ID: w.mailpit(t, tc)}); err != nil {
			t.Fatal(err)
		}
		if len(w.sharedResources()["network"]) != 1 {
			t.Fatal("the network is not left behind")
		}
		if err := w.sharedDown(tc, commands.SharedDownOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := w.sharedResources(); len(got["network"]) != 0 || len(got["volume"]) != 1 {
			t.Fatalf("after: %v", got)
		}
	})

	t.Run("then up starts it again on the same ports, from the retained volume", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		store := tc.Registry.Store(result.Registration.RepositoryID)
		before, _ := store.FindShared("mailpit")
		if err := w.sharedDown(tc, commands.SharedDownOptions{}); err != nil {
			t.Fatal(err)
		}
		volume := w.sharedResources()["volume"]
		again := w.mustUp(t, tc)
		after, _ := store.FindShared("mailpit")
		if after.Ports[1025] != before.Ports[1025] || after.Ports[8025] != before.Ports[8025] {
			t.Fatalf("ports %v -> %v", before.Ports, after.Ports)
		}
		if got := w.sharedResources()["volume"]; len(got) != 1 || got[0] != volume[0] {
			t.Errorf("volumes = %v, were %v", got, volume)
		}
		network := w.Docker.WorktreeDefaultNetwork(refOf(again))
		if got := w.Docker.Aliases(w.mailpit(t, tc), network); len(got) != 1 {
			t.Errorf("not reconnected after the restart: %v", got)
		}
	})

	t.Run("--volumes asks first, deletes the data, and releases the ports after the volumes are gone", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		store := tc.Registry.Store(result.Registration.RepositoryID)

		err := w.sharedDown(tc, commands.SharedDownOptions{Volumes: true})
		if err == nil || !strings.Contains(err.Error(), `Interactive confirmation required: "remove acme shared services"`) {
			t.Fatalf("without --yes: %v", err)
		}
		if len(w.sharedResources()["container"]) != 1 {
			t.Fatal("it removed something before being confirmed")
		}

		if err := w.sharedDown(tc, commands.SharedDownOptions{Volumes: true, Yes: true}); err != nil {
			t.Fatalf("shared down --volumes: %v", err)
		}
		for kind, ids := range w.sharedResources() {
			if len(ids) != 0 {
				t.Errorf("%s left behind: %v", kind, ids)
			}
		}
		if record, _ := store.FindShared("mailpit"); record != nil {
			t.Errorf("the pinned ports were kept: %+v", record)
		}
		// Containers first, then volumes, then the record: volume rm comes after rm of the container.
		container, volume := callIndex(w, 0, "rm --force"), callIndex(w, 0, "volume rm")
		if container < 0 || volume < container {
			t.Errorf("container removed at %d, volume at %d", container, volume)
		}
		if left := w.Docker.Owned(refOf(result), brand.LabelNamespace); len(left) == 0 {
			t.Fatal("the worktree's resources went too")
		}
	})

	t.Run("works with no berth.toml at all, finding everything by labels", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		bare := downgrade(tc)
		if err := commands.SharedDownWithContext(context.Background(), bare.CommandContext, &commands.Env{CommandName: "berth"}, commands.SharedDownOptions{Volumes: true, Yes: true}); err != nil {
			t.Fatalf("shared down: %v", err)
		}
		for kind, ids := range w.sharedResources() {
			if len(ids) != 0 {
				t.Errorf("%s left behind: %v", kind, ids)
			}
		}
		if record, _ := tc.Registry.Store(result.Registration.RepositoryID).FindShared("mailpit"); record != nil {
			t.Errorf("record = %+v", record)
		}
	})

	t.Run("looks under --label-namespace when there is no berth.toml to say", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.contextNamespace("", ns("dev.example"))
		w.mustUp(t, tc)
		bare := downgrade(tc)
		env := &commands.Env{CommandName: "berth"}
		if err := commands.SharedDownWithContext(context.Background(), bare.CommandContext, env, commands.SharedDownOptions{}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(lines(bare), "No shared services of this repository to stop (Docker label namespace dev.berth)") {
			t.Fatalf("output:\n%s", lines(bare))
		}
		if len(w.Docker.Find("container", map[string]string{"dev.example.shared": "true"})) != 1 {
			t.Fatal("it removed something under another namespace")
		}
		if err := commands.SharedDownWithContext(context.Background(), bare.CommandContext, env, commands.SharedDownOptions{LabelNamespace: "nope"}); err == nil {
			t.Fatal("a malformed namespace was accepted")
		}
		if err := commands.SharedDownWithContext(context.Background(), bare.CommandContext, env, commands.SharedDownOptions{LabelNamespace: "dev.example"}); err != nil {
			t.Fatal(err)
		}
		if len(w.Docker.Find("container", map[string]string{"dev.example.shared": "true"})) != 0 {
			t.Fatal("it did not remove it under the namespace it was told")
		}
	})

	t.Run("a named service takes what only served it along, and leaves a sibling's container", func(t *testing.T) {
		w := newSharedWorld(t)
		w.Docker.SetSharedService("db", map[string]any{"image": "busybox"})
		w.Docker.SetSharedService("mailpit", map[string]any{
			"image": "axllent/mailpit:v1.31.3", "depends_on": map[string]any{"db": map[string]any{}},
			"volumes": []any{map[string]any{"type": "volume", "source": "mailbox", "target": "/data"}},
		})
		w.Docker.SetSharedService("cache", map[string]any{"image": "busybox"})
		tc := w.context("")
		port := 6379
		tc.Loaded.Config.Shared.Service = append(tc.Loaded.Config.Shared.Service, configShared("cache", port, "api"))
		w.mustUp(t, tc)
		if got := len(w.sharedResources()["container"]); got != 3 {
			t.Fatalf("containers = %d, want mailpit, db and cache", got)
		}

		if err := w.sharedDown(tc, commands.SharedDownOptions{Names: []string{"mailpit"}}); err != nil {
			t.Fatal(err)
		}
		var services []string
		for _, id := range w.sharedResources()["container"] {
			services = append(services, w.Docker.Labels("container", id)["com.docker.compose.service"])
		}
		slices.Sort(services)
		if !slices.Equal(services, []string{"cache", "db"}) {
			t.Fatalf("services left = %v, want cache (declared, not named) and db (it may serve cache)", services)
		}

		// With cache gone too, nothing declared is left and the helper goes.
		if err := w.sharedDown(tc, commands.SharedDownOptions{Names: []string{"cache"}}); err != nil {
			t.Fatal(err)
		}
		if got := w.sharedResources()["container"]; len(got) != 0 {
			t.Fatalf("containers left = %v", got)
		}
	})
}

func TestSharedDependencyPorts(t *testing.T) {
	t.Run("a declared service that is only started as a dependency gets its pinned ports published", func(t *testing.T) {
		w := newSharedWorld(t)
		w.Docker.SetSharedService("db", map[string]any{"image": "busybox"})
		w.Docker.SetSharedService("mailpit", map[string]any{
			"image": "axllent/mailpit:v1.31.3", "depends_on": map[string]any{"db": map[string]any{}},
			"volumes": []any{map[string]any{"type": "volume", "source": "mailbox", "target": "/data"}},
		})
		tc := w.context("")
		// db is declared but used by no worktree service, so `up` asks only for mailpit.
		tc.Loaded.Config.Shared.Service = append(tc.Loaded.Config.Shared.Service, config.SharedService{Name: "db", Ports: []int{5432}})
		result := w.mustUp(t, tc)

		record, err := tc.Registry.Store(result.Registration.RepositoryID).FindShared("db")
		if err != nil || record == nil || record.Ports[5432] == 0 {
			t.Fatalf("db record = %+v, %v", record, err)
		}
		id := w.Docker.ContainerNamed(w.project(t, tc) + "-db-1")
		want := fmt.Sprintf("127.0.0.1:%d", record.Ports[5432])
		if got := w.Docker.Published(id, 5432); !slices.Equal(got, []string{want}) {
			t.Fatalf("db publishes %v, want [%s]", got, want)
		}
	})
}

// configShared is a shared service used by one worktree service, with one port.
func configShared(name string, port int, usedBy ...string) config.SharedService {
	return config.SharedService{Name: name, Ports: []int{port}, UsedBy: usedBy}
}

func TestStatusReportsSharedServices(t *testing.T) {
	ctx := context.Background()
	shared := func(t *testing.T, tc *commandstest.TestContext) []commands.SharedStatusEntry {
		t.Helper()
		tc.Logger.Lines, tc.Logger.Printed = nil, nil
		if _, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{JSON: true}); err != nil {
			t.Fatal(err)
		}
		_, printed := tc.Logger.Snapshot()
		var report commands.StatusReport
		if err := json.Unmarshal([]byte(strings.Join(printed, "\n")), &report); err != nil {
			t.Fatalf("status --json is not JSON: %v\n%v", err, printed)
		}
		return report.Shared
	}

	t.Run("status --json gains a shared array with state, addresses, definition and connection", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		record, _ := tc.Registry.Store(result.Registration.RepositoryID).FindShared("mailpit")

		got := shared(t, tc)
		if len(got) != 1 {
			t.Fatalf("shared = %+v", got)
		}
		entry := got[0]
		if entry.Name != "mailpit" || entry.State != "running" || entry.Definition != "current" || !entry.Connected || entry.DefinedBy != "feature-login.acme.localhost" {
			t.Errorf("entry = %+v", entry)
		}
		want := []commands.SharedStatusAddr{
			{Port: 8025, Addr: "127.0.0.1:" + itoa(record.Ports[8025])},
			{Port: 1025, Addr: "127.0.0.1:" + itoa(record.Ports[1025])},
		}
		if !slices.Equal(entry.Addrs, want) {
			t.Errorf("addrs = %+v, want %+v", entry.Addrs, want)
		}
	})

	t.Run("a failed shared lookup is a warning: the worktree's own services are reported and shared is left out", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		w.Docker.FailCall("label="+brand.SharedLabel(brand.LabelNamespace), "Cannot connect to the Docker daemon", 1, -1)

		tc.Logger.Lines, tc.Logger.Printed = nil, nil
		statuses, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{JSON: true})
		if err != nil {
			t.Fatalf("status failed over the shared lookup: %v", err)
		}
		if len(statuses) == 0 {
			t.Error("the worktree's own services were not reported")
		}
		_, printed := tc.Logger.Snapshot()
		var report commands.StatusReport
		if err := json.Unmarshal([]byte(strings.Join(printed, "\n")), &report); err != nil {
			t.Fatalf("status --json is not JSON: %v\n%v", err, printed)
		}
		if !report.Registered || len(report.Services) == 0 || report.Shared != nil {
			t.Errorf("report = %+v", report)
		}
		if out := lines(tc); !strings.Contains(out, "Could not read the shared services' state, so they are left out") {
			t.Errorf("no warning:\n%s", out)
		}
	})

	t.Run("says differs, not connected, stopped and absent as each becomes true", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		id := w.mailpit(t, tc)

		w.Docker.SetSharedService("mailpit", mailpitDefinition("axllent/mailpit:v2"))
		if got := shared(t, tc)[0]; got.Definition != "differs" {
			t.Errorf("definition = %q", got.Definition)
		}
		if err := w.Docker.Detach(id, w.Docker.WorktreeDefaultNetwork(refOf(result))); err != nil {
			t.Fatal(err)
		}
		if got := shared(t, tc)[0]; got.Connected {
			t.Errorf("connected after the attachment went")
		}
		w.Docker.SetStatus(id, "exited")
		if got := shared(t, tc)[0]; got.State != "stopped" || got.Definition != "unknown" || got.DefinedBy != "" || got.Connected {
			t.Errorf("stopped: %+v", got)
		}
		if err := w.sharedDown(tc, commands.SharedDownOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := shared(t, tc)[0]; got.State != "absent" || len(got.Addrs) != 2 {
			t.Errorf("absent: %+v (the pinned ports remain)", got)
		}
	})

	t.Run("is left out of status --json when berth.toml declares no shared services", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace, "api")
		tc := w.Context("", acme, nil)
		if _, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{JSON: true}); err != nil {
			t.Fatal(err)
		}
		_, printed := tc.Logger.Snapshot()
		if strings.Contains(strings.Join(printed, ""), "shared") {
			t.Fatalf("output:\n%v", printed)
		}
	})

	t.Run("the text form lists them under the services", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		record, _ := tc.Registry.Store(result.Registration.RepositoryID).FindShared("mailpit")
		tc.Logger.Lines, tc.Logger.Printed = nil, nil
		if _, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{}); err != nil {
			t.Fatal(err)
		}
		_, printed := tc.Logger.Snapshot()
		want := []string{
			"  shared services",
			"    mailpit  running  (definition current, connected, started by feature-login.acme.localhost)",
			"      container port 8025 -> 127.0.0.1:" + itoa(record.Ports[8025]),
			"      container port 1025 -> 127.0.0.1:" + itoa(record.Ports[1025]),
		}
		if len(printed) < len(want) || !slices.Equal(printed[len(printed)-len(want):], want) {
			t.Fatalf("printed = %q", printed)
		}
	})

	t.Run("shared status prints the same, as text and as JSON", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		tc.Logger.Lines, tc.Logger.Printed = nil, nil
		if err := commands.SharedStatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{JSON: true}); err != nil {
			t.Fatal(err)
		}
		_, printed := tc.Logger.Snapshot()
		var report commands.SharedStatusReport
		if err := json.Unmarshal([]byte(strings.Join(printed, "\n")), &report); err != nil || len(report.Shared) != 1 || report.Shared[0].Name != "mailpit" {
			t.Fatalf("report = %+v, %v\n%v", report, err, printed)
		}
		tc.Logger.Lines, tc.Logger.Printed = nil, nil
		if err := commands.SharedStatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{}); err != nil {
			t.Fatal(err)
		}
		_, printed = tc.Logger.Snapshot()
		if len(printed) == 0 || !strings.HasPrefix(printed[0], "mailpit  running  (definition current, connected") {
			t.Fatalf("printed = %q", printed)
		}
	})

	t.Run("shared status with no shared services says so, and prints an empty array as JSON", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		if err := commands.SharedStatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{JSON: true}); err != nil {
			t.Fatal(err)
		}
		_, printed := tc.Logger.Snapshot()
		if !strings.Contains(strings.Join(printed, ""), `"shared": []`) {
			t.Fatalf("printed = %q", printed)
		}
	})
}

func TestUnlockShared(t *testing.T) {
	setup := func(t *testing.T) (*sharedWorld, *commandstest.TestContext, lock.Scope) {
		w := newSharedWorld(t)
		tc := w.context("")
		result := w.mustUp(t, tc)
		return w, tc, lock.SharedScope(w.ControlHome, result.Registration.RepositoryID)
	}
	unlockShared := func(tc *commandstest.TestContext, options commands.UnlockOptions) (*commands.UnlockResult, error) {
		options.Shared = true
		return commands.UnlockSharedWithContext(context.Background(), tc.CommandContext, options)
	}

	t.Run("says there is nothing to unlock when nothing holds the shared lock", func(t *testing.T) {
		_, tc, _ := setup(t)
		tc.Logger.Lines, tc.Logger.Printed = nil, nil
		result, err := unlockShared(tc, commands.UnlockOptions{})
		if err != nil || len(result.Cleared) != 0 {
			t.Fatalf("result = %+v, %v", result, err)
		}
		if got := tc.Logger.Lines; !equalLines(got, []string{"info: Nothing to unlock: no one holds the shared-services lock of feature-login.acme.localhost."}) {
			t.Fatalf("lines = %v", got)
		}
	})

	t.Run("says so for a repository that never took the lock", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		w.Register("", acme, "feature-login", brand.LabelNamespace)
		tc := w.Context("", acme, nil)
		if _, err := unlockShared(tc, commands.UnlockOptions{}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(tc.Logger.Lines, "\n"), "Nothing to unlock") {
			t.Fatalf("lines = %v", tc.Logger.Lines)
		}
	})

	t.Run("refuses a live holder", func(t *testing.T) {
		_, tc, scope := setup(t)
		holder := lock.New(scope, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
		if _, err := holder.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		defer holder.Release()
		_, err := unlockShared(tc, commands.UnlockOptions{})
		wantErr(t, err, `^The shared-services lock is held, so there is nothing to unlock\.`)
	})

	t.Run("clears the lock of a holder proven gone, through the same Clear checks, and nothing else", func(t *testing.T) {
		_, tc, scope := setup(t)
		lk := lock.New(scope, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
		if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		if err := lk.Release(); err != nil {
			t.Fatal(err)
		}
		mutateRecord(t, scope, func(rec map[string]any) {
			rec["pid"] = deadPID
			delete(rec, "releasedAt")
		})
		tc.Logger.Lines, tc.Logger.Printed = nil, nil

		result, err := unlockShared(tc, commands.UnlockOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Cleared) != 1 || result.Cleared[0].Kind != "shared" || result.Cleared[0].Target != scope.Target {
			t.Fatalf("cleared = %+v", result.Cleared)
		}
		out := strings.Join(tc.Logger.Lines, "\n")
		if !strings.Contains(out, "Checking that the holder of the shared-services lock of feature-login.acme.localhost, and every command it started on this machine, is gone.") ||
			!strings.Contains(out, "Unlocked the shared-services lock of feature-login.acme.localhost.") {
			t.Fatalf("output:\n%s", out)
		}
		// The shared lock works again.
		again := lock.New(scope, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
		if _, err := again.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatalf("the lock is still stuck: %v", err)
		}
		_ = again.Release()
	})

	t.Run("refuses a holder it cannot check without --holder-unverifiable", func(t *testing.T) {
		_, tc, scope := setup(t)
		lk := lock.New(scope, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
		if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		_ = lk.Release()
		mutateRecord(t, scope, func(rec map[string]any) {
			rec["pid"] = deadPID
			rec["machineId"] = "another-machine"
			delete(rec, "releasedAt")
		})
		if _, err := unlockShared(tc, commands.UnlockOptions{}); err == nil {
			t.Fatal("a holder on another machine was cleared on no evidence")
		}
		result, err := unlockShared(tc, commands.UnlockOptions{AllowUnverifiable: true})
		if err != nil || len(result.Cleared) != 1 {
			t.Fatalf("result = %+v, %v", result, err)
		}
	})

	t.Run("runs where berth.toml is broken, since recovery needs only labels and records", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		file := filepath.Join(dir, "berth.toml")
		if err := os.WriteFile(file, []byte("not = [valid"), 0o644); err != nil {
			t.Fatal(err)
		}
		home := registrytest.TempDir(t)
		getenv := func(key string) (string, bool) {
			switch key {
			case "BERTH_HOME":
				return filepath.Join(home, "berth"), true
			case "HOME":
				return filepath.Join(home, "user"), true
			}
			return "", false
		}
		env := &commands.Env{CommandName: "berth", Logger: &logger.Recording{}, Getenv: getenv, Cwd: dir, ConfigPath: file, Runner: noRunner{t}}
		err := commands.Unlock(context.Background(), env, commands.UnlockOptions{Shared: true, Identifier: "nosuch"})
		wantErr(t, err, `^No worktree is registered as nosuch\.`)
	})

	t.Run("cannot be combined with --registry or --router", func(t *testing.T) {
		err := commands.Unlock(context.Background(), &commands.Env{CommandName: "berth"}, commands.UnlockOptions{Shared: true, Registry: true})
		wantErr(t, err, `--shared cannot be combined with --registry or --router`)
	})
}

func TestRemovingTheLastWorktreeHintsAtSharedDown(t *testing.T) {
	hint := "its shared services are still running (mailpit). 'berth shared down' stops them"
	t.Run("when shared services are still running", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		tc.Logger.Lines, tc.Logger.Printed = nil, nil
		if _, err := remove(t, tc, approved(commands.RemoveOptions{})); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(tc.Logger.Lines, "\n"), hint) {
			t.Fatalf("output:\n%s", strings.Join(tc.Logger.Lines, "\n"))
		}
	})

	t.Run("looks for the stack under the removing command's namespace, not the removed worktree's", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		// The stack is labelled under dev.other, as a command with that
		// namespace would have started it; the worktree is under the default.
		id := w.mailpit(t, tc)
		labels := w.Docker.Labels("container", id)
		for key, value := range maps.Clone(labels) {
			if rest, ok := strings.CutPrefix(key, brand.LabelNamespace+"."); ok {
				labels["dev.other."+rest] = value
				delete(labels, key)
			}
		}
		remover := w.Context("", acme, ns("dev.other"))
		remover.Logger.Lines, remover.Logger.Printed = nil, nil
		if _, err := remove(t, remover, approved(commands.RemoveOptions{})); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(remover.Logger.Lines, "\n"), hint) {
			t.Fatalf("output:\n%s", strings.Join(remover.Logger.Lines, "\n"))
		}
	})

	t.Run("not while another worktree is registered", func(t *testing.T) {
		w := newSharedWorld(t)
		first := w.context("")
		w.mustUp(t, first)
		w.mustUp(t, w.context(w.Worktree("second", "feature/two")))
		first.Logger.Lines, first.Logger.Printed = nil, nil
		if _, err := remove(t, first, approved(commands.RemoveOptions{})); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.Join(first.Logger.Lines, "\n"), "shared down") {
			t.Fatalf("output:\n%s", strings.Join(first.Logger.Lines, "\n"))
		}
	})

	t.Run("not when no shared service runs, and a Docker failure never fails the removal", func(t *testing.T) {
		w := newSharedWorld(t)
		tc := w.context("")
		w.mustUp(t, tc)
		if err := w.sharedDown(tc, commands.SharedDownOptions{}); err != nil {
			t.Fatal(err)
		}
		tc.Logger.Lines, tc.Logger.Printed = nil, nil
		if _, err := remove(t, tc, approved(commands.RemoveOptions{})); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.Join(tc.Logger.Lines, "\n"), "shared down") {
			t.Fatalf("output:\n%s", strings.Join(tc.Logger.Lines, "\n"))
		}

		other := newSharedWorld(t)
		otc := other.context("")
		other.mustUp(t, otc)
		// The two disconnects before the first reap each list the shared containers; the hint's probe is the third.
		other.Docker.FailCallAfter("label=dev.berth.shared=true", 2, "Error response from daemon: i/o timeout", 1, -1)
		if _, err := remove(t, otc, approved(commands.RemoveOptions{})); err != nil {
			t.Fatalf("a failed probe failed the removal: %v", err)
		}
	})
}
