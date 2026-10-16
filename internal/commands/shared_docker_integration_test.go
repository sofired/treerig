//go:build integration

package commands_test

// Shared services (#123) on a real daemon: one instance for two worktrees of a
// repository, reached from each by its name on the worktree's own network and
// from the host on a pinned loopback port; replaced on request with its data
// kept; left alone by a worktree's removal; and removed, by its labels alone,
// by `shared down`. The stand-in for Mailpit is busybox's httpd on both of
// Mailpit's ports, so nothing is pulled but an image the other suites use.
// Every port comes from the test range, never the default one.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/commands/commandstest"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
)

const sharedWalkToml = `[project]
name = "shared-walk"
domain = "shared-walk.localhost"
label-namespace = %q
[compose]
file = "compose.yaml"
[shared]
file = "compose.shared.yaml"
[shared.service.mailpit]
port = 8025
ports = [1025]
used-by = ["api"]
`

const sharedWalkCompose = `services:
  api:
    image: busybox:1.37.0
    init: true
    command: ["sh", "-c", "exec sleep 3600"]
  idle:
    image: busybox:1.37.0
    init: true
    command: ["sh", "-c", "exec sleep 3600"]
`

// sharedWalkSharedCompose is the shared file; %s is what the stand-in serves,
// so a test changes the definition by changing it.
const sharedWalkSharedCompose = `services:
  mailpit:
    image: busybox:1.37.0
    init: true
    command: ["sh", "-c", "mkdir -p /www && echo '%s' > /www/index.html && (httpd -f -p 1025 -h /www &) ; exec httpd -f -p 8025 -h /www"]
    volumes: ["mailbox:/data"]
volumes:
  mailbox: {}
`

// sharedWalk is two checkouts of one repository over a real daemon.
type sharedWalk struct {
	*walk
	second    string
	secondEnv commands.Env
	router    *commandstest.FakeRouter
}

// newSharedWalk is a repository with treerig.toml, both Compose files and a
// second worktree carrying the same files.
func newSharedWalk(t *testing.T) *sharedWalk {
	t.Helper()
	w := newWalkFixture(t, "shared", "feature/walk", sharedWalkToml, sharedWalkCompose)
	w.writeShared(t, w.checkout, "shared v1")
	sw := &sharedWalk{walk: w, router: commandstest.NewFakeRouter(80)}
	sw.second = filepath.Join(w.base, "checkout2")
	gitIn(t, w.checkout, "worktree", "add", "-b", "feature/two", sw.second)
	for _, name := range []string{"treerig.toml", "compose.yaml", "compose.shared.yaml"} {
		data, err := os.ReadFile(filepath.Join(w.checkout, name))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(sw.second, name), string(data), 0o644)
	}
	sw.secondEnv = *w.env
	sw.secondEnv.Cwd = sw.second
	return sw
}

// writeShared writes the shared file of a checkout serving text.
func (w *walk) writeShared(t *testing.T, checkout, text string) {
	t.Helper()
	writeFile(t, filepath.Join(checkout, "compose.shared.yaml"), fmt.Sprintf(sharedWalkSharedCompose, text), 0o644)
}

// contextFor is a CommandContext for the checkout env points at.
func (w *sharedWalk) contextFor(t *testing.T, env *commands.Env) *commands.CommandContext {
	t.Helper()
	cctx, err := commands.Context(env)
	if err != nil {
		t.Fatal(err)
	}
	return cctx
}

// upIn runs `up` in the checkout env points at.
func (w *sharedWalk) upIn(t *testing.T, env *commands.Env) *commands.UpResult {
	t.Helper()
	cctx := w.contextFor(t, env)
	result, err := commands.UpWithContext(context.Background(), cctx, w.router, &commandstest.FakeIndex{}, commands.UpOptions{})
	if err != nil {
		t.Fatalf("up in %s: %v", env.Cwd, err)
	}
	return result
}

// sharedIDs lists the ids of kind carrying the shared label of the walk's namespace.
func (w *sharedWalk) sharedIDs(t *testing.T, kind ...string) []string {
	t.Helper()
	return dockerIDs(t, append(kind, "--filter", "label="+brand.SharedLabel(w.namespace)+"=true")...)
}

