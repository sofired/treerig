package checkoutindex

// Service is tied to a lock.Scope. Plus new tests for #15-#18.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/checkoutindex/card"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/run/osrun"
)

// fakeRegistry is a checkoutindex.Registry double.
type fakeRegistry struct {
	registrations []registry.RegisteredWorktree
	err           error
}

// ListAll answers f.registrations, or f.err.
func (f *fakeRegistry) ListAll(context.Context) ([]registry.RegisteredWorktree, error) {
	return f.registrations, f.err
}

// fakeIdentities is a checkoutindex.Identities double.
type fakeIdentities struct {
	repositoryID        string
	repositoryIDPresent bool
	repositoryIDErr     error
	// outside makes Repository answer that the directory is in no Git work tree.
	outside       bool
	identities    []registry.WorktreeIdentity
	identitiesErr error
}

// Repository answers the fixed values f was built with.
func (f *fakeIdentities) Repository(context.Context, string) (string, bool, bool, error) {
	if f.outside {
		return "", false, false, nil
	}
	return f.repositoryID, f.repositoryIDPresent, true, f.repositoryIDErr
}

// ActiveIdentities answers f.identities, or f.identitiesErr.
func (f *fakeIdentities) ActiveIdentities(context.Context) ([]registry.WorktreeIdentity, error) {
	return f.identities, f.identitiesErr
}

// dockerFake is a run.Runner double answering `docker ps` calls.
type dockerFake struct {
	mu      sync.Mutex
	calls   []run.Spec
	stdout  string
	exit    int
	missing bool
	failure error
	timeout bool
}

// Run records spec and answers per whichever of d.failure, d.missing, or
// d.stdout/d.exit/d.timeout was set.
func (d *dockerFake) Run(ctx context.Context, spec run.Spec) (run.Result, error) {
	d.mu.Lock()
	d.calls = append(d.calls, spec)
	d.mu.Unlock()
	if d.failure != nil {
		return run.Result{}, d.failure
	}
	if d.missing {
		return run.Result{}, &run.StartError{ExitCode: 127, Detail: "ENOENT"}
	}
	return run.Result{Stdout: d.stdout, ExitCode: d.exit, TimedOut: d.timeout}, nil
}

// Which always answers true: every test here assumes docker is on PATH.
func (d *dockerFake) Which(string) bool { return true }

// Calls is every run.Spec recorded so far, in call order.
func (d *dockerFake) Calls() []run.Spec {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]run.Spec(nil), d.calls...)
}

// serviceHarness is one Service wired to fakes, plus the temp control home
// under it.
type serviceHarness struct {
	controlHome string
	docker      *dockerFake
	logger      *logger.Recording
	service     *Service
}

// newServiceHarness builds a Service wired to docker, registrations, and
// identities, under a fresh temp control home, index file, and lock.
func newServiceHarness(t *testing.T, docker *dockerFake, registrations []registry.RegisteredWorktree, identities []registry.WorktreeIdentity) *serviceHarness {
	t.Helper()
	controlHome := t.TempDir()
	log := &logger.Recording{}
	svc := NewService(Options{
		ControlHome: controlHome,
		RouterPort:  8080,
		IndexFile:   filepath.Join(t.TempDir(), "index.html"),
		Lock:        lock.Scope{Target: filepath.Join(t.TempDir(), "lock"), Description: "shared router"},
		Registry:    &fakeRegistry{registrations: registrations},
		Identities: &fakeIdentities{
			repositoryID: testRepo, repositoryIDPresent: true, identities: identities,
		},
		IdentityRoot: "/work/acme",
		Runner:       docker,
		Logger:       log,
		Now:          func() time.Time { return time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC) },
	})
	return &serviceHarness{controlHome: controlHome, docker: docker, logger: log, service: svc}
}

// writeTestCard writes a valid index card for record under controlHome, with
// the fixed branch/root/labelNamespace values every test in this file shares.
func writeTestCard(t *testing.T, controlHome string, record registry.RegisteredWorktree, services []card.IndexService) {
	t.Helper()
	home := filepath.Dir(card.IndexCardPath(controlHome, record.RepositoryID, record.WorktreeID))
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := card.WriteIndexCard(controlHome, record.RepositoryID, record.WorktreeID, card.IndexCard{
		Project: "acme", Domain: record.Domain, Slug: record.Slug,
		Host: fmt.Sprintf("%s.%s", record.Slug, record.Domain), Root: "/last/seen/" + record.Slug,
		Branch: "branch/" + record.Slug, LabelNamespace: "dev.treerig", Services: services,
		UpdatedAt: "2026-09-21T10:00:00.000Z",
	}); err != nil {
		t.Fatalf("WriteIndexCard: %v", err)
	}
}

