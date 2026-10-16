package commands_test

// Loopback ports (#119): what `up`, `worktree remove`, `urls`, `status` and
// `run` do with the port each routed service is allocated.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/commands/commandstest"
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/worktree"
)

// portsWorld is a fresh world whose config routes api and mail on port 8080,
// with a fake router and index, ready for `up`.
type portsWorld struct {
	*commandstest.World
	tc     *commandstest.TestContext
	router *commandstest.FakeRouter
	index  *commandstest.FakeIndex
}

// newPortsWorld is a world for feature/login that has not been brought up.
func newPortsWorld(t *testing.T) *portsWorld {
	t.Helper()
	w := commandstest.New(t, "feature/login", acme)
	tc := w.Context("", acme, nil)
	givePorts(tc)
	return &portsWorld{World: w, tc: tc, router: commandstest.NewFakeRouter(80), index: &commandstest.FakeIndex{}}
}

// up runs `up` for pw's worktree.
func (pw *portsWorld) up(t *testing.T, options commands.UpOptions) (*commands.UpResult, error) {
	t.Helper()
	return up(t, pw.tc, pw.router, pw.index, options)
}

// recordPorts are the ports recorded for the worktree `up` registered.
func (pw *portsWorld) recordPorts(t *testing.T) map[string]int {
	t.Helper()
	current, err := pw.tc.Registry.Current(context.Background())
	if err != nil || current == nil {
		t.Fatalf("Current: %v, %v", current, err)
	}
	return current.Ports
}

// ref is the worktree `up` registered.
func (pw *portsWorld) ref(t *testing.T) worktree.Ref {
	t.Helper()
	current, err := pw.tc.Registry.Current(context.Background())
	if err != nil || current == nil {
		t.Fatalf("Current: %v, %v", current, err)
	}
	return worktree.Ref{RepositoryID: current.RepositoryID, WorktreeID: current.WorktreeID, Slug: current.Slug}
}

