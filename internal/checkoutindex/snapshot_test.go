package checkoutindex

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/checkoutindex/card"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/worktree"
	"github.com/sofired/treerig/internal/worktreestate"
)

// (rendering and planIndexUpdate are in render_test.go.)

const (
	testRepo      = "11111111-1111-4111-8111-111111111111"
	testOtherRepo = "33333333-3333-4333-8333-333333333333"
)

var testWorktreeIDs = []string{
	"22222222-2222-4222-8222-222222222221",
	"22222222-2222-4222-8222-222222222222",
	"22222222-2222-4222-8222-222222222223",
	"22222222-2222-4222-8222-222222222224",
}

// testRegistrations is feature-a, feature-b, and gone in one
// repository/domain, main in another.
func testRegistrations() []registry.RegisteredWorktree {
	return []registry.RegisteredWorktree{
		{RepositoryID: testRepo, WorktreeID: testWorktreeIDs[0], Slug: "feature-a", Domain: "acme.localhost"},
		{RepositoryID: testRepo, WorktreeID: testWorktreeIDs[1], Slug: "feature-b", Domain: "acme.localhost"},
		{RepositoryID: testRepo, WorktreeID: testWorktreeIDs[2], Slug: "gone", Domain: "acme.localhost"},
		{RepositoryID: testOtherRepo, WorktreeID: testWorktreeIDs[3], Slug: "main", Domain: "widgets.localhost"},
	}
}

// testIdentity is a live Git worktree for record, checked out at branch.
func testIdentity(record registry.RegisteredWorktree, branch string) registry.WorktreeIdentity {
	return registry.WorktreeIdentity{
		Branch:       branch,
		GitCommonDir: "/repo/.git",
		GitDir:       "/repo/.git/worktrees/" + record.Slug,
		RepositoryID: record.RepositoryID,
		Root:         "/Users/dev/worktrees/" + record.Slug,
		WorktreeID:   record.WorktreeID,
	}
}

// testCard is a well-formed card for record, naming project and carrying
// services.
func testCard(record registry.RegisteredWorktree, project string, services ...card.IndexService) card.IndexCard {
	if services == nil {
		services = []card.IndexService{}
	}
	return card.IndexCard{
		Project:        project,
		Domain:         record.Domain,
		Slug:           record.Slug,
		Host:           record.Slug + "." + record.Domain,
		Root:           "/Users/dev/last-seen/" + record.Slug,
		Branch:         "branch/" + record.Slug,
		LabelNamespace: "dev.treerig",
		Services:       services,
		UpdatedAt:      "2026-09-21T10:00:00.000Z",
	}
}

var (
	apiService  = card.IndexService{Name: "api", URL: "http://feature-a.acme.localhost", Label: "API", Purpose: "REST surface"}
	mailService = card.IndexService{Name: "mail", URL: "http://mail.feature-a.acme.localhost", Label: "Mailpit", Purpose: "Captured email"}
)

// hosted is the same services, on the host the registration says this
// checkout has.
func hosted(record registry.RegisteredWorktree, services ...card.IndexService) []card.IndexService {
	host := record.Slug + "." + record.Domain
	result := make([]card.IndexService, len(services))
	for i, service := range services {
		if service.Name == "api" {
			service.URL = "http://" + host
		} else {
			service.URL = "http://" + service.Name + "." + host
		}
		result[i] = service
	}
	return result
}

// testLine is one `docker ps` row for record's service, defaulting to a
// healthy, running container when stateAndStatus is omitted.
func testLine(record registry.RegisteredWorktree, service string, stateAndStatus ...string) string {
	state, status := "running", "Up 20 seconds (healthy)"
	if len(stateAndStatus) > 0 {
		state = stateAndStatus[0]
	}
	if len(stateAndStatus) > 1 {
		status = stateAndStatus[1]
	}
	return strings.Join([]string{state, status, record.RepositoryID, record.WorktreeID, service, "", "id-" + service}, "\t")
}

// mustParseInventory is worktreestate.ParseInventory over lines joined with
// newlines, failing the test on any parse error. (The parsing itself is
// tested in internal/worktreestate.)
func mustParseInventory(t *testing.T, lines ...string) []worktreestate.Container {
	t.Helper()
	records, err := worktreestate.ParseInventory(strings.Join(lines, "\n"))
	if err != nil {
		t.Fatalf("ParseInventory: %v", err)
	}
	return records
}

