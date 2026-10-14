package shared_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/commands/commandstest"
	"github.com/sofired/berth/internal/docker"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/run/fakerun"
	"github.com/sofired/berth/internal/shared"
	"github.com/sofired/berth/internal/worktree"
)

const (
	repository = "a08b4892-5111-49f1-a47a-5ab988c7f676"
	otherRepo  = "7c1d52a0-1234-4abc-8def-0123456789ab"
	worktreeA  = "2b41d9d4-ed88-49c1-be9c-5439bdf6a431"
	ns         = brand.LabelNamespace
)

var project = layout.SharedProjectName(repository)

// fixture is a stack over the stateful Docker fake.
type fixture struct {
	t       *testing.T
	docker  *commandstest.Docker
	stack   *shared.Stack
	control string
	roots   []string
}

// newFixture is a stack of repository over a fake daemon whose shared file
// declares a mailpit that depends on a db.
func newFixture(t *testing.T, roots ...string) *fixture {
	t.Helper()
	f := &fixture{t: t, docker: commandstest.NewDocker(), control: filepath.Join(registrytest.TempDir(t), "control"), roots: roots}
	f.docker.SetSharedService("mailpit", mailpitDefinition())
	f.docker.SetSharedService("db", map[string]any{"image": "busybox:1.37.0"})
	f.docker.SetSharedService("other", map[string]any{"image": "busybox:1.37.0"})
	f.rebuild()
	return f
}

// rebuild makes the stack again, after a test changed its options.
func (f *fixture) rebuild() {
	f.stack = shared.New(shared.Options{
		RepositoryID: repository, LabelNamespace: ns, ControlHome: f.control,
		ComposeFile: filepath.Join(f.control, "..", "compose.shared.yaml"), ProjectName: "acme",
		WorktreeRoots: f.roots, Runner: &fakerun.Runner{Handler: f.docker.Handle}, Logger: logger.Silent{},
	})
}

// start reads the definition and starts services with the pinned ports.
func (f *fixture) start(services ...string) shared.Definition {
	f.t.Helper()
	definition, err := f.stack.ReadDefinition(context.Background())
	if err != nil {
		f.t.Fatalf("ReadDefinition: %v", err)
	}
	err = f.stack.Up(context.Background(), shared.UpRequest{
		Definition: definition, Services: services, DefinedBy: worktreeA,
		Published: map[string]map[int]int{"mailpit": {1025: 30001, 8025: 30002}},
	})
	if err != nil {
		f.t.Fatalf("Up: %v", err)
	}
	return definition
}