// reach is what a container sees fetching url, and whether it worked.
func reach(container, url string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "exec", container, "wget", "-qO-", "-T", "5", url).CombinedOutput()
	return strings.TrimSpace(string(output)), err == nil
}

func TestSharedServicesOnARealDaemon(t *testing.T) {
	w := newSharedWalk(t)
	ctx := context.Background()

	one := w.upIn(t, w.env)
	two := w.upIn(t, &w.secondEnv)

	containers := w.sharedIDs(t, "ps", "-aq")
	if len(containers) != 1 {
		t.Fatalf("shared containers = %v, want one for both worktrees", containers)
	}
	shared := containers[0]

	t.Run("each worktree's services reach it by its name, and it carries no worktree label", func(t *testing.T) {
		for _, result := range []*commands.UpResult{one, two} {
			api := w.owned(t, result.Registration.WorktreeID, "ps", "-q", "--filter", "label=com.docker.compose.service=api")
			if len(api) != 1 {
				t.Fatalf("api containers = %v", api)
			}
			for _, port := range []string{"8025", "1025"} {
				if body, ok := reach(api[0], "http://mailpit:"+port+"/"); !ok || body != "shared v1" {
					t.Errorf("%s reaches mailpit:%s as %q, %v", result.Registration.Slug, port, body, ok)
				}
			}
		}
		labels, err := exec.Command("docker", "inspect", "--format", `{{index .Config.Labels "`+brand.WorktreeLabel(w.namespace)+`"}}`, shared).Output()
		if err != nil || strings.TrimSpace(string(labels)) != "" {
			t.Fatalf("worktree label on the shared container: %q, %v", labels, err)
		}
	})

	t.Run("the host reaches it on the pinned loopback ports", func(t *testing.T) {
		record, err := registry.NewStore(w.controlHome, one.Registration.RepositoryID).FindShared("mailpit")
		if err != nil || record == nil || len(record.Ports) != 2 {
			t.Fatalf("record = %+v, %v", record, err)
		}
		for container, host := range record.Ports {
			if !registrytest.PortRange.Contains(host) {
				t.Errorf("container port %d pinned to %d, outside the test range", container, host)
			}
			eventuallyOnLoopback(t, host, "shared v1\n")
		}
		published, err := exec.Command("docker", "ps", "--filter", "id="+shared, "--format", "{{.Ports}}").Output()
		if err != nil || strings.Contains(string(published), "0.0.0.0") {
			t.Fatalf("published %q (%v)", published, err)
		}
	})

	t.Run("a second up leaves it alone", func(t *testing.T) {
		w.upIn(t, &w.secondEnv)
		if again := w.sharedIDs(t, "ps", "-aq"); len(again) != 1 || again[0] != shared {
			t.Fatalf("containers = %v, was %v", again, shared)
		}
	})

	t.Run("neither inventory lists the other's resources, though they are attached", func(t *testing.T) {
		cctx := w.contextFor(t, w.env)
		owned, err := cctx.Runtime(w.namespace).Inventory(ctx, refOf(one))
		if err != nil {
			t.Fatal(err)
		}
		sharedAll := map[string]bool{}
		for _, kind := range [][]string{{"ps", "-aq"}, {"volume", "ls", "-q"}, {"network", "ls", "-q"}} {
			for _, id := range w.sharedIDs(t, kind...) {
				sharedAll[id] = true
			}
		}
		for _, resource := range owned {
			if sharedAll[resource.ID] {
				t.Errorf("the worktree's inventory lists the shared %s", resource.ID)
			}
		}
		inventory, err := commands.SharedStackForTest(cctx, one.Registration.RepositoryID).Inventory(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(inventory) != 3 {
			t.Fatalf("shared inventory = %v", inventory)
		}
		for _, resource := range inventory {
			for _, mine := range owned {
				if resource.ID == mine.ID {
					t.Errorf("the shared inventory lists the worktree's %s", resource.ID)
				}
			}
		}
	})

	t.Run("a differing definition warns on up and is refused by shared up without --recreate", func(t *testing.T) {
		// Only the second checkout's file changes: its definition now differs from the running one.
		w.writeShared(t, w.second, "shared v2")
		cctx := w.contextFor(t, &w.secondEnv)
		if _, err := commands.UpWithContext(ctx, cctx, w.router, &commandstest.FakeIndex{}, commands.UpOptions{}); err != nil {
			t.Fatal(err)
		}
		if again := w.sharedIDs(t, "ps", "-aq"); len(again) != 1 || again[0] != shared {
			t.Fatalf("up recreated it: %v", again)
		}
		err := commands.SharedUpWithContext(ctx, cctx, nil, nil, commands.SharedUpOptions{})
		if err == nil || !strings.Contains(err.Error(), "different definition") {
			t.Fatalf("shared up: %v", err)
		}
	})

	t.Run("shared up --recreate replaces it, keeps its data, and reconnects both worktrees", func(t *testing.T) {
		volumes := w.sharedIDs(t, "volume", "ls", "-q")
		cctx := w.contextFor(t, &w.secondEnv)
		if err := commands.SharedUpWithContext(ctx, cctx, nil, nil, commands.SharedUpOptions{Names: []string{"mailpit"}, Recreate: true}); err != nil {
			t.Fatalf("shared up --recreate: %v", err)
		}
		again := w.sharedIDs(t, "ps", "-aq")
		if len(again) != 1 || again[0] == shared {
			t.Fatalf("containers = %v, was %v", again, shared)
		}
		if now := w.sharedIDs(t, "volume", "ls", "-q"); strings.Join(now, " ") != strings.Join(volumes, " ") {
			t.Fatalf("volumes = %v, were %v", now, volumes)
		}
		for _, result := range []*commands.UpResult{one, two} {
			api := w.owned(t, result.Registration.WorktreeID, "ps", "-q", "--filter", "label=com.docker.compose.service=api")
			if body, ok := reach(api[0], "http://mailpit:8025/"); !ok || body != "shared v2" {
				t.Errorf("%s reaches mailpit as %q, %v after the recreate", result.Registration.Slug, body, ok)
			}
		}
		shared = again[0]
	})

	t.Run("down disconnects first, so the worktree's network goes and the shared service stays", func(t *testing.T) {
		if err := commands.DownWithContext(ctx, w.contextFor(t, &w.secondEnv)); err != nil {
			t.Fatalf("down: %v", err)
		}
		if left := w.owned(t, two.Registration.WorktreeID, "network", "ls", "-q"); len(left) != 0 {
			t.Fatalf("networks left behind: %v", left)
		}
		if running := w.sharedIDs(t, "ps", "-q"); len(running) != 1 {
			t.Fatalf("shared containers running = %v", running)
		}
	})

	t.Run("worktree remove succeeds while it is attached, and leaves it and its data", func(t *testing.T) {
		if _, err := commands.RemoveWithContext(ctx, w.contextFor(t, w.env), w.env, commands.RemoveOptions{Yes: true}); err != nil {
			t.Fatalf("worktree remove: %v", err)
		}
		for _, kind := range [][]string{{"ps", "-aq"}, {"volume", "ls", "-q"}, {"network", "ls", "-q"}} {
			if left := w.owned(t, one.Registration.WorktreeID, kind...); len(left) != 0 {
				t.Errorf("%v left behind: %v", kind, left)
			}
		}
		if running := w.sharedIDs(t, "ps", "-q"); len(running) != 1 {
			t.Fatalf("shared containers running = %v", running)
		}
		if volumes := w.sharedIDs(t, "volume", "ls", "-q"); len(volumes) != 1 {
			t.Fatalf("shared volumes = %v", volumes)
		}
		record, _ := registry.NewStore(w.controlHome, one.Registration.RepositoryID).FindShared("mailpit")
		if record == nil {
			t.Fatal("worktree remove released the shared ports")
		}
	})

	t.Run("shared down finds everything by label from any checkout, keeps the data, and --volumes deletes it", func(t *testing.T) {
		cctx := w.contextFor(t, &w.secondEnv)
		if err := commands.SharedDownWithContext(ctx, cctx, &w.secondEnv, commands.SharedDownOptions{}); err != nil {
			t.Fatalf("shared down: %v", err)
		}
		if left := w.sharedIDs(t, "ps", "-aq"); len(left) != 0 {
			t.Fatalf("containers left: %v", left)
		}
		if volumes := w.sharedIDs(t, "volume", "ls", "-q"); len(volumes) != 1 {
			t.Fatalf("shared down removed the data: %v", volumes)
		}
		if err := commands.SharedDownWithContext(ctx, cctx, &w.secondEnv, commands.SharedDownOptions{Volumes: true, Yes: true}); err != nil {
			t.Fatalf("shared down --volumes: %v", err)
		}
		for _, kind := range [][]string{{"ps", "-aq"}, {"volume", "ls", "-q"}, {"network", "ls", "-q"}} {
			if left := w.sharedIDs(t, kind...); len(left) != 0 {
				t.Errorf("%v left behind: %v", kind, left)
			}
		}
		if record, _ := registry.NewStore(w.controlHome, one.Registration.RepositoryID).FindShared("mailpit"); record != nil {
			t.Fatalf("the pinned ports were kept: %+v", record)
		}
	})

	if _, err := commands.RemoveWithContext(ctx, w.contextFor(t, &w.secondEnv), &w.secondEnv, commands.RemoveOptions{Yes: true}); err != nil {
		t.Fatalf("remove second: %v", err)
	}
}

// sharedAnonymousCompose is the shared file of the anonymous-volume walk: the
// stand-in keeps its data in an anonymous volume, which only the container
// that Compose recreated in place carries over.
const sharedAnonymousCompose = `services:
  mailpit:
    image: busybox:1.37.0
    init: true
    command: ["sh", "-c", "mkdir -p /www && (httpd -f -p 1025 -h /www &) ; exec httpd -f -p 8025 -h /www"]
    volumes: ["/anon"]
`

// mountedVolumes lists the names of the volumes a container mounts.
func mountedVolumes(t *testing.T, container string) []string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "--format", `{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}} {{end}}{{end}}`, container).Output()
	if err != nil {
		t.Fatalf("docker inspect %s: %v", container, err)
	}
	return strings.Fields(string(out))
}

