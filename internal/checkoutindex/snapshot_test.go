package checkoutindex

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/checkoutindex/card"
	"github.com/sofired/berth/internal/registry"
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
		LabelNamespace: "dev.berth",
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
	return strings.Join([]string{state, status, record.RepositoryID, record.WorktreeID, service}, "\t")
}

// mustParseInventory is ParseContainerInventory over lines joined with
// newlines, failing the test on any parse error.
func mustParseInventory(t *testing.T, lines ...string) []ContainerRecord {
	t.Helper()
	records, err := ParseContainerInventory(strings.Join(lines, "\n"))
	if err != nil {
		t.Fatalf("ParseContainerInventory: %v", err)
	}
	return records
}

// TestParseContainerInventory covers parseContainerInventory.
func TestParseContainerInventory(t *testing.T) {
	regs := testRegistrations()
	r0, r1 := regs[0], regs[1]

	t.Run("marks a container available only when running and not unhealthy or still starting", func(t *testing.T) {
		parsed := mustParseInventory(t,
			testLine(r0, "api"),
			testLine(r0, "mail", "running", "Up 3 seconds (health: starting)"),
			testLine(r1, "api", "exited", "Exited (0) 2 minutes ago"),
			testLine(r1, "mail", "running", "Up 5 seconds (unhealthy)"),
			testLine(r1, "db", "running", "Up 5 seconds"),
		)
		got := make([][2]any, len(parsed))
		for i, c := range parsed {
			got[i] = [2]any{c.Service, c.Available}
		}
		want := [][2]any{{"api", true}, {"mail", false}, {"api", false}, {"mail", false}, {"db", true}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("rejects a line that does not have exactly five fields", func(t *testing.T) {
		if _, err := ParseContainerInventory("running\tUp\tonly-three"); err == nil {
			t.Fatal("expected an error")
		} else if !strings.Contains(err.Error(), "unrecognized container inventory line") {
			t.Errorf("error = %q", err.Error())
		}
		// A tab inside a status would shift every field right, and the row
		// would read Stopped rather than being refused.
		if _, err := ParseContainerInventory("running\tUp 2 seconds\t(healthy)\tr\tw\tapi"); err == nil {
			t.Fatal("expected an error")
		} else if !strings.Contains(err.Error(), "unrecognized container inventory line") {
			t.Errorf("error = %q", err.Error())
		}
	})

	// Pinned because SnapshotInput.Containers relies on the nil-vs-empty-slice
	// distinction — nil means Docker could not be asked at all — and a
	// caller merging several namespaces' inventories could otherwise turn
	// "asked, found nothing" into "never asked" by accident.
	t.Run("never returns a nil slice on success, even for empty output", func(t *testing.T) {
		records, err := ParseContainerInventory("")
		if err != nil {
			t.Fatalf("ParseContainerInventory(\"\"): %v", err)
		}
		if records == nil {
			t.Error("got a nil slice, want a non-nil empty slice")
		}
		if len(records) != 0 {
			t.Errorf("got %v, want empty", records)
		}
	})
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
		GeneratedAt:         "2026-09-21T12:00:00.000Z",
		RouterPort:          80,
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
	t.Run("derives running, partial, stopped, and missing, sorted by project with missing last", func(t *testing.T) {
		snapshot := BuildSnapshot(baseSnapshotInput(t))
		got := make([][3]string, len(snapshot.Entries))
		for i, e := range snapshot.Entries {
			got[i] = [3]string{e.Project, e.Slug, string(e.Status)}
		}
		want := [][3]string{
			{"acme", "feature-a", "running"},
			{"acme", "feature-b", "partial"},
			{"acme", "gone", "missing"},
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
		// A missing checkout still shows where it was last seen.
		if branchText(entries[2]) != "branch/gone" || entries[2].Root != "/Users/dev/last-seen/gone" {
			t.Errorf("entries[2] = %+v", entries[2])
		}
	})

	t.Run("links only services whose container is available, and none for a missing checkout", func(t *testing.T) {
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

	t.Run("reports unknown when Docker could not be asked, and stopped when a worktree has no containers", func(t *testing.T) {
		unknownInput := baseSnapshotInput(t)
		unknownInput.Containers = nil
		unknown := BuildSnapshot(unknownInput).Entries
		gotUnknown := make([]string, len(unknown))
		for i, e := range unknown {
			gotUnknown[i] = string(e.Status)
		}
		if want := []string{"unknown", "unknown", "missing", "unknown"}; !reflect.DeepEqual(gotUnknown, want) {
			t.Errorf("got %v, want %v", gotUnknown, want)
		}

		emptyInput := baseSnapshotInput(t)
		emptyInput.Containers = []ContainerRecord{}
		empty := BuildSnapshot(emptyInput).Entries
		gotEmpty := make([]string, len(empty))
		for i, e := range empty {
			gotEmpty[i] = string(e.Status)
		}
		if want := []string{"stopped", "stopped", "missing", "stopped"}; !reflect.DeepEqual(gotEmpty, want) {
			t.Errorf("got %v, want %v", gotEmpty, want)
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

	t.Run("sorts a detached checkout by its slug, having no branch name to sort by", func(t *testing.T) {
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

	t.Run("is not Running while one replica of a scaled service is down", func(t *testing.T) {
		// Every replica carries the same Compose service label, so a
		// service is only answering when all of its containers are.
		regs := testRegistrations()
		a := regs[0]
		input := baseSnapshotInput(t)
		input.Containers = mustParseInventory(t,
			testLine(a, "api"), testLine(a, "api", "exited", "Exited (1) 1 minute ago"), testLine(a, "mail"),
		)
		scaled := findBySlug(BuildSnapshot(input).Entries, "feature-a")
		if scaled == nil || scaled.Status != StatusPartial {
			t.Errorf("scaled = %+v, want status partial", scaled)
		}
	})

	t.Run("is not Running while a declared service has no container at all", func(t *testing.T) {
		// `up --services api` on a worktree that also indexes mail: one
		// container, two services. A green pill there would say every
		// service answers.
		regs := testRegistrations()
		a := regs[0]
		input := baseSnapshotInput(t)
		input.Containers = mustParseInventory(t, testLine(a, "api"))
		partial := findBySlug(BuildSnapshot(input).Entries, "feature-a")
		if partial == nil || partial.Status != StatusPartial {
			t.Errorf("partial = %+v, want status partial", partial)
		}
		if want := [][2]any{{"API", true}, {"Mailpit", false}}; !reflect.DeepEqual(servicePairs(partial.Services), want) {
			t.Errorf("services = %v, want %v", servicePairs(partial.Services), want)
		}
	})

	t.Run("records whether Docker could be asked at all, so the page can say so", func(t *testing.T) {
		if got := BuildSnapshot(baseSnapshotInput(t)).DockerReachable; !got {
			t.Errorf("DockerReachable = %v, want true", got)
		}
		emptyInput := baseSnapshotInput(t)
		emptyInput.Containers = []ContainerRecord{}
		if got := BuildSnapshot(emptyInput).DockerReachable; !got {
			t.Errorf("DockerReachable (empty containers) = %v, want true", got)
		}
		nilInput := baseSnapshotInput(t)
		nilInput.Containers = nil
		if got := BuildSnapshot(nilInput).DockerReachable; got {
			t.Errorf("DockerReachable (nil containers) = %v, want false", got)
		}
	})

	t.Run("is not Stopped while a scaled service is partly up, and says the same in the link", func(t *testing.T) {
		// One declared service, three replicas, one down: the legend says
		// Stopped means none answer, and two of them do.
		regs := testRegistrations()
		a := regs[0]
		input := baseSnapshotInput(t)
		input.Cards = map[string]card.IndexCard{EntryKey(a.RepositoryID, a.WorktreeID): testCard(a, "acme", hosted(a, apiService)...)}
		input.Containers = mustParseInventory(t,
			testLine(a, "api"), testLine(a, "api"), testLine(a, "api", "exited", "Exited (1) 1 minute ago"),
		)
		partly := findBySlug(BuildSnapshot(input).Entries, "feature-a")
		if partly == nil || partly.Status != StatusPartial {
			t.Errorf("partly = %+v, want status partial", partly)
		}
		// The pill and the link cannot contradict each other.
		if want := []bool{true}; !reflect.DeepEqual(availabilities(partly.Services), want) {
			t.Errorf("availabilities = %v, want %v", availabilities(partly.Services), want)
		}
	})

	t.Run("drops a service the card points somewhere other than this checkout", func(t *testing.T) {
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

// availabilities is each service's Available flag, in order.
func availabilities(services []IndexServiceLink) []bool {
	result := make([]bool, len(services))
	for i, s := range services {
		result[i] = s.Available
	}
	return result
}
