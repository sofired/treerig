package commands

// This file tests doctor's unexported pieces directly — doctorSections and
// the helpers it is built from — the way internal/router and
// internal/config test their own unexported details from an in-package
// _test.go. doctorOptions has every external dependency injected, so a
// scenario is built here with a fake CommandRunner or RouterSource, without
// a real filesystem, Docker daemon, or Git checkout except where the test
// itself needs one (commandstest.World).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/procid"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/router"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/settings"
)

/* -------------------------------------------------------------- fakes ---- */

// outcome is a run.Result or an error, one of which a fake command run
// stores.
type outcome struct {
	result run.Result
	err    error
}

// okOutcome is a successful outcome with the given stdout.
func okOutcome(stdout string) outcome { return outcome{result: run.Result{Stdout: stdout}} }

// doctorRunnerState is a fake run.Runner for doctor's own tests: answers
// `git --version`, `docker …`, and any other command as the verify hook;
// Git identity queries go to git (a real registrytest.GitRunner).
//
// mu guards calls alone: dockerChecks asks Docker Engine and Compose
// concurrently (so a Docker that answers neither costs one timeout rather
// than two), so two goroutines can record a call at once. Every other field
// is set up before doctorSections runs and never mutated while it does.
type doctorRunnerState struct {
	mu           sync.Mutex
	calls        []run.Spec
	git          *registrytest.GitRunner
	gitVersion   outcome
	dockerOnPath bool
	docker       map[string]outcome
	hook         outcome
}

// recordCall appends spec to the call log under mu, safe for the concurrent
// Docker Engine and Compose queries dockerChecks makes.
func (s *doctorRunnerState) recordCall(spec run.Spec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, spec)
}

// callsSnapshot is every call recorded so far, safe to range over even while
// dockerChecks' own goroutines might still be running.
func (s *doctorRunnerState) callsSnapshot() []run.Spec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]run.Spec(nil), s.calls...)
}

// newDoctorRunnerState is a doctorRunnerState answering a current Git, a
// healthy Docker Engine and Compose, and a successful verify hook; git nil
// gets a fresh registrytest.GitRunner.
func newDoctorRunnerState(git *registrytest.GitRunner) *doctorRunnerState {
	if git == nil {
		git = registrytest.NewGitRunner()
	}
	return &doctorRunnerState{
		git:          git,
		gitVersion:   okOutcome("git version 2.43.0\n"),
		dockerOnPath: true,
		docker: map[string]outcome{
			"version": okOutcome("29.7.2\n"),
			"compose": okOutcome("2.38.2\n"),
			"ps":      okOutcome(""),
			"inspect": okOutcome(""),
		},
		hook: okOutcome(""),
	}
}

// doctorRunner is a run.Runner over doctorRunnerState.
type doctorRunner struct{ state *doctorRunnerState }

// newDoctorRunner is a run.Runner backed by a fresh newDoctorRunnerState(git).
func newDoctorRunner(git *registrytest.GitRunner) *doctorRunner {
	return &doctorRunner{state: newDoctorRunnerState(git)}
}

// Which reports docker as on PATH according to state.dockerOnPath; every
// other command is reported found.
func (d *doctorRunner) Which(name string) bool {
	if name != "docker" {
		return true
	}
	return d.state.dockerOnPath
}

// Run records spec and answers it: the configured Git version for `git
// --version`, the real git runner for any other `git` command, the
// configured outcome keyed by its first argument for `docker`, and the
// configured hook outcome for anything else.
func (d *doctorRunner) Run(ctx context.Context, spec run.Spec) (run.Result, error) {
	d.state.recordCall(spec)
	if spec.Command == "git" && len(spec.Args) > 0 && spec.Args[0] == "--version" {
		return finishOutcome(spec, d.state.gitVersion)
	}
	if spec.Command == "git" {
		result, err := d.state.git.Handler()(ctx, spec)
		if err != nil {
			return result, err
		}
		return run.Finish(spec, result)
	}
	if spec.Command == "docker" {
		key := ""
		if len(spec.Args) > 0 {
			key = spec.Args[0]
		}
		o, ok := d.state.docker[key]
		if !ok {
			o = okOutcome("")
		}
		return finishOutcome(spec, o)
	}
	return finishOutcome(spec, d.state.hook)
}

// finishOutcome turns o into what run.Runner.Run returns: o.err directly, or
// o.result passed through run.Finish to fill in what a real run would set.
func finishOutcome(spec run.Spec, o outcome) (run.Result, error) {
	if o.err != nil {
		return run.Result{}, o.err
	}
	return run.Finish(spec, o.result)
}

// fakeRouterSource fakes the shared router for doctor's Router section,
// plus owner/ownerErr for the router-owner check (#82).
type fakeRouterSource struct {
	network  string
	status   router.RouterStatus
	owner    router.Ownership
	ownerErr error
	err      error
	asked    int
	// published, publishedRunning and publishedErr are what PublishedPort
	// answers (#79).
	published        int
	publishedRunning bool
	publishedErr     error
}

// newFakeRouterSource is a fakeRouterSource reporting a reachable, running,
// clean router on network "berth-router".
func newFakeRouterSource() *fakeRouterSource {
	return &fakeRouterSource{network: "berth-router", status: router.RouterStatus{Reachable: true, Running: true, Port: 80, Network: "berth-router"}, published: 80, publishedRunning: true}
}

// Status counts the call and returns f.err if set, otherwise f.status.
func (f *fakeRouterSource) Status(ctx context.Context) (router.RouterStatus, error) {
	f.asked++
	if f.err != nil {
		return router.RouterStatus{}, f.err
	}
	return f.status, nil
}

// Network returns f.network.
func (f *fakeRouterSource) Network() string { return f.network }

// Owner returns f.ownerErr if set, otherwise f.owner — the zero Ownership
// (not running) by default, so a test that never sets it sees no "router
// owner" line at all.
func (f *fakeRouterSource) Owner(ctx context.Context) (router.Ownership, error) {
	if f.ownerErr != nil {
		return router.Ownership{}, f.ownerErr
	}
	return f.owner, nil
}

// PublishedPort answers the staged published port, running flag and error.
func (f *fakeRouterSource) PublishedPort(context.Context) (int, bool, error) {
	return f.published, f.publishedRunning, f.publishedErr
}

// fakeCheckoutRegistry is a checkoutRegistry fake, injected via
// doctorOptions.RegistryFor.
type fakeCheckoutRegistry struct {
	current         func(ctx context.Context) (*registry.WorktreeRegistration, error)
	pruneCandidates func(ctx context.Context) ([]registry.RegisteredWorktree, error)
}

// Current delegates to f.current.
func (f fakeCheckoutRegistry) Current(ctx context.Context) (*registry.WorktreeRegistration, error) {
	return f.current(ctx)
}

// PruneCandidates delegates to f.pruneCandidates.
func (f fakeCheckoutRegistry) PruneCandidates(ctx context.Context) ([]registry.RegisteredWorktree, error) {
	return f.pruneCandidates(ctx)
}

/* ------------------------------------------------------------- helpers ---- */

// testOpts is a doctorOptions wired to fakes (a fresh doctorRunner, a silent
// logger, a healthy fakeRouterSource, no checkout found), then adjusted by
// overrides when given.
func testOpts(t *testing.T, overrides func(*doctorOptions)) doctorOptions {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	opts := doctorOptions{
		CommandName: "berth", ControlHome: filepath.Join(base, "control"),
		Home: home, RouterStateDir: layout.RouterStateDirFrom(home), UserHome: filepath.Join(base, "user-home"),
		RouterPort: settings.PortResolution{Port: 80, Source: settings.PortDefault, SettingsPath: layout.SettingsPathFrom(home)},
		Runner:     newDoctorRunner(nil), Logger: logger.Silent{},
		Router: newFakeRouterSource(), Checkout: checkoutNone("/nowhere"),
	}
	if overrides != nil {
		overrides(&opts)
	}
	return opts
}

// section returns the checks of the section named title, or nil if sections
// has none by that title.
func section(sections []Section, title string) []Check {
	for _, s := range sections {
		if s.Title == title {
			return s.Checks
		}
	}
	return nil
}

// statuses is the section named title's checks, each rendered as "<status>
// <subject>" for terse assertions.
func statuses(sections []Section, title string) []string {
	checks := section(sections, title)
	out := make([]string, len(checks))
	for i, c := range checks {
		out[i] = string(c.Status) + " " + c.Subject
	}
	return out
}

// sectionTitles is sections' titles, in order.
func sectionTitles(sections []Section) []string {
	out := make([]string, len(sections))
	for i, s := range sections {
		out[i] = s.Title
	}
	return out
}

// testWorktree is a registry.RegisteredWorktree with the given slug and
// domain and fresh randomTestUUID identifiers.
func testWorktree(slug, domain string) registry.RegisteredWorktree {
	return registry.RegisteredWorktree{RepositoryID: randomTestUUID(), WorktreeID: randomTestUUID(), Slug: slug, Domain: domain}
}

var uuidCounter int

// randomTestUUID is not cryptographically random — only distinct within one
// test run, which registryChecks's own tests need and nothing more.
func randomTestUUID() string {
	uuidCounter++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", uuidCounter)
}

// cancelled is a *clierr.Cancelled for sig.
func cancelled(sig syscall.Signal) *clierr.Cancelled { return &clierr.Cancelled{Signal: sig} }

// fixtureDomain is every checkoutFixture's project domain.
const fixtureDomain = "acme.localhost"

// checkoutFixture is a hermetic checkout for the "doctor in a checkout"
// tests — the in-package counterpart of commandstest.World, built without it
// because commandstest imports this package (an import cycle otherwise).
type checkoutFixture struct {
	t           *testing.T
	base        string
	controlHome string
	root        string
	common      string
	git         *registrytest.GitRunner
	runner      *doctorRunner
	loaded      *config.Loaded
}