func TestSharedAnonymousVolumesOnARealDaemon(t *testing.T) {
	w := newSharedWalk(t)
	ctx := context.Background()
	writeFile(t, filepath.Join(w.checkout, "compose.shared.yaml"), sharedAnonymousCompose, 0o644)
	one := w.upIn(t, w.env)
	containers := w.sharedIDs(t, "ps", "-aq")
	if len(containers) != 1 {
		t.Fatalf("shared containers = %v", containers)
	}
	volumes := mountedVolumes(t, containers[0])
	if len(volumes) != 1 {
		t.Fatalf("mounted volumes = %v, want the one anonymous volume", volumes)
	}
	if out, err := exec.Command("docker", "exec", containers[0], "sh", "-c", "echo kept > /anon/file").CombinedOutput(); err != nil {
		t.Fatalf("writing the file: %v\n%s", err, out)
	}

	t.Run("a stopped instance restarted by up keeps its anonymous volume", func(t *testing.T) {
		if out, err := exec.Command("docker", "stop", containers[0]).CombinedOutput(); err != nil {
			t.Fatalf("docker stop: %v\n%s", err, out)
		}
		w.upIn(t, w.env)
		now := w.sharedIDs(t, "ps", "-q")
		if len(now) != 1 {
			t.Fatalf("running shared containers = %v", now)
		}
		if got := mountedVolumes(t, now[0]); strings.Join(got, " ") != strings.Join(volumes, " ") {
			t.Fatalf("mounted volumes = %v, were %v", got, volumes)
		}
		out, err := exec.Command("docker", "exec", now[0], "cat", "/anon/file").CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "kept" {
			t.Fatalf("the data was lost: %q, %v", out, err)
		}
	})

	t.Run("shared down keeps it and shared down --volumes removes it", func(t *testing.T) {
		cctx := w.contextFor(t, w.env)
		if err := commands.SharedDownWithContext(ctx, cctx, w.env, commands.SharedDownOptions{}); err != nil {
			t.Fatalf("shared down: %v", err)
		}
		if out, err := exec.Command("docker", "volume", "inspect", volumes[0]).CombinedOutput(); err != nil {
			t.Fatalf("shared down removed the anonymous volume: %v\n%s", err, out)
		}
		// The plain down stranded it: the next start gets a fresh anonymous
		// volume, and --volumes can no longer find this one. The test made it,
		// so the test removes it.
		stranded := volumes[0]
		t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "--force", stranded).Run() })
		w.upIn(t, w.env)
		volumes = mountedVolumes(t, w.sharedIDs(t, "ps", "-q")[0])
		if volumes[0] == stranded {
			t.Fatalf("the new container reattached the old anonymous volume %s", stranded)
		}
		if err := commands.SharedDownWithContext(ctx, cctx, w.env, commands.SharedDownOptions{Volumes: true, Yes: true}); err != nil {
			t.Fatalf("shared down --volumes: %v", err)
		}
		if out, err := exec.Command("docker", "volume", "inspect", volumes[0]).CombinedOutput(); err == nil {
			t.Fatalf("the anonymous volume survived --volumes: %s", out)
		}
	})

	if _, err := commands.RemoveWithContext(ctx, w.contextFor(t, w.env), w.env, commands.RemoveOptions{Yes: true}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	_ = one
}