// baseSnapshotInput is BuildSnapshot's defaults. Each test starts here and
// mutates the fields it needs.
func baseSnapshotInput(t *testing.T) SnapshotInput {
	t.Helper()
	regs := testRegistrations()
	a, b, gone, foreign := regs[0], regs[1], regs[2], regs[3]
	cards := map[string]card.IndexCard{
		EntryKey(a.RepositoryID, a.WorktreeID):             testCard(a, "acme", hosted(a, apiService, mailService)...),
		EntryKey(b.RepositoryID, b.WorktreeID):             testCard(b, "acme", hosted(b, apiService)...),
		EntryKey(gone.RepositoryID, gone.WorktreeID):       testCard(gone, "acme", hosted(gone, apiService)...),
		EntryKey(foreign.RepositoryID, foreign.WorktreeID): testCard(foreign, "widgets"),
	}
	containers := mustParseInventory(t,
		testLine(a, "api"), testLine(a, "mail"), testLine(a, "db"),
		testLine(b, "api"), testLine(b, "db", "exited", "Exited (1) 1 minute ago"),
		testLine(gone, "api"),
		testLine(foreign, "web", "exited", "Exited (0) 1 hour ago"),
	)
	current := testRepo
	return SnapshotInput{
		Registrations:       regs,
		Cards:               cards,
		Identities:          []registry.WorktreeIdentity{testIdentity(a, "feature/a"), testIdentity(b, "feature/b")},
		CurrentRepositoryID: &current,
		Containers:          containers,
		States: map[string]worktreestate.Result{
			EntryKey(a.RepositoryID, a.WorktreeID):             {State: worktree.StateRunning},
			EntryKey(b.RepositoryID, b.WorktreeID):             {State: worktree.StatePartlyRunning},
			EntryKey(gone.RepositoryID, gone.WorktreeID):       {State: worktree.StateDirectoryMissing},
			EntryKey(foreign.RepositoryID, foreign.WorktreeID): {State: worktree.StateStopped},
		},
		GeneratedAt: "2026-09-21T12:00:00.000Z",
		RouterPort:  80,
	}
}

// findBySlug is the entry named slug, or nil if none is.
func findBySlug(entries []IndexEntry, slug string) *IndexEntry {
	for i, entry := range entries {
		if entry.Slug == slug {
			return &entries[i]
		}
	}
	return nil
}

// branchText reads an entry's branch for assertions, distinguishing a nil
// branch (nothing recorded one) from every other value.
func branchText(entry IndexEntry) string {
	if entry.Branch == nil {
		return "<nil>"
	}
	return *entry.Branch
}

// servicePairs is each service's label and availability, for assertions
// that only care about those two fields.
func servicePairs(services []IndexServiceLink) [][2]any {
	pairs := make([][2]any, len(services))
	for i, service := range services {
		pairs[i] = [2]any{service.Label, service.Available}
	}
	return pairs
}