// withNamespace is record with an explicit LabelNamespace, as a registration
// written under a namespace the config recorded rather than left to the
// registry's own default.
func withNamespace(record registry.RegisteredWorktree, namespace string) registry.RegisteredWorktree {
	record.LabelNamespace = &namespace
	return record
}

// writeTestCardWithNamespace is writeTestCard with an explicit label
// namespace, for a card that is the only place recording one.
func writeTestCardWithNamespace(t *testing.T, controlHome string, record registry.RegisteredWorktree, namespace string, services []card.IndexService) {
	t.Helper()
	home := filepath.Dir(card.IndexCardPath(controlHome, record.RepositoryID, record.WorktreeID))
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := card.WriteIndexCard(controlHome, record.RepositoryID, record.WorktreeID, card.IndexCard{
		Project: "acme", Domain: record.Domain, Slug: record.Slug,
		Host: fmt.Sprintf("%s.%s", record.Slug, record.Domain), Root: "/last/seen/" + record.Slug,
		Branch: "branch/" + record.Slug, LabelNamespace: namespace, Services: services,
		UpdatedAt: "2026-09-21T10:00:00.000Z",
	}); err != nil {
		t.Fatalf("WriteIndexCard: %v", err)
	}
}

// filterArgs is every `docker ps` call's own --filter argument (index 3,
// after "ps", "--all", "--filter"), one per namespace asked about.
func filterArgs(calls []run.Spec) []string {
	args := make([]string, len(calls))
	for i, call := range calls {
		args[i] = call.Args[3]
	}
	return args
}

// dockerLine is one `docker ps --format` row for record's own repository and
// worktree IDs.
func dockerLine(record registry.RegisteredWorktree, service, state, status string) string {
	return strings.Join([]string{state, status, record.RepositoryID, record.WorktreeID, service, ""}, "\t")
}