// sharedNetworkedCompose is the worktree's Compose file with a label on its
// default network, which Compose answers by recreating the network.
const sharedNetworkedCompose = `services:
  api:
    image: busybox:1.37.0
    init: true
    command: ["sh", "-c", "exec sleep 3600"]
networks:
  default:
    labels:
      changed: "yes"
`

func TestUpWhileAttachedWhenComposeRecreatesTheNetwork(t *testing.T) {
	w := newSharedWalk(t)
	w.upIn(t, w.env)
	writeFile(t, filepath.Join(w.checkout, "compose.yaml"), sharedNetworkedCompose, 0o644)
	cctx := w.contextFor(t, w.env)
	shared := w.sharedIDs(t, "ps", "-q")
	// Measured on Docker 29.7.2 / Compose 5.5.0: the first `compose up` stops
	// the worktree's containers and cannot remove the network ("has active
	// endpoints"). `up` disconnects the shared service and ends with the
	// message that names the way out; it does not run `compose up` again,
	// because a second attempt fails on the half-recreated project ("is not
	// connected to the network"). If a Compose release can recreate the
	// network around an attached container, up succeeds and this test accepts
	// that too.
	_, err := commands.UpWithContext(context.Background(), cctx, w.router, &commandstest.FakeIndex{}, commands.UpOptions{})
	if err != nil && !strings.Contains(err.Error(), "Run 'treerig down' and then 'treerig up' again") {
		t.Fatalf("up: %v", err)
	}
	t.Logf("up with the network changed while the shared service is attached: %v", err)
	if err != nil {
		if derr := commands.DownWithContext(context.Background(), cctx); derr != nil {
			t.Fatalf("down: %v", derr)
		}
		w.upIn(t, w.env)
	}
	if again := w.sharedIDs(t, "ps", "-q"); len(again) != 1 || len(shared) != 1 || again[0] != shared[0] {
		t.Fatalf("shared containers = %v, were %v", again, shared)
	}
	api := w.owned(t, w.upIn(t, w.env).Registration.WorktreeID, "ps", "-q", "--filter", "label=com.docker.compose.service=api")
	if len(api) != 1 {
		t.Fatalf("api containers = %v", api)
	}
	if body, ok := reach(api[0], "http://mailpit:8025/"); !ok || body != "shared v1" {
		t.Fatalf("api reaches mailpit as %q, %v after the network was recreated", body, ok)
	}
	if _, err := commands.RemoveWithContext(context.Background(), w.contextFor(t, w.env), w.env, commands.RemoveOptions{Yes: true}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := commands.SharedDownWithContext(context.Background(), cctx, w.env, commands.SharedDownOptions{Volumes: true, Yes: true}); err != nil {
		t.Fatalf("shared down: %v", err)
	}
}
