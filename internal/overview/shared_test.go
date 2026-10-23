package overview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/overview/card"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/worktree"
	"github.com/sofired/treerig/internal/worktreestate"
)

// sharedRecord is mailpit's record in the test repository: its web UI on
// container port 8025, pinned to 30010, and a raw SMTP port.
func sharedRecord(name string) registry.SharedRecord {
	return registry.SharedRecord{
		RepositoryID: testRepo, Name: name, Domain: "acme.localhost",
		Ports: map[int]int{8025: 30010, 1025: 30011}, Routed: 8025,
	}
}

// sharedLine is one `docker ps` row for a container of a repository's shared
// services: no worktree label, and the shared label set.
func sharedLine(service string, stateAndStatus ...string) string {
	state, status := "running", "Up 20 seconds (healthy)"
	if len(stateAndStatus) > 0 {
		state = stateAndStatus[0]
	}
	if len(stateAndStatus) > 1 {
		status = stateAndStatus[1]
	}
	return strings.Join([]string{state, status, testRepo, "", service, "true", "id-" + service}, "\t")
}

// TestSharedServicesInTheIndex covers the group of shared-service web UIs
// (docs/design/routing.md, decision 20).
func TestSharedServicesInTheIndex(t *testing.T) {
	t.Run("reads a shared container, which has no worktree label, instead of refusing the inventory", func(t *testing.T) {
		parsed := mustParseInventory(t, sharedLine("mailpit"), testLine(testRegistrations()[0], "api"))
		if !parsed[0].Shared || parsed[0].WorktreeID != "" || parsed[0].Service != "mailpit" || !parsed[0].Available() {
			t.Errorf("shared record = %+v", parsed[0])
		}
		if parsed[1].Shared {
			t.Errorf("a worktree's container read as shared: %+v", parsed[1])
		}
	})

	t.Run("refuses a worktree container without a worktree, and a shared one that names one", func(t *testing.T) {
		for _, line := range []string{
			strings.Join([]string{"running", "Up", testRepo, "", "api", "", "x"}, "\t"),
			strings.Join([]string{"running", "Up", testRepo, testWorktreeIDs[0], "mailpit", "true", "x"}, "\t"),
			strings.Join([]string{"running", "Up", testRepo, testWorktreeIDs[0], "api", "yes", "x"}, "\t"),
		} {
			if _, err := worktreestate.ParseInventory(line); err == nil {
				t.Errorf("%q was accepted", line)
			}
		}
	})

	t.Run("lists a routed record at its shared host, with the project its worktrees gave", func(t *testing.T) {
		input := baseSnapshotInput(t)
		input.SharedRecords = []registry.SharedRecord{sharedRecord("mailpit")}
		input.Containers = append(input.Containers, mustParseInventory(t, sharedLine("mailpit"))...)
		shared := BuildSnapshot(input).Shared
		if len(shared) != 1 {
			t.Fatalf("shared = %+v", shared)
		}
		got := shared[0]
		if got.Host != "mailpit.shared.acme.localhost" || got.URL != "http://mailpit.shared.acme.localhost" ||
			got.Project != "acme" || !got.Available {
			t.Errorf("shared = %+v", got)
		}
	})

	t.Run("folds a router port other than 80 into the URL", func(t *testing.T) {
		input := baseSnapshotInput(t)
		input.RouterPort = 8080
		input.SharedRecords = []registry.SharedRecord{sharedRecord("mailpit")}
		if got := BuildSnapshot(input).Shared[0].URL; got != "http://mailpit.shared.acme.localhost:8080" {
			t.Errorf("url = %q", got)
		}
	})

	t.Run("leaves out a record that routes nothing", func(t *testing.T) {
		record := sharedRecord("smtp")
		record.Routed = 0
		input := baseSnapshotInput(t)
		input.SharedRecords = []registry.SharedRecord{record}
		if shared := BuildSnapshot(input).Shared; len(shared) != 0 {
			t.Errorf("shared = %+v", shared)
		}
	})

	t.Run("is not available while its container is stopped or absent, or Docker could not be asked", func(t *testing.T) {
		input := baseSnapshotInput(t)
		input.SharedRecords = []registry.SharedRecord{sharedRecord("mailpit")}
		if BuildSnapshot(input).Shared[0].Available {
			t.Error("available with no container")
		}
		input.Containers = append(input.Containers, mustParseInventory(t, sharedLine("mailpit", "exited", "Exited (0) 1 minute ago"))...)
		if BuildSnapshot(input).Shared[0].Available {
			t.Error("available with a stopped container")
		}
	})

	t.Run("does not take a worktree's service of the same name for the shared one", func(t *testing.T) {
		input := baseSnapshotInput(t)
		input.SharedRecords = []registry.SharedRecord{sharedRecord("api")}
		if BuildSnapshot(input).Shared[0].Available {
			t.Error("a worktree's api container made the shared api available")
		}
	})

	t.Run("does not count a shared container toward any worktree's state", func(t *testing.T) {
		input := baseSnapshotInput(t)
		before := BuildSnapshot(input).Entries
		input.Containers = append(input.Containers, mustParseInventory(t, sharedLine("mailpit"), sharedLine("db", "exited", "Exited (1)"))...)
		after := BuildSnapshot(input).Entries
		for i := range before {
			if before[i].State != after[i].State {
				t.Errorf("%s: %s became %s", before[i].Slug, before[i].State, after[i].State)
			}
		}
	})

	t.Run("sorts by domain, project, then name", func(t *testing.T) {
		other := sharedRecord("mailpit")
		other.RepositoryID, other.Domain = testOtherRepo, "widgets.localhost"
		input := baseSnapshotInput(t)
		input.SharedRecords = []registry.SharedRecord{other, sharedRecord("minio"), sharedRecord("mailpit")}
		var got []string
		for _, service := range BuildSnapshot(input).Shared {
			got = append(got, service.Domain+"/"+service.Name)
		}
		want := "acme.localhost/mailpit acme.localhost/minio widgets.localhost/mailpit"
		if strings.Join(got, " ") != want {
			t.Errorf("order = %v", got)
		}
	})
}