// TestServiceRefresh covers Service.Refresh, plus #1's own lock timeout
// addition.
func TestServiceRefresh(t *testing.T) {
	regs := testRegistrations()
	a, b := regs[0], regs[1]

	t.Run("writes the page from registrations, cards, identities, and one docker ps per namespace", func(t *testing.T) {
		docker := &dockerFake{stdout: dockerLine(a, "api", "running", "Up 20 seconds (healthy)")}
		h := newServiceHarness(t, docker, []registry.RegisteredWorktree{a, b}, []registry.WorktreeIdentity{testIdentity(a, "feature/a")})
		writeTestCard(t, h.controlHome, a, []card.IndexService{{Name: "api", URL: "http://feature-a.acme.localhost:8080", Label: "API", Purpose: "REST"}})

		snapshot, err := h.service.Refresh(context.Background())
		if err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if h.service.URL() != "http://treerig.localhost:8080" {
			t.Fatalf("URL = %q", h.service.URL())
		}
		if len(snapshot.Entries) != 2 || snapshot.Entries[0].Status != StatusRunning || snapshot.Entries[1].Status != StatusMissing {
			t.Fatalf("entries = %+v", snapshot.Entries)
		}
		page, err := os.ReadFile(h.service.opts.IndexFile)
		if err != nil {
			t.Fatalf("read index file: %v", err)
		}
		if !strings.Contains(string(page), "feature-a.acme.localhost") {
			t.Fatalf("page does not mention feature-a: %s", page)
		}
		calls := docker.Calls()
		if len(calls) != 1 || calls[0].Args[0] != "ps" {
			t.Fatalf("calls = %+v", calls)
		}
	})

	t.Run("leaves the served directory and page readable by the index container on a fresh control home", func(t *testing.T) {
		h := newServiceHarness(t, &dockerFake{}, nil, nil)
		if _, err := h.service.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		info, err := os.Stat(h.service.opts.IndexFile)
		if err != nil {
			t.Fatalf("stat index file: %v", err)
		}
		// AtomicWrite's own mode, matching render.go's own comment: readable
		// by the cap-dropped index container. This harness writes the file
		// directly under t.TempDir() rather than a real router state
		// directory (see newServiceHarness), so only the file's own mode is
		// checked here — the directory permissions router.SecureState
		// establishes are internal/router's own concern, not this package's.
		if info.Mode().Perm() != 0o644 {
			t.Fatalf("index file mode = %v, want 0644", info.Mode().Perm())
		}
	})

	t.Run("still writes the page when docker ps fails, with unknown statuses and a reason", func(t *testing.T) {
		docker := &dockerFake{exit: 1}
		h := newServiceHarness(t, docker, []registry.RegisteredWorktree{a}, []registry.WorktreeIdentity{testIdentity(a, "feature/a")})

		snapshot, err := h.service.Refresh(context.Background())
		if err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if snapshot.DockerReachable {
			t.Fatalf("expected DockerReachable false")
		}
		if snapshot.Entries[0].Status != StatusUnknown {
			t.Fatalf("status = %v", snapshot.Entries[0].Status)
		}
		if !containsSubstring(snapshotLines(h.logger), "could not be listed") {
			t.Fatalf("lines = %v", snapshotLines(h.logger))
		}
	})

	t.Run("still writes the page when docker itself cannot be started, saying why the states are unknown", func(t *testing.T) {
		docker := &dockerFake{missing: true}
		h := newServiceHarness(t, docker, []registry.RegisteredWorktree{a}, []registry.WorktreeIdentity{testIdentity(a, "feature/a")})

		snapshot, err := h.service.Refresh(context.Background())
		if err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if snapshot.Entries[0].Status != StatusUnknown {
			t.Fatalf("status = %v", snapshot.Entries[0].Status)
		}
		want := "could not be listed: Docker is not available: the `docker` command is not on PATH (ENOENT); is Docker installed?"
		if !containsSubstring(snapshotLines(h.logger), want) {
			t.Fatalf("lines = %v", snapshotLines(h.logger))
		}
	})

	t.Run("lets a cancelled run through rather than rewriting the page as Unknown", func(t *testing.T) {
		docker := &dockerFake{failure: &clierr.Cancelled{Signal: syscall.SIGINT}}
		h := newServiceHarness(t, docker, nil, nil)
		before, _ := os.ReadFile(h.service.opts.IndexFile)

		if _, err := h.service.Refresh(context.Background()); !errors.As(err, new(*clierr.Cancelled)) {
			t.Fatalf("Refresh err = %v, want *clierr.Cancelled", err)
		}

		// The page it would have written says every service is Unknown, and
		// blames Docker for something the operator did — neither ever
		// happens: the old page (here, none at all) is left exactly alone.
		after, _ := os.ReadFile(h.service.opts.IndexFile)
		if string(after) != string(before) {
			t.Fatalf("index file changed: before %q, after %q", before, after)
		}
		if containsSubstring(snapshotLines(h.logger), "could not be listed") {
			t.Fatalf("lines = %v", snapshotLines(h.logger))
		}
	})

	t.Run("gives Docker a deadline, and says so on the page when it misses it", func(t *testing.T) {
		docker := &dockerFake{timeout: true}
		h := newServiceHarness(t, docker, []registry.RegisteredWorktree{a}, []registry.WorktreeIdentity{testIdentity(a, "feature/a")})

		snapshot, err := h.service.Refresh(context.Background())
		if err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if snapshot.DockerReachable {
			t.Fatalf("expected DockerReachable false")
		}
		if !containsSubstring(snapshotLines(h.logger), "did not answer within 30s") {
			t.Fatalf("lines = %v", snapshotLines(h.logger))
		}
	})

	t.Run("refreshBestEffort warns instead of failing the command", func(t *testing.T) {
		h := newServiceHarness(t, &dockerFake{}, nil, nil)
		h.service.opts.Registry = &fakeRegistry{err: errors.New("registry exploded")}

		h.service.RefreshBestEffort(context.Background(), "up")

		if !containsSubstring(snapshotLines(h.logger), "Could not refresh the checkout index after up: registry exploded") {
			t.Fatalf("lines = %v", snapshotLines(h.logger))
		}
	})

	t.Run("says once why the states are unknown, however many namespaces it had to ask", func(t *testing.T) {
		docker := &dockerFake{exit: 1}
		namespaceA, namespaceB := "dev.one", "dev.two"
		h := newServiceHarness(t, docker,
			[]registry.RegisteredWorktree{withNamespace(a, namespaceA), withNamespace(b, namespaceB)}, nil)
		h.service.opts.Identities = &fakeIdentities{}

		if _, err := h.service.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}

		count := 0
		for _, line := range snapshotLines(h.logger) {
			if strings.Contains(line, "could not be listed") {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("warned %d times about namespaces %s, %s not being listable, want 1: %v", count, namespaceA, namespaceB, snapshotLines(h.logger))
		}
	})

	t.Run("asks Docker about a namespace the registration records even when the card is gone", func(t *testing.T) {
		// No card for either: the home is gone, and the registration is all
		// that is left.
		docker := &dockerFake{}
		h := newServiceHarness(t, docker,
			[]registry.RegisteredWorktree{a, withNamespace(b, "dev.custom")}, nil)
		h.service.opts.Identities = &fakeIdentities{}

		if _, err := h.service.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}

		// Concurrent since #2 (errgroup, not a serial loop): the namespaces
		// themselves are still exactly these two, but which one's `docker
		// ps` call lands first in the transcript is no longer guaranteed.
		got := filterArgs(docker.Calls())
		sort.Strings(got)
		want := []string{"label=dev.custom.managed=true", "label=dev.treerig.managed=true"}
		if !slices.Equal(got, want) {
			t.Fatalf("filters = %v, want %v", got, want)
		}
	})

	t.Run("asks Docker about a namespace only the card records, wherever that card is", func(t *testing.T) {
		for _, quarantined := range []bool{false, true} {
			docker := &dockerFake{}
			h := newServiceHarness(t, docker, []registry.RegisteredWorktree{a, b}, nil)
			h.service.opts.Identities = &fakeIdentities{}
			// The registration predates namespace tracking, so only the card knows.
			writeTestCardWithNamespace(t, h.controlHome, b, "dev.custom", nil)
			if quarantined {
				wt := layout.WorktreeLayout(h.controlHome, b.RepositoryID, b.WorktreeID)
				if err := os.Rename(wt.Home, wt.QuarantinePath); err != nil {
					t.Fatalf("quarantine: %v", err)
				}
			}

			if _, err := h.service.Refresh(context.Background()); err != nil {
				t.Fatalf("Refresh: %v", err)
			}

			got := filterArgs(docker.Calls())
			sort.Strings(got)
			want := []string{"label=dev.custom.managed=true", "label=dev.treerig.managed=true"}
			if !slices.Equal(got, want) {
				t.Fatalf("quarantined=%v: filters = %v, want %v", quarantined, got, want)
			}
		}
	})

	t.Run("shows the page rather than failing when Docker answers with a line it cannot read", func(t *testing.T) {
		docker := &dockerFake{stdout: "running\tUp 2 seconds\t(healthy)\tr\tw\tapi\t"}
		h := newServiceHarness(t, docker, nil, nil)

		snapshot, err := h.service.Refresh(context.Background())
		if err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if snapshot.DockerReachable {
			t.Fatalf("expected DockerReachable false")
		}
		if !containsSubstring(snapshotLines(h.logger), "unrecognized container inventory line") {
			t.Fatalf("lines = %v", snapshotLines(h.logger))
		}
	})

	t.Run("still finds the namespace in a card the rest of the schema refuses", func(t *testing.T) {
		// A key this version does not know: no card at all, as far as the
		// page is concerned — but its containers still have to be found.
		docker := &dockerFake{}
		h := newServiceHarness(t, docker, []registry.RegisteredWorktree{a, b}, nil)
		h.service.opts.Identities = &fakeIdentities{}
		home := filepath.Dir(card.IndexCardPath(h.controlHome, b.RepositoryID, b.WorktreeID))
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		raw := fmt.Sprintf(`{"version":1,"project":"acme","domain":%q,"slug":%q,"host":"h","root":"","branch":"","labelNamespace":"dev.custom","services":[],"updatedAt":"","somethingNewer":true}`, b.Domain, b.Slug)
		if err := os.WriteFile(card.IndexCardPath(h.controlHome, b.RepositoryID, b.WorktreeID), []byte(raw), 0o600); err != nil {
			t.Fatalf("write card: %v", err)
		}

		if _, err := h.service.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}

		if got, err := card.ReadIndexCard(h.controlHome, b.RepositoryID, b.WorktreeID); err != nil || got != nil {
			t.Fatalf("ReadIndexCard = %+v, %v; want nil, nil", got, err)
		}
		got := filterArgs(docker.Calls())
		sort.Strings(got)
		want := []string{"label=dev.custom.managed=true", "label=dev.treerig.managed=true"}
		if !slices.Equal(got, want) {
			t.Fatalf("filters = %v, want %v", got, want)
		}
	})

	t.Run("does not read a file too large to be a card, whatever it would have parsed as", func(t *testing.T) {
		docker := &dockerFake{}
		h := newServiceHarness(t, docker, []registry.RegisteredWorktree{a}, nil)
		h.service.opts.Identities = &fakeIdentities{}
		// A card that would parse, padded past the limit with insignificant space.
		home := filepath.Dir(card.IndexCardPath(h.controlHome, a.RepositoryID, a.WorktreeID))
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		valid := fmt.Sprintf(`{"version":1,"project":"acme","domain":%q,"slug":%q,"host":"h","root":"","branch":"","labelNamespace":"dev.custom","services":[],"updatedAt":""}`, a.Domain, a.Slug)
		padded := valid + strings.Repeat(" ", 128*1024)
		if err := os.WriteFile(card.IndexCardPath(h.controlHome, a.RepositoryID, a.WorktreeID), []byte(padded), 0o600); err != nil {
			t.Fatalf("write card: %v", err)
		}

		if got, err := card.ReadIndexCard(h.controlHome, a.RepositoryID, a.WorktreeID); err != nil || got != nil {
			t.Fatalf("ReadIndexCard = %+v, %v; want nil, nil", got, err)
		}
		if _, err := h.service.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		got := filterArgs(docker.Calls())
		want := []string{"label=dev.treerig.managed=true"}
		if !slices.Equal(got, want) {
			t.Fatalf("filters = %v, want %v", got, want)
		}
	})

	// #1: Refresh must never wait out lock.RouterTimeout (20 minutes) for a
	// lock someone else holds — it is best-effort, and down/worktree remove/
	// up itself must not stall behind it. LockTimeout is overridden short
	// here only so the test itself does not have to wait 60 real seconds;
	// production leaves it at defaultLockTimeout.
	t.Run("gives up on a busy router lock well inside its own short timeout, not the router's 20-minute one", func(t *testing.T) {
		h := newServiceHarness(t, &dockerFake{}, nil, nil)
		h.service.opts.LockTimeout = 200 * time.Millisecond

		holder := lock.New(h.service.opts.Lock, lock.Options{Watchdog: -1})
		if _, err := holder.Acquire(context.Background(), "another holder"); err != nil {
			t.Fatalf("Acquire (holder): %v", err)
		}
		defer func() { _ = holder.Release() }()

		start := time.Now()
		h.service.RefreshBestEffort(context.Background(), "down")
		elapsed := time.Since(start)

		if elapsed > 5*time.Second {
			t.Fatalf("RefreshBestEffort took %s; expected it to give up near LockTimeout (200ms), nowhere near lock.RouterTimeout (20m)", elapsed)
		}
		if !containsSubstring(snapshotLines(h.logger), "Could not refresh the checkout index after down:") {
			t.Fatalf("lines = %v", snapshotLines(h.logger))
		}
	})

	t.Run("never calls a checkout missing when Git could not be asked which worktrees exist", func(t *testing.T) {
		docker := &dockerFake{}
		h := newServiceHarness(t, docker, []registry.RegisteredWorktree{a, b}, nil)
		h.service.opts.Identities = &fakeIdentities{
			repositoryID: testRepo, repositoryIDPresent: true, identitiesErr: errors.New("Invalid identity marker for a sibling"),
		}

		snapshot, err := h.service.Refresh(context.Background())
		if err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		for _, entry := range snapshot.Entries {
			if entry.Status != StatusUnknown {
				t.Fatalf("entries = %+v", snapshot.Entries)
			}
		}
		if snapshot.IdentityError == nil || !snapshot.IdentityError.RowsUnknown {
			t.Fatalf("identityError = %+v", snapshot.IdentityError)
		}
	})

	t.Run("says the repository could not be identified, rather than blaming the worktree list", func(t *testing.T) {
		docker := &dockerFake{}
		h := newServiceHarness(t, docker, []registry.RegisteredWorktree{a, b}, nil)
		h.service.opts.Identities = &fakeIdentities{repositoryIDErr: errors.New("fatal: detected dubious ownership in repository")}

		snapshot, err := h.service.Refresh(context.Background())
		if err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if snapshot.IdentityError == nil || snapshot.IdentityError.RowsUnknown {
			t.Fatalf("identityError = %+v", snapshot.IdentityError)
		}
		for _, entry := range snapshot.Entries {
			if entry.Status != StatusStopped {
				t.Fatalf("entries = %+v", snapshot.Entries)
			}
		}
	})
}