// newCheckoutFixture is a checkout on branch feature/login, with a real Git
// identity (registrytest.GitRunner) but not yet registered — this fixture
// never runs `up` itself; register() below is what a finished `up` would
// have left. When verify names a "./"-relative script (every caller's own
// choice today), a real executable file is written there too: the fake
// runner's own hook outcome (doctorRunnerState.hook) still decides what
// actually running it reports, but hookResolutionChecks now predicts that
// outcome from the real filesystem first, exactly as it would for a real
// checkout, so a fixture with nothing on disk at that path would otherwise
// fail resolution before the fake run is ever reached.
func newCheckoutFixture(t *testing.T, verify string) *checkoutFixture {
	t.Helper()
	base := registrytest.TempDir(t)
	controlHome := filepath.Join(base, "control")
	root := filepath.Join(base, "worktree")
	common := filepath.Join(base, "common")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(common, "worktrees", "worktree"), 0o700); err != nil {
		t.Fatal(err)
	}

	git := registrytest.NewGitRunner()
	git.Add(root, "feature/login", common)
	runner := newDoctorRunner(git)

	var hooksCfg config.Hooks
	if verify != "" {
		hooksCfg.Verify = &verify
		if rel, ok := strings.CutPrefix(verify, "./"); ok {
			script := filepath.Join(root, rel)
			if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	loaded := &config.Loaded{
		Config: config.Config{
			Project: config.Project{Name: "acme", Domain: fixtureDomain},
			Compose: config.Compose{File: "compose.yaml"},
			Service: []config.Service{{Name: "api"}},
			Hooks:   hooksCfg,
		},
		File: filepath.Join(root, brand.ConfigFile), Root: root,
	}

	return &checkoutFixture{t: t, base: base, controlHome: controlHome, root: root, common: common, git: git, runner: runner, loaded: loaded}
}

// register is what a finished `up` leaves behind, as far as doctor can see:
// identity markers and a complete registry record.
func (f *checkoutFixture) register(slug string) registry.RegisteredWorktree {
	return f.registerAt(f.root, slug)
}

// registerAt is register for a worktree other than f.root — a second
// checkout that shares this fixture's control home.
func (f *checkoutFixture) registerAt(root, slug string) registry.RegisteredWorktree {
	f.t.Helper()
	// A fixed placeholder, nowhere near f.controlHome (always a fresh temp
	// directory): AssertAdoptableControlHome's own home-family checks need
	// something to compare against, never exercised by this fixture.
	userHome := filepath.Join(f.base, "user-home")
	if err := files.SecureControlDirectories(layout.ControlLayout(f.controlHome), userHome); err != nil {
		f.t.Fatal(err)
	}
	identities := registry.NewIdentityService(root, f.runner)
	identity, err := registrytest.Register(context.Background(), f.controlHome, identities, root, fixtureDomain, slug, nil, "complete")
	if err != nil {
		f.t.Fatal(err)
	}
	if err := files.SecureWorktreeDirectories(layout.WorktreeLayout(f.controlHome, identity.RepositoryID, identity.WorktreeID), userHome); err != nil {
		f.t.Fatal(err)
	}
	return registry.RegisteredWorktree{RepositoryID: identity.RepositoryID, WorktreeID: identity.WorktreeID, Slug: slug, Domain: fixtureDomain}
}

// opts is a doctorOptions for this fixture's loaded checkout, with f's own
// control home, runner and a healthy fake router.
func (f *checkoutFixture) opts() doctorOptions {
	home := filepath.Join(f.base, "home")
	return doctorOptions{
		CommandName: "berth", ControlHome: f.controlHome, Home: home,
		RouterStateDir: layout.RouterStateDirFrom(home), UserHome: filepath.Join(f.base, "user-home"),
		Runner: f.runner, Logger: logger.Silent{},
		Router: newFakeRouterSource(), Checkout: checkoutIsLoaded(f.loaded), Invocation: "berth doctor",
	}
}

// wantCancelled fails t unless err is a *clierr.Cancelled with the given
// message.
func wantCancelled(t *testing.T, err error, msg string) {
	t.Helper()
	var c *clierr.Cancelled
	if !errors.As(err, &c) {
		t.Fatalf("err = %v, want a *clierr.Cancelled", err)
	}
	if err.Error() != msg {
		t.Fatalf("err.Error() = %q, want %q", err.Error(), msg)
	}
}

/* ------------------------------------------------------------ versions ---- */

func TestVersions(t *testing.T) {
	t.Run("reads the first version in what a tool prints, patch optional", func(t *testing.T) {
		cases := []struct {
			in   string
			want toolVersion
			ok   bool
		}{
			{"git version 2.43.0 (Apple Git-145)", toolVersion{"2.43.0", [3]int{2, 43, 0}}, true},
			{"v2.21.0", toolVersion{"2.21.0", [3]int{2, 21, 0}}, true},
			{"2.29.2-desktop.1", toolVersion{"2.29.2", [3]int{2, 29, 2}}, true},
			{"2.36", toolVersion{"2.36", [3]int{2, 36, 0}}, true},
			{"no version here", toolVersion{}, false},
		}
		for _, c := range cases {
			got, ok := readVersion(c.in)
			if ok != c.ok || got != c.want {
				t.Errorf("readVersion(%q) = %+v, %v; want %+v, %v", c.in, got, ok, c.want, c.ok)
			}
		}
	})

	t.Run("compares versions part by part, equal counting as enough", func(t *testing.T) {
		cases := []struct {
			version, minimum [3]int
			want             bool
		}{
			{[3]int{2, 36, 0}, [3]int{2, 36, 0}, true},
			{[3]int{2, 36, 1}, [3]int{2, 36, 0}, true},
			{[3]int{2, 35, 9}, [3]int{2, 36, 0}, false},
			{[3]int{3, 0, 0}, [3]int{2, 36, 0}, true},
			{[3]int{1, 99, 99}, [3]int{2, 0, 0}, false},
		}
		for _, c := range cases {
			if got := atLeast(c.version, c.minimum); got != c.want {
				t.Errorf("atLeast(%v, %v) = %v, want %v", c.version, c.minimum, got, c.want)
			}
		}
	})

	// "holds Node to the engines range: the floor included, the next major
	// excluded" and "checks Node against exactly the range package.json
	// declares" have no Go counterpart: satisfiesEngine, NODE_ENGINE and
	// nodeCheck do not exist here at all — there is no Go runtime version
	// for a Go build to be running under, so doctor drops the Node.js
	// engines check entirely. "names the command and the range when Node
	// is outside it" is the same story.
	t.Run("holds Node to the engines range: the floor included, the next major excluded", func(t *testing.T) {
		t.Skip("doctor drops the Node.js engines check entirely; " +
			"there is no Go runtime version for a Go build to be running under, so satisfiesEngine has no Go counterpart")
	})
	t.Run("checks Node against exactly the range package.json declares", func(t *testing.T) {
		t.Skip("doctor drops the Node.js engines check entirely; no NODE_ENGINE constant exists to check")
	})
	t.Run("names the command and the range when Node is outside it", func(t *testing.T) {
		t.Skip("doctor drops the Node.js engines check entirely; nodeCheck has no Go counterpart")
	})

	t.Run("passes a tool at the minimum, fails one below it, and fails output it cannot read", func(t *testing.T) {
		if got := versionCheck("prerequisites.git", "Git", "git version 2.36.0", "2.36"); got != (Check{ID: "prerequisites.git", Status: StatusOK, Subject: "Git", Detail: "2.36.0"}) {
			t.Errorf("got %+v", got)
		}
		if got := versionCheck("prerequisites.git", "Git", "git version 2.35.1", "2.36"); got != (Check{ID: "prerequisites.git", Status: StatusFail, Subject: "Git", Detail: "2.35.1; 2.36 or newer is needed"}) {
			t.Errorf("got %+v", got)
		}
		got := versionCheck("prerequisites.git", "Git", "garbled", "2.36")
		if got.Status != StatusFail || got.Detail != `could not read a version from "garbled"` {
			t.Errorf("got %+v", got)
		}
	})
}

/* ------------------------------------------------------------ registry ---- */

func TestRegistryChecksInternal(t *testing.T) {
	scan := func(overrides func(*registry.RegistryScan)) registry.RegistryScan {
		s := registry.RegistryScan{Conflicts: registry.HostnameConflicts{}}
		if overrides != nil {
			overrides(&s)
		}
		return s
	}

	t.Run("says there is nothing yet, or how many records read cleanly", func(t *testing.T) {
		got := registryChecks(scan(nil), "berth")
		want := []Check{{ID: "registry.registrations", Status: StatusOK, Subject: "registrations", Detail: "none yet"}}
		if len(got) != 1 || got[0] != want[0] {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		got = registryChecks(scan(func(s *registry.RegistryScan) {
			s.Records = []registry.RegisteredWorktree{testWorktree("a", "acme.localhost"), testWorktree("b", "acme.localhost")}
		}), "berth")
		if len(got) != 1 || got[0] != (Check{ID: "registry.registrations", Status: StatusOK, Subject: "registrations", Detail: "2 readable"}) {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("fails each unreadable record with its reason", func(t *testing.T) {
		got := registryChecks(scan(func(s *registry.RegistryScan) {
			s.Problems = []registry.RegistryProblem{{File: "/x.json", Reason: "Registry record is not valid JSON: /x.json"}}
		}), "berth")
		if got[1] != (Check{ID: "registry.unreadable-record", Instance: "x.json", Status: StatusFail, Subject: "unreadable record", Detail: "Registry record is not valid JSON: /x.json"}) {
			t.Fatalf("got %+v", got[1])
		}
	})

	t.Run("fails a hostname two worktrees claim, naming both", func(t *testing.T) {
		first, second := testWorktree("shared", "acme.localhost"), testWorktree("shared", "acme.localhost")
		got := registryChecks(scan(func(s *registry.RegistryScan) {
			s.Records = []registry.RegisteredWorktree{first, second}
			s.Conflicts = registry.HostnameConflicts{"shared.acme.localhost": {first, second}}
		}), "duke")
		if got[1].Status != StatusFail || got[1].Subject != "shared.acme.localhost" || got[1].ID != "registry.host-conflict" || got[1].Instance != "shared.acme.localhost" {
			t.Fatalf("got %+v", got[1])
		}
		want := fmt.Sprintf("registered to 2 worktrees (%s, %s); requests for it may reach any of them that is running. Remove all but one with 'duke worktree remove <uuid>'.",
			first.WorktreeID, second.WorktreeID)
		if got[1].Detail != want {
			t.Fatalf("detail = %q, want %q", got[1].Detail, want)
		}
	})

	t.Run("bounds the list of worktrees claiming a hostname", func(t *testing.T) {
		claimants := make([]registry.RegisteredWorktree, 12)
		for i := range claimants {
			claimants[i] = testWorktree("shared", "acme.localhost")
		}
		got := registryChecks(scan(func(s *registry.RegistryScan) {
			s.Records = claimants
			s.Conflicts = registry.HostnameConflicts{"shared.acme.localhost": claimants}
		}), "berth")
		start := len("registered to 12 worktrees (")
		end := len(got[1].Detail) - len("); requests for it may reach any of them that is running. Remove all but one with 'berth worktree remove <uuid>'.")
		listed := got[1].Detail[start:end]
		if n := utf8.RuneCountInString(listed); n != 200 {
			t.Fatalf("listed = %q (%d runes), want 200", listed, n)
		}
		if !isEllipsisSuffix(listed) {
			t.Fatalf("listed = %q, want to end with an ellipsis", listed)
		}
	})

	t.Run("fails a registration that sits on the index's own hostname, and only that one", func(t *testing.T) {
		squatter := registry.RegisteredWorktree{RepositoryID: randomTestUUID(), WorktreeID: randomTestUUID(), Slug: "berth", Domain: "localhost"}
		namesake := registry.RegisteredWorktree{RepositoryID: randomTestUUID(), WorktreeID: randomTestUUID(), Slug: "berth", Domain: "acme.localhost"}
		got := registryChecks(scan(func(s *registry.RegistryScan) { s.Records = []registry.RegisteredWorktree{squatter, namesake} }), "berth")
		var fails []Check
		for _, c := range got {
			if c.Status == StatusFail {
				fails = append(fails, c)
			}
		}
		if len(fails) != 1 || fails[0].Subject != router.IndexHost {
			t.Fatalf("fails = %+v", fails)
		}
		if !contains(got[1].Detail, squatter.WorktreeID) {
			t.Fatalf("detail = %q, want to contain %s", got[1].Detail, squatter.WorktreeID)
		}
	})
}

// isEllipsisSuffix reports whether s ends with the single-rune ellipsis
// text.OneLine truncates with.
func isEllipsisSuffix(s string) bool {
	r := []rune(s)
	return len(r) > 0 && r[len(r)-1] == '…'
}

// contains reports whether haystack contains needle.
func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

// indexOf returns the byte index of needle's first occurrence in haystack,
// or -1 if it does not occur.
func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

/* -------------------------------------------------------------- router ---- */

func TestRouterChecksInternal(t *testing.T) {
	status := router.RouterStatus{Reachable: true, Running: true, Port: 80, Network: "berth-router"}

	t.Run("reports a running, clean router and a stopped one as healthy, in router status's words", func(t *testing.T) {
		got := routerStatusChecks(status)
		if len(got) != 1 || got[0] != (Check{ID: "router.container", Status: StatusOK, Subject: "router", Detail: "running on 127.0.0.1:80, network berth-router"}) {
			t.Fatalf("got %+v", got)
		}
		stopped := status
		stopped.Running, stopped.Problems = false, []router.Problem{{Code: "router.absent", Message: "the router is not running"}}
		got = routerStatusChecks(stopped)
		if len(got) != 1 || got[0] != (Check{ID: "router.container", Status: StatusOK, Subject: "router", Detail: "not running (would use 127.0.0.1:80, network berth-router)"}) {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("fails each problem of a degraded router, and a router Docker could not be asked about", func(t *testing.T) {
		degraded := status
		degraded.Problems = []router.Problem{{Code: "router.not-healthy", Message: "router: not healthy"}, {Code: "index.image", Message: "index: image is not pinned"}}
		got := routerStatusChecks(degraded)
		want := []Check{
			{ID: "router.container", Instance: "router.not-healthy", Status: StatusFail, Subject: "router", Detail: "router: not healthy"},
			{ID: "router.container", Instance: "index.image", Status: StatusFail, Subject: "router", Detail: "index: image is not pinned"},
		}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("got %+v", got)
		}
		unreachable := router.RouterStatus{Reachable: false, Running: false, Problems: []router.Problem{{Code: router.CodeUnreachable, Message: "Docker is not available: boom."}}, Port: 80, Network: "berth-router"}
		got = routerStatusChecks(unreachable)
		if len(got) != 1 || got[0] != (Check{ID: "router.container", Instance: router.CodeUnreachable, Status: StatusFail, Subject: "router", Detail: "Router state unknown: Docker is not available: boom"}) {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("says nothing about ownership when nothing is running", func(t *testing.T) {
		fake := &fakeRouterSource{owner: router.Ownership{Running: false}}
		got, err := routerOwnerCheck(context.Background(), fake, "/home/dev/.berth", "berth")
		if err != nil || got != nil {
			t.Fatalf("got %+v, %v", got, err)
		}
	})

	t.Run("reports this berth root as the running router's own owner", func(t *testing.T) {
		fake := &fakeRouterSource{owner: router.Ownership{Running: true, Labelled: true, Home: "/home/dev/.berth"}}
		got, err := routerOwnerCheck(context.Background(), fake, "/home/dev/.berth", "berth")
		want := &Check{ID: "router.owner", Status: StatusOK, Subject: "router owner", Detail: "this berth root (/home/dev/.berth)"}
		if err != nil || got == nil || *got != *want {
			t.Fatalf("got %+v, %v", got, err)
		}
	})

	t.Run("fails a router that belongs to a different berth root", func(t *testing.T) {
		fake := &fakeRouterSource{owner: router.Ownership{Running: true, Labelled: true, Home: "/home/other/.berth"}}
		got, err := routerOwnerCheck(context.Background(), fake, "/home/dev/.berth", "berth")
		want := &Check{ID: "router.owner", Status: StatusFail, Subject: "router owner", Detail: "a different berth root (/home/other/.berth); this one's 'berth up' or 'berth router up' refuses it until 'berth router down' hands it over"}
		if err != nil || got == nil || *got != *want {
			t.Fatalf("got %+v, %v", got, err)
		}
	})

	t.Run("fails a running router that carries no berth-root label, the same as one belonging to another root", func(t *testing.T) {
		fake := &fakeRouterSource{owner: router.Ownership{Running: true, Labelled: false}}
		got, err := routerOwnerCheck(context.Background(), fake, "/home/dev/.berth", "berth")
		want := &Check{ID: "router.owner", Status: StatusFail, Subject: "router owner", Detail: "not owned by this berth root; this one's 'berth up' or 'berth router up' refuses it until 'berth router down' hands it over"}
		if err != nil || got == nil || *got != *want {
			t.Fatalf("got %+v, %v", got, err)
		}
	})

	t.Run("propagates a failure to ask who owns the router", func(t *testing.T) {
		fake := &fakeRouterSource{ownerErr: errors.New("boom")}
		if _, err := routerOwnerCheck(context.Background(), fake, "/home/dev/.berth", "berth"); err == nil || err.Error() != "boom" {
			t.Fatalf("got %v", err)
		}
	})
}

// Go-only, per #55: routeConflicts now reads dynamic-configuration
// fragments, not routed containers' own Traefik labels (Traefik's
// Docker-provider discovery is gone). writeFragmentInfo stages one real
// fragment on disk under dir, as `up` would have left it, and returns the
// router.FragmentInfo routeConflicts reads it back through.
func writeFragmentInfo(t *testing.T, dir, repositoryID, worktreeID, service, routerName, rule string) router.FragmentInfo {
	t.Helper()
	state := layout.RouterLayout{DynamicDir: dir}
	if err := router.WriteFragment(state, repositoryID, worktreeID, service, router.NewFragmentDocument(routerName, rule, routerName, 8080)); err != nil {
		t.Fatal(err)
	}
	return router.FragmentInfo{
		RepositoryID: repositoryID, WorktreeID: worktreeID, Service: service,
		Path: router.FragmentPath(state, repositoryID, worktreeID, service),
	}
}

func TestRouteConflictsInternal(t *testing.T) {
	const (
		repoA = "11111111-1111-1111-1111-111111111111"
		repoB = "22222222-2222-2222-2222-222222222222"
		repoC = "33333333-3333-3333-3333-333333333333"
	)

	t.Run("finds a rule two different fragments claim, naming their own repository.worktree.service identities when neither has a registry record", func(t *testing.T) {
		dir := t.TempDir()
		a := writeFragmentInfo(t, dir, repoA, repoB, "mail", "a-mail", "Host(`mail.main.acme.localhost`)")
		b := writeFragmentInfo(t, dir, repoB, repoA, "api", "b-api", "Host(`mail.main.acme.localhost`)")
		c := writeFragmentInfo(t, dir, repoC, repoC, "api", "c-api", "Host(`other.acme.localhost`)")

		conflicts := routeConflicts(dir, []router.FragmentInfo{a, b, c}, nil)
		if len(conflicts) != 1 {
			t.Fatalf("conflicts = %+v", conflicts)
		}
		names := conflicts["Host(`mail.main.acme.localhost`)"]
		wantA, wantB := repoA+"."+repoB+".mail", repoB+"."+repoA+".api"
		if len(names) != 2 || names[0] != minString(wantA, wantB) || names[1] != maxString(wantA, wantB) {
			t.Fatalf("names = %v, want %v and %v", names, wantA, wantB)
		}
	})

	// #55, found in review: a raw UUID triple is hard for a person to act
	// on. A claimant the registry still has a record for is named the way
	// the Locks section already names a lock's own worktree: its routed
	// hostname (layout.WorktreeHost) plus the service, resolved from
	// byKey — lockChecks' own map, keyed "<repositoryID>/<worktreeID>" —
	// so the two sections resolve an identity the same way. A claimant with
	// no record (removed, or never registered here) falls back to the raw
	// identity, since there is nothing else to show.
	t.Run("resolves a claiming fragment's identity against the registry, falling back to the raw identity when it has no record", func(t *testing.T) {
		dir := t.TempDir()
		a := writeFragmentInfo(t, dir, repoA, repoB, "mail", "a-mail", "Host(`mail.main.acme.localhost`)")
		b := writeFragmentInfo(t, dir, repoB, repoA, "api", "b-api", "Host(`mail.main.acme.localhost`)")
		byKey := map[string]registry.RegisteredWorktree{
			repoA + "/" + repoB: {RepositoryID: repoA, WorktreeID: repoB, Slug: "feature-login", Domain: "acme.localhost"},
		}

		conflicts := routeConflicts(dir, []router.FragmentInfo{a, b}, byKey)
		if len(conflicts) != 1 {
			t.Fatalf("conflicts = %+v", conflicts)
		}
		names := conflicts["Host(`mail.main.acme.localhost`)"]
		wantResolved, wantRaw := "feature-login.acme.localhost (mail)", repoB+"."+repoA+".api"
		if len(names) != 2 || names[0] != minString(wantResolved, wantRaw) || names[1] != maxString(wantResolved, wantRaw) {
			t.Fatalf("names = %v, want %v and %v", names, wantResolved, wantRaw)
		}
	})

	t.Run("does not conflict when only one fragment claims a rule", func(t *testing.T) {
		dir := t.TempDir()
		a := writeFragmentInfo(t, dir, repoA, repoB, "api", "a-api", "Host(`x`)")
		if got := routeConflicts(dir, []router.FragmentInfo{a}, nil); len(got) != 0 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("skips a fragment it cannot read or decode, rather than failing or counting it as a claimant", func(t *testing.T) {
		dir := t.TempDir()
		missing := router.FragmentInfo{RepositoryID: repoA, WorktreeID: repoB, Service: "gone", Path: filepath.Join(dir, "gone.yaml")}
		if err := os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte("not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		broken := router.FragmentInfo{RepositoryID: repoA, WorktreeID: repoC, Service: "broken", Path: filepath.Join(dir, "broken.yaml")}
		good := writeFragmentInfo(t, dir, repoB, repoC, "api", "b-api", "Host(`x`)")

		if got := routeConflicts(dir, []router.FragmentInfo{missing, broken, good}, nil); len(got) != 0 {
			t.Fatalf("expected no conflicts (only one readable claimant), got %+v", got)
		}
	})

	// Found in review, round 2 of #55: readFragmentRules read a fragment's
	// path with a plain os.ReadFile, no safe-path check at all, unlike the
	// writers (router.WriteFragment, via files.AssertSafeFilePathNamed) —
	// so a symlink planted directly under the dynamic-configuration
	// directory, at a name that still parses as one worktree's fragment,
	// would have doctor follow it out and read whatever it points to,
	// reporting that as a route conflict rather than refusing it the way a
	// write there already would.
	t.Run("skips a fragment path that is a symlink escaping the dynamic-configuration directory, rather than following it", func(t *testing.T) {
		dir := registrytest.TempDir(t)
		target := registrytest.TempDir(t)
		secretPath := filepath.Join(target, "secret.yaml")
		if err := os.WriteFile(secretPath, []byte(`{"http":{"routers":{"evil":{"rule":"Host(`+"`x`"+`)"}}}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		linkPath := filepath.Join(dir, repoA+"."+repoB+".api.yaml")
		if err := os.Symlink(secretPath, linkPath); err != nil {
			t.Fatal(err)
		}
		planted := router.FragmentInfo{RepositoryID: repoA, WorktreeID: repoB, Service: "api", Path: linkPath}
		good := writeFragmentInfo(t, dir, repoB, repoC, "api", "b-api", "Host(`x`)")

		if got := routeConflicts(dir, []router.FragmentInfo{planted, good}, nil); len(got) != 0 {
			t.Fatalf("expected the symlinked fragment skipped, not read, got %+v", got)
		}
	})
}

// minString is the lexically smaller of a and b: the first of a two-element
// names slice, which routeConflicts sorts the same way.
func minString(a, b string) string {
	if a < b {
		return a
	}
	return b
}

// maxString is the lexically larger of a and b: the second of that slice.
func maxString(a, b string) string {
	if a > b {
		return a
	}
	return b
}

/* --------------------------------------------------------------- report ---- */

func TestReportInternal(t *testing.T) {
	sections := []Section{
		{Title: "First", Checks: []Check{{Status: StatusOK, Subject: "a", Detail: "fine"}, {Status: StatusWarn, Subject: "longer", Detail: "hmm"}}},
		{Title: "Second", Checks: []Check{{Status: StatusSkip, Subject: "x\x1b[31m", Detail: "line\nbreak"}}},
	}

	t.Run("aligns each section and filters everything borrowed", func(t *testing.T) {
		got := formatDoctorReport(sections)
		want := []string{
			"First",
			"  ok    a       fine",
			"  warn  longer  hmm",
			"",
			"Second",
			"  skip  x�[31m  line break",
		}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("line %d = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("exits 0 without failures, 1 with one, and with the hook's own code when the hook failed", func(t *testing.T) {
		if got := doctorExitCode(sections); got != 0 {
			t.Fatalf("got %d, want 0", got)
		}
		failing := func(exitCode *int) []Section {
			return append(append([]Section{}, sections...), Section{Title: "T", Checks: []Check{
				{ID: "router.container", Status: StatusFail, Subject: "router", Detail: "x"},
				{ID: "checkout.hooks.verify", Status: StatusFail, Subject: "verify hook", Detail: "y", ExitCode: exitCode},
			}})
		}
		if got := doctorExitCode(failing(nil)); got != 1 {
			t.Fatalf("got %d, want 1", got)
		}
		three := 3
		if got := doctorExitCode(failing(&three)); got != 3 {
			t.Fatalf("got %d, want 3", got)
		}
		zero := 0
		// A failure never exits 0, whatever code it carries.
		if got := doctorExitCode(failing(&zero)); got != 1 {
			t.Fatalf("got %d, want 1", got)
		}
	})

	t.Run("sums up failures as an error, or says as information that nothing failed and what was only noted", func(t *testing.T) {
		if got := doctorSummary(sections); got.level != "info" || got.message != "No checks failed (1 warning(s), 1 skipped)." {
			t.Fatalf("got %+v", got)
		}
		if got := doctorSummary([]Section{{Title: "T", Checks: []Check{{Status: StatusOK, Subject: "a"}}}}); got.level != "info" || got.message != "No checks failed." {
			t.Fatalf("got %+v", got)
		}
		failing := append(append([]Section{}, sections...), Section{Title: "T", Checks: []Check{{Status: StatusFail, Subject: "a"}}})
		if got := doctorSummary(failing); got.level != "error" || got.message != "1 check(s) failed." {
			t.Fatalf("got %+v", got)
		}
	})
}

/* -------------------------------------------------------------- doctor ---- */

// doctorRepoA/B and doctorWorktreeA/B are fixed, valid-shape UUIDs for
// staging dynamic-configuration fragments across this function's subtests.
const (
	doctorRepoA     = "11111111-1111-1111-1111-111111111111"
	doctorRepoB     = "22222222-2222-2222-2222-222222222222"
	doctorWorktreeA = "33333333-3333-3333-3333-333333333333"
	doctorWorktreeB = "44444444-4444-4444-4444-444444444444"
)

func TestDoctorSections(t *testing.T) {
	t.Run("reports a healthy machine outside any checkout, and exits 0", func(t *testing.T) {
		opts := testOpts(t, nil)
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"Prerequisites", "Registry", "Locks", "Router", "This checkout"}
		got := sectionTitles(sections)
		if len(got) != len(want) {
			t.Fatalf("titles = %v", got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("titles = %v, want %v", got, want)
			}
		}
		// No Node.js check: Prerequisites is Git, Docker Engine, Docker Compose.
		wantPrereq := []string{"ok Git", "ok Docker Engine", "ok Docker Compose"}
		gotPrereq := statuses(sections, "Prerequisites")
		if len(gotPrereq) != len(wantPrereq) {
			t.Fatalf("prerequisites = %v", gotPrereq)
		}
		for i := range wantPrereq {
			if gotPrereq[i] != wantPrereq[i] {
				t.Fatalf("prerequisites = %v, want %v", gotPrereq, wantPrereq)
			}
		}
		// Registry[0] is "berth root" (#80); the control-home check follows it.
		if !contains(section(sections, "Registry")[1].Detail, "not created yet") {
			t.Fatalf("registry[1] = %+v", section(sections, "Registry")[1])
		}
		wantRouter := []string{"ok router", "ok routes", "ok address pools", "ok router port"}
		gotRouter := statuses(sections, "Router")
		if len(gotRouter) != 4 || gotRouter[0] != wantRouter[0] || gotRouter[1] != wantRouter[1] || gotRouter[2] != wantRouter[2] || gotRouter[3] != wantRouter[3] {
			t.Fatalf("router = %v", gotRouter)
		}
		checkout := section(sections, "This checkout")
		if len(checkout) != 1 || checkout[0].Status != StatusSkip || checkout[0].Subject != "berth.toml" {
			t.Fatalf("this checkout = %+v", checkout)
		}
		if code := doctorExitCode(sections); code != 0 {
			t.Fatalf("code = %d, want 0", code)
		}
	})

	t.Run("bounds every Docker question in time", func(t *testing.T) {
		runner := newDoctorRunner(nil)
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner })
		if _, err := doctorSections(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
		var dockerArgs []string
		for _, call := range runner.state.callsSnapshot() {
			if call.Command == "docker" {
				dockerArgs = append(dockerArgs, call.Args[0])
				if call.Timeout != dockerProbeTimeout {
					t.Errorf("call %v: timeout = %v, want %v", call.Args, call.Timeout, dockerProbeTimeout)
				}
			}
		}
		// version and compose run concurrently (so a Docker that answers
		// neither costs one timeout rather than two; see dockerChecks), so
		// their relative order is not guaranteed. network (the
		// address-pools check; routes itself makes no Docker call at all
		// with no fragments staged) always comes last, once both have
		// resolved.
		if len(dockerArgs) != 3 || dockerArgs[2] != "network" {
			t.Fatalf("dockerArgs = %v, want [version compose] (either order) then network", dockerArgs)
		}
		firstTwo := map[string]bool{dockerArgs[0]: true, dockerArgs[1]: true}
		if !firstTwo["version"] || !firstTwo["compose"] {
			t.Fatalf("dockerArgs = %v, want [version compose] (either order) then network", dockerArgs)
		}
	})

	// #82: doctor's own read-only report of which berth root started the
	// running router, right after the router status line.
	t.Run("fails the running router's own owner when it belongs to a different berth root", func(t *testing.T) {
		fakeRouter := newFakeRouterSource()
		fakeRouter.owner = router.Ownership{Running: true, Labelled: true, Home: "/home/other/.berth"}
		opts := testOpts(t, func(o *doctorOptions) { o.Router = fakeRouter })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		wantRouter := []string{"ok router", "fail router owner", "ok routes", "ok address pools", "ok router port"}
		gotRouter := statuses(sections, "Router")
		if len(gotRouter) != len(wantRouter) {
			t.Fatalf("router = %v, want %v", gotRouter, wantRouter)
		}
		for i := range wantRouter {
			if gotRouter[i] != wantRouter[i] {
				t.Fatalf("router = %v, want %v", gotRouter, wantRouter)
			}
		}
		if !contains(section(sections, "Router")[1].Detail, "/home/other/.berth") {
			t.Fatalf("router[1] = %+v", section(sections, "Router")[1])
		}
		if code := doctorExitCode(sections); code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
	})

	t.Run("skips the router's own state, which is read through Compose, when Compose does not answer, and still reads routes", func(t *testing.T) {
		runner := newDoctorRunner(nil)
		runner.state.docker["compose"] = outcome{result: run.Result{Stderr: "docker: 'compose' is not a docker command.", ExitCode: 1}}
		fakeRouter := newFakeRouterSource()
		// Without Compose the port line must not ask the router for its
		// published port at all (found in review of #79): a fake that
		// would fail that call proves it is never made.
		fakeRouter.publishedErr = errors.New("compose absent")
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner; o.Router = fakeRouter })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := []Check{
			{ID: "router.container", Status: StatusSkip, Subject: "router", Detail: "needs Docker Compose"},
			{ID: "router.routes", Status: StatusOK, Subject: "routes", Detail: "no route fragments"},
			{ID: "router.address-pools", Status: StatusOK, Subject: "address pools", Detail: "0 bridge networks"},
			{ID: "router.port", Status: StatusOK, Subject: "router port", Detail: opts.RouterPort.Describe()},
		}
		got := section(sections, "Router")
		if len(got) != 4 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
			t.Fatalf("router = %+v", got)
		}
		if fakeRouter.asked != 0 {
			t.Fatalf("asked = %d, want 0", fakeRouter.asked)
		}
	})

	// This scenario does not apply to Go: run.Runner.Which returns a plain
	// bool and cannot fail the way a real subprocess check could reject.
	// dockerChecks has nothing to catch here.
	t.Run("reports Docker checks that could not even be made, and skips the router rather than guessing", func(t *testing.T) {
		t.Skip("run.Runner.Which returns a plain bool (internal/run/run.go) and cannot fail the way " +
			"a real subprocess check could reject; dockerChecks has nothing to catch")
	})

	t.Run("fails Docker off PATH, skips Compose and the router, and never asks the router", func(t *testing.T) {
		runner := newDoctorRunner(nil)
		runner.state.dockerOnPath = false
		fakeRouter := newFakeRouterSource()
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner; o.Router = fakeRouter })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		gotPrereq := statuses(sections, "Prerequisites")[1:]
		want := []string{"fail Docker Engine", "skip Docker Compose"}
		if len(gotPrereq) != 2 || gotPrereq[0] != want[0] || gotPrereq[1] != want[1] {
			t.Fatalf("prerequisites[1:] = %v", gotPrereq)
		}
		gotRouter := section(sections, "Router")
		if len(gotRouter) != 1 || gotRouter[0] != (Check{ID: "router.container", Status: StatusSkip, Subject: "router", Detail: "needs Docker"}) {
			t.Fatalf("router = %+v", gotRouter)
		}
		if fakeRouter.asked != 0 {
			t.Fatalf("asked = %d, want 0", fakeRouter.asked)
		}
		if code := doctorExitCode(sections); code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
	})

	t.Run("fails a daemon that does not answer in Docker's own words, and skips the router", func(t *testing.T) {
		runner := newDoctorRunner(nil)
		runner.state.docker["version"] = outcome{result: run.Result{
			Stderr: "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?", ExitCode: 1,
		}}
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := Check{ID: "prerequisites.docker.engine", Status: StatusFail, Subject: "Docker Engine",
			Detail: "Docker is not available: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"}
		got := section(sections, "Prerequisites")[1]
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		if gotRouter := statuses(sections, "Router"); len(gotRouter) != 1 || gotRouter[0] != "skip router" {
			t.Fatalf("router = %v", gotRouter)
		}
	})

	t.Run("fails a Docker that does not answer in time, and one that refuses for another reason", func(t *testing.T) {
		runner := newDoctorRunner(nil)
		runner.state.docker["version"] = outcome{result: run.Result{ExitCode: 1, TimedOut: true}}
		runner.state.docker["compose"] = outcome{result: run.Result{Stderr: "WARNING: noise\nunexpected failure", ExitCode: 2}}
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := []Check{
			{ID: "prerequisites.docker.engine", Status: StatusFail, Subject: "Docker Engine", Detail: "`docker version --format {{.Server.Version}}` did not answer within 30 s"},
			{ID: "prerequisites.docker.compose", Status: StatusFail, Subject: "Docker Compose", Detail: "unexpected failure"},
		}
		got := section(sections, "Prerequisites")[1:]
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("fails a Compose or Git older than the minimum, and a Git that cannot be started", func(t *testing.T) {
		runner := newDoctorRunner(nil)
		runner.state.docker["compose"] = okOutcome("2.20.3\n")
		runner.state.gitVersion = okOutcome("git version 2.30.1\n")
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		found20, found30 := false, false
		for _, c := range section(sections, "Prerequisites") {
			if c.Detail == "2.20.3; 2.21 or newer is needed" {
				found20 = true
			}
			if c.Detail == "2.30.1; 2.36 or newer is needed" {
				found30 = true
			}
		}
		if !found20 || !found30 {
			t.Fatalf("prerequisites = %+v", section(sections, "Prerequisites"))
		}

		runner.state.gitVersion = outcome{err: &run.StartError{Command: "git", Code: "ENOENT", Detail: "is not on PATH (ENOENT).", ExitCode: 127}}
		sections, err = doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := Check{ID: "prerequisites.git", Status: StatusFail, Subject: "Git", Detail: "`git` could not be started: it is not on PATH (ENOENT)."}
		if got := section(sections, "Prerequisites")[0]; got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}

		runner.state.gitVersion = outcome{result: run.Result{Stderr: "fatal: bad\n", ExitCode: 128}}
		sections, err = doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want = Check{ID: "prerequisites.git", Status: StatusFail, Subject: "Git", Detail: "`git --version` failed: fatal: bad"}
		if got := section(sections, "Prerequisites")[0]; got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	// "fails Node outside the engines range" has no Go counterpart: there is
	// no Node.js check at all.
	t.Run("fails Node outside the engines range", func(t *testing.T) {
		t.Skip("doctor drops the Node.js engines check entirely")
	})

	t.Run("refuses a control home berth did not create, and reads registry problems from it", func(t *testing.T) {
		controlHome := t.TempDir()
		if err := os.WriteFile(filepath.Join(controlHome, "someone-elses.txt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		opts := testOpts(t, func(o *doctorOptions) { o.ControlHome = controlHome })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		// Registry[0] is "berth root" (#80), always ok; the control-home
		// check itself is [1].
		got := section(sections, "Registry")[1]
		if got.Status != StatusFail || got.Subject != "control home" || !contains(got.Detail, "Refusing to use") {
			t.Fatalf("got %+v", got)
		}

		if err := os.Remove(filepath.Join(controlHome, "someone-elses.txt")); err != nil {
			t.Fatal(err)
		}
		// A marker with this build's own format makes the control home
		// adoptable, distinct from the refusal above.
		if err := os.WriteFile(filepath.Join(controlHome, files.ControlHomeMarker), []byte(files.ControlHomeFormat), 0o600); err != nil {
			t.Fatal(err)
		}
		repositoryID := randomTestUUID()
		regDir := registry.RegistryDirectory(controlHome, repositoryID)
		if err := os.MkdirAll(regDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(regDir, randomTestUUID()+".json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		sections, err = doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"ok berth root", "ok control home", "ok router state", "ok registrations", "fail unreadable record"}
		gotStatuses := statuses(sections, "Registry")
		if len(gotStatuses) != len(want) {
			t.Fatalf("registry = %v", gotStatuses)
		}
		for i := range want {
			if gotStatuses[i] != want[i] {
				t.Fatalf("registry = %v, want %v", gotStatuses, want)
			}
		}
	})

	t.Run("fails a control home that cannot be looked at, rather than calling it not created yet", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "a-file")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		opts := testOpts(t, func(o *doctorOptions) { o.ControlHome = filepath.Join(file, "control") })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		// Registry[0] is "berth root" (#80), always ok; the control-home
		// check itself is [1].
		got := section(sections, "Registry")[1]
		// The os package formats this underlying error in its own words
		// ("not a directory") rather than an errno name.
		if got.Status != StatusFail || got.Subject != "control home" || !contains(got.Detail, "not a directory") {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("reports a router it could not describe, and one whose status call throws, without losing the rest", func(t *testing.T) {
		opts := testOpts(t, func(o *doctorOptions) {
			o.Router = nil
			o.RouterErr = fmt.Errorf(`BERTH_ROUTER_PORT must be a port number between 1 and 65535, not "x".`)
		})
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := []Check{{ID: "router.container", Status: StatusFail, Subject: "router", Detail: `BERTH_ROUTER_PORT must be a port number between 1 and 65535, not "x".`}}
		if got := section(sections, "Router"); len(got) != 1 || got[0] != want[0] {
			t.Fatalf("got %+v", got)
		}

		fakeRouter := newFakeRouterSource()
		fakeRouter.err = errors.New("exploded")
		opts = testOpts(t, func(o *doctorOptions) { o.Router = fakeRouter })
		sections, err = doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		wantRouter := []Check{
			{ID: "router.container", Status: StatusFail, Subject: "router", Detail: "exploded"},
			{ID: "router.routes", Status: StatusOK, Subject: "routes", Detail: "no route fragments"},
			{ID: "router.address-pools", Status: StatusOK, Subject: "address pools", Detail: "0 bridge networks"},
			{ID: "router.port", Status: StatusOK, Subject: "router port", Detail: opts.RouterPort.Describe()},
		}
		got := section(sections, "Router")
		if len(got) != 4 || got[0] != wantRouter[0] || got[1] != wantRouter[1] || got[2] != wantRouter[2] || got[3] != wantRouter[3] {
			t.Fatalf("got %+v", got)
		}
		found := false
		for _, title := range sectionTitles(sections) {
			if title == "This checkout" {
				found = true
			}
		}
		if !found {
			t.Fatal("expected a This checkout section")
		}
	})

	t.Run("lets a cancellation end the command instead of becoming a line", func(t *testing.T) {
		fakeRouter := newFakeRouterSource()
		fakeRouter.err = cancelled(syscall.SIGINT)
		opts := testOpts(t, func(o *doctorOptions) { o.Router = fakeRouter })
		_, err := doctorSections(context.Background(), opts)
		wantCancelled(t, err, "Operation cancelled by SIGINT.")
	})

	// Blocking review finding: askDocker used to fold every error, a
	// cancellation included, into a failed Check. It now catches only
	// Docker being unavailable; everything else — Cancelled among it —
	// propagates. These four pin that for every docker call
	// doctor makes on its own: the Engine and Compose probes (run
	// concurrently, so each needs its own case) and the routes section's
	// `ps` and `inspect` calls.
	t.Run("lets a cancellation during the Docker Engine probe end the command", func(t *testing.T) {
		runner := newDoctorRunner(nil)
		runner.state.docker["version"] = outcome{err: cancelled(syscall.SIGINT)}
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner })
		_, err := doctorSections(context.Background(), opts)
		wantCancelled(t, err, "Operation cancelled by SIGINT.")
	})

	t.Run("lets a cancellation during the Docker Compose probe end the command", func(t *testing.T) {
		runner := newDoctorRunner(nil)
		runner.state.docker["compose"] = outcome{err: cancelled(syscall.SIGINT)}
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner })
		_, err := doctorSections(context.Background(), opts)
		wantCancelled(t, err, "Operation cancelled by SIGINT.")
	})

	// Go-only, per #55: routeChecks now reads dynamic-configuration
	// fragments from disk (router.ListFragments), not routed containers'
	// own Traefik labels through `docker ps`/`docker inspect` — Traefik's
	// Docker-provider discovery is gone, so there is no longer a bulk
	// `docker inspect` call whose own timeout or output-overflow handling
	// could apply (the two "half-reads an inspect..." cases below have no
	// replacement for the same reason: reading a fragment file either
	// succeeds or is skipped, never half-reads). The one Docker call left
	// in routeChecks at all is orphanedFragmentsCheck's own `docker network
	// ls` of the router's labelled worktree networks — a cancellation there
	// is what the case below now pins.
	t.Run("lets a cancellation while listing the router's worktree networks end the command", func(t *testing.T) {
		state := layout.RouterLayout{DynamicDir: t.TempDir()}
		if err := router.WriteFragment(state, doctorRepoA, doctorWorktreeA, "api",
			router.NewFragmentDocument("x", "Host(`x`)", "x", 8080)); err != nil {
			t.Fatal(err)
		}
		runner := newDoctorRunner(nil)
		runner.state.docker["network"] = outcome{err: cancelled(syscall.SIGINT)}
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner; o.RouterState = state })
		_, err := doctorSections(context.Background(), opts)
		wantCancelled(t, err, "Operation cancelled by SIGINT.")
	})

	t.Run("fails two fragments that claim the same rule", func(t *testing.T) {
		state := layout.RouterLayout{DynamicDir: t.TempDir()}
		if err := router.WriteFragment(state, doctorRepoA, doctorWorktreeA, "mail",
			router.NewFragmentDocument("berth-aaa-mail", "Host(`mail.main.acme.localhost`)", "berth-aaa-mail", 8080)); err != nil {
			t.Fatal(err)
		}
		if err := router.WriteFragment(state, doctorRepoB, doctorWorktreeB, "api",
			router.NewFragmentDocument("berth-bbb-api", "Host(`mail.main.acme.localhost`)", "berth-bbb-api", 8080)); err != nil {
			t.Fatal(err)
		}
		opts := testOpts(t, func(o *doctorOptions) { o.RouterState = state })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		nameA := doctorRepoA + "." + doctorWorktreeA + ".mail"
		nameB := doctorRepoB + "." + doctorWorktreeB + ".api"
		want := Check{ID: "router.route-conflict", Instance: "mail.main.acme.localhost", Status: StatusFail, Subject: "routes",
			Detail: fmt.Sprintf("Host(`mail.main.acme.localhost`) is claimed by %s, %s; requests for it may reach any of them",
				minString(nameA, nameB), maxString(nameA, nameB))}
		if got := section(sections, "Router")[1]; got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("keeps a conflict line when listing the router's worktree networks fails, reporting that failure on its own line", func(t *testing.T) {
		state := layout.RouterLayout{DynamicDir: t.TempDir()}
		if err := router.WriteFragment(state, doctorRepoA, doctorWorktreeA, "mail",
			router.NewFragmentDocument("berth-aaa-mail", "Host(`mail.main.acme.localhost`)", "berth-aaa-mail", 8080)); err != nil {
			t.Fatal(err)
		}
		if err := router.WriteFragment(state, doctorRepoB, doctorWorktreeB, "api",
			router.NewFragmentDocument("berth-bbb-api", "Host(`mail.main.acme.localhost`)", "berth-bbb-api", 8080)); err != nil {
			t.Fatal(err)
		}
		runner := newDoctorRunner(nil)
		runner.state.docker["network"] = outcome{result: run.Result{ExitCode: 1, Stderr: "Cannot connect to the Docker daemon"}}
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner; o.RouterState = state })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		var conflict, failure bool
		for _, check := range section(sections, "Router") {
			if check.Subject != "routes" || check.Status != StatusFail {
				continue
			}
			if strings.Contains(check.Detail, "is claimed by") {
				conflict = true
			} else {
				failure = true
			}
		}
		if !conflict || !failure {
			t.Fatalf("want both the conflict and the network-listing failure as routes lines, got %+v", section(sections, "Router"))
		}
	})

	t.Run("bounds a rule any fragment could have written before it reaches the report", func(t *testing.T) {
		state := layout.RouterLayout{DynamicDir: t.TempDir()}
		rule := "Host(`" + repeatString("a", 500) + "`)"
		if err := router.WriteFragment(state, doctorRepoA, doctorWorktreeA, "one", router.NewFragmentDocument("one", rule, "one", 8080)); err != nil {
			t.Fatal(err)
		}
		if err := router.WriteFragment(state, doctorRepoB, doctorWorktreeB, "two", router.NewFragmentDocument("two", rule, "two", 8080)); err != nil {
			t.Fatal(err)
		}
		opts := testOpts(t, func(o *doctorOptions) { o.RouterState = state })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		detail := section(sections, "Router")[1].Detail
		want := string([]rune(rule)[:199]) + "…"
		if len(detail) < len(want) || detail[:len(want)] != want {
			t.Fatalf("detail = %q, want prefix %q", detail, want)
		}
	})

	t.Run("bounds the list of fragments claiming a rule, however many there are", func(t *testing.T) {
		state := layout.RouterLayout{DynamicDir: t.TempDir()}
		for i := 0; i < 60; i++ {
			worktreeID := fmt.Sprintf("%08d-0000-4000-8000-000000000000", i)
			name := fmt.Sprintf("claimant-%02d", i)
			if err := router.WriteFragment(state, doctorRepoA, worktreeID, "api", router.NewFragmentDocument(name, "Host(`x`)", name, 8080)); err != nil {
				t.Fatal(err)
			}
		}
		opts := testOpts(t, func(o *doctorOptions) { o.RouterState = state })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		detail := section(sections, "Router")[1].Detail
		prefix := "Host(`x`) is claimed by "
		idx := indexOf(detail, "; requests")
		if idx < 0 || len(detail) < len(prefix) {
			t.Fatalf("detail = %q", detail)
		}
		claimed := detail[len(prefix):idx]
		if n := utf8.RuneCountInString(claimed); n != 200 || !isEllipsisSuffix(claimed) {
			t.Fatalf("claimed = %q (%d runes)", claimed, n)
		}
	})

	t.Run("counts route fragments when no rule is shared", func(t *testing.T) {
		state := layout.RouterLayout{DynamicDir: t.TempDir()}
		if err := router.WriteFragment(state, doctorRepoA, doctorWorktreeA, "api",
			router.NewFragmentDocument("berth-aaa-api", "Host(`a`)", "berth-aaa-api", 8080)); err != nil {
			t.Fatal(err)
		}
		opts := testOpts(t, func(o *doctorOptions) { o.RouterState = state })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := Check{ID: "router.routes", Status: StatusOK, Subject: "routes", Detail: "1 route fragment; no two routers share a rule"}
		if got := section(sections, "Router")[1]; got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	// Found in review of #55: the singular case above must not read "1
	// route fragment(s)"; this pins the plural alongside it.
	t.Run("pluralizes route fragments in the clean-routes count", func(t *testing.T) {
		state := layout.RouterLayout{DynamicDir: t.TempDir()}
		if err := router.WriteFragment(state, doctorRepoA, doctorWorktreeA, "api",
			router.NewFragmentDocument("berth-aaa-api", "Host(`a`)", "berth-aaa-api", 8080)); err != nil {
			t.Fatal(err)
		}
		if err := router.WriteFragment(state, doctorRepoA, doctorWorktreeA, "web",
			router.NewFragmentDocument("berth-aaa-web", "Host(`b`)", "berth-aaa-web", 8081)); err != nil {
			t.Fatal(err)
		}
		opts := testOpts(t, func(o *doctorOptions) { o.RouterState = state })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := Check{ID: "router.routes", Status: StatusOK, Subject: "routes", Detail: "2 route fragments; no two routers share a rule"}
		if got := section(sections, "Router")[1]; got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	// Go-only, per #55: doctor's own report-only half of orphan
	// reconciliation (internal/router's Ensure runs the repair half).
	t.Run("warns about route fragments whose worktree's router network is gone, without removing anything", func(t *testing.T) {
		state := layout.RouterLayout{DynamicDir: t.TempDir()}
		if err := router.WriteFragment(state, doctorRepoA, doctorWorktreeA, "api",
			router.NewFragmentDocument("berth-aaa-api", "Host(`a`)", "berth-aaa-api", 8080)); err != nil {
			t.Fatal(err)
		}
		runner := newDoctorRunner(nil)
		runner.state.docker["network"] = okOutcome("") // the router labels no worktree network
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner; o.RouterState = state })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := Check{ID: "router.orphaned-routes", Status: StatusWarn, Subject: "routes",
			Detail: "1 route fragment for 1 worktree whose router network is gone; the next 'berth up' or 'berth router up' removes them"}
		if got := section(sections, "Router")[2]; got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		// Never removed: doctor reads, never writes.
		fragments, err := router.ListFragments(state)
		if err != nil {
			t.Fatal(err)
		}
		if len(fragments) != 1 {
			t.Fatalf("expected the fragment to survive doctor untouched, got %d", len(fragments))
		}
	})

	// Pluralizes every count independently, including "network(s)" and its
	// own verb — a first cut of this message only ever pluralized "fragment"
	// and "worktree", leaving ">1 worktree" reading as "...whose router
	// network is gone" (singular subject, and wrong: each worktree here has
	// its own network).
	t.Run("pluralizes route fragments and worktrees separately in the orphan warning", func(t *testing.T) {
		state := layout.RouterLayout{DynamicDir: t.TempDir()}
		if err := router.WriteFragment(state, doctorRepoA, doctorWorktreeA, "api",
			router.NewFragmentDocument("berth-aaa-api", "Host(`a`)", "berth-aaa-api", 8080)); err != nil {
			t.Fatal(err)
		}
		if err := router.WriteFragment(state, doctorRepoA, doctorWorktreeA, "web",
			router.NewFragmentDocument("berth-aaa-web", "Host(`a2`)", "berth-aaa-web", 8081)); err != nil {
			t.Fatal(err)
		}
		if err := router.WriteFragment(state, doctorRepoB, doctorWorktreeB, "api",
			router.NewFragmentDocument("berth-bbb-api", "Host(`b`)", "berth-bbb-api", 8080)); err != nil {
			t.Fatal(err)
		}
		runner := newDoctorRunner(nil)
		runner.state.docker["network"] = okOutcome("") // neither worktree's network is labelled
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner; o.RouterState = state })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := Check{ID: "router.orphaned-routes", Status: StatusWarn, Subject: "routes",
			Detail: "3 route fragments for 2 worktrees whose router networks are gone; the next 'berth up' or 'berth router up' removes them"}
		if got := section(sections, "Router")[2]; got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	// The regression the first cut had: liveness was read from containers'
	// ownership labels in the brand's own namespace, so a project that
	// overrides its label-namespace looked orphaned while it was up. The
	// network's name and RoutedByLabel do not depend on that override.
	t.Run("does not warn about a live worktree's fragments, whatever label-namespace its project uses", func(t *testing.T) {
		state := layout.RouterLayout{DynamicDir: t.TempDir()}
		if err := router.WriteFragment(state, doctorRepoA, doctorWorktreeA, "api",
			router.NewFragmentDocument("berth-aaa-api", "Host(`a`)", "berth-aaa-api", 8080)); err != nil {
			t.Fatal(err)
		}
		runner := newDoctorRunner(nil)
		runner.state.docker["network"] = okOutcome(layout.WorktreeRouterNetwork(doctorRepoA, doctorWorktreeA) + "\n")
		runner.state.docker["ps"] = okOutcome("") // no container in the brand's own namespace
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner; o.RouterState = state })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, check := range section(sections, "Router") {
			if check.Status == StatusWarn && check.Subject == "routes" {
				t.Fatalf("warned about a live worktree's fragment: %+v", check)
			}
		}
	})

	t.Run("warns about address pools once 20 bridge networks exist, not counting Docker's own bridge", func(t *testing.T) {
		for _, tc := range []struct {
			own  int
			want Check
		}{
			{1, Check{ID: "router.address-pools", Status: StatusOK, Subject: "address pools", Detail: "1 bridge network"}},
			{19, Check{ID: "router.address-pools", Status: StatusOK, Subject: "address pools", Detail: "19 bridge networks"}},
			{20, Check{ID: "router.address-pools", Status: StatusWarn, Subject: "address pools", Detail: "20 bridge networks exist; with Docker's default address pools (about 31) a later 'berth up' may fail to " +
				"create its network. Remove networks nothing uses with `docker network prune`, or give the " +
				"Docker daemon smaller or more `default-address-pools`."}},
		} {
			names := []string{"bridge"}
			for i := 0; i < tc.own; i++ {
				names = append(names, fmt.Sprintf("net-%02d", i))
			}
			runner := newDoctorRunner(nil)
			runner.state.docker["network"] = okOutcome(strings.Join(names, "\n") + "\n")
			opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner })
			sections, err := doctorSections(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			var got *Check
			for _, check := range section(sections, "Router") {
				if check.Subject == "address pools" {
					got = &check
				}
			}
			if got == nil || *got != tc.want {
				t.Fatalf("%d bridge networks: got %+v, want %+v", tc.own, got, tc.want)
			}
		}
	})

	t.Run("fails a config file that does not load", func(t *testing.T) {
		opts := testOpts(t, func(o *doctorOptions) { o.Checkout = checkoutUnloadable(errors.New("berth.toml: bad")) })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := []Check{{ID: "checkout.config", Status: StatusFail, Subject: "berth.toml", Detail: "berth.toml: bad"}}
		if got := section(sections, "This checkout"); len(got) != 1 || got[0] != want[0] {
			t.Fatalf("got %+v", got)
		}
		if code := doctorExitCode(sections); code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
	})
}

// repeatString is s repeated n times.
func repeatString(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

/* ------------------------------------------------------ doctor in a checkout ---- */

func TestDoctorInACheckoutSections(t *testing.T) {
	// #19: doctor answers "would up work?", so its berth.toml line fails on
	// the same control character in
	// a [service.index] label or purpose that `up` and `config validate`
	// refuse, with the exact message `config validate` itself would print
	// — not a warning, since `up` against this same file refuses outright.
	t.Run("fails the berth.toml check when a service index label or purpose carries a control character", func(t *testing.T) {
		f := newCheckoutFixture(t, "")
		f.loaded.Config.Service[0].Index = &config.ServiceIndex{Label: "a\tb"}
		sections, err := doctorSections(context.Background(), f.opts())
		if err != nil {
			t.Fatal(err)
		}
		checks := section(sections, "This checkout")
		want := config.CheckStart(f.loaded).Error()
		if checks[0].Status != StatusFail || checks[0].Subject != brand.ConfigFile || checks[0].Detail != want {
			t.Fatalf("got %+v, want fail %s: %q", checks[0], brand.ConfigFile, want)
		}
		if code := doctorExitCode(sections); code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
	})

	t.Run("skips the worktree and its verify hook until the worktree is registered", func(t *testing.T) {
		f := newCheckoutFixture(t, "./verify")
		sections, err := doctorSections(context.Background(), f.opts())
		if err != nil {
			t.Fatal(err)
		}
		checks := section(sections, "This checkout")
		// The resolution check runs whether or not the worktree is
		// registered — that's the whole point of #105 — so it appears
		// before the registration-gated "skip" the run itself still gives.
		want := []string{"ok berth.toml", "skip this worktree", "ok verify hook file", "skip verify hook"}
		got := make([]string, len(checks))
		for i, c := range checks {
			got[i] = string(c.Status) + " " + c.Subject
		}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
		if checks[3].Detail != "needs a registered worktree; run 'berth up' first" {
			t.Fatalf("detail = %q", checks[3].Detail)
		}
	})

	t.Run("says so when no verify hook is configured", func(t *testing.T) {
		f := newCheckoutFixture(t, "")
		f.register("feature-login")
		sections, err := doctorSections(context.Background(), f.opts())
		if err != nil {
			t.Fatal(err)
		}
		checks := section(sections, "This checkout")
		last := checks[len(checks)-1]
		if last != (Check{ID: "checkout.hooks.verify", Status: StatusSkip, Subject: "verify hook", Detail: "none configured"}) {
			t.Fatalf("got %+v", last)
		}
	})

	// hookResolutionChecks (#105): one line per configured hook, predicting
	// the exit code a real run would give without ever spawning anything,
	// whether or not this worktree is registered.
	t.Run("fails a configured hook whose file is missing, naming the hook and the exit code 127", func(t *testing.T) {
		f := newCheckoutFixture(t, "")
		seed := "./hooks/seed"
		f.loaded.Config.Hooks.Seed = &seed
		sections, err := doctorSections(context.Background(), f.opts())
		if err != nil {
			t.Fatal(err)
		}
		checks := section(sections, "This checkout")
		var found *Check
		for i, c := range checks {
			if c.Subject == "seed hook file" {
				found = &checks[i]
			}
		}
		if found == nil {
			t.Fatalf("no seed hook file check in %+v", checks)
		}
		wantDetail := "would not start (exit 127): `" + filepath.Join(f.root, "hooks", "seed") +
			"` does not exist — or the interpreter on its #! line is missing, as when the file has CRLF line endings (ENOENT)."
		if found.Status != StatusFail || found.Subject != "seed hook file" || found.Detail != wantDetail {
			t.Fatalf("got %+v, want detail %q", *found, wantDetail)
		}
		// The predicted 127 lives only in the line's own Detail: a
		// resolution failure never supplies doctorExitCode's own code (the
		// README promise is the verify hook's own code, or 1).
		if found.ExitCode != nil {
			t.Fatalf("exitCode = %v, want nil", found.ExitCode)
		}
		if code := doctorExitCode(sections); code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
	})

	t.Run("fails a configured hook that exists but is not executable, naming the hook and the exit code 126", func(t *testing.T) {
		f := newCheckoutFixture(t, "")
		prepare := "./hooks/prepare"
		f.loaded.Config.Hooks.Prepare = &prepare
		if err := os.MkdirAll(filepath.Join(f.root, "hooks"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.root, "hooks", "prepare"), []byte("#!/bin/sh\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		sections, err := doctorSections(context.Background(), f.opts())
		if err != nil {
			t.Fatal(err)
		}
		checks := section(sections, "This checkout")
		var found *Check
		for i, c := range checks {
			if c.Subject == "prepare hook file" {
				found = &checks[i]
			}
		}
		if found == nil {
			t.Fatalf("no prepare hook file check in %+v", checks)
		}
		if found.Status != StatusFail || found.ExitCode != nil {
			t.Fatalf("got %+v, want ExitCode nil", *found)
		}
		if !strings.Contains(found.Detail, "exit 126") || !strings.Contains(found.Detail, "is not executable (EACCES)") {
			t.Fatalf("detail = %q", found.Detail)
		}
		if code := doctorExitCode(sections); code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
	})

	t.Run("fails a configured hook with no #! line and no native header, naming the hook and the exit code 126", func(t *testing.T) {
		f := newCheckoutFixture(t, "")
		backup := "./hooks/backup"
		f.loaded.Config.Hooks.Backup = &backup
		if err := os.MkdirAll(filepath.Join(f.root, "hooks"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.root, "hooks", "backup"), []byte("not a script\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		sections, err := doctorSections(context.Background(), f.opts())
		if err != nil {
			t.Fatal(err)
		}
		checks := section(sections, "This checkout")
		var found *Check
		for i, c := range checks {
			if c.Subject == "backup hook file" {
				found = &checks[i]
			}
		}
		if found == nil {
			t.Fatalf("no backup hook file check in %+v", checks)
		}
		if found.Status != StatusFail || found.ExitCode != nil {
			t.Fatalf("got %+v, want ExitCode nil", *found)
		}
		if !strings.Contains(found.Detail, "exit 126") || !strings.Contains(found.Detail, "ENOEXEC") || !strings.Contains(found.Detail, "needs a #! line") {
			t.Fatalf("detail = %q", found.Detail)
		}
	})

	// #105 round 1 (Blocking): a broken hook file's predicted exit code must
	// never race or override the verify hook's own, real, observed one —
	// only the verify run's own failure ever sets doctorExitCode's code.
	t.Run("keeps the verify hook's own exit code when a different hook file is broken", func(t *testing.T) {
		f := newCheckoutFixture(t, "./verify")
		f.register("feature-login")
		backup := "./hooks/backup"
		f.loaded.Config.Hooks.Backup = &backup // never written: missing, 127 predicted
		f.runner.state.hook = outcome{result: run.Result{ExitCode: 7}}
		sections, err := doctorSections(context.Background(), f.opts())
		if err != nil {
			t.Fatal(err)
		}
		if code := doctorExitCode(sections); code != 7 {
			t.Fatalf("code = %d, want the verify hook's own 7", code)
		}
	})

	t.Run("exits 1 for a broken hook file alone, never its own predicted code", func(t *testing.T) {
		f := newCheckoutFixture(t, "")
		backup := "./hooks/backup"
		f.loaded.Config.Hooks.Backup = &backup // never written: missing, 127 predicted
		sections, err := doctorSections(context.Background(), f.opts())
		if err != nil {
			t.Fatal(err)
		}
		if code := doctorExitCode(sections); code != 1 {
			t.Fatalf("code = %d, want 1, not the predicted 127", code)
		}
	})

	t.Run("reports one line per configured hook, in schema order, regardless of registration", func(t *testing.T) {
		f := newCheckoutFixture(t, "./verify")
		for name, field := range map[string]**string{
			"prepare": &f.loaded.Config.Hooks.Prepare,
			"seed":    &f.loaded.Config.Hooks.Seed,
			"backup":  &f.loaded.Config.Hooks.Backup,
			"restore": &f.loaded.Config.Hooks.Restore,
		} {
			command := "./" + name
			*field = &command
			if err := os.WriteFile(filepath.Join(f.root, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		// Not registered: only the resolution checks can run.
		sections, err := doctorSections(context.Background(), f.opts())
		if err != nil {
			t.Fatal(err)
		}
		checks := section(sections, "This checkout")
		var got []string
		for _, c := range checks {
			if strings.HasSuffix(c.Subject, " hook file") || c.Subject == "verify hook" {
				got = append(got, string(c.Status)+" "+c.Subject)
			}
		}
		want := []string{
			"ok prepare hook file", "ok seed hook file", "ok verify hook file",
			"ok backup hook file", "ok restore hook file", "skip verify hook",
		}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("runs the verify hook with its output on stderr, and passes when it exits 0", func(t *testing.T) {
		f := newCheckoutFixture(t, "./verify")
		f.register("feature-login")
		opts := f.opts()
		// #28: verify's own BERTH_URL/BERTH_SERVICE_URL_API need a resolved
		// router port; f.opts()'s own zero value is fine for every other
		// check here, so it is set only on this local copy.
		opts.RouterPort = settings.PortResolution{Port: 80}
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		checks := section(sections, "This checkout")
		// One line for the resolution check ("verify hook file"), one for
		// the run ("verify hook") — distinct subjects (see hookResolutionChecks).
		want := []string{"ok berth.toml", "ok this worktree", "ok verify hook file", "ok verify hook"}
		got := make([]string, len(checks))
		for i, c := range checks {
			got[i] = string(c.Status) + " " + c.Subject
		}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
		if checks[1].Detail != "registered as feature-login.acme.localhost" {
			t.Fatalf("detail = %q", checks[1].Detail)
		}
		if checks[2].Detail != "resolves to "+filepath.Join(f.root, "verify") {
			t.Fatalf("resolution detail = %q", checks[2].Detail)
		}
		var hookCall *run.Spec
		for _, call := range f.runner.state.callsSnapshot() {
			call := call
			if call.Command == filepath.Join(f.root, "verify") {
				hookCall = &call
			}
		}
		if hookCall == nil {
			t.Fatal("verify hook was not run")
		}
		if hookCall.Output != run.TeeToStderr {
			t.Fatalf("output = %v, want TeeToStderr", hookCall.Output)
		}
		// #28: the fixture's one declared service ("api", no subdomain or
		// path — newCheckoutFixture) claims the host root, so verify sees
		// it as both BERTH_URL and BERTH_SERVICE_URL_API.
		wantEnv := map[string]string{
			"BERTH_URL":             "http://feature-login.acme.localhost",
			"BERTH_SERVICE_URL_API": "http://feature-login.acme.localhost",
		}
		for name, value := range wantEnv {
			found := false
			for _, entry := range hookCall.Env {
				if entry == name+"="+value {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("expected %s=%s in env %v", name, value, hookCall.Env)
			}
		}
	})

	t.Run("fails a verify hook that exits non-zero, and keeps its code", func(t *testing.T) {
		f := newCheckoutFixture(t, "./verify")
		f.register("feature-login")
		f.runner.state.hook = outcome{result: run.Result{ExitCode: 3}}
		sections, err := doctorSections(context.Background(), f.opts())
		if err != nil {
			t.Fatal(err)
		}
		checks := section(sections, "This checkout")
		last := checks[len(checks)-1]
		if last.Status != StatusFail || last.Subject != "verify hook" || last.Detail != "The verify hook failed (exit 3)." {
			t.Fatalf("got %+v", last)
		}
		if last.ExitCode == nil || *last.ExitCode != 3 {
			t.Fatalf("exitCode = %v, want 3", last.ExitCode)
		}
		if code := doctorExitCode(sections); code != 3 {
			t.Fatalf("code = %d, want 3", code)
		}
	})

	t.Run("lets a cancellation during the verify hook end the command", func(t *testing.T) {
		f := newCheckoutFixture(t, "./verify")
		f.register("feature-login")
		f.runner.state.hook = outcome{err: cancelled(syscall.SIGINT)}
		_, err := doctorSections(context.Background(), f.opts())
		wantCancelled(t, err, "Operation cancelled by SIGINT.")
	})

	t.Run("reports a verify hook that could not be run at all as its own failure, keeping the lines before it", func(t *testing.T) {
		f := newCheckoutFixture(t, "./verify")
		f.register("feature-login")
		f.runner.state.hook = outcome{err: errors.New("spawn went wrong")}
		sections, err := doctorSections(context.Background(), f.opts())
		if err != nil {
			t.Fatal(err)
		}
		checks := section(sections, "This checkout")
		want := []string{"ok berth.toml", "ok this worktree", "ok verify hook file", "fail verify hook"}
		got := make([]string, len(checks))
		for i, c := range checks {
			got[i] = string(c.Status) + " " + c.Subject
		}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
		last := checks[len(checks)-1]
		// Strictly: no exit code of its own, since no hook verdict was reached.
		if last != (Check{ID: "checkout.hooks.verify", Status: StatusFail, Subject: "verify hook", Detail: "spawn went wrong"}) {
			t.Fatalf("got %+v", last)
		}
		if code := doctorExitCode(sections); code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
	})

	t.Run("keeps what it learned about the checkout when Git cannot identify it, and says why verify did not run", func(t *testing.T) {
		f := newCheckoutFixture(t, "./verify")
		opts := f.opts()
		opts.RegistryFor = func(*config.Loaded) (checkoutRegistry, error) {
			return fakeCheckoutRegistry{
				current: func(context.Context) (*registry.WorktreeRegistration, error) {
					return nil, errors.New("Command failed (git rev-parse): not a git repository")
				},
				pruneCandidates: func(context.Context) ([]registry.RegisteredWorktree, error) {
					return nil, errors.New("Command failed (git worktree list): not a git repository")
				},
			}, nil
		}
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := []Check{
			{ID: "checkout.config", Status: StatusOK, Subject: brand.ConfigFile, Detail: f.loaded.File},
			{ID: "checkout.worktree", Status: StatusFail, Subject: "this worktree", Detail: "Command failed (git rev-parse): not a git repository"},
			{ID: "checkout.missing-scan", Status: StatusFail, Subject: "missing checkouts", Detail: "Command failed (git worktree list): not a git repository"},
			{ID: "checkout.hooks.resolve", Instance: "verify", Status: StatusOK, Subject: "verify hook file", Detail: "resolves to " + filepath.Join(f.root, "verify")},
			{ID: "checkout.hooks.verify", Status: StatusSkip, Subject: "verify hook", Detail: "needs this worktree to be identified"},
		}
		got := section(sections, "This checkout")
		if len(got) != len(want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
	})

	t.Run("keeps the config line when the registry cannot even be built", func(t *testing.T) {
		f := newCheckoutFixture(t, "")
		opts := f.opts()
		opts.RegistryFor = func(*config.Loaded) (checkoutRegistry, error) { return nil, errors.New("no registry here") }
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := []Check{
			{ID: "checkout.config", Status: StatusOK, Subject: brand.ConfigFile, Detail: f.loaded.File},
			{ID: "checkout.worktree", Status: StatusFail, Subject: "this worktree", Detail: "no registry here"},
			{ID: "checkout.hooks.verify", Status: StatusSkip, Subject: "verify hook", Detail: "none configured"},
		}
		got := section(sections, "This checkout")
		if len(got) != len(want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
	})

	t.Run("still runs verify for a registered worktree when only the missing-checkout scan fails", func(t *testing.T) {
		f := newCheckoutFixture(t, "./verify")
		f.register("feature-login")
		realRegistry := registry.New(registry.Options{
			ControlHome: f.controlHome, Root: f.root, Domain: fixtureDomain, LabelNamespace: brand.LabelNamespace,
			Runner: f.runner, Logger: logger.Silent{}, CommandName: "berth",
			UserHome: filepath.Join(f.base, "user-home"),
		})
		opts := f.opts()
		opts.RegistryFor = func(*config.Loaded) (checkoutRegistry, error) {
			return fakeCheckoutRegistry{
				current: realRegistry.Current,
				pruneCandidates: func(context.Context) ([]registry.RegisteredWorktree, error) {
					return nil, errors.New("git worktree list failed")
				},
			}, nil
		}
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"ok berth.toml", "ok this worktree", "fail missing checkouts", "ok verify hook file", "ok verify hook"}
		checks := section(sections, "This checkout")
		got := make([]string, len(checks))
		for i, c := range checks {
			got[i] = string(c.Status) + " " + c.Subject
		}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	})

	t.Run("lets a cancellation while identifying the worktree end the command", func(t *testing.T) {
		f := newCheckoutFixture(t, "")
		opts := f.opts()
		opts.RegistryFor = func(*config.Loaded) (checkoutRegistry, error) {
			return fakeCheckoutRegistry{
				current:         func(context.Context) (*registry.WorktreeRegistration, error) { return nil, cancelled(syscall.SIGTERM) },
				pruneCandidates: func(context.Context) ([]registry.RegisteredWorktree, error) { return nil, nil },
			}, nil
		}
		_, err := doctorSections(context.Background(), opts)
		wantCancelled(t, err, "Operation cancelled by SIGTERM.")
	})

	t.Run("warns about a registration whose checkout Git no longer lists", func(t *testing.T) {
		f := newCheckoutFixture(t, "")
		f.register("feature-login")
		gone := filepath.Join(f.base, "gone")
		if err := os.MkdirAll(gone, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(f.common, "worktrees", "gone"), 0o700); err != nil {
			t.Fatal(err)
		}
		f.git.Add(gone, "feature/gone", f.common)
		registered := f.registerAt(gone, "feature-gone")
		f.git.SetActive([]string{f.root}) // git worktree list no longer shows "gone"
		if err := os.RemoveAll(gone); err != nil {
			t.Fatal(err)
		}
		sections, err := doctorSections(context.Background(), f.opts())
		if err != nil {
			t.Fatal(err)
		}
		var warning *Check
		for i, c := range section(sections, "This checkout") {
			if c.Status == StatusWarn {
				warning = &section(sections, "This checkout")[i]
			}
		}
		if warning == nil {
			t.Fatal("expected a warning")
		}
		want := Check{ID: "checkout.missing", Instance: registered.WorktreeID, Status: StatusWarn, Subject: "feature-gone.acme.localhost", Detail: fmt.Sprintf(
			"registered, but Git no longer lists its checkout. Remove what it left with 'berth worktree remove %s'.", registered.WorktreeID)}
		if *warning != want {
			t.Fatalf("got %+v, want %+v", *warning, want)
		}
	})
}

/* --------------------------------------------------------- findCheckout ---- */

func TestFindCheckoutInternal(t *testing.T) {
	// The search walks up to the filesystem root, so these assume no
	// berth.toml above the system's temporary directory — as the CLI tests do.
	t.Run("finds nothing where there is no config, and loads the nearest one otherwise", func(t *testing.T) {
		empty := t.TempDir()
		got, err := findCheckout("", empty)
		if err != nil {
			t.Fatal(err)
		}
		if got.kind != "none" || got.searchedFrom != empty {
			t.Fatalf("got %+v", got)
		}

		project := t.TempDir()
		if err := os.WriteFile(filepath.Join(project, brand.ConfigFile),
			[]byte("[project]\nname = \"acme\"\ndomain = \"acme.localhost\"\n[compose]\nfile = \"compose.yaml\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		nested := filepath.Join(project, "src")
		if err := os.Mkdir(nested, 0o700); err != nil {
			t.Fatal(err)
		}
		got, err = findCheckout("", nested)
		if err != nil {
			t.Fatal(err)
		}
		if got.kind != "loaded" || got.loaded == nil || got.loaded.Root != project {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("turns a config that does not load into a reportable error", func(t *testing.T) {
		project := t.TempDir()
		if err := os.WriteFile(filepath.Join(project, brand.ConfigFile), []byte("not = [valid"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := findCheckout("", project)
		if err != nil {
			t.Fatal(err)
		}
		if got.kind != "unloadable" || got.unloadable == nil {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("uses a named config instead of the nearest one, even when the nearest one is fine", func(t *testing.T) {
		valid := "[project]\nname = \"acme\"\ndomain = \"acme.localhost\"\n[compose]\nfile = \"compose.yaml\"\n"
		project := t.TempDir()
		if err := os.WriteFile(filepath.Join(project, brand.ConfigFile), []byte(valid), 0o600); err != nil {
			t.Fatal(err)
		}
		elsewhere := filepath.Join(t.TempDir(), "other.toml")
		if err := os.WriteFile(elsewhere, []byte(replaceAcme(valid)), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := findCheckout(elsewhere, project)
		if err != nil {
			t.Fatal(err)
		}
		if got.kind != "loaded" || got.loaded == nil || got.loaded.File != elsewhere {
			t.Fatalf("got %+v", got)
		}
		got, err = findCheckout(filepath.Join(project, "missing.toml"), project)
		if err != nil {
			t.Fatal(err)
		}
		if got.kind != "unloadable" {
			t.Fatalf("got %+v", got)
		}
	})
}

// replaceAcme replaces every occurrence of "acme" in s with "other".
func replaceAcme(s string) string {
	out := make([]byte, 0, len(s))
	i := 0
	for i < len(s) {
		if i+4 <= len(s) && s[i:i+4] == "acme" {
			out = append(out, "other"...)
			i += 4
			continue
		}
		out = append(out, s[i])
		i++
	}
	return string(out)
}

/* --------------------------------------------------------- doctor Locks ---- */

func TestDoctorLocksInternal(t *testing.T) {
	const testRepository = "a08b4892-5111-49f1-a47a-5ab988c7f676"
	const testWorktreeID = "2b41d9d4-ed88-49c1-be9c-5439bdf6a431"

	locksOf := func(t *testing.T, controlHome string) []Check {
		t.Helper()
		opts := testOpts(t, func(o *doctorOptions) { o.ControlHome = controlHome })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		return section(sections, "Locks")
	}

	registerLocksWorktree := func(t *testing.T, controlHome, slug string) {
		t.Helper()
		if _, err := registrytest.WriteRecord(controlHome, registrytest.RecordJSON{
			Version: 1, RepositoryID: testRepository, WorktreeID: testWorktreeID, Slug: slug, Domain: fixtureDomain, Initialization: "complete",
		}); err != nil {
			t.Fatal(err)
		}
	}

	// silentUnreleased leaves scope taken and then unreleased: the flock
	// design needs the flock actually let go while its record stays
	// unreleased (see internal/lock/recover.go's recoverPreviousHolder).
	silentUnreleased := func(t *testing.T, scope lock.Scope, deadPID int) {
		t.Helper()
		lk := lock.New(scope, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
		if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		if err := lk.Release(); err != nil {
			t.Fatal(err)
		}
		mutateHolderRecord(t, scope, func(rec map[string]any) {
			if deadPID > 0 {
				rec["pid"] = deadPID
			}
			delete(rec, "releasedAt")
		})
	}

	t.Run("reports none held when no lock is taken", func(t *testing.T) {
		got := locksOf(t, t.TempDir())
		want := []Check{{ID: "locks.none", Status: StatusOK, Subject: "locks", Detail: "none held"}}
		if len(got) != 1 || got[0] != want[0] {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("reports a live holder as an answer, not a problem", func(t *testing.T) {
		controlHome := t.TempDir()
		registerLocksWorktree(t, controlHome, "main")
		operation, _ := lock.WorktreeScopes(controlHome, testRepository, testWorktreeID, "main", procid.Markers{})
		holder := lock.New(operation, lock.Options{Logger: logger.Silent{}, Watchdog: -1, Holder: "berth up"})
		if _, err := holder.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		defer holder.Release()
		got := locksOf(t, controlHome)
		if len(got) != 1 || got[0].Status != StatusOK || got[0].Subject != "main.acme.localhost" || !contains(got[0].Detail, "held by") {
			t.Fatalf("got %+v", got)
		}
	})

	// "fails a silent worktree lock, and names the command that clears it
	// safely" has no Go counterpart to fail: a silent (unreleased-but-gone)
	// lock self-heals the next time it is taken, so doctor reports it ok,
	// not fail, and names no command. See "reports an unreleased-but-gone
	// worktree lock as recovered automatically" below for the Go-accurate
	// version of this scenario.
	t.Run("fails a silent worktree lock, and names the command that clears it safely", func(t *testing.T) {
		t.Skip("a silent lock self-heals the next time it is taken in the flock design, so doctor reports it ok, " +
			"not fail, and names no recovery command; " +
			"see \"reports an unreleased-but-gone worktree lock as recovered automatically\"")
	})

	t.Run("reports an unreleased-but-gone worktree lock as recovered automatically", func(t *testing.T) {
		controlHome := t.TempDir()
		registerLocksWorktree(t, controlHome, "main")
		operation, development := lock.WorktreeScopes(controlHome, testRepository, testWorktreeID, "main", procid.Markers{})
		silentUnreleased(t, operation, deadTestPID)
		silentUnreleased(t, development, deadTestPID)
		got := locksOf(t, controlHome)
		subjects := map[string]bool{}
		for _, c := range got {
			subjects[c.Subject] = true
			if c.Status != StatusOK || !contains(c.Detail, "recovered automatically the next time this lock is taken") {
				t.Errorf("check %+v", c)
			}
		}
		if !subjects["main.acme.localhost"] || !subjects["main.acme.localhost (development session)"] {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("finds a worktree whose only lock left is its development session's", func(t *testing.T) {
		controlHome := t.TempDir()
		registerLocksWorktree(t, controlHome, "main")
		_, development := lock.WorktreeScopes(controlHome, testRepository, testWorktreeID, "main", procid.Markers{})
		silentUnreleased(t, development, deadTestPID)
		got := locksOf(t, controlHome)
		if len(got) != 1 || got[0].Status != StatusOK || got[0].Subject != "main.acme.localhost (development session)" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("names a worktree nothing is registered under by its UUID", func(t *testing.T) {
		controlHome := t.TempDir()
		operation, _ := lock.WorktreeScopes(controlHome, testRepository, testWorktreeID, testWorktreeID, procid.Markers{})
		silentUnreleased(t, operation, deadTestPID)
		got := locksOf(t, controlHome)
		if len(got) != 1 || got[0].Status != StatusOK || got[0].Subject != "worktree "+testWorktreeID {
			t.Fatalf("got %+v", got)
		}
	})

	// Pins the fix for #21: the registry and router locks now have
	// `worktree unlock --registry`/`--router` of their own, so doctor's
	// unverifiable-holder fallback names that instead of a hand `rm`.
	t.Run("points the registry lock's unverifiable fallback at worktree unlock --registry", func(t *testing.T) {
		controlHome := t.TempDir()
		registryScope := lock.RegistryScope(controlHome)
		silentUnreleased(t, registryScope, deadTestPID)
		got := locksOf(t, controlHome)
		if len(got) != 1 || got[0].Status != StatusOK || got[0].Subject != "registry" {
			t.Fatalf("got %+v", got)
		}
		wantHint := "run 'berth worktree unlock --registry --holder-unverifiable' once you have made sure its holder is gone"
		if !contains(got[0].Detail, wantHint) {
			t.Fatalf("detail = %q, want to contain %q", got[0].Detail, wantHint)
		}
		if contains(got[0].Detail, "rm '") || contains(got[0].Detail, "rmdir") {
			t.Fatalf("detail = %q, want no rm/rmdir advice", got[0].Detail)
		}
	})

	t.Run("points the router lock's unverifiable fallback at worktree unlock --router", func(t *testing.T) {
		controlHome := t.TempDir()
		routerScope := lock.Scope{
			Target: filepath.Join(t.TempDir(), "router-lock"), Description: "shared router",
			Markers: procid.Markers{Project: "berth-router"},
		}
		silentUnreleased(t, routerScope, deadTestPID)
		opts := testOpts(t, func(o *doctorOptions) { o.ControlHome = controlHome; o.RouterLockScope = &routerScope })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		got := section(sections, "Locks")
		if len(got) != 1 || got[0].Status != StatusOK || got[0].Subject != "router" {
			t.Fatalf("got %+v", got)
		}
		wantHint := "run 'berth worktree unlock --router --holder-unverifiable' once you have made sure its holder, and everything it started, is gone"
		if !contains(got[0].Detail, wantHint) {
			t.Fatalf("detail = %q, want to contain %q", got[0].Detail, wantHint)
		}
		if contains(got[0].Detail, "rm '") || contains(got[0].Detail, "rmdir") {
			t.Fatalf("detail = %q, want no rm/rmdir advice", got[0].Detail)
		}
	})

	// "is not misled by recovery mutexes, temporary files or directories
	// that are not a worktree's" exercises worktreeRecoveryLockTarget, a
	// `.recovery` mutex file the heartbeat design needed and the flock
	// design does not have at all: flock(2) needs no sidecar mutex the way
	// the heartbeat design once did. The parts that still apply to Go — a
	// `.tmp.NNN` leftover and a non-UUID sibling directory, both of which
	// lockedWorktreeIDs must skip — are covered directly.
	t.Run("is not misled by recovery mutexes, temporary files or directories that are not a worktree's", func(t *testing.T) {
		controlHome := t.TempDir()
		worktreesDir := layout.ControlLayout(controlHome).LocksDir + "/worktrees"
		repoDir := filepath.Join(worktreesDir, testRepository)
		if err := os.MkdirAll(repoDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repoDir, testWorktreeID+".tmp.123"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(worktreesDir, "not-a-repository"), 0o700); err != nil {
			t.Fatal(err)
		}
		got := locksOf(t, controlHome)
		want := []Check{{ID: "locks.none", Status: StatusOK, Subject: "locks", Detail: "none held"}}
		if len(got) != 1 || got[0] != want[0] {
			t.Fatalf("got %+v", got)
		}
	})
}

// deadTestPID is a PID no test system running this suite is expected to
// have — the same choice internal/commands/unlock_test.go makes for the
// same reason.
const deadTestPID = 999999

// mutateHolderRecord rewrites a lock's sidecar holder record in place — the
// in-package counterpart of unlock_test.go's mutateRecord (unexported to
// that other, external test package, so this file needs its own).
func mutateHolderRecord(t *testing.T, scope lock.Scope, edit func(rec map[string]any)) {
	t.Helper()
	path := scope.Target + ".holder"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	edit(rec)
	out, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDoctorRouterPort is #79's "router port" line: the port doctor resolved
// and where it came from, warned about when the running router publishes a
// different one, and failed on its own when that port cannot be read.
func TestDoctorRouterPort(t *testing.T) {
	last := func(t *testing.T, sections []Section) Check {
		t.Helper()
		checks := section(sections, "Router")
		if len(checks) == 0 {
			t.Fatal("no Router checks")
		}
		return checks[len(checks)-1]
	}
	fromFile := settings.PortResolution{Port: 8080, Source: settings.PortFile, SettingsPath: "/berth/settings.toml"}

	t.Run("reports the resolved router port and where it came from", func(t *testing.T) {
		fake := newFakeRouterSource()
		fake.status.Port, fake.published = 8080, 8080
		opts := testOpts(t, func(o *doctorOptions) { o.Router = fake; o.RouterPort = fromFile })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := Check{ID: "router.port", Status: StatusOK, Subject: "router port", Detail: "port 8080 from /berth/settings.toml"}
		if got := last(t, sections); got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("warns when the running router publishes a different port than the one resolved", func(t *testing.T) {
		fake := newFakeRouterSource()
		fake.published = 80
		opts := testOpts(t, func(o *doctorOptions) {
			o.Router = fake
			o.RouterPort = settings.PortResolution{Port: 8080, Source: settings.PortEnv, SettingsPath: "/berth/settings.toml"}
		})
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := Check{ID: "router.port", Status: StatusWarn, Subject: "router port", Detail: "port 8080 from BERTH_ROUTER_PORT (overriding /berth/settings.toml), but the running router publishes 80; the next 'berth up' or 'berth router up' moves it, and URLs already open keep the old port until then"}
		if got := last(t, sections); got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		if code := doctorExitCode(sections); code != 0 {
			t.Fatalf("a warning must not fail doctor, got exit %d", code)
		}
	})

	t.Run("says only where the port came from when no router is running", func(t *testing.T) {
		fake := newFakeRouterSource()
		fake.status.Running, fake.publishedRunning, fake.published = false, false, 0
		opts := testOpts(t, func(o *doctorOptions) { o.Router = fake; o.RouterPort = fromFile })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := Check{ID: "router.port", Status: StatusOK, Subject: "router port", Detail: "port 8080 from /berth/settings.toml"}
		if got := last(t, sections); got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("fails the router port line, and nothing else, when the running router's port cannot be read", func(t *testing.T) {
		fake := newFakeRouterSource()
		fake.publishedErr = errors.New("inspect exploded")
		opts := testOpts(t, func(o *doctorOptions) { o.Router = fake; o.RouterPort = fromFile })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		checks := section(sections, "Router")
		want := Check{ID: "router.port", Status: StatusFail, Subject: "router port", Detail: "inspect exploded"}
		if got := checks[len(checks)-1]; got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		for _, check := range checks[:len(checks)-1] {
			if check.Status == StatusFail {
				t.Fatalf("another line failed too: %+v", check)
			}
		}
	})
}

// documentedDoctorIDs is every check id `doctor` can emit, mirrored by the
// table in README.md. A new check must be added here and there, which is
// where its id, its section and its instance key get decided.
var documentedDoctorIDs = []string{
	"prerequisites.git",
	"prerequisites.docker.engine",
	"prerequisites.docker.compose",
	"registry.berth-root",
	"registry.control-home",
	"registry.router-state",
	"registry.registrations",
	"registry.unreadable-record",
	"registry.host-conflict",
	"registry.index-host",
	"locks.none",
	"locks.scan",
	"locks.registry",
	"locks.router",
	"locks.worktree.operation",
	"locks.worktree.development",
	"router.container",
	"router.owner",
	"router.routes",
	"router.route-conflict",
	"router.orphaned-routes",
	"router.address-pools",
	"router.port",
	"checkout.config",
	"checkout.worktree",
	"checkout.missing",
	"checkout.missing-scan",
	"checkout.hooks.resolve",
	"checkout.hooks.verify",
}

// checkReport fails the test unless every check in sections has a documented
// id under its section's own id (sections are found by ID, not Title), and
// (id, instance) is a key: no pair appears twice.
func checkReport(t *testing.T, sections []Section) {
	t.Helper()
	documented := map[string]bool{}
	for _, id := range documentedDoctorIDs {
		documented[id] = true
	}
	seen := map[[2]string]bool{}
	for _, section := range sections {
		if section.ID == "" {
			t.Fatalf("section %q has no ID", section.Title)
		}
		for _, check := range section.Checks {
			if !documented[check.ID] {
				t.Errorf("check %q emits the undocumented id %q", check.Subject, check.ID)
			}
			if !strings.HasPrefix(check.ID, section.ID+".") {
				t.Errorf("id %q is not under its section %q", check.ID, section.ID)
			}
			key := [2]string{check.ID, check.Instance}
			if seen[key] {
				t.Errorf("(id, instance) = (%q, %q) appears twice", check.ID, check.Instance)
			}
			seen[key] = true
		}
	}
}

// sectionByID is a section's checks, looked up by its ID.
func sectionByID(sections []Section, id string) []Check {
	for _, s := range sections {
		if s.ID == id {
			return s.Checks
		}
	}
	return nil
}

// instanceKeys is every per-item check of one kind in checks, keyed by
// (id, instance) and mapped to its subject.
func instanceKeys(checks []Check, id string) map[[2]string]string {
	out := map[[2]string]string{}
	for _, check := range checks {
		if check.ID == id {
			out[[2]string{check.ID, check.Instance}] = check.Subject
		}
	}
	return out
}

// TestDoctorInstancesAreDurable pins that a per-item check keeps its (id,
// instance) whatever order the items appeared in and whichever of their
// siblings are gone — the reason a script can key on it.
func TestDoctorInstancesAreDurable(t *testing.T) {
	t.Run("keeps a locked worktree's key in either order, and when the other is released", func(t *testing.T) {
		// hold takes worktree worktreeID's operation lock. Its development
		// lock (the second scope WorktreeScopes returns) is not taken here;
		// only the operation kind is exercised. The release is registered
		// with t.Cleanup as well as returned, and is idempotent, so a Fatal
		// cannot leave a flock held.
		hold := func(t *testing.T, controlHome, repo, worktreeID, slug string) func() {
			t.Helper()
			if _, err := registrytest.WriteRecord(controlHome, registrytest.RecordJSON{
				Version: 1, RepositoryID: repo, WorktreeID: worktreeID, Slug: slug, Domain: fixtureDomain, Initialization: "complete",
			}); err != nil {
				t.Fatal(err)
			}
			operation, _ := lock.WorktreeScopes(controlHome, repo, worktreeID, slug, procid.Markers{})
			holder := lock.New(operation, lock.Options{Logger: logger.Silent{}, Watchdog: -1, Holder: "berth up"})
			if _, err := holder.Acquire(context.Background(), "berth up"); err != nil {
				t.Fatal(err)
			}
			var once sync.Once
			release := func() {
				once.Do(func() {
					if err := holder.Release(); err != nil {
						t.Error(err)
					}
				})
			}
			t.Cleanup(release)
			return release
		}
		locks := func(t *testing.T, controlHome string) map[[2]string]string {
			t.Helper()
			sections, err := doctorSections(context.Background(), testOpts(t, func(o *doctorOptions) { o.ControlHome = controlHome }))
			if err != nil {
				t.Fatal(err)
			}
			checkReport(t, sections)
			return instanceKeys(sectionByID(sections, "locks"), "locks.worktree.operation")
		}
		keyA := [2]string{"locks.worktree.operation", doctorWorktreeA}
		keyB := [2]string{"locks.worktree.operation", doctorWorktreeB}

		var first map[[2]string]string
		for _, orderAThenB := range []bool{true, false} {
			controlHome := registrytest.TempDir(t)
			var releaseA, releaseB func()
			if orderAThenB {
				releaseA = hold(t, controlHome, doctorRepoA, doctorWorktreeA, "aaa")
				releaseB = hold(t, controlHome, doctorRepoB, doctorWorktreeB, "bbb")
			} else {
				releaseB = hold(t, controlHome, doctorRepoB, doctorWorktreeB, "bbb")
				releaseA = hold(t, controlHome, doctorRepoA, doctorWorktreeA, "aaa")
			}
			got := locks(t, controlHome)
			_, hasA := got[keyA]
			_, hasB := got[keyB]
			if len(got) != 2 || !hasA || !hasB {
				t.Fatalf("A then B = %v: keys = %v", orderAThenB, got)
			}
			if first != nil && !maps.Equal(first, got) {
				t.Fatalf("the order changed the keys: %v vs %v", got, first)
			}
			first = got
			// B's lock is released cleanly, so its line goes; A's keeps its key.
			releaseB()
			after := locks(t, controlHome)
			if _, ok := after[keyA]; !ok || len(after) != 1 {
				t.Fatalf("after releasing B: keys = %v", after)
			}
			releaseA()
		}
	})

	t.Run("keeps a route conflict's key when another conflict goes away", func(t *testing.T) {
		const (
			repoC = "55555555-5555-5555-5555-555555555555"
			repoD = "66666666-6666-6666-6666-666666666666"
			wtC   = "77777777-7777-7777-7777-777777777777"
			wtD   = "88888888-8888-8888-8888-888888888888"
		)
		write := func(t *testing.T, state layout.RouterLayout, repo, wt, service, rule string) {
			t.Helper()
			if err := router.WriteFragment(state, repo, wt, service, router.NewFragmentDocument(service+wt, rule, service+wt, 8080)); err != nil {
				t.Fatal(err)
			}
		}
		conflicts := func(t *testing.T, withSecond bool) map[[2]string]string {
			t.Helper()
			state := layout.RouterLayout{DynamicDir: registrytest.TempDir(t)}
			write(t, state, doctorRepoA, doctorWorktreeA, "mail", "Host(`one.acme.localhost`)")
			write(t, state, doctorRepoB, doctorWorktreeB, "api", "Host(`one.acme.localhost`)")
			if withSecond {
				write(t, state, repoC, wtC, "mail", "Host(`two.acme.localhost`) && PathPrefix(`/api`)")
				write(t, state, repoD, wtD, "api", "Host(`two.acme.localhost`) && PathPrefix(`/api`)")
			}
			sections, err := doctorSections(context.Background(), testOpts(t, func(o *doctorOptions) { o.RouterState = state }))
			if err != nil {
				t.Fatal(err)
			}
			checkReport(t, sections)
			return instanceKeys(sectionByID(sections, "router"), "router.route-conflict")
		}
		both, one := conflicts(t, true), conflicts(t, false)
		keyOne := [2]string{"router.route-conflict", "one.acme.localhost"}
		keyTwo := [2]string{"router.route-conflict", "two.acme.localhost/api"}
		if _, ok := both[keyOne]; !ok || len(both) != 2 {
			t.Fatalf("both = %v", both)
		}
		if _, ok := both[keyTwo]; !ok {
			t.Fatalf("both = %v", both)
		}
		if _, ok := one[keyOne]; !ok || len(one) != 1 {
			t.Fatalf("removing the other conflict changed the surviving key: %v", one)
		}
	})
}

// TestRecordInstance pins an unreadable record's instance: the repository
// directory and the file, or the file alone when there is no directory.
func TestRecordInstance(t *testing.T) {
	controlHome := registrytest.TempDir(t)
	broken := filepath.Join(layout.RepositoryLayout(controlHome, doctorRepoA).RegistryDir, "broken.json")
	for file, want := range map[string]string{
		broken:    filepath.Base(filepath.Dir(filepath.Dir(broken))) + "/broken.json",
		"/x.json": "x.json",
	} {
		if got := recordInstance(file); got != want {
			t.Errorf("recordInstance(%q) = %q, want %q", file, got, want)
		}
	}
	if got := recordInstance(broken); !strings.HasPrefix(got, doctorRepoA+"/") {
		t.Errorf("recordInstance(%q) = %q, want it to start with the repository's directory %s", broken, got, doctorRepoA)
	}
}

// TestRuleClaim pins the route-conflict instance: what a rule claims, as a
// host and path, and a rule berth did not write as itself.
func TestRuleClaim(t *testing.T) {
	for rule, want := range map[string]string{
		"Host(`auth.feature-x.acme.localhost`)":                       "auth.feature-x.acme.localhost",
		"Host(`auth.feature-x.acme.localhost`) && PathPrefix(`/api`)": "auth.feature-x.acme.localhost/api",
		"HostRegexp(`{sub:.+}.acme.localhost`)":                       "HostRegexp(`{sub:.+}.acme.localhost`)",
	} {
		if got := ruleClaim(rule); got != want {
			t.Errorf("ruleClaim(%q) = %q, want %q", rule, got, want)
		}
	}
}

// TestDoctorEmitsOnlyDocumentedIDs runs the default fixture and checks its
// report against the documented set.
func TestDoctorEmitsOnlyDocumentedIDs(t *testing.T) {
	sections, err := doctorSections(context.Background(), testOpts(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	checkReport(t, sections)
}

// TestDoctorIDsAreTheDocumentedSet reads doctor.go for the ids it hands to
// Check literals and to the helpers that take one, and compares that set with
// documentedDoctorIDs; then checks README.md has a table row for each id and
// lists every hook stage as the instance of checkout.hooks.resolve.
func TestDoctorIDsAreTheDocumentedSet(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "doctor.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	add := func(expr ast.Expr) {
		if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if value, err := strconv.Unquote(lit.Value); err == nil && strings.Contains(value, ".") {
				found[value] = true
			}
		}
	}
	// The helpers whose argument at that index is a check id.
	idArgument := map[string]int{"failureCheck": 0, "guarded": 0, "lockCheck": 0, "versionCheck": 0, "askDocker": 2}
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CompositeLit:
			for _, element := range node.Elts {
				if kv, ok := element.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "ID" {
						add(kv.Value)
					}
				}
			}
		case *ast.CallExpr:
			if name, ok := node.Fun.(*ast.Ident); ok {
				if index, ok := idArgument[name.Name]; ok && index < len(node.Args) {
					add(node.Args[index])
				}
			}
		}
		return true
	})
	documented := map[string]bool{}
	for _, id := range documentedDoctorIDs {
		documented[id] = true
	}
	for id := range found {
		if !documented[id] {
			t.Errorf("doctor.go uses the id %q, which documentedDoctorIDs (and the README table) does not list", id)
		}
	}
	for id := range documented {
		if !found[id] {
			t.Errorf("documentedDoctorIDs lists %q, which doctor.go no longer uses", id)
		}
	}

	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	for _, line := range strings.Split(string(readme), "\n") {
		if cells := strings.Split(line, "|"); len(cells) > 2 && strings.HasPrefix(strings.TrimSpace(cells[1]), "`") {
			rows[strings.Trim(strings.TrimSpace(cells[1]), "`")] = line
		}
	}
	for _, id := range documentedDoctorIDs {
		if _, ok := rows[id]; !ok {
			t.Errorf("README.md has no table row for the id %q", id)
		}
	}
	all := config.Hooks{}
	value := "x"
	all.Prepare, all.Seed, all.Verify, all.Backup, all.Restore = &value, &value, &value, &value, &value
	for _, stage := range all.Configured() {
		if !strings.Contains(rows["checkout.hooks.resolve"], "`"+string(stage)+"`") {
			t.Errorf("the README row for checkout.hooks.resolve does not name the stage %q", stage)
		}
	}
}