// sharedSnapshot is a snapshot with one worktree and one shared UI.
func sharedSnapshot(available bool) IndexSnapshot {
	snapshot := renderTestSnapshot()
	snapshot.Shared = []IndexSharedService{{
		RepositoryID: testRepo, Name: "mailpit", Project: "acme", Domain: "acme.localhost",
		Host: "mailpit.shared.acme.localhost", URL: "http://mailpit.shared.acme.localhost", Available: available,
	}}
	return snapshot
}

// TestSharedServicesRendering covers the page.
func TestSharedServicesRendering(t *testing.T) {
	pinHomeDir(t, "/Users/dev")

	t.Run("a page without shared services is unchanged", func(t *testing.T) {
		if strings.Contains(RenderIndexHTML(renderTestSnapshot()), "Shared services") {
			t.Error("the group appeared with nothing to list")
		}
	})

	t.Run("lists a running UI as a link in a group of its own after the worktrees", func(t *testing.T) {
		html := RenderIndexHTML(sharedSnapshot(true))
		group := strings.Index(html, `id="shared-services"`)
		worktrees := strings.Index(html, `id="worktrees-0"`)
		if group < 0 || worktrees < 0 || group < worktrees {
			t.Fatalf("group at %d, worktrees at %d", group, worktrees)
		}
		for _, want := range []string{
			`<h2 id="shared-services">Shared services</h2>`,
			`href="http://mailpit.shared.acme.localhost"`,
			`<th scope="row"><strong>mailpit</strong></th>`,
		} {
			if !strings.Contains(html, want) {
				t.Errorf("page lacks %q", want)
			}
		}
	})

	t.Run("greys out a UI nothing answers for, and links it live when Docker was not asked", func(t *testing.T) {
		if html := RenderIndexHTML(sharedSnapshot(false)); !strings.Contains(html, "service-disabled") || !strings.Contains(html, "(unavailable)") {
			t.Error("a stopped UI is linked")
		}
		unchecked := sharedSnapshot(false)
		unchecked.DockerReachable = false
		if html := RenderIndexHTML(unchecked); !strings.Contains(html, `href="http://mailpit.shared.acme.localhost"`) {
			t.Error("an unchecked UI is greyed out")
		}
	})

	t.Run("escapes what a record could carry", func(t *testing.T) {
		snapshot := sharedSnapshot(true)
		snapshot.Shared[0].Project = `<script>alert(1)</script>`
		if strings.Contains(RenderIndexHTML(snapshot), "<script>alert(1)") {
			t.Error("a project name reached the page unescaped")
		}
	})

	t.Run("shows the group even before any worktree is indexed", func(t *testing.T) {
		snapshot := sharedSnapshot(true)
		snapshot.Entries = nil
		if !strings.Contains(RenderIndexHTML(snapshot), `id="shared-services"`) {
			t.Error("the group is missing")
		}
		if !strings.Contains(RenderIndexHTML(snapshot), `class="empty"`) {
			t.Error("the empty-state paragraph is missing")
		}
	})
}