// TestServiceOutsideAnyRepository is the index refreshed by a command run from
// a directory in no Git work tree: no current repository, and nothing to report
// about Git failing to identify one.
func TestServiceOutsideAnyRepository(t *testing.T) {
	regs := testRegistrations()
	docker := &dockerFake{}
	h := newServiceHarness(t, docker, []registry.RegisteredWorktree{regs[0], regs[1]}, nil)
	h.service.opts.Identities = &fakeIdentities{outside: true}

	snapshot, err := h.service.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if snapshot.IdentityError != nil {
		t.Fatalf("identityError = %+v, want none outside a repository", snapshot.IdentityError)
	}
}

// TestServiceCountsUnindexedServiceTowardStatus is #16: a service the
// config never gave a [service.index] block still keeps its checkout from
// reading Running while it is stopped.
func TestServiceCountsUnindexedServiceTowardStatus(t *testing.T) {
	regs := testRegistrations()
	a := regs[0]
	// api is running and indexed; mail is declared (recorded with "" label,
	// as up.go now writes for a service with no [service.index]) but has no
	// container at all.
	docker := &dockerFake{stdout: dockerLine(a, "api", "running", "Up 20 seconds (healthy)")}
	h := newServiceHarness(t, docker, []registry.RegisteredWorktree{a}, []registry.WorktreeIdentity{testIdentity(a, "feature/a")})
	writeTestCard(t, h.controlHome, a, []card.IndexService{
		{Name: "api", URL: "http://feature-a.acme.localhost:8080", Label: "API", Purpose: "REST"},
		{Name: "mail", URL: "http://mail.feature-a.acme.localhost:8080"},
	})

	snapshot, err := h.service.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if snapshot.Entries[0].Status != StatusPartial {
		t.Fatalf("status = %v, want partial", snapshot.Entries[0].Status)
	}
	// mail has no label, so it is counted but never becomes a link.
	for _, service := range snapshot.Entries[0].Services {
		if service.Name == "mail" {
			t.Fatalf("unlabelled service should not be a link: %+v", service)
		}
	}
}