func TestDefinitionHash(t *testing.T) {
	hashOf := func(t *testing.T, f *fixture, service string) string {
		t.Helper()
		definition, err := f.stack.ReadDefinition(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		hash, err := definition.Hash(service)
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}

	t.Run("is v1 and a sha256, the same on every read", func(t *testing.T) {
		f := newFixture(t)
		hash := hashOf(t, f, "mailpit")
		if !regexp.MustCompile(`^v1:[0-9a-f]{64}$`).MatchString(hash) || hash != hashOf(t, f, "mailpit") {
			t.Fatalf("hash = %q", hash)
		}
	})

	t.Run("covers the service and every service it transitively depends on", func(t *testing.T) {
		f := newFixture(t)
		f.docker.SetSharedService("db", map[string]any{"image": "busybox:1.37.0", "depends_on": map[string]any{"disk": map[string]any{}}})
		f.docker.SetSharedService("disk", map[string]any{"image": "busybox:1.37.0"})
		before := hashOf(t, f, "mailpit")
		f.docker.SetSharedService("disk", map[string]any{"image": "busybox:1.38.0"})
		if hashOf(t, f, "mailpit") == before {
			t.Fatal("a change to a dependency's dependency left the hash alone")
		}
	})

	t.Run("ignores a service outside the closure", func(t *testing.T) {
		f := newFixture(t)
		before := hashOf(t, f, "mailpit")
		f.docker.SetSharedService("other", map[string]any{"image": "busybox:9"})
		if hashOf(t, f, "mailpit") != before {
			t.Fatal("an unrelated service changed the hash")
		}
		if hashOf(t, f, "other") == hashOf(t, f, "db") {
			t.Fatal("two different services hash alike")
		}
	})

	t.Run("does not depend on the order Compose prints keys in", func(t *testing.T) {
		f := newFixture(t)
		f.docker.SetSharedService("db", map[string]any{"image": "i", "environment": map[string]any{"A": "1", "B": "2"}})
		first := hashOf(t, f, "db")
		f.docker.SetSharedService("db", map[string]any{"environment": map[string]any{"B": "2", "A": "1"}, "image": "i"})
		if hashOf(t, f, "db") != first {
			t.Fatal("key order changed the hash")
		}
	})

	t.Run("names a service the file does not define", func(t *testing.T) {
		f := newFixture(t)
		definition, _ := f.stack.ReadDefinition(context.Background())
		if _, err := definition.Hash("nope"); err == nil || !strings.Contains(err.Error(), "defines no service called nope") {
			t.Fatalf("err = %v", err)
		}
		if definition.Has("nope") || !definition.Has("mailpit") {
			t.Fatal("Has is wrong")
		}
		if got := strings.Join(definition.Closure("mailpit"), ","); got != "db,mailpit" {
			t.Fatalf("closure = %s", got)
		}
	})
}

func TestReadDefinitionRefusals(t *testing.T) {
	root := "/work/acme"
	t.Run("refuses build:", func(t *testing.T) {
		f := newFixture(t, root)
		f.docker.SetSharedService("db", map[string]any{"build": map[string]any{"context": "/work/acme/db"}})
		_, err := f.stack.ReadDefinition(context.Background())
		if err == nil || !strings.Contains(err.Error(), "Shared service db") || !strings.Contains(err.Error(), "has build:") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("refuses a bind mount inside a worktree, naming the source and the worktree", func(t *testing.T) {
		f := newFixture(t, root, "/work/acme-feature")
		f.docker.SetSharedService("db", map[string]any{"image": "i", "volumes": []any{map[string]any{"type": "bind", "source": "/work/acme-feature/data", "target": "/d"}}})
		_, err := f.stack.ReadDefinition(context.Background())
		if err == nil || !strings.Contains(err.Error(), "/work/acme-feature/data") || !strings.Contains(err.Error(), "inside the worktree /work/acme-feature") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("refuses the worktree's root itself, but not a sibling directory that shares its prefix", func(t *testing.T) {
		f := newFixture(t, root)
		f.docker.SetSharedService("db", map[string]any{"image": "i", "volumes": []any{map[string]any{"type": "bind", "source": root, "target": "/d"}}})
		if _, err := f.stack.ReadDefinition(context.Background()); err == nil {
			t.Fatal("the worktree root itself was accepted")
		}
		f.docker.SetSharedService("db", map[string]any{"image": "i", "volumes": []any{map[string]any{"type": "bind", "source": "/work/acme-data", "target": "/d"}}})
		if _, err := f.stack.ReadDefinition(context.Background()); err != nil {
			t.Fatalf("a path that only shares a prefix: %v", err)
		}
	})

	t.Run("accepts images, named volumes and an absolute path outside every worktree", func(t *testing.T) {
		f := newFixture(t, root)
		f.docker.SetSharedService("db", map[string]any{"image": "i", "volumes": []any{
			map[string]any{"type": "volume", "source": "data", "target": "/d"},
			map[string]any{"type": "bind", "source": "/srv/shared/db", "target": "/e"},
		}})
		if _, err := f.stack.ReadDefinition(context.Background()); err != nil {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("reports a file Compose cannot read with its own words", func(t *testing.T) {
		f := newFixture(t, root)
		f.docker.FailCall("config --format json", "yaml: line 3: mapping values are not allowed", 1, 1)
		_, err := f.stack.ReadDefinition(context.Background())
		if err == nil || !strings.Contains(err.Error(), "Compose could not read") || !strings.Contains(err.Error(), "mapping values") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("reads the file under the shared environment only", func(t *testing.T) {
		f := newFixture(t, root)
		runner := &fakerun.Runner{Handler: f.docker.Handle}
		stack := shared.New(shared.Options{
			RepositoryID: repository, LabelNamespace: ns, ControlHome: f.control, ComposeFile: "/work/compose.shared.yaml",
			ProjectName: "acme", Runner: runner, Environ: []string{"PATH=/bin", "BERTH_WORKTREE_ID=leak", "COMPOSE_FILE=other.yaml"},
		})
		if _, err := stack.ReadDefinition(context.Background()); err != nil {
			t.Fatal(err)
		}
		calls := runner.Calls()
		if len(calls) != 1 {
			t.Fatalf("calls = %v", runner.Argv())
		}
		env := strings.Join(calls[0].Env, "\n")
		for _, want := range []string{"COMPOSE_PROJECT_NAME=" + project, "BERTH_PROJECT=acme", "BERTH_REPOSITORY_ID=" + repository, "PATH=/bin"} {
			if !strings.Contains(env, want) {
				t.Errorf("environment lacks %s", want)
			}
		}
		for _, banned := range []string{"BERTH_WORKTREE_ID", "BERTH_ROOT", "COMPOSE_FILE"} {
			if strings.Contains(env, banned+"=") {
				t.Errorf("environment carries %s", banned)
			}
		}
	})
}

func TestUp(t *testing.T) {
	t.Run("creates the container with the shared labels and never a worktree label", func(t *testing.T) {
		f := newFixture(t)
		definition := f.start("mailpit")
		id := f.docker.ContainerNamed(project + "-mailpit-1")
		if id == "" {
			t.Fatal("no mailpit container")
		}
		labels := f.docker.Labels("container", id)
		hash, _ := definition.Hash("mailpit")
		for name, want := range map[string]string{
			brand.ManagedLabel(ns): "true", brand.RepositoryLabel(ns): repository, brand.SharedLabel(ns): "true",
			brand.SharedDefinitionLabel(ns): hash, brand.SharedDefinedByLabel(ns): worktreeA,
			"com.docker.compose.project": project, "com.docker.compose.service": "mailpit",
		} {
			if labels[name] != want {
				t.Errorf("label %s = %q, want %q", name, labels[name], want)
			}
		}
		if _, has := labels[brand.WorktreeLabel(ns)]; has {
			t.Error("the shared container carries a worktree label")
		}
	})

	t.Run("starts what the service depends on, labelled the same way, each with its own hash", func(t *testing.T) {
		f := newFixture(t)
		definition := f.start("mailpit")
		db := f.docker.ContainerNamed(project + "-db-1")
		if db == "" {
			t.Fatal("the dependency did not start")
		}
		labels := f.docker.Labels("container", db)
		hash, _ := definition.Hash("db")
		if labels[brand.SharedLabel(ns)] != "true" || labels[brand.SharedDefinitionLabel(ns)] != hash {
			t.Errorf("db labels = %v", labels)
		}
		if f.docker.ContainerNamed(project+"-other-1") != "" {
			t.Error("an unrelated service started")
		}
	})

	t.Run("publishes each pinned port on loopback only", func(t *testing.T) {
		f := newFixture(t)
		f.start("mailpit")
		id := f.docker.ContainerNamed(project + "-mailpit-1")
		if got := f.docker.Published(id, 1025); len(got) != 1 || got[0] != "127.0.0.1:30001" {
			t.Errorf("1025 -> %v", got)
		}
		if got := f.docker.Published(id, 8025); len(got) != 1 || got[0] != "127.0.0.1:30002" {
			t.Errorf("8025 -> %v", got)
		}
	})

	t.Run("labels the stack's volumes and default network as its own", func(t *testing.T) {
		f := newFixture(t)
		f.start("mailpit")
		for _, kind := range []string{"volume", "network"} {
			ids := f.docker.Find(kind, map[string]string{brand.SharedLabel(ns): "true"})
			if len(ids) != 1 {
				t.Fatalf("%s: %v", kind, ids)
			}
			labels := f.docker.Labels(kind, ids[0])
			if labels[brand.RepositoryLabel(ns)] != repository || labels[brand.ManagedLabel(ns)] != "true" {
				t.Errorf("%s labels = %v", kind, labels)
			}
			if _, has := labels[brand.WorktreeLabel(ns)]; has {
				t.Errorf("%s carries a worktree label", kind)
			}
		}
	})

	t.Run("asks Compose not to recreate anything, and to wait", func(t *testing.T) {
		f := newFixture(t)
		f.start("mailpit")
		var up string
		for _, call := range f.docker.Calls() {
			if strings.Contains(call, " up ") {
				up = call
			}
		}
		for _, want := range []string{"--project-name " + project, "--no-recreate", "--wait", "--detach", "-f ", "berth.override.yaml"} {
			if !strings.Contains(up, want) {
				t.Errorf("%q lacks %q", up, want)
			}
		}
		if strings.Contains(up, "--force-recreate") || strings.Contains(up, "--build") || strings.Contains(up, "--remove-orphans") {
			t.Errorf("%q", up)
		}
	})

	t.Run("writes the override into the repository's shared directory", func(t *testing.T) {
		f := newFixture(t)
		f.start("mailpit")
		data, err := os.ReadFile(layout.SharedLayout(f.control, repository).OverridePath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"published": "30001"`) || !strings.Contains(string(data), `"host_ip": "127.0.0.1"`) {
			t.Errorf("override:\n%s", data)
		}
	})

	t.Run("leaves a running container exactly as it is", func(t *testing.T) {
		f := newFixture(t)
		f.start("mailpit")
		id := f.docker.ContainerNamed(project + "-mailpit-1")
		before := f.docker.Labels("container", id)
		definition, _ := f.stack.ReadDefinition(context.Background())
		f.docker.SetSharedService("mailpit", map[string]any{"image": "axllent/mailpit:v9"})
		changed, _ := f.stack.ReadDefinition(context.Background())
		_ = definition
		if err := f.stack.Up(context.Background(), shared.UpRequest{Definition: changed, Services: []string{"mailpit"}, DefinedBy: "someone-else"}); err != nil {
			t.Fatal(err)
		}
		if got := f.docker.ContainerNamed(project + "-mailpit-1"); got != id {
			t.Fatalf("the container was recreated: %s -> %s", id, got)
		}
		after := f.docker.Labels("container", id)
		if after[brand.SharedDefinitionLabel(ns)] != before[brand.SharedDefinitionLabel(ns)] || after[brand.SharedDefinedByLabel(ns)] != worktreeA {
			t.Errorf("labels changed: %v", after)
		}
	})

	t.Run("a service the file does not define is refused before anything is written", func(t *testing.T) {
		f := newFixture(t)
		definition, _ := f.stack.ReadDefinition(context.Background())
		err := f.stack.Up(context.Background(), shared.UpRequest{Definition: definition, Services: []string{"nope"}})
		if err == nil || !strings.Contains(err.Error(), "defines no service called nope") {
			t.Fatalf("err = %v", err)
		}
		if _, statErr := os.Stat(layout.SharedLayout(f.control, repository).OverridePath); statErr == nil {
			t.Error("an override was written")
		}
	})

	t.Run("a start that lost the race to create the container is a ConflictError", func(t *testing.T) {
		f := newFixture(t)
		definition, _ := f.stack.ReadDefinition(context.Background())
		f.docker.FailCall(" up ", `Error response from daemon: Conflict. The container name "/x-mailpit-1" is already in use by container "abc".`, 1, 1)
		err := f.stack.Up(context.Background(), shared.UpRequest{Definition: definition, Services: []string{"mailpit"}})
		var conflict *shared.ConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("err = %v (%T)", err, err)
		}
		f.docker.FailCall(" up ", "pull access denied for mailpit", 1, 1)
		err = f.stack.Up(context.Background(), shared.UpRequest{Definition: definition, Services: []string{"mailpit"}})
		if err == nil || errors.As(err, &conflict) {
			t.Fatalf("a pull failure was read as a conflict: %v", err)
		}
		f.docker.FailCall(" up ", "Error response from daemon: volume x already exists", 1, 1)
		err = f.stack.Up(context.Background(), shared.UpRequest{Definition: definition, Services: []string{"mailpit"}})
		if err == nil || errors.As(err, &conflict) {
			t.Fatalf("an existing volume was read as a conflict: %v", err)
		}
	})
}

func TestInstances(t *testing.T) {
	ctx := context.Background()
	t.Run("is absent when Docker answers and there is no container", func(t *testing.T) {
		f := newFixture(t)
		instance, err := f.stack.Instance(ctx, "mailpit")
		if err != nil || instance.State != shared.Absent || instance.Live() {
			t.Fatalf("instance = %+v, %v", instance, err)
		}
	})

	t.Run("reads the state, the definition hash and who defined it from the container", func(t *testing.T) {
		f := newFixture(t)
		definition := f.start("mailpit")
		instance, err := f.stack.Instance(ctx, "mailpit")
		if err != nil {
			t.Fatal(err)
		}
		hash, _ := definition.Hash("mailpit")
		if instance.State != shared.Running || !instance.Live() || instance.Definition != hash || instance.DefinedBy != worktreeA {
			t.Fatalf("instance = %+v", instance)
		}
	})

	t.Run("uses the worktree service words: running, starting, stopped, with unhealthy as a detail", func(t *testing.T) {
		f := newFixture(t)
		f.start("mailpit")
		id := f.docker.ContainerNamed(project + "-mailpit-1")
		f.docker.SetHealth(id, "unhealthy")
		if got, _ := f.stack.Instance(ctx, "mailpit"); got.State != shared.Running || got.Detail != "unhealthy" || !got.Live() {
			t.Errorf("unhealthy: %+v", got)
		}
		f.docker.SetHealth(id, "starting")
		if got, _ := f.stack.Instance(ctx, "mailpit"); got.State != shared.Starting || got.Detail != "" || !got.Live() {
			t.Errorf("a pending health check: %+v", got)
		}
		f.docker.SetHealth(id, "healthy")
		if got, _ := f.stack.Instance(ctx, "mailpit"); got.State != shared.Running || got.Detail != "" {
			t.Errorf("healthy: %+v", got)
		}
		for _, status := range []string{"exited", "created", "dead", "paused"} {
			f.docker.SetStatus(id, status)
			if got, _ := f.stack.Instance(ctx, "mailpit"); got.State != shared.Stopped || got.Live() {
				t.Errorf("%s: %+v", status, got)
			}
		}
		f.docker.SetStatus(id, "restarting")
		if got, _ := f.stack.Instance(ctx, "mailpit"); got.State != shared.Starting || !got.Live() {
			t.Errorf("restarting: %+v", got)
		}
	})

	t.Run("fails closed on a state it does not know, rather than calling it absent or stopped", func(t *testing.T) {
		f := newFixture(t)
		f.start("mailpit")
		f.docker.SetStatus(f.docker.ContainerNamed(project+"-mailpit-1"), "levitating")
		if _, err := f.stack.Instance(ctx, "mailpit"); err == nil || !strings.Contains(err.Error(), "Could not tell what state") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("fails closed when Docker cannot answer, rather than calling the service absent", func(t *testing.T) {
		f := newFixture(t)
		f.docker.FailCall("ps -aq", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?", 1, 1)
		_, err := f.stack.Instance(ctx, "mailpit")
		if err == nil || !docker.Unavailable(err) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("fails closed when an inspect fails for a reason other than absence", func(t *testing.T) {
		f := newFixture(t)
		f.start("mailpit")
		f.docker.FailCall("container inspect", "Error response from daemon: i/o timeout", 1, 1)
		if _, err := f.stack.Instance(ctx, "mailpit"); err == nil || !strings.Contains(err.Error(), "Could not inspect container") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a container that vanished between the list and the inspect is not a holder", func(t *testing.T) {
		f := newFixture(t)
		f.start("mailpit")
		id := f.docker.ContainerNamed(project + "-mailpit-1")
		f.docker.FailCall("container inspect "+id, "Error response from daemon: No such container: "+id, 1, 1)
		got, err := f.stack.Instance(ctx, "mailpit")
		if err != nil || got.State != shared.Absent {
			t.Fatalf("instance = %+v, %v", got, err)
		}
	})

	t.Run("refuses a service with two containers", func(t *testing.T) {
		f := newFixture(t)
		f.start("mailpit")
		labels := map[string]string{}
		for k, v := range f.docker.Labels("container", f.docker.ContainerNamed(project+"-mailpit-1")) {
			labels[k] = v
		}
		f.docker.SetName(f.docker.Add("container", labels), "stray")
		if _, err := f.stack.Instance(ctx, "mailpit"); err == nil || !strings.Contains(err.Error(), "more than one container") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("Instances answers several services from one listing", func(t *testing.T) {
		f := newFixture(t)
		f.start("mailpit")
		listings := f.docker.Count("ps -aq")
		found, err := f.stack.Instances(ctx, []string{"mailpit", "db", "other"})
		if err != nil {
			t.Fatal(err)
		}
		if found["mailpit"].State != shared.Running || found["db"].State != shared.Running || found["other"].State != shared.Absent {
			t.Fatalf("found = %+v", found)
		}
		if f.docker.Count("ps -aq")-listings != 1 {
			t.Error("more than one listing")
		}
	})
}

func TestOwnershipAndRemoval(t *testing.T) {
	ctx := context.Background()
	ref := worktree.Ref{RepositoryID: repository, WorktreeID: worktreeA, Slug: "feature-login"}

	t.Run("lists by the shared labels and never a worktree's resources", func(t *testing.T) {
		f := newFixture(t)
		f.start("mailpit")
		f.docker.Up(ref, ns, "api")
		inventory, err := f.stack.Inventory(ctx)
		if err != nil {
			t.Fatal(err)
		}
		kinds := map[string]int{}
		for _, resource := range inventory {
			kinds[resource.Kind]++
		}
		if kinds["container"] != 2 || kinds["network"] != 1 || kinds["volume"] != 1 {
			t.Fatalf("inventory = %v", inventory)
		}
		for _, resource := range inventory {
			if f.docker.Labels(resource.Kind, resource.ID)[brand.WorktreeLabel(ns)] != "" {
				t.Errorf("listed a worktree resource: %s", resource)
			}
		}
		if inventory[0].Kind != "container" || inventory[len(inventory)-1].Kind != "volume" {
			t.Errorf("not in removal order: %v", inventory)
		}
	})

	t.Run("does not list another repository's shared resources", func(t *testing.T) {
		f := newFixture(t)
		f.start("mailpit")
		other := commandstest.Ownership(worktree.Ref{RepositoryID: otherRepo, WorktreeID: worktreeA}, ns)
		delete(other, brand.WorktreeLabel(ns))
		other[brand.SharedLabel(ns)] = "true"
		other[brand.RepositoryLabel(ns)] = otherRepo
		other["com.docker.compose.project"] = layout.SharedProjectName(otherRepo)
		stranger := f.docker.Add("container", other)
		inventory, err := f.stack.Inventory(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, resource := range inventory {
			if resource.ID == stranger {
				t.Fatal("listed another repository's container")
			}
		}
	})

	t.Run("refuses everything when a resource that claims to be shared carries a worktree label", func(t *testing.T) {
		f := newFixture(t)
		f.start("mailpit")
		labels := commandstest.Ownership(ref, ns)
		labels[brand.SharedLabel(ns)] = "true"
		labels["com.docker.compose.project"] = project
		f.docker.Add("container", labels)
		if _, err := f.stack.Inventory(ctx); err == nil || !strings.Contains(err.Error(), "do not say it belongs to this repository's shared services") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("refuses a resource whose Compose project is not the stack's", func(t *testing.T) {
		f := newFixture(t)
		f.start("mailpit")
		id := f.docker.ContainerNamed(project + "-db-1")
		labels := f.docker.Labels("container", id)
		labels["com.docker.compose.project"] = "someone-elses"
		if _, err := f.stack.Inventory(ctx); err == nil {
			t.Fatal("a container in another project was listed")
		}
	})

	t.Run("stamps the stack's containers and networks with the format label, and its volumes with none", func(t *testing.T) {
		f := newFixture(t)
		f.start("mailpit")
		for _, kind := range []string{"container", "network", "volume"} {
			ids := f.docker.Find(kind, map[string]string{brand.SharedLabel(ns): "true"})
			if len(ids) == 0 {
				t.Fatalf("no shared %s", kind)
			}
			for _, id := range ids {
				got, stamped := f.docker.Labels(kind, id)[brand.FormatLabel(ns)]
				// Compose offers to recreate a volume whose label changed,
				// losing its data, so a volume never carries the label.
				if kind == "volume" && stamped {
					t.Errorf("volume %s carries the format label %q", id, got)
				}
				if kind != "volume" && got != "1" {
					t.Errorf("%s %s: format label = %q", kind, id, got)
				}
			}
		}
	})

	t.Run("refuses a container or network of a newer format, or with no format label, listing and removing nothing", func(t *testing.T) {
		for name, mutate := range map[string]func(labels map[string]string){
			"a newer format": func(labels map[string]string) { labels[brand.FormatLabel(ns)] = "2" },
			"no format":      func(labels map[string]string) { delete(labels, brand.FormatLabel(ns)) },
			"a format of 0":  func(labels map[string]string) { labels[brand.FormatLabel(ns)] = "0" },
		} {
			for _, kind := range []string{"container", "network"} {
				f := newFixture(t)
				f.start("mailpit")
				id := f.docker.Find(kind, map[string]string{brand.SharedLabel(ns): "true"})[0]
				mutate(f.docker.Labels(kind, id))

				_, err := f.stack.Inventory(ctx)
				want := "Refusing to touch the " + kind + " " + id
				if err == nil || !strings.HasPrefix(err.Error(), want) {
					t.Fatalf("%s, %s: err = %v", name, kind, err)
				}
				if name == "a newer format" && !strings.HasSuffix(err.Error(), ": it is format 2, written by a newer berth, and this build reads format 1. Upgrade berth, then run this again.") {
					t.Errorf("%s: err = %v", kind, err)
				}
				if name != "a newer format" && !strings.HasSuffix(err.Error(), "its dev.berth.format label is missing or not recognised, so berth cannot prove it belongs to this repository's shared services. "+
					"After checking that nothing needs it, remove it with `docker "+kind+" rm "+id+"`.") {
					t.Errorf("%s, %s: err = %v", name, kind, err)
				}
				if reapErr := f.stack.Reap(ctx, shared.Resource{Kind: kind, ID: id}); reapErr == nil || !f.docker.Exists(kind, id) {
					t.Errorf("%s, %s: Reap = %v, exists = %v", name, kind, reapErr, f.docker.Exists(kind, id))
				}
			}
		}
	})

	t.Run("refuses to start over a container or network of a newer format, before writing the override or running Compose", func(t *testing.T) {
		for _, kind := range []string{"container", "network"} {
			f := newFixture(t)
			definition := f.start("mailpit")
			id := f.docker.Find(kind, map[string]string{brand.SharedLabel(ns): "true"})[0]
			f.docker.Labels(kind, id)[brand.FormatLabel(ns)] = "2"
			before := len(f.docker.Calls())
			err := f.stack.Up(ctx, shared.UpRequest{
				Definition: definition, Services: []string{"mailpit"}, DefinedBy: worktreeA,
				Published: map[string]map[int]int{"mailpit": {1025: 30001, 8025: 30002}},
			})
			want := "Refusing to touch the " + kind + " " + id + ": it is format 2, written by a newer berth, and this build reads format 1. Upgrade berth, then run this again."
			if err == nil || err.Error() != want {
				t.Fatalf("%s: err = %v\nwant %s", kind, err, want)
			}
			for _, call := range f.docker.Calls()[before:] {
				if strings.Contains(call, "compose") {
					t.Errorf("%s: Compose ran after the refusal: %s", kind, call)
				}
			}
		}
	})

	t.Run("proves a volume by the ownership labels alone, whatever its format label says", func(t *testing.T) {
		for _, value := range []string{"", "2", "junk"} {
			f := newFixture(t)
			f.start("mailpit")
			id := f.docker.Find("volume", map[string]string{brand.SharedLabel(ns): "true"})[0]
			if value != "" {
				f.docker.Labels("volume", id)[brand.FormatLabel(ns)] = value
			}
			inventory, err := f.stack.Inventory(ctx)
			if err != nil || !slices.ContainsFunc(inventory, func(r shared.Resource) bool { return r.ID == id }) {
				t.Fatalf("format %q: inventory = %v, err = %v", value, inventory, err)
			}
		}
	})

	t.Run("reaps a container gracefully, keeping its volume and network", func(t *testing.T) {
		f := newFixture(t)
		f.start("mailpit")
		id := f.docker.ContainerNamed(project + "-mailpit-1")
		if err := f.stack.Reap(ctx, shared.Resource{Kind: "container", ID: id}); err != nil {
			t.Fatal(err)
		}
		if f.docker.Exists("container", id) {
			t.Fatal("still there")
		}
		calls := strings.Join(f.docker.Calls(), "\n")
		stop := strings.Index(calls, "stop "+id)
		if stop < 0 || stop > strings.Index(calls, "rm --force "+id) {
			t.Errorf("not stopped before removed:\n%s", calls)
		}
		if len(f.docker.Find("volume", map[string]string{brand.SharedLabel(ns): "true"})) != 1 {
			t.Error("the volume went with the container")
		}
	})

	t.Run("re-verifies a resource at removal time and refuses one whose labels changed", func(t *testing.T) {
		f := newFixture(t)
		f.start("mailpit")
		id := f.docker.ContainerNamed(project + "-mailpit-1")
		f.docker.TamperAfter(id, 0, commandstest.Ownership(ref, ns))
		err := f.stack.Reap(ctx, shared.Resource{Kind: "container", ID: id})
		if err == nil || !strings.Contains(err.Error(), "Refusing to touch container") {
			t.Fatalf("err = %v", err)
		}
		if !f.docker.Exists("container", id) {
			t.Fatal("a container that failed verification was removed")
		}
	})

	t.Run("will not reap a worktree's container even when asked by ID", func(t *testing.T) {
		f := newFixture(t)
		ids := f.docker.Up(ref, ns, "api")
		for i, kind := range []string{"container", "volume"} {
			if err := f.stack.Reap(ctx, shared.Resource{Kind: kind, ID: ids[i]}); err == nil {
				t.Fatalf("a worktree's %s was reaped", kind)
			}
			if !f.docker.Exists(kind, ids[i]) {
				t.Fatalf("a worktree's %s is gone", kind)
			}
		}
	})

	t.Run("reaping something already gone is success", func(t *testing.T) {
		f := newFixture(t)
		for _, kind := range []string{"container", "network", "volume"} {
			if err := f.stack.Reap(ctx, shared.Resource{Kind: kind, ID: "gone"}); err != nil {
				t.Errorf("%s: %v", kind, err)
			}
		}
		if err := f.stack.Reap(ctx, shared.Resource{Kind: "image", ID: "x"}); err == nil {
			t.Error("an unknown kind was accepted")
		}
	})

	t.Run("a network with something still attached is not removed", func(t *testing.T) {
		f := newFixture(t)
		f.start("mailpit")
		network := f.docker.Find("network", map[string]string{brand.SharedLabel(ns): "true"})[0]
		if err := f.stack.Reap(ctx, shared.Resource{Kind: "network", ID: network}); err == nil || !strings.Contains(err.Error(), "active endpoints") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestAttachAndDetach(t *testing.T) {
	ctx := context.Background()
	ref := worktree.Ref{RepositoryID: repository, WorktreeID: worktreeA, Slug: "feature-login"}
	setup := func(t *testing.T) (*fixture, string, string) {
		f := newFixture(t)
		f.start("mailpit")
		network := f.docker.AddNetwork("wt_default", commandstest.Ownership(ref, ns))
		return f, f.docker.ContainerNamed(project + "-mailpit-1"), network
	}

	t.Run("connects the container to the network under its name as alias", func(t *testing.T) {
		f, id, network := setup(t)
		if err := f.stack.Attach(ctx, id, shared.Proven(network), "mailpit"); err != nil {
			t.Fatal(err)
		}
		if got := f.docker.Aliases(id, network); len(got) != 1 || got[0] != "mailpit" {
			t.Fatalf("aliases = %v", got)
		}
		instance, _ := f.stack.Instance(ctx, "mailpit")
		if !instance.Attached(network) || instance.Attached("elsewhere") {
			t.Fatalf("Attached is wrong for %+v", instance.Container.Networks)
		}
	})

	t.Run("connecting twice is success", func(t *testing.T) {
		f, id, network := setup(t)
		for range 2 {
			if err := f.stack.Attach(ctx, id, shared.Proven(network), "mailpit"); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("says when the container or the network is gone", func(t *testing.T) {
		f, id, network := setup(t)
		if err := f.stack.Attach(ctx, "missing", shared.Proven(network), "mailpit"); !errors.Is(err, shared.ErrContainerGone) {
			t.Errorf("missing container: %v", err)
		}
		if err := f.stack.Attach(ctx, id, shared.Proven("missing"), "mailpit"); !errors.Is(err, shared.ErrNetworkGone) {
			t.Errorf("missing network: %v", err)
		}
		f.docker.FailCall("network connect", "Error response from daemon: No such container: "+id, 1, 1)
		if err := f.stack.Attach(ctx, id, shared.Proven(network), "mailpit"); !errors.Is(err, shared.ErrContainerGone) {
			t.Errorf("container gone between verify and connect: %v", err)
		}
	})

	t.Run("reports any other connect failure", func(t *testing.T) {
		f, id, network := setup(t)
		f.docker.FailCall("network connect", "Error response from daemon: boom", 1, 1)
		if err := f.stack.Attach(ctx, id, shared.Proven(network), "mailpit"); err == nil || errors.Is(err, shared.ErrContainerGone) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("never connects a container that is not the stack's own", func(t *testing.T) {
		f, _, network := setup(t)
		ids := f.docker.Up(ref, ns, "api")
		if err := f.stack.Attach(ctx, ids[0], shared.Proven(network), "mailpit"); err == nil || !strings.Contains(err.Error(), "Refusing to touch container") {
			t.Fatalf("err = %v", err)
		}
		if len(f.docker.Attachments(ids[0])) != 0 {
			t.Fatal("the worktree's own container was attached")
		}
	})

	t.Run("disconnects, and every flavour of already-gone is success", func(t *testing.T) {
		f, id, network := setup(t)
		if err := f.stack.Attach(ctx, id, shared.Proven(network), "mailpit"); err != nil {
			t.Fatal(err)
		}
		if err := f.stack.Detach(ctx, id, shared.Proven(network)); err != nil {
			t.Fatal(err)
		}
		for _, a := range f.docker.Attachments(id) {
			if a == network {
				t.Fatal("still attached")
			}
		}
		if err := f.stack.Detach(ctx, id, shared.Proven(network)); err != nil {
			t.Errorf("not connected: %v", err)
		}
		if err := f.stack.Detach(ctx, "missing", shared.Proven(network)); err != nil {
			t.Errorf("container gone: %v", err)
		}
		if err := f.stack.Detach(ctx, id, shared.Proven("missing")); err != nil {
			t.Errorf("network gone: %v", err)
		}
		f.docker.FailCall("network disconnect", "Error response from daemon: boom", 1, 1)
		if err := f.stack.Detach(ctx, id, shared.Proven(network)); err == nil {
			t.Error("another failure was swallowed")
		}
	})

	t.Run("never disconnects a container that is not the stack's own", func(t *testing.T) {
		f, _, network := setup(t)
		ids := f.docker.Up(ref, ns, "api")
		f.docker.Attach(ids[0], network)
		if err := f.stack.Detach(ctx, ids[0], shared.Proven(network)); err == nil {
			t.Fatal("a worktree's container was disconnected")
		}
		if len(f.docker.Attachments(ids[0])) != 1 {
			t.Fatal("the attachment went")
		}
	})
}

func TestReadsThePinnedInputsOnly(t *testing.T) {
	f := newFixture(t)
	if _, err := f.stack.ReadDefinition(context.Background()); err != nil {
		t.Fatal(err)
	}
	var config string
	for _, call := range f.docker.Calls() {
		if strings.Contains(call, " config ") {
			config = call
		}
	}
	for _, want := range []string{"--env-file /dev/null", "--profile *", "--project-name " + project} {
		if !strings.Contains(config, want) {
			t.Errorf("%q lacks %q", config, want)
		}
	}
	f.start("mailpit")
	for _, call := range f.docker.Calls() {
		if strings.Contains(call, " up ") && (!strings.Contains(call, "--env-file /dev/null") || !strings.Contains(call, "--profile *")) {
			t.Errorf("up reads what the hash does not: %q", call)
		}
	}
}

func TestPortsInTheSharedFileAreRefused(t *testing.T) {
	declare := func(f *fixture) {
		f.stack = shared.New(shared.Options{
			RepositoryID: repository, LabelNamespace: ns, ControlHome: f.control, ComposeFile: "/work/compose.shared.yaml", ProjectName: "acme",
			Declared: []string{"mailpit"}, Runner: &fakerun.Runner{Handler: f.docker.Handle}, Logger: logger.Silent{},
		})
	}
	t.Run("on a declared service, naming where to declare them", func(t *testing.T) {
		f := newFixture(t)
		declare(f)
		entry := mailpitDefinition()
		entry["ports"] = []any{map[string]any{"target": 8025, "published": "8025", "host_ip": "127.0.0.1"}}
		f.docker.SetSharedService("mailpit", entry)
		_, err := f.stack.ReadDefinition(context.Background())
		if err == nil || !strings.Contains(err.Error(), "mailpit") || !strings.Contains(err.Error(), "has ports:") || !strings.Contains(err.Error(), "[shared.service.mailpit] in berth.toml") ||
			!strings.Contains(err.Error(), "127.0.0.1") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("on what a declared service needs, even a port with no host port", func(t *testing.T) {
		f := newFixture(t)
		declare(f)
		f.docker.SetSharedService("db", map[string]any{"image": "i", "ports": []any{map[string]any{"target": 5432}}})
		if _, err := f.stack.ReadDefinition(context.Background()); err == nil || !strings.Contains(err.Error(), "Shared service db") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("not on a service nothing declared uses", func(t *testing.T) {
		f := newFixture(t)
		declare(f)
		f.docker.SetSharedService("other", map[string]any{"image": "i", "ports": []any{map[string]any{"target": 1}}})
		if _, err := f.stack.ReadDefinition(context.Background()); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
}

// mailpitDefinition is the fixture's mailpit entry, to edit.
func mailpitDefinition() map[string]any {
	return map[string]any{
		"image": "axllent/mailpit:v1.31.3", "depends_on": map[string]any{"db": map[string]any{"condition": "service_started", "required": true}},
		"volumes": []any{map[string]any{"type": "volume", "source": "mailbox", "target": "/data"}},
	}
}

func TestUpRecreate(t *testing.T) {
	ctx := context.Background()
	t.Run("replaces a stopped container with the caller's definition, keeping its volumes, named and anonymous", func(t *testing.T) {
		f := newFixture(t)
		entry := mailpitDefinition()
		entry["volumes"] = []any{
			map[string]any{"type": "volume", "source": "mailbox", "target": "/data"},
			map[string]any{"type": "volume", "target": "/anon"},
		}
		f.docker.SetSharedService("mailpit", entry)
		f.start("mailpit")
		old := f.docker.ContainerNamed(project + "-mailpit-1")
		volumes := f.docker.Find("volume", map[string]string{})
		f.docker.SetStatus(old, "exited")
		definition, _ := f.stack.ReadDefinition(ctx)
		before := len(f.docker.Calls())

		err := f.stack.Up(ctx, shared.UpRequest{Definition: definition, Recreate: []string{"mailpit"}, DefinedBy: "someone-else",
			Published: map[string]map[int]int{"mailpit": {1025: 30001}}})
		if err != nil {
			t.Fatal(err)
		}
		id := f.docker.ContainerNamed(project + "-mailpit-1")
		if id == "" || id == old || f.docker.Exists("container", old) {
			t.Fatalf("mailpit = %q (was %q)", id, old)
		}
		if got := f.docker.Find("volume", map[string]string{}); strings.Join(got, ",") != strings.Join(volumes, ",") {
			t.Fatalf("volumes = %v, were %v", got, volumes)
		}
		var forced []string
		for _, call := range f.docker.Calls()[before:] {
			if strings.Contains(call, "--force-recreate") {
				forced = append(forced, call)
			}
		}
		if len(forced) != 1 || !strings.HasSuffix(forced[0], "--force-recreate --no-deps mailpit") || strings.Contains(forced[0], "--no-recreate") {
			t.Fatalf("forced calls = %q: one, of mailpit alone", forced)
		}
		if f.docker.Labels("container", id)[brand.SharedDefinedByLabel(ns)] != "someone-else" {
			t.Error("the new container does not carry the caller's definition")
		}
		if f.docker.Count("rm --force") != 0 {
			t.Error("a container was removed with docker rm; that would lose its anonymous volumes")
		}
	})

	t.Run("starts what the service needs first, without recreating it", func(t *testing.T) {
		f := newFixture(t)
		f.start("mailpit")
		db := f.docker.ContainerNamed(project + "-db-1")
		mailpit := f.docker.ContainerNamed(project + "-mailpit-1")
		f.docker.SetStatus(db, "exited")
		f.docker.SetStatus(mailpit, "exited")
		definition, _ := f.stack.ReadDefinition(ctx)
		before := len(f.docker.Calls())
		if err := f.stack.Up(ctx, shared.UpRequest{Definition: definition, Recreate: []string{"mailpit"}}); err != nil {
			t.Fatal(err)
		}
		if got := f.docker.ContainerNamed(project + "-db-1"); got != db {
			t.Fatalf("the dependency was recreated: %s -> %s", db, got)
		}
		if f.docker.Count("--no-recreate") < 2 {
			t.Error("the dependency was not started with --no-recreate")
		}
		calls := f.docker.Calls()[before:]
		var order []string
		for _, call := range calls {
			switch {
			case strings.Contains(call, "--no-recreate") && strings.Contains(call, " db"):
				order = append(order, "deps")
			case strings.Contains(call, "--force-recreate"):
				order = append(order, "recreate")
			}
		}
		if strings.Join(order, ",") != "deps,recreate" {
			t.Fatalf("order = %v", order)
		}
	})
}

func TestAnonymousVolumesAreTheirContainersAlone(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	entry := mailpitDefinition()
	entry["volumes"] = []any{map[string]any{"type": "volume", "target": "/anon"}, map[string]any{"type": "volume", "source": "mailbox", "target": "/data"}}
	f.docker.SetSharedService("mailpit", entry)
	f.start("mailpit")
	containers, err := f.stack.Containers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found, err := f.stack.AnonymousVolumes(ctx, containers)
	if err != nil || len(found) != 1 || !found[0].Anonymous {
		t.Fatalf("anonymous = %+v, %v", found, err)
	}
	t.Run("reaped only if Docker marks it anonymous", func(t *testing.T) {
		unlabelled := f.docker.Add("volume", map[string]string{})
		if err := f.stack.Reap(ctx, shared.Resource{Kind: "volume", ID: unlabelled, Anonymous: true}); err == nil || !strings.Contains(err.Error(), "not an anonymous volume") {
			t.Fatalf("err = %v", err)
		}
		if !f.docker.Exists("volume", unlabelled) {
			t.Fatal("a volume that is not anonymous was removed")
		}
		for _, container := range containers {
			if err := f.stack.Reap(ctx, shared.Resource{Kind: "container", ID: container.ID}); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.stack.Reap(ctx, shared.Resource{Kind: "volume", ID: found[0].ID, Anonymous: true}); err != nil {
			t.Fatal(err)
		}
		if f.docker.Exists("volume", found[0].ID) {
			t.Fatal("still there")
		}
	})
}

func TestReapToleratesAContainerThatVanishedBeforeItStopped(t *testing.T) {
	f := newFixture(t)
	f.start("mailpit")
	id := f.docker.ContainerNamed(project + "-mailpit-1")
	f.docker.FailCall("stop "+id, "Error response from daemon: No such container: "+id, 1, 1)
	if err := f.stack.Reap(context.Background(), shared.Resource{Kind: "container", ID: id}); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	f2 := newFixture(t)
	f2.start("mailpit")
	id2 := f2.docker.ContainerNamed(project + "-mailpit-1")
	f2.docker.FailCall("stop "+id2, "Error response from daemon: boom", 1, 1)
	if err := f2.stack.Reap(context.Background(), shared.Resource{Kind: "container", ID: id2}); err == nil {
		t.Fatal("another stop failure was swallowed")
	}
}

func TestStatusesSurviveOneBadContainer(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.start("mailpit")
	f.docker.SetStatus(f.docker.ContainerNamed(project+"-mailpit-1"), "levitating")
	labels := commandstest.Ownership(worktree.Ref{RepositoryID: repository, WorktreeID: worktreeA}, ns)
	labels[brand.SharedLabel(ns)] = "true"
	labels["com.docker.compose.project"] = project
	stray := f.docker.Add("container", labels)

	found, unproven, err := f.stack.Statuses(ctx, []string{"mailpit", "db", "other"})
	if err != nil {
		t.Fatal(err)
	}
	if got := found["mailpit"]; got.State != shared.Unknown || !strings.Contains(got.Detail, "levitating") {
		t.Errorf("mailpit = %+v", got)
	}
	if found["db"].State != shared.Running || found["other"].State != shared.Absent {
		t.Errorf("the others = %+v, %+v", found["db"], found["other"])
	}
	if len(unproven) != 1 || unproven[0].ID != stray || !strings.Contains(unproven[0].Error(), "Refusing to touch container") {
		t.Errorf("unproven = %+v", unproven)
	}
	// The strict reads still refuse, so a start never guesses.
	if _, err := f.stack.Instance(ctx, "db"); err == nil {
		t.Error("Instance accepted a stack with a container that cannot be proven")
	}
}

// checkout is a shared file at <parent>/acme/docker/compose.shared.yaml, one
// checkout of a repository, under its own parent directory.
type checkout struct {
	parent string
	root   string
	file   string
	// roots are the worktree roots the stack is given; newCheckout sets the
	// checkout's own.
	roots []string
}

// newCheckout makes a checkout with its own parent directory and the shared
// file's directory on disk.
func newCheckout(t *testing.T) checkout {
	t.Helper()
	parent := registrytest.TempDir(t)
	root := filepath.Join(parent, "acme")
	file := filepath.Join(root, "docker", "compose.shared.yaml")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(file), "db.env"), []byte("X=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return checkout{parent: parent, root: root, file: file, roots: []string{root}}
}

// recorder is a logger that keeps its warnings.
type recorder struct {
	logger.Silent
	warnings []string
}

// Warn keeps the message.
func (r *recorder) Warn(message string) { r.warnings = append(r.warnings, message) }

// stack is the shared stack as a command run in c would build it, over the
// given runner handler, with the given inherited environment and logger.
func (c checkout) stack(t *testing.T, handler fakerun.Handler, log logger.Logger, environ ...string) *shared.Stack {
	t.Helper()
	return shared.New(shared.Options{
		RepositoryID: repository, LabelNamespace: ns, ControlHome: filepath.Join(c.parent, "control"),
		ComposeFile: c.file, ProjectName: "acme", WorktreeRoots: c.roots, Environ: environ,
		Runner: &fakerun.Runner{Handler: handler}, Logger: log,
	})
}

// normalise replaces c's directories in text with stable names, so a message
// can be pinned whole.
func (c checkout) normalise(text string) string {
	return strings.NewReplacer(c.parent, "/work").Replace(text)
}

// TestDefinitionHashIsTheSameInEveryCheckout pins decision 16's promise: two
// checkouts of one repository, under different parent directories, either
// compute the same hash for the shared file or one of them refuses it, never a
// different hash that would show as a false "definition differs".
func TestDefinitionHashIsTheSameInEveryCheckout(t *testing.T) {
	first, second := newCheckout(t), newCheckout(t)
	hashIn := func(t *testing.T, c checkout, d *commandstest.Docker, environ ...string) (string, error) {
		t.Helper()
		definition, err := c.stack(t, d.Handle, logger.Silent{}, environ...).ReadDefinition(context.Background())
		if err != nil {
			return "", err
		}
		return definition.Hash("db")
	}
	service := func(volume, extra any) *commandstest.Docker {
		d := commandstest.NewDocker()
		entry := map[string]any{"image": "postgres", "volumes": []any{
			map[string]any{"type": "bind", "source": volume, "target": "/data"},
		}}
		if extra != nil {
			entry["environment"] = map[string]any{"HERE": extra}
		}
		d.SetSharedService("db", entry)
		return d
	}
	const remedy = "Use an absolute path outside every worktree, a named volume, or a value that is the same in every checkout."

	t.Run("an absolute bind source outside every worktree hashes alike", func(t *testing.T) {
		d := service("/srv/shared/db", nil)
		a, err := hashIn(t, first, d)
		if err != nil {
			t.Fatal(err)
		}
		b, err := hashIn(t, second, d)
		if err != nil {
			t.Fatal(err)
		}
		if a != b {
			t.Fatalf("hashes differ: %s, %s", a, b)
		}
	})

	t.Run("refuses a relative bind source that climbs out of the checkout, in these words", func(t *testing.T) {
		d := service(commandstest.ProjectGrandparent+"/shared-data", nil)
		for _, c := range []checkout{first, second} {
			_, err := hashIn(t, c, d)
			want := `docker/compose.shared.yaml reads differently in each checkout: services.db.volumes[0].source resolves to "/work/shared-data" here, from a relative path. ` + remedy
			if err == nil || c.normalise(err.Error()) != want {
				t.Fatalf("err = %v\nwant %s", err, want)
			}
			if len(want) > 300 {
				t.Fatalf("the message is %d characters; shared status cuts at 300", len(want))
			}
		}
	})

	t.Run("refuses ${PWD} in the file, and says so", func(t *testing.T) {
		d := service("/srv/shared/db", commandstest.WorkingDirectory)
		_, err := hashIn(t, first, d, "PWD=/home/dev/acme/docker")
		want := `services.db.environment.HERE resolves to "/home/dev/acme/docker" here, from ${PWD}.`
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v\nwant %s", err, want)
		}
	})

	t.Run("reports the first difference in sorted key order when two differ", func(t *testing.T) {
		d := service(commandstest.ProjectGrandparent+"/shared-data", commandstest.WorkingDirectory)
		_, err := hashIn(t, first, d, "PWD=/home/dev/acme/docker")
		if err == nil || !strings.Contains(err.Error(), "services.db.environment.HERE") || strings.Contains(err.Error(), "volumes[0]") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("shows the path of a difference safe to print", func(t *testing.T) {
		d := commandstest.NewDocker()
		d.SetSharedService("db", map[string]any{"image": "postgres", "environment": map[string]any{"A\u001b[2KB": commandstest.WorkingDirectory}})
		_, err := hashIn(t, first, d, "PWD=/home/dev/acme")
		if err == nil || strings.Contains(err.Error(), "\x1b") {
			t.Fatalf("err = %q", err)
		}
	})

	t.Run("does not check a file outside every worktree, where every checkout reads the same", func(t *testing.T) {
		outside := first
		outside.roots = []string{filepath.Join(first.parent, "elsewhere")}
		d := service(commandstest.ProjectGrandparent+"/shared-data", commandstest.WorkingDirectory)
		runner := &fakerun.Runner{Handler: d.Handle}
		stack := shared.New(shared.Options{
			RepositoryID: repository, LabelNamespace: ns, ControlHome: filepath.Join(outside.parent, "control"),
			ComposeFile: outside.file, ProjectName: "acme", WorktreeRoots: outside.roots, Environ: []string{"PWD=/x"},
			Runner: runner, Logger: logger.Silent{},
		})
		if _, err := stack.ReadDefinition(context.Background()); err != nil {
			t.Fatalf("err = %v", err)
		}
		if n := len(runner.Calls()); n != 1 {
			t.Fatalf("%d Compose calls, want 1: %v", n, runner.Argv())
		}
	})

	t.Run("leaves no scratch directory behind", func(t *testing.T) {
		d := service(commandstest.ProjectGrandparent+"/shared-data", nil)
		_, _ = hashIn(t, first, d)
		found, err := filepath.Glob(filepath.Join(first.parent, "control", "shared-probe-*"))
		if err != nil || len(found) != 0 {
			t.Fatalf("scratch directories left: %v, %v", found, err)
		}
	})

	t.Run("a second read Compose cannot make accepts the file with a warning naming Compose's error", func(t *testing.T) {
		d := service(commandstest.ProjectGrandparent+"/shared-data", nil)
		d.FailCall("shared-probe-", "env file ../../outside.env not found\nsecond line", 1, -1)
		log := &recorder{}
		if _, err := first.stack(t, d.Handle, log).ReadDefinition(context.Background()); err != nil {
			t.Fatalf("err = %v", err)
		}
		want := "Could not check whether docker/compose.shared.yaml reads the same from every checkout (env file ../../outside.env not found); " +
			"a relative path or ${PWD} there may give each checkout its own definition. " +
			"Use absolute paths there, or keep the files it reads under its own directory, to silence this."
		if len(log.warnings) != 1 || log.warnings[0] != want {
			t.Fatalf("warnings = %q\nwant %q", log.warnings, want)
		}
	})

	t.Run("a second read that is not JSON accepts the file with a warning", func(t *testing.T) {
		d := service("/srv/shared/db", nil)
		handler := func(ctx context.Context, spec run.Spec) (run.Result, error) {
			if strings.Contains(strings.Join(spec.Args, " "), "shared-probe-") {
				return run.Finish(spec, run.Result{Stdout: "not json"})
			}
			return d.Handle(ctx, spec)
		}
		log := &recorder{}
		if _, err := first.stack(t, handler, log).ReadDefinition(context.Background()); err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(log.warnings) != 1 || !strings.Contains(log.warnings[0], "Could not check whether docker/compose.shared.yaml") ||
			!strings.Contains(log.warnings[0], "decoding the second read") {
			t.Fatalf("warnings = %q", log.warnings)
		}
	})

	t.Run("prints a control character in the file's name safely", func(t *testing.T) {
		c := newCheckout(t)
		named := filepath.Join(filepath.Dir(c.file), "compose\x1b[2K.yaml")
		c.file = named
		d := service("/srv/shared/db", nil)
		d.FailCall("shared-probe-", "boom", 1, -1)
		log := &recorder{}
		if _, err := c.stack(t, d.Handle, log).ReadDefinition(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(log.warnings) != 1 || strings.Contains(log.warnings[0], "\x1b") {
			t.Fatalf("warnings = %q", log.warnings)
		}
	})

	t.Run("a scratch directory that cannot be made is a warning, not a failure", func(t *testing.T) {
		d := service("/srv/shared/db", nil)
		blocked := newCheckout(t)
		if err := os.WriteFile(filepath.Join(blocked.parent, "control"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		log := &recorder{}
		if _, err := blocked.stack(t, d.Handle, log).ReadDefinition(context.Background()); err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(log.warnings) != 1 || !strings.Contains(log.warnings[0], "Could not check whether") {
			t.Fatalf("warnings = %q", log.warnings)
		}
	})

	t.Run("a cancelled context is returned, not skipped", func(t *testing.T) {
		d := service("/srv/shared/db", nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		handler := func(callCtx context.Context, spec run.Spec) (run.Result, error) {
			if strings.Contains(strings.Join(spec.Args, " "), "shared-probe-") {
				cancel()
				return run.Result{}, context.Canceled
			}
			return d.Handle(callCtx, spec)
		}
		log := &recorder{}
		_, err := first.stack(t, handler, log).ReadDefinition(ctx)
		if !errors.Is(err, context.Canceled) || len(log.warnings) != 0 {
			t.Fatalf("err = %v, warnings = %q", err, log.warnings)
		}
	})
}