// TestBuildSnapshot covers buildSnapshot.
func TestBuildSnapshot(t *testing.T) {
	t.Run("shows the state each registration was classified as, sorted by project with directory missing last", func(t *testing.T) {
		snapshot := BuildSnapshot(baseSnapshotInput(t))
		got := make([][3]string, len(snapshot.Entries))
		for i, e := range snapshot.Entries {
			got[i] = [3]string{e.Project, e.Slug, e.State.String()}
		}
		want := [][3]string{
			{"acme", "feature-a", "running"},
			{"acme", "feature-b", "partly running"},
			{"acme", "gone", "directory missing"},
			{"widgets", "main", "stopped"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("prefers live Git identity over the card, and falls back to the card for other repositories", func(t *testing.T) {
		entries := BuildSnapshot(baseSnapshotInput(t)).Entries
		if branchText(entries[0]) != "feature/a" || entries[0].Root != "/Users/dev/worktrees/feature-a" {
			t.Errorf("entries[0] = %+v", entries[0])
		}
		if branchText(entries[3]) != "branch/main" || entries[3].Root != "/Users/dev/last-seen/main" {
			t.Errorf("entries[3] = %+v", entries[3])
		}
		// A worktree whose directory is gone still shows where it was last seen.
		if branchText(entries[2]) != "branch/gone" || entries[2].Root != "/Users/dev/last-seen/gone" {
			t.Errorf("entries[2] = %+v", entries[2])
		}
	})

	t.Run("links only services whose container is available, and none for a worktree whose directory is missing", func(t *testing.T) {
		entries := BuildSnapshot(baseSnapshotInput(t)).Entries
		if want := [][2]any{{"API", true}, {"Mailpit", true}}; !reflect.DeepEqual(servicePairs(entries[0].Services), want) {
			t.Errorf("entries[0].services = %v, want %v", servicePairs(entries[0].Services), want)
		}
		if want := [][2]any{{"API", true}}; !reflect.DeepEqual(servicePairs(entries[1].Services), want) {
			t.Errorf("entries[1].services = %v, want %v", servicePairs(entries[1].Services), want)
		}
		if want := [][2]any{{"API", false}}; !reflect.DeepEqual(servicePairs(entries[2].Services), want) {
			t.Errorf("entries[2].services = %v, want %v", servicePairs(entries[2].Services), want)
		}
	})

	t.Run("carries the cause of an unknown state, and reads unknown for a registration nothing classified", func(t *testing.T) {
		input := baseSnapshotInput(t)
		regs := testRegistrations()
		input.States = map[string]worktreestate.Result{
			EntryKey(regs[0].RepositoryID, regs[0].WorktreeID): {State: worktree.StateUnknown, Hint: "Docker didn't answer: down"},
		}
		entries := BuildSnapshot(input).Entries
		if got := findBySlug(entries, "feature-a"); got.State != worktree.StateUnknown || got.Hint != "Docker didn't answer: down" {
			t.Errorf("feature-a = %+v", got)
		}
		if got := findBySlug(entries, "feature-b"); got.State != worktree.StateUnknown || got.Hint != "" {
			t.Errorf("feature-b = %+v, want unknown with no cause given", got)
		}
	})

	t.Run("keeps a detached HEAD detached instead of showing the card's last branch", func(t *testing.T) {
		regs := testRegistrations()
		input := baseSnapshotInput(t)
		input.Identities = []registry.WorktreeIdentity{testIdentity(regs[0], "")}
		detached := findBySlug(BuildSnapshot(input).Entries, "feature-a")
		if detached == nil {
			t.Fatal("feature-a not found")
		}
		if branchText(*detached) != "" {
			t.Errorf("branch = %q, want empty (detached)", branchText(*detached))
		}
	})

	t.Run("sorts a detached worktree by its slug, having no branch name to sort by", func(t *testing.T) {
		// `||` in the comparator, not `??`: an empty branch is not a sort key.
		regs := testRegistrations()
		input := baseSnapshotInput(t)
		input.Identities = []registry.WorktreeIdentity{testIdentity(regs[0], ""), testIdentity(regs[1], "aaa")}
		entries := BuildSnapshot(input).Entries
		got := make([]string, len(entries))
		for i, e := range entries {
			got[i] = e.Slug
		}
		if want := []string{"feature-b", "feature-a", "gone", "main"}; !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("links a scaled service while any replica answers", func(t *testing.T) {
		// Every replica carries the same Compose service label; the state
		// column says a replica is down, and the link still reaches the
		// ones that are up.
		regs := testRegistrations()
		a := regs[0]
		input := baseSnapshotInput(t)
		input.Containers = mustParseInventory(t,
			testLine(a, "api"), testLine(a, "api", "exited", "Exited (1) 1 minute ago"), testLine(a, "mail"),
		)
		scaled := findBySlug(BuildSnapshot(input).Entries, "feature-a")
		if want := [][2]any{{"API", true}, {"Mailpit", true}}; scaled == nil || !reflect.DeepEqual(servicePairs(scaled.Services), want) {
			t.Errorf("services = %v, want %v", scaled, want)
		}
	})

	t.Run("does not link a declared service that has no container at all", func(t *testing.T) {
		// `up --services api` on a worktree that also indexes mail: one
		// container, two services.
		regs := testRegistrations()
		a := regs[0]
		input := baseSnapshotInput(t)
		input.Containers = mustParseInventory(t, testLine(a, "api"))
		partial := findBySlug(BuildSnapshot(input).Entries, "feature-a")
		if want := [][2]any{{"API", true}, {"Mailpit", false}}; !reflect.DeepEqual(servicePairs(partial.Services), want) {
			t.Errorf("services = %v, want %v", servicePairs(partial.Services), want)
		}
	})

	t.Run("records whether Docker could be asked at all, so the page can say so", func(t *testing.T) {
		if got := BuildSnapshot(baseSnapshotInput(t)).DockerReachable; !got {
			t.Errorf("DockerReachable = %v, want true", got)
		}
		emptyInput := baseSnapshotInput(t)
		emptyInput.Containers = []worktreestate.Container{}
		if got := BuildSnapshot(emptyInput).DockerReachable; !got {
			t.Errorf("DockerReachable (empty containers) = %v, want true", got)
		}
		nilInput := baseSnapshotInput(t)
		nilInput.Containers = nil
		if got := BuildSnapshot(nilInput).DockerReachable; got {
			t.Errorf("DockerReachable (nil containers) = %v, want false", got)
		}
	})

	t.Run("drops a service the card points somewhere other than this worktree", func(t *testing.T) {
		// Every field in a card is self-attested, and the card lives in a
		// home every lifecycle hook is handed. Only the registration says
		// where a checkout is, so a link anywhere else is not published at
		// all.
		regs := testRegistrations()
		a := regs[0]
		elsewhere := testCard(a, "acme",
			card.IndexService{Name: apiService.Name, URL: "http://attacker.example.com/login", Label: apiService.Label, Purpose: apiService.Purpose},
			card.IndexService{Name: "real", URL: "http://feature-a.acme.localhost/real", Label: apiService.Label, Purpose: apiService.Purpose},
		)
		elsewhere.Host = "attacker.example.com"
		input := baseSnapshotInput(t)
		input.Cards = map[string]card.IndexCard{EntryKey(a.RepositoryID, a.WorktreeID): elsewhere}
		entries := BuildSnapshot(input).Entries
		row := findBySlug(entries, "feature-a")
		if row == nil {
			t.Fatal("feature-a not found")
		}
		gotURLs := make([]string, len(row.Services))
		for i, s := range row.Services {
			gotURLs[i] = s.URL
		}
		if want := []string{"http://feature-a.acme.localhost/real"}; !reflect.DeepEqual(gotURLs, want) {
			t.Errorf("service urls = %v, want %v", gotURLs, want)
		}

		// Every worktree host is loopback, so another port is another
		// listener on this machine — and another scheme is not a routed
		// service at all.
		offPortInput := baseSnapshotInput(t)
		offPortInput.RouterPort = 80
		offPortInput.Cards = map[string]card.IndexCard{EntryKey(a.RepositoryID, a.WorktreeID): testCard(a, "acme",
			card.IndexService{Name: apiService.Name, URL: "http://feature-a.acme.localhost:11434/api", Label: apiService.Label, Purpose: apiService.Purpose},
			card.IndexService{Name: "tls", URL: "https://feature-a.acme.localhost/", Label: apiService.Label, Purpose: apiService.Purpose},
		)}
		offPort := findBySlug(BuildSnapshot(offPortInput).Entries, "feature-a")
		if offPort == nil || len(offPort.Services) != 0 {
			t.Errorf("offPort.services = %+v, want none", offPort)
		}

		if html := RenderIndexHTML(IndexSnapshot{Entries: entries, GeneratedAt: "now", DockerReachable: true}); strings.Contains(html, "attacker.example.com") {
			t.Error("rendered page names the attacker's host")
		}
	})

	t.Run("shows a registration that never produced a card under its domain, with no links", func(t *testing.T) {
		input := baseSnapshotInput(t)
		input.Cards = map[string]card.IndexCard{}
		entries := BuildSnapshot(input).Entries
		main := findBySlug(entries, "main")
		if main == nil || main.Project != "widgets.localhost" || len(main.Services) != 0 {
			t.Errorf("main = %+v, want project widgets.localhost with no services", main)
		}
		if main.Branch != nil {
			t.Errorf("branch = %q, want nil", branchText(*main))
		}
	})
}