// snapshotLines is every line a *logger.Recording has recorded, ignoring
// what it printed as output.
func snapshotLines(l *logger.Recording) []string {
	lines, _ := l.Snapshot()
	return lines
}

// containsSubstring reports whether any line in lines contains want.
func containsSubstring(lines []string, want string) bool {
	for _, line := range lines {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}

// TestBuildSnapshotIdentityFailureInvariant is #17: BuildSnapshot must not
// trust a caller-supplied RowsUnknown that disagrees with whether
// CurrentRepositoryID is known — it recomputes it instead, so a
// SnapshotInput built the "wrong" way still renders correctly.
// TestBuildSnapshotIdentityFailureInvariant is #17: BuildSnapshot computes
// IndexSnapshot.IdentityError.RowsUnknown itself, from CurrentRepositoryID —
// the input type gives a caller no RowsUnknown to set at all (see
// IdentityFailureInput and TestSnapshotInputIdentityErrorHasNoRowsUnknown,
// which pins that at the type level), so this only has correctness left to
// check, not the bad-combination case a previous round of this fix ran at
// runtime.
func TestBuildSnapshotIdentityFailureInvariant(t *testing.T) {
	regs := testRegistrations()
	a := regs[0]
	repositoryID := testRepo

	t.Run("a known repository's own identity failure computes RowsUnknown true", func(t *testing.T) {
		snapshot := BuildSnapshot(SnapshotInput{
			Registrations:       []registry.RegisteredWorktree{a},
			CurrentRepositoryID: &repositoryID,
			Containers:          []ContainerRecord{},
			IdentityError:       &IdentityFailureInput{Reason: "boom"},
		})
		if snapshot.IdentityError == nil || !snapshot.IdentityError.RowsUnknown {
			t.Fatalf("IdentityError = %+v, want RowsUnknown true", snapshot.IdentityError)
		}
		// A row with no live identity of a repository this build could not
		// even identify is not "missing" — it was never comparable in the
		// first place — so it reads Unknown instead.
		if snapshot.Entries[0].Status != StatusUnknown {
			t.Fatalf("status = %v, want unknown", snapshot.Entries[0].Status)
		}
	})

	t.Run("an unidentified repository's own identity failure computes RowsUnknown false", func(t *testing.T) {
		snapshot := BuildSnapshot(SnapshotInput{
			Registrations: []registry.RegisteredWorktree{a},
			Containers:    []ContainerRecord{},
			IdentityError: &IdentityFailureInput{Reason: "boom"},
		})
		if snapshot.IdentityError == nil || snapshot.IdentityError.RowsUnknown {
			t.Fatalf("IdentityError = %+v, want RowsUnknown false", snapshot.IdentityError)
		}
	})
}

// TestSnapshotInputIdentityErrorHasNoRowsUnknown pins #17's type-level fix:
// the issue's own done-when asked for the bad combination (a known
// CurrentRepositoryID together with RowsUnknown: false) to fail to
// type-check, not merely to be recomputed correctly at runtime. There is no
// Go language construct that asserts "this field does not exist" other than
// checking it by reflection — this test exists so that a future change
// which re-adds a RowsUnknown field to SnapshotInput's identity-failure type
// (IdentityFailureInput) fails a test immediately, rather than silently
// reopening the door #17 closed.
func TestSnapshotInputIdentityErrorHasNoRowsUnknown(t *testing.T) {
	inputType := reflect.TypeOf(IdentityFailureInput{})
	if _, ok := inputType.FieldByName("RowsUnknown"); ok {
		t.Fatalf("%s must not have a RowsUnknown field: only BuildSnapshot may decide it, from CurrentRepositoryID (#17)", inputType)
	}
	if _, ok := inputType.FieldByName("Reason"); !ok {
		t.Fatalf("%s is missing its own Reason field", inputType)
	}
}

// TestReadCardsBoundsConcurrency is #18, made deterministic: a fake
// readCard counts concurrent in-flight calls (asserting the count never
// exceeds CardConcurrency) and fails one specific card on purpose, proving
// that read renders as a distinguishable failure rather than "no card" —
// without needing real file descriptor exhaustion to observe either.
func TestReadCardsBoundsConcurrency(t *testing.T) {
	const count = 200
	const bound = 8
	registrations := make([]registry.RegisteredWorktree, count)
	for i := range registrations {
		id := fmt.Sprintf("wt-%04d", i)
		registrations[i] = registry.RegisteredWorktree{RepositoryID: testRepo, WorktreeID: id, Slug: id, Domain: "acme.localhost"}
	}
	failing := registrations[count/2].WorktreeID
	failure := errors.New("simulated EMFILE")

	var inFlight, maxInFlight atomic.Int64
	svc := NewService(Options{Logger: logger.Silent{}, CardConcurrency: bound})
	svc.readCard = func(_, _, worktreeID string) (*card.IndexCard, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		// Long enough that, with no bound at all, every one of count
		// goroutines would be in flight at once well before any of them
		// returns — proving the semaphore actually holds them back, not
		// merely that they happened to finish fast enough not to overlap.
		time.Sleep(2 * time.Millisecond)
		if worktreeID == failing {
			return nil, failure
		}
		return nil, nil
	}

	reads := svc.readCards(registrations)

	if got := maxInFlight.Load(); got > bound {
		t.Fatalf("max concurrent card reads = %d, want at most CardConcurrency (%d)", got, bound)
	}
	for _, read := range reads {
		want := read.record.WorktreeID == failing
		if read.failed != want {
			t.Fatalf("read %s: failed = %v, want %v", read.record.WorktreeID, read.failed, want)
		}
	}
}

// TestReadCardsUnderRealFileDescriptorPressure is extra coverage for #18
// alongside the deterministic TestReadCardsBoundsConcurrency above: a real,
// tightened RLIMIT_NOFILE proves the bound keeps an actual card-read fan-out
// under it, on a real filesystem, not just in a fake.
func TestReadCardsUnderRealFileDescriptorPressure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("RLIMIT_NOFILE tightening is exercised on Linux only")
	}

	const count = 300
	controlHome := t.TempDir()
	registrations := make([]registry.RegisteredWorktree, count)
	for i := range registrations {
		record := registry.RegisteredWorktree{
			RepositoryID: testRepo, WorktreeID: fmt.Sprintf("wt-%04d", i),
			Slug: fmt.Sprintf("wt-%04d", i), Domain: "acme.localhost",
		}
		registrations[i] = record
		writeTestCard(t, controlHome, record, nil)
	}

	// Tighten this process's own file descriptor limit so a card-read fan-out
	// with no bound at all would exhaust it; a bounded one must not.
	var original syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &original); err != nil {
		t.Fatalf("Getrlimit: %v", err)
	}
	tight := syscall.Rlimit{Cur: 64, Max: original.Max}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &tight); err != nil {
		t.Skipf("could not tighten RLIMIT_NOFILE: %v", err)
	}
	defer func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &original); err != nil {
			t.Fatalf("could not restore RLIMIT_NOFILE: %v", err)
		}
	}()

	bounded := NewService(Options{ControlHome: controlHome, Logger: logger.Silent{}, CardConcurrency: 8})
	reads := bounded.readCards(registrations)
	for _, read := range reads {
		if read.failed {
			t.Fatalf("bounded read failed for %s despite the concurrency bound", read.record.WorktreeID)
		}
	}

	unbounded := NewService(Options{ControlHome: controlHome, Logger: logger.Silent{}, CardConcurrency: count})
	var failed atomic.Bool
	for _, read := range unbounded.readCards(registrations) {
		if read.failed {
			failed.Store(true)
		}
	}
	if !failed.Load() {
		t.Skip("this machine's descriptor accounting did not reproduce EMFILE under an unbounded fan-out; nothing more to assert")
	}
}