// pinSharedUI records mailpit's web UI in controlHome for the test repository.
func pinSharedUI(t *testing.T, controlHome string) {
	t.Helper()
	routed := 8025
	if _, err := registry.NewStore(controlHome, testRepo).SyncSharedPorts(context.Background(), registry.Writer{Invocation: "test", UserHome: t.TempDir()}, registry.SharedPortPlan{
		Name: "mailpit", Domain: "acme.localhost", Ports: []int{8025}, Routed: &routed,
		Range: layout.PortRange{Low: 30000, High: 30099}, Probe: registrytest.FreeProbe{},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestServiceListsSharedServices covers the service reading the records and
// the shared containers off disk and Docker.
func TestServiceListsSharedServices(t *testing.T) {
	regs := testRegistrations()
	a := regs[0]

	t.Run("lists a routed UI from its record, available when its shared container runs", func(t *testing.T) {
		docker := &dockerFake{stdout: dockerLine(a, "api", "running", "Up 20 seconds (healthy)") + "\n" + sharedLine("mailpit")}
		h := newServiceHarness(t, docker, []registry.RegisteredWorktree{a}, []registry.WorktreeIdentity{testIdentity(a, "feature/a")})
		pinSharedUI(t, h.controlHome)
		snapshot, err := h.service.Refresh(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !snapshot.DockerReachable || len(snapshot.Shared) != 1 {
			t.Fatalf("snapshot = %+v", snapshot)
		}
		if got := snapshot.Shared[0]; !got.Available || got.URL != "http://mailpit.shared.acme.localhost:8080" {
			t.Errorf("shared = %+v", got)
		}
		page, err := os.ReadFile(h.service.opts.IndexFile)
		if err != nil || !strings.Contains(string(page), `href="http://mailpit.shared.acme.localhost:8080"`) {
			t.Errorf("page lacks the link: %v", err)
		}
	})

	t.Run("a shared container does not make Docker unreadable for the worktrees", func(t *testing.T) {
		docker := &dockerFake{stdout: dockerLine(a, "api", "running", "Up 20 seconds (healthy)") + "\n" + sharedLine("mailpit")}
		h := newServiceHarness(t, docker, []registry.RegisteredWorktree{a}, []registry.WorktreeIdentity{testIdentity(a, "feature/a")})
		writeTestCard(t, h.controlHome, a, []card.IndexService{{Name: "api", URL: "http://feature-a.acme.localhost:8080", Label: "API", Purpose: "REST"}})
		snapshot, err := h.service.Refresh(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !snapshot.DockerReachable || snapshot.Entries[0].State != worktree.StateRunning {
			t.Errorf("snapshot = %+v", snapshot)
		}
	})

	t.Run("asks Docker for the shared label alongside the others", func(t *testing.T) {
		docker := &dockerFake{}
		h := newServiceHarness(t, docker, []registry.RegisteredWorktree{a}, nil)
		if _, err := h.service.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		format := docker.calls[0].Args[len(docker.calls[0].Args)-1]
		if !strings.HasSuffix(format, "{{.Label \"dev.treerig.shared\"}}\t{{.ID}}") || strings.Count(format, "\t") != 6 {
			t.Errorf("format = %q", format)
		}
	})

	t.Run("leaves out a record it cannot read, with a warning, and still writes the page", func(t *testing.T) {
		docker := &dockerFake{}
		h := newServiceHarness(t, docker, []registry.RegisteredWorktree{a}, nil)
		dir := layout.SharedLayout(h.controlHome, testRepo).Dir
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "mailpit.json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := h.service.Refresh(context.Background())
		if err != nil || len(snapshot.Shared) != 0 {
			t.Fatalf("snapshot = %+v, %v", snapshot.Shared, err)
		}
		if !strings.Contains(strings.Join(h.logger.Lines, "\n"), "leaves out a shared service whose record could not be read") {
			t.Errorf("log = %v", h.logger.Lines)
		}
	})
}