// overrideBytes are the override file the last `up` wrote.
func (pw *portsWorld) overrideBytes(t *testing.T) []byte {
	t.Helper()
	ref := pw.ref(t)
	data, err := os.ReadFile(filepath.Join(layout.WorktreeLayout(pw.ControlHome, ref.RepositoryID, ref.WorktreeID).Home, "treerig.override.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// printedText is everything the command wrote to stdout.
func printedText(tc *commandstest.TestContext) string {
	_, printed := tc.Logger.Snapshot()
	return strings.Join(printed, "\n")
}

func TestUpAllocatesLoopbackPorts(t *testing.T) {
	t.Run("allocates a port per routed service inside the range and records it", func(t *testing.T) {
		pw := newPortsWorld(t)
		result, err := pw.up(t, commands.UpOptions{})
		if err != nil {
			t.Fatal(err)
		}
		ports := pw.recordPorts(t)
		if len(ports) != 2 || ports["api"] == ports["mail"] {
			t.Fatalf("ports = %v", ports)
		}
		for service, port := range ports {
			if !registrytest.PortRange.Contains(port) {
				t.Errorf("%s: %d is outside %v", service, port, registrytest.PortRange)
			}
		}
		for _, entry := range result.URLs {
			if entry.Port != ports[entry.Service] {
				t.Errorf("URL entry %+v does not carry port %d", entry, ports[entry.Service])
			}
		}
	})

	t.Run("publishes each routed service on 127.0.0.1 at its port, and writes the same override on a second up", func(t *testing.T) {
		pw := newPortsWorld(t)
		if _, err := pw.up(t, commands.UpOptions{}); err != nil {
			t.Fatal(err)
		}
		ports := pw.recordPorts(t)
		first := pw.overrideBytes(t)
		for _, service := range []string{"api", "mail"} {
			want := `"published": "` + strconv.Itoa(ports[service]) + `"`
			if !bytes.Contains(first, []byte(want)) {
				t.Fatalf("the override does not publish %s at %s:\n%s", service, want, first)
			}
		}
		if !bytes.Contains(first, []byte(`"host_ip": "127.0.0.1"`)) || !bytes.Contains(first, []byte(`"target": 8080`)) {
			t.Fatalf("the override does not target the routed port on loopback:\n%s", first)
		}

		if _, err := pw.up(t, commands.UpOptions{}); err != nil {
			t.Fatal(err)
		}
		if second := pw.overrideBytes(t); !bytes.Equal(first, second) {
			t.Fatalf("a second up changed the override, which would recreate containers:\n%s\n---\n%s", first, second)
		}
		if again := pw.recordPorts(t); !reflect.DeepEqual(again, ports) {
			t.Fatalf("ports changed across ups: %v then %v", ports, again)
		}
	})

	t.Run("gives hooks TREERIG_SERVICE_PORT_<NAME> beside the unchanged URL variables", func(t *testing.T) {
		pw := newPortsWorld(t)
		pw.tc.Loaded.Config.Hooks = config.Hooks{Prepare: ns("./hooks/prepare")}
		var mu sync.Mutex
		var hookEnv []string
		fallback := pw.Runner.Handler
		pw.Runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
			if strings.HasSuffix(spec.Command, "/hooks/prepare") {
				mu.Lock()
				hookEnv = spec.Env
				mu.Unlock()
				return run.Result{}, nil
			}
			return fallback(ctx, spec)
		}
		if _, err := pw.up(t, commands.UpOptions{}); err != nil {
			t.Fatal(err)
		}
		ports := pw.recordPorts(t)
		mu.Lock()
		defer mu.Unlock()
		if !containsEnv(hookEnv, "TREERIG_SERVICE_PORT_API", strconv.Itoa(ports["api"])) ||
			!containsEnv(hookEnv, "TREERIG_SERVICE_PORT_MAIL", strconv.Itoa(ports["mail"])) {
			t.Errorf("hook env lacks the ports %v: %v", ports, hookEnv)
		}
		if !containsEnv(hookEnv, "TREERIG_URL", "http://feature-login.acme.localhost") ||
			!containsEnv(hookEnv, "TREERIG_SERVICE_URL_MAIL", "http://mail.feature-login.acme.localhost") {
			t.Errorf("the URL variables changed: %v", hookEnv)
		}
	})

	t.Run("allocates before anything starts, and before the router is asked for a network", func(t *testing.T) {
		pw := newPortsWorld(t)
		pw.Probe.Occupy(registrytest.PortRange.Low)
		_, err := pw.up(t, commands.UpOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for service, port := range pw.recordPorts(t) {
			if port == registrytest.PortRange.Low {
				t.Errorf("%s took %d, which the probe reports busy", service, port)
			}
		}
	})

	t.Run("refuses a routed service that needs a port and cannot have one, before starting anything", func(t *testing.T) {
		pw := newPortsWorld(t)
		// Two ports in a one-port range: the second service cannot be allocated.
		pw.tc.PortRange = layout.PortRange{Low: 30500, High: 30500}
		_, err := pw.up(t, commands.UpOptions{})
		wantErr(t, err, "No free port for mail in 30500-30500")
		for _, call := range pw.Docker.Calls() {
			if strings.HasPrefix(call, "compose ") && strings.Contains(call, " up ") {
				t.Fatalf("compose up ran after the allocation failed: %s", call)
			}
		}
	})

	t.Run("releases the port of a service removed from treerig.toml", func(t *testing.T) {
		pw := newPortsWorld(t)
		if _, err := pw.up(t, commands.UpOptions{}); err != nil {
			t.Fatal(err)
		}
		before := pw.recordPorts(t)
		pw.tc.Loaded.Config.Service = pw.tc.Loaded.Config.Service[:1] // api only
		if _, err := pw.up(t, commands.UpOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := pw.recordPorts(t); !reflect.DeepEqual(got, map[string]int{"api": before["api"]}) {
			t.Fatalf("ports after dropping mail = %v, was %v", got, before)
		}
	})

	t.Run("keeps a dropped service's port until a full up has succeeded", func(t *testing.T) {
		pw := newPortsWorld(t)
		if _, err := pw.up(t, commands.UpOptions{}); err != nil {
			t.Fatal(err)
		}
		before := pw.recordPorts(t)
		pw.tc.Loaded.Config.Service = pw.tc.Loaded.Config.Service[:1] // api only

		// A subset up never removes the orphan, so nothing is released.
		if _, err := pw.up(t, commands.UpOptions{Services: []string{"api"}}); err != nil {
			t.Fatal(err)
		}
		if got := pw.recordPorts(t); got["mail"] != before["mail"] {
			t.Fatalf("a subset up released mail's port: %v", got)
		}
		// A failed full up doesn't either.
		pw.Docker.FailUp("pull access denied", nil)
		if _, err := pw.up(t, commands.UpOptions{}); err == nil {
			t.Fatal("expected the failed up to fail")
		}
		if got := pw.recordPorts(t); got["mail"] != before["mail"] {
			t.Fatalf("a failed up released mail's port: %v", got)
		}
	})

	t.Run("refuses a service that cannot publish a port before recording one", func(t *testing.T) {
		pw := newPortsWorld(t)
		pw.Docker.SetServiceConfig("api", "network_mode", "host")
		_, err := pw.up(t, commands.UpOptions{})
		wantErr(t, err, `Service api in .*compose\.yaml uses network_mode: host`)
		if got := pw.recordPorts(t); len(got) != 0 {
			t.Fatalf("a port was recorded for a service that cannot publish it: %v", got)
		}
	})

	t.Run("routes nothing and records no ports for a config without [service.<name>] tables", func(t *testing.T) {
		pw := newPortsWorld(t)
		pw.tc.Loaded.Config.Service = nil
		if _, err := pw.up(t, commands.UpOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := pw.recordPorts(t); len(got) != 0 {
			t.Fatalf("ports = %v", got)
		}
	})
}

func TestUpRefusesAHeldPort(t *testing.T) {
	// firstUp brings the worktree up once and returns its api port, so a later
	// up finds a recorded port to check.
	firstUp := func(t *testing.T) (*portsWorld, int) {
		t.Helper()
		pw := newPortsWorld(t)
		if _, err := pw.up(t, commands.UpOptions{}); err != nil {
			t.Fatal(err)
		}
		return pw, pw.recordPorts(t)["api"]
	}
	composeUps := func(pw *portsWorld) int {
		count := 0
		for _, call := range pw.Docker.Calls() {
			if strings.HasPrefix(call, "compose ") && strings.Contains(call, " up ") {
				count++
			}
		}
		return count
	}

	t.Run("names a container that is not this worktree's, found by its labels", func(t *testing.T) {
		pw, port := firstUp(t)
		// Named like this worktree's own containers, but carrying no treerig labels.
		pw.Docker.PublishForeign("acme-api-1", port)
		pw.Probe.Occupy(port)
		before := composeUps(pw)

		_, err := pw.up(t, commands.UpOptions{})
		wantErr(t, err, `api: 127\.0\.0\.1:`+strconv.Itoa(port)+` is held by container acme-api-1`)
		wantErr(t, err, `--reallocate-port <service>`)
		if composeUps(pw) != before {
			t.Fatal("compose up ran although a pinned port is held")
		}
	})

	t.Run("names another treerig worktree's container as such", func(t *testing.T) {
		pw, port := firstUp(t)
		other := worktree.Ref{RepositoryID: "99999999-9999-4999-8999-999999999999", WorktreeID: "88888888-8888-4888-8888-888888888888", Slug: "other"}
		pw.Docker.PublishManaged(other, brand.LabelNamespace, "other-api-1", port)
		pw.Probe.Occupy(port)
		_, err := pw.up(t, commands.UpOptions{})
		wantErr(t, err, `held by container other-api-1 \(belongs to another treerig worktree\)`)
	})

	t.Run("names another worktree that records the port", func(t *testing.T) {
		pw, port := firstUp(t)
		pw.Docker.PublishForeign("squatter", port)
		pw.Probe.Occupy(port)
		if _, err := registrytest.WriteRecord(pw.ControlHome, registrytest.RecordJSON{
			Version: 1, RepositoryID: "77777777-7777-4777-8777-777777777777", WorktreeID: "66666666-6666-4666-8666-666666666666",
			Slug: "elsewhere", Domain: acme, Initialization: "complete", Ports: map[string]int{"db": port},
		}); err != nil {
			t.Fatal(err)
		}
		_, err := pw.up(t, commands.UpOptions{})
		wantErr(t, err, `worktree elsewhere\.acme\.localhost \(its db service\)`)
	})

	t.Run("says another process when no container publishes the busy port, with how to look it up on this OS", func(t *testing.T) {
		for os, hint := range map[string]string{"linux": "ss -ltnp 'sport = :%d'", "darwin": "lsof -nP -iTCP:%d -sTCP:LISTEN"} {
			pw, port := firstUp(t)
			pw.tc.OS = os
			pw.Probe.Occupy(port)
			_, err := pw.up(t, commands.UpOptions{})
			wantErr(t, err, `api: 127\.0\.0\.1:`+strconv.Itoa(port)+` is held by another process \(find it with: `)
			if !strings.Contains(err.Error(), fmt.Sprintf(hint, port)) {
				t.Fatalf("%s: err = %v, want the hint %q", os, err, fmt.Sprintf(hint, port))
			}
		}
	})

	t.Run("ignores a record that names the port when this worktree's own container publishes it", func(t *testing.T) {
		pw, port := firstUp(t)
		pw.Docker.Publish(pw.ref(t), brand.LabelNamespace, "api", "acme-api-1", port)
		pw.Probe.Occupy(port)
		if _, err := registrytest.WriteRecord(pw.ControlHome, registrytest.RecordJSON{
			Version: 1, RepositoryID: "77777777-7777-4777-8777-777777777777", WorktreeID: "66666666-6666-4666-8666-666666666666",
			Slug: "elsewhere", Domain: acme, Initialization: "complete", Ports: map[string]int{"db": port},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := pw.up(t, commands.UpOptions{}); err != nil {
			t.Fatalf("an up over its own container: %v", err)
		}
	})

	t.Run("checks only the services it was asked to start", func(t *testing.T) {
		pw, port := firstUp(t)
		pw.Probe.Occupy(port) // api's port; mail is the only one started
		if _, err := pw.up(t, commands.UpOptions{Services: []string{"mail"}}); err != nil {
			t.Fatalf("starting mail over a held api port: %v", err)
		}
		_, err := pw.up(t, commands.UpOptions{Services: []string{"api"}})
		wantErr(t, err, `api: 127\.0\.0\.1:`+strconv.Itoa(port)+` is held`)
	})

	t.Run("goes ahead when the holder is this worktree's own container", func(t *testing.T) {
		pw, port := firstUp(t)
		pw.Docker.Publish(pw.ref(t), brand.LabelNamespace, "api", "acme-api-1", port)
		pw.Probe.Occupy(port)
		if _, err := pw.up(t, commands.UpOptions{}); err != nil {
			t.Fatalf("an up over its own running container: %v", err)
		}
	})

	t.Run("re-probes after a failed compose up and names who took the port meanwhile", func(t *testing.T) {
		pw, port := firstUp(t)
		pw.Docker.FailUp("Bind for 127.0.0.1:"+strconv.Itoa(port)+" failed: port is already allocated", func() {
			pw.Docker.PublishForeign("rival", port)
			pw.Probe.Occupy(port)
		})
		_, err := pw.up(t, commands.UpOptions{})
		wantErr(t, err, `api: 127\.0\.0\.1:`+strconv.Itoa(port)+` is held by container rival`)
		var refusal *clierr.Error
		if !errors.As(err, &refusal) || refusal.Cause == nil || !strings.Contains(refusal.Cause.Error(), "Bind for") {
			t.Fatalf("the refusal does not keep Compose's error as its cause: %#v", err)
		}
	})

	t.Run("asks Docker who publishes each pinned port after a failed start even when the probe says free", func(t *testing.T) {
		pw, port := firstUp(t)
		// With the userland proxy off a published port can be invisible to a bind probe.
		pw.Docker.FailUp("driver failed programming external connectivity", func() {
			pw.Docker.PublishForeign("rival", port)
		})
		_, err := pw.up(t, commands.UpOptions{})
		wantErr(t, err, `api: 127\.0\.0\.1:`+strconv.Itoa(port)+` is held by container rival`)
	})

	t.Run("returns a compose failure no held port explains as it was", func(t *testing.T) {
		pw, _ := firstUp(t)
		pw.Docker.FailUp("pull access denied", nil)
		_, err := pw.up(t, commands.UpOptions{})
		if err == nil || strings.Contains(err.Error(), "pinned loopback port") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestUpReallocatePort(t *testing.T) {
	t.Run("gives the service a new port and says the loopback URL changed", func(t *testing.T) {
		pw := newPortsWorld(t)
		if _, err := pw.up(t, commands.UpOptions{}); err != nil {
			t.Fatal(err)
		}
		before := pw.recordPorts(t)
		pw.Docker.PublishForeign("squatter", before["api"])
		pw.Probe.Occupy(before["api"])

		if _, err := pw.up(t, commands.UpOptions{ReallocatePorts: []string{"api"}}); err != nil {
			t.Fatal(err)
		}
		after := pw.recordPorts(t)
		if after["api"] == before["api"] || after["mail"] != before["mail"] {
			t.Fatalf("ports were %v, now %v", before, after)
		}
		lines, _ := pw.tc.Logger.Snapshot()
		want := "api's loopback port changed from " + strconv.Itoa(before["api"]) + " to " + strconv.Itoa(after["api"])
		found := false
		for _, line := range lines {
			found = found || strings.Contains(line, want)
		}
		if !found {
			t.Fatalf("no line says %q in %v", want, lines)
		}
	})

	t.Run("refuses while this worktree's own container holds the port, moving nothing", func(t *testing.T) {
		pw := newPortsWorld(t)
		if _, err := pw.up(t, commands.UpOptions{}); err != nil {
			t.Fatal(err)
		}
		before := pw.recordPorts(t)
		pw.Docker.Publish(pw.ref(t), brand.LabelNamespace, "api", "acme-api-1", before["api"])
		pw.Probe.Occupy(before["api"])

		_, err := pw.up(t, commands.UpOptions{ReallocatePorts: []string{"api"}})
		wantErr(t, err, `api's port `+strconv.Itoa(before["api"])+` is held by this worktree's own container acme-api-1`)
		if after := pw.recordPorts(t); !reflect.DeepEqual(after, before) {
			t.Fatalf("a refused reallocation changed the ports: %v then %v", before, after)
		}
	})

	t.Run("does not mistake a container that is merely named like ours for it", func(t *testing.T) {
		pw := newPortsWorld(t)
		if _, err := pw.up(t, commands.UpOptions{}); err != nil {
			t.Fatal(err)
		}
		before := pw.recordPorts(t)
		pw.Docker.PublishForeign("acme-api-1", before["api"])
		pw.Probe.Occupy(before["api"])
		if _, err := pw.up(t, commands.UpOptions{ReallocatePorts: []string{"api"}}); err != nil {
			t.Fatalf("a foreign container is no reason to refuse: %v", err)
		}
	})

	t.Run("refuses a service that is not routed, before anything else", func(t *testing.T) {
		pw := newPortsWorld(t)
		_, err := pw.up(t, commands.UpOptions{ReallocatePorts: []string{"ghost"}})
		wantErr(t, err, `--reallocate-port ghost: no such routed service\. treerig\.toml routes api, mail\.`)
		if pw.router.Ensures() != 0 {
			t.Fatal("the router was ensured before the flag was checked")
		}
	})

	t.Run("allocates a service that had no port yet", func(t *testing.T) {
		pw := newPortsWorld(t)
		if _, err := pw.up(t, commands.UpOptions{ReallocatePorts: []string{"api"}}); err != nil {
			t.Fatal(err)
		}
		if got := pw.recordPorts(t); got["api"] == 0 {
			t.Fatalf("ports = %v", got)
		}
	})
}

func TestWorktreeRemoveReleasesPorts(t *testing.T) {
	t.Run("keeps the ports while the worktree is not fully gone, and drops them with the record", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api", "mail")
		w.SetPorts(ref, map[string]int{"api": 30010, "mail": 30011})
		tc := w.Context("", acme, nil)
		store := registry.NewStore(w.ControlHome, ref.RepositoryID)

		// A network the router will not let go of keeps the worktree registered.
		w.Docker.Add("network", commandstest.Ownership(ref, brand.LabelNamespace))
		w.Docker.FailNetworkRemove(1, "network has active endpoints")
		result, err := remove(t, tc, approved(commands.RemoveOptions{}))
		if err != nil || !result.NetworkKept {
			t.Fatalf("Remove = %+v, %v", result, err)
		}
		record, err := store.Find(ref.WorktreeID)
		if err != nil || record == nil || !reflect.DeepEqual(record.Ports, map[string]int{"api": 30010, "mail": 30011}) {
			t.Fatalf("a removal that kept a network released the ports: %+v, %v", record, err)
		}

		w.Docker.FailNetworkRemove(0, "")
		if _, err := remove(t, tc, approved(commands.RemoveOptions{})); err != nil {
			t.Fatal(err)
		}
		if record, err := store.Find(ref.WorktreeID); err != nil || record != nil {
			t.Fatalf("the record survived a finished removal: %+v, %v", record, err)
		}
	})
}

func TestPortReporting(t *testing.T) {
	ctx := context.Background()
	registered := func(t *testing.T) (*commandstest.World, *commandstest.TestContext, worktree.Ref) {
		t.Helper()
		w := commandstest.New(t, "feature/login", acme)
		ref := w.Register("", acme, "feature-login", brand.LabelNamespace, "api", "mail")
		w.SetPorts(ref, map[string]int{"api": 30010})
		tc := w.Context("", acme, nil)
		givePorts(tc)
		return w, tc, ref
	}

	t.Run("urls --json adds a port key for a service with one, and the URLs are unchanged", func(t *testing.T) {
		_, tc, _ := registered(t)
		list, err := commands.UrlsWithContext(ctx, tc.CommandContext, commands.OutputOptions{JSON: true})
		if err != nil {
			t.Fatal(err)
		}
		if list[0].Service != "api" || list[0].Port != 30010 || list[0].URL != "http://feature-login.acme.localhost" {
			t.Fatalf("api = %+v", list[0])
		}
		if list[1].Port != 0 || list[1].URL != "http://mail.feature-login.acme.localhost" {
			t.Fatalf("mail = %+v", list[1])
		}
		printed := printedText(tc)
		if !strings.Contains(printed, `"port": 30010`) || strings.Count(printed, `"port"`) != 1 {
			t.Fatalf("json = %s", printed)
		}
	})

	t.Run("the text form of urls does not mention ports", func(t *testing.T) {
		_, tc, _ := registered(t)
		if _, err := commands.UrlsWithContext(ctx, tc.CommandContext, commands.OutputOptions{}); err != nil {
			t.Fatal(err)
		}
		if printed := printedText(tc); strings.Contains(printed, "30010") {
			t.Fatalf("urls printed a port: %q", printed)
		}
	})

	t.Run("status --json adds a port key for a routed service with one", func(t *testing.T) {
		_, tc, _ := registered(t)
		if _, err := commands.StatusWithContext(ctx, tc.CommandContext, commands.OutputOptions{JSON: true}); err != nil {
			t.Fatal(err)
		}
		printed := printedText(tc)
		if !strings.Contains(printed, `"port": 30010`) || strings.Count(printed, `"port"`) != 1 {
			t.Fatalf("json = %s", printed)
		}
	})

	t.Run("run gives a command TREERIG_SERVICE_PORT_<NAME> for a service with a port", func(t *testing.T) {
		rw := newRunWorld(t, twoCommands())
		rw.SetPorts(rw.ref, map[string]int{"api": 30010})
		if err := commands.RunWithContext(ctx, rw.tc.CommandContext, commands.RunOptions{Name: "plan"}); err != nil {
			t.Fatal(err)
		}
		env := rw.started()[0].Env
		if !containsEnv(env, "TREERIG_SERVICE_PORT_API", "30010") {
			t.Errorf("env = %v", env)
		}
		for _, entry := range env {
			if strings.HasPrefix(entry, "TREERIG_SERVICE_PORT_MAIL=") {
				t.Errorf("mail has no port but got %s", entry)
			}
		}
		if !containsEnv(env, "TREERIG_SERVICE_URL_API", "http://feature-login.acme.localhost") {
			t.Errorf("env = %v", env)
		}
	})
}