// TestServiceCardReadFailureNotice is #18's page-level notice (item 5 of
// round-1 review): a card-read failure must be visible on the page itself,
// in both forms, not only warned about at refresh time.
func TestServiceCardReadFailureNotice(t *testing.T) {
	regs := testRegistrations()
	a, b := regs[0], regs[1]
	h := newServiceHarness(t, &dockerFake{}, []registry.RegisteredWorktree{a, b}, nil)
	h.service.opts.Identities = &fakeIdentities{}
	failure := errors.New("simulated EMFILE")
	h.service.readCard = func(_, repositoryID, worktreeID string) (*card.IndexCard, error) {
		if worktreeID == a.WorktreeID {
			return nil, failure
		}
		return nil, nil
	}

	snapshot, err := h.service.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if snapshot.CardReadFailures != 1 {
		t.Fatalf("CardReadFailures = %d, want 1", snapshot.CardReadFailures)
	}
	html := RenderIndexHTML(snapshot)
	if !strings.Contains(html, "One checkout&#39;s index card could not be read, so its state shows Unknown") {
		t.Fatalf("HTML page missing the card-read-failure notice:\n%s", html)
	}
	if !strings.Contains(html, "<code>treerig index</code>") {
		t.Fatalf("HTML page missing the notice's treerig index command span:\n%s", html)
	}
	// Wrapped for a terminal at 80 columns, so (matching the existing
	// identityNotice test's own approach) only a short phrase is matched,
	// not the whole sentence, which a line break could fall in the middle of.
	text := FormatIndexText(snapshot, h.service.URL())
	if !strings.Contains(text, "index card could not be read") {
		t.Fatalf("text page missing the card-read-failure notice:\n%s", text)
	}
	if !strings.Contains(text, "too many open files") {
		t.Fatalf("text page missing the card-read-failure notice's cause:\n%s", text)
	}
}

// TestServiceRefreshTimesOutOnAHungDocker is #12's precedent applied to the
// checkout index: a `docker` that never answers must not hang Refresh
// forever. Mirrors internal/router's own TestRouterUpTimesOutOnAHungDocker,
// but on a fake rather than a real Docker daemon: a real, hanging
// subprocess (found only through this test's own PATH override), run
// through the real osrun.Runner, so the real timeout-and-kill machinery is
// what is exercised — not a hand-rolled fake that merely claims to time out.
func TestServiceRefreshTimesOutOnAHungDocker(t *testing.T) {
	dir := t.TempDir()
	fakeDocker := filepath.Join(dir, "docker")
	if err := os.WriteFile(fakeDocker, []byte("#!/bin/sh\nsleep 999999\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	regs := testRegistrations()
	controlHome := t.TempDir()
	log := &logger.Recording{}
	svc := NewService(Options{
		ControlHome: controlHome,
		RouterPort:  8080,
		IndexFile:   filepath.Join(t.TempDir(), "index.html"),
		Lock:        lock.Scope{Target: filepath.Join(t.TempDir(), "lock"), Description: "shared router"},
		Registry:    &fakeRegistry{registrations: []registry.RegisteredWorktree{regs[0]}},
		Identities:  &fakeIdentities{},
		Runner:      osrun.New(),
		Logger:      log,
		// Well short of osrun's own default kill grace, so this test does
		// not wait long, and well short of the 10s ceiling below it is
		// checked against.
		InventoryTimeout: 300 * time.Millisecond,
	})

	started := time.Now()
	snapshot, err := svc.Refresh(context.Background())
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("expected the timeout to end this well under 10s, took %s", elapsed)
	}
	if snapshot.DockerReachable {
		t.Fatalf("expected DockerReachable false")
	}
	if !containsSubstring(snapshotLines(log), "did not answer within") {
		t.Fatalf("lines = %v", snapshotLines(log))
	}
	page, err := os.ReadFile(svc.opts.IndexFile)
	if err != nil {
		t.Fatalf("read index file: %v", err)
	}
	if !strings.Contains(string(page), "did not answer within") {
		t.Fatalf("page does not say the states are unknown: %s", page)
	}
}

// TestReadCardsWarnsOnceHoweverManyCardsFailed is a pinning test for review
// round 2's item 1: thousands of failed reads (#18's own EMFILE scenario)
// must not flood stderr with one warning per read. readCards emits exactly
// one, naming the count and the first failure (in registration order).
func TestReadCardsWarnsOnceHoweverManyCardsFailed(t *testing.T) {
	regs := testRegistrations()
	a, b := regs[0], regs[1]
	failure := errors.New("simulated EMFILE")

	t.Run("one failure reads as singular, naming it", func(t *testing.T) {
		log := &logger.Recording{}
		svc := NewService(Options{Logger: log})
		svc.readCard = func(_, repositoryID, worktreeID string) (*card.IndexCard, error) {
			if worktreeID == a.WorktreeID {
				return nil, failure
			}
			return nil, nil
		}

		svc.readCards([]registry.RegisteredWorktree{a, b})

		want := "warn: Could not read one checkout's index card: feature-a.acme.localhost: simulated EMFILE"
		lines := snapshotLines(log)
		if len(lines) != 1 || lines[0] != want {
			t.Fatalf("lines = %v, want exactly [%q]", lines, want)
		}
	})

	t.Run("several failures read as plural, naming the count and the first", func(t *testing.T) {
		log := &logger.Recording{}
		svc := NewService(Options{Logger: log})
		svc.readCard = func(_, repositoryID, worktreeID string) (*card.IndexCard, error) {
			return nil, failure
		}

		svc.readCards([]registry.RegisteredWorktree{a, b})

		want := "warn: Could not read 2 checkouts' index cards; the first was feature-a.acme.localhost: simulated EMFILE"
		lines := snapshotLines(log)
		if len(lines) != 1 || lines[0] != want {
			t.Fatalf("lines = %v, want exactly [%q]", lines, want)
		}
	})
}
