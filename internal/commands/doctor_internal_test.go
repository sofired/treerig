package commands

// Ported from tests/unit/doctor.test.ts, title for title. This file tests
// doctor's unexported pieces directly — doctorSections and the helpers it is
// built from — the way internal/router and internal/config test their own
// unexported details from an in-package _test.go. doctorOptions is this
// port's counterpart of TypeScript's own DoctorOptions: every external
// dependency is injected, so a scenario TypeScript builds with a fake
// CommandRunner or RouterSource is built here the same way, without a real
// filesystem, Docker daemon, or Git checkout except where the test itself
// needs one (commandstest.World, exactly as TypeScript's commandWorld()).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
)

/* -------------------------------------------------------------- fakes ---- */

// outcome is a run.Result or an error — the Go counterpart of the union
// tests/unit/doctor.test.ts's DoctorRunner stores per command.
type outcome struct {
	result run.Result
	err    error
}

// okOutcome is a successful outcome with the given stdout.
func okOutcome(stdout string) outcome { return outcome{result: run.Result{Stdout: stdout}} }

// doctorRunnerState is DoctorRunner from tests/unit/doctor.test.ts: answers
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

// fakeRouterSource is routerSource() from tests/unit/doctor.test.ts.
type fakeRouterSource struct {
	network string
	status  router.RouterStatus
	err     error
	asked   int
}

// newFakeRouterSource is a fakeRouterSource reporting a reachable, running,
// clean router on network "berth-router".
func newFakeRouterSource() *fakeRouterSource {
	return &fakeRouterSource{network: "berth-router", status: router.RouterStatus{Reachable: true, Running: true, Port: 80, Network: "berth-router"}}
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

// fakeCheckoutRegistry is CheckoutRegistry from src/commands/doctor.ts,
// injected via doctorOptions.RegistryFor.
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
	opts := doctorOptions{
		CommandName: "berth", ControlHome: t.TempDir() + "/control",
		Runner: newDoctorRunner(nil), Logger: logger.Silent{},
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
// identity (registrytest.GitRunner) but not yet registered — `up` itself is
// not ported by WP6; register() below is what a finished `up` would have left.
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
	if err := files.SecureControlDirectories(layout.ControlLayout(f.controlHome)); err != nil {
		f.t.Fatal(err)
	}
	identities := registry.NewIdentityService(root, f.runner)
	identity, err := registrytest.Register(context.Background(), f.controlHome, identities, root, fixtureDomain, slug, nil, "complete")
	if err != nil {
		f.t.Fatal(err)
	}
	if err := files.SecureWorktreeDirectories(layout.WorktreeLayout(f.controlHome, identity.RepositoryID, identity.WorktreeID)); err != nil {
		f.t.Fatal(err)
	}
	return registry.RegisteredWorktree{RepositoryID: identity.RepositoryID, WorktreeID: identity.WorktreeID, Slug: slug, Domain: fixtureDomain}
}

// opts is a doctorOptions for this fixture's loaded checkout, with f's own
// control home, runner and a healthy fake router.
func (f *checkoutFixture) opts() doctorOptions {
	return doctorOptions{
		CommandName: "berth", ControlHome: f.controlHome, Runner: f.runner, Logger: logger.Silent{},
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
	// nodeCheck do not exist here at all (divergence ledger row 17). "names
	// the command and the range when Node is outside it" is the same story.
	t.Run("holds Node to the engines range: the floor included, the next major excluded", func(t *testing.T) {
		t.Skip("doctor drops the Node.js engines check entirely (docs/port/divergences.md, row 17); " +
			"there is no Go runtime version for a Go build to be running under, so satisfiesEngine has no Go counterpart")
	})
	t.Run("checks Node against exactly the range package.json declares", func(t *testing.T) {
		t.Skip("doctor drops the Node.js engines check entirely (docs/port/divergences.md, row 17); no NODE_ENGINE constant exists to check")
	})
	t.Run("names the command and the range when Node is outside it", func(t *testing.T) {
		t.Skip("doctor drops the Node.js engines check entirely (docs/port/divergences.md, row 17); nodeCheck has no Go counterpart")
	})

	t.Run("passes a tool at the minimum, fails one below it, and fails output it cannot read", func(t *testing.T) {
		if got := versionCheck("Git", "git version 2.36.0", "2.36"); got != (Check{Status: StatusOK, Subject: "Git", Detail: "2.36.0"}) {
			t.Errorf("got %+v", got)
		}
		if got := versionCheck("Git", "git version 2.35.1", "2.36"); got != (Check{Status: StatusFail, Subject: "Git", Detail: "2.35.1; 2.36 or newer is needed"}) {
			t.Errorf("got %+v", got)
		}
		got := versionCheck("Git", "garbled", "2.36")
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
		want := []Check{{Status: StatusOK, Subject: "registrations", Detail: "none yet"}}
		if len(got) != 1 || got[0] != want[0] {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		got = registryChecks(scan(func(s *registry.RegistryScan) {
			s.Records = []registry.RegisteredWorktree{testWorktree("a", "acme.localhost"), testWorktree("b", "acme.localhost")}
		}), "berth")
		if len(got) != 1 || got[0] != (Check{Status: StatusOK, Subject: "registrations", Detail: "2 readable"}) {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("fails each unreadable record with its reason", func(t *testing.T) {
		got := registryChecks(scan(func(s *registry.RegistryScan) {
			s.Problems = []registry.RegistryProblem{{File: "/x.json", Reason: "Registry record is not valid JSON: /x.json"}}
		}), "berth")
		if got[1] != (Check{Status: StatusFail, Subject: "unreadable record", Detail: "Registry record is not valid JSON: /x.json"}) {
			t.Fatalf("got %+v", got[1])
		}
	})

	t.Run("fails a hostname two worktrees claim, naming both", func(t *testing.T) {
		first, second := testWorktree("shared", "acme.localhost"), testWorktree("shared", "acme.localhost")
		got := registryChecks(scan(func(s *registry.RegistryScan) {
			s.Records = []registry.RegisteredWorktree{first, second}
			s.Conflicts = registry.HostnameConflicts{"shared.acme.localhost": {first, second}}
		}), "duke")
		if got[1].Status != StatusFail || got[1].Subject != "shared.acme.localhost" {
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
		if len(got) != 1 || got[0] != (Check{Status: StatusOK, Subject: "router", Detail: "running on 127.0.0.1:80, network berth-router"}) {
			t.Fatalf("got %+v", got)
		}
		stopped := status
		stopped.Running, stopped.Problems = false, []string{"the router is not running"}
		got = routerStatusChecks(stopped)
		if len(got) != 1 || got[0] != (Check{Status: StatusOK, Subject: "router", Detail: "not running (would use 127.0.0.1:80, network berth-router)"}) {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("fails each problem of a degraded router, and a router Docker could not be asked about", func(t *testing.T) {
		degraded := status
		degraded.Problems = []string{"router: not healthy", "index: image is not pinned"}
		got := routerStatusChecks(degraded)
		want := []Check{
			{Status: StatusFail, Subject: "router", Detail: "router: not healthy"},
			{Status: StatusFail, Subject: "router", Detail: "index: image is not pinned"},
		}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("got %+v", got)
		}
		unreachable := router.RouterStatus{Reachable: false, Running: false, Problems: []string{"Docker is not available: boom."}, Port: 80, Network: "berth-router"}
		got = routerStatusChecks(unreachable)
		if len(got) != 1 || got[0] != (Check{Status: StatusFail, Subject: "router", Detail: "Router state unknown: Docker is not available: boom"}) {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestRouteConflictsInternal(t *testing.T) {
	routed := func(name, routerName, rule string) routedContainer {
		return routedContainer{Name: name, Labels: map[string]string{
			"traefik.http.routers." + routerName + ".rule":        rule,
			"traefik.http.routers." + routerName + ".entrypoints": "web",
		}}
	}

	t.Run("finds a rule two routers claim, naming the containers behind them", func(t *testing.T) {
		conflicts := routeConflicts([]routedContainer{
			routed("b-api-1", "b-api", "Host(`mail.main.acme.localhost`)"),
			routed("a-mail-1", "a-mail", "Host(`mail.main.acme.localhost`)"),
			routed("c-api-1", "c-api", "Host(`other.acme.localhost`)"),
		})
		if len(conflicts) != 1 {
			t.Fatalf("conflicts = %+v", conflicts)
		}
		names := conflicts["Host(`mail.main.acme.localhost`)"]
		if len(names) != 2 || names[0] != "a-mail-1" || names[1] != "b-api-1" {
			t.Fatalf("names = %v", names)
		}
	})

	t.Run("does not count replicas of one service, which share a router, as a conflict", func(t *testing.T) {
		replica := func(name string) routedContainer {
			c := routed(name, "a-api", "Host(`x`)")
			c.Labels["com.docker.compose.project"] = "a"
			c.Labels["com.docker.compose.service"] = "api"
			return c
		}
		if got := routeConflicts([]routedContainer{replica("a-api-1"), replica("a-api-2")}); len(got) != 0 {
			t.Fatalf("got %+v", got)
		}
		if got := routeConflicts([]routedContainer{routed("a-api-1", "a-api", "Host(`x`)"), routed("a-api-2", "a-api", "Host(`x`)")}); len(got) != 0 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("sees through a container that copies a service's router name and rule from another project", func(t *testing.T) {
		genuine := routed("a-api-1", "a-api", "Host(`x`)")
		genuine.Labels["com.docker.compose.project"], genuine.Labels["com.docker.compose.service"] = "a", "api"
		copyContainer := routed("impostor", "a-api", "Host(`x`)")
		copyContainer.Labels["com.docker.compose.project"], copyContainer.Labels["com.docker.compose.service"] = "b", "api"
		conflicts := routeConflicts([]routedContainer{genuine, copyContainer})
		if len(conflicts) != 1 {
			t.Fatalf("conflicts = %+v", conflicts)
		}
		names := conflicts["Host(`x`)"]
		if len(names) != 2 || names[0] != "a-api-1" || names[1] != "impostor" {
			t.Fatalf("names = %v", names)
		}

		copyOtherService := routed("impostor", "a-api", "Host(`x`)")
		copyOtherService.Labels["com.docker.compose.project"], copyOtherService.Labels["com.docker.compose.service"] = "a", "web"
		otherService := routeConflicts([]routedContainer{genuine, copyOtherService})
		if len(otherService) != 1 {
			t.Fatalf("otherService = %+v", otherService)
		}
	})

	t.Run("reads names and labels from inspect output, skipping what it cannot parse", func(t *testing.T) {
		stdout := "/a-api-1\t{\"traefik.http.routers.a-api.rule\":\"Host(`x`)\",\"n\":1}\n" +
			"no tab here\n" +
			"/broken\t{not json\n" +
			"/empty\tnull\n"
		got := parseRoutedContainers(stdout)
		want := []routedContainer{{Name: "a-api-1", Labels: map[string]string{"traefik.http.routers.a-api.rule": "Host(`x`)"}}}
		if len(got) != 1 || got[0].Name != want[0].Name || len(got[0].Labels) != 1 || got[0].Labels["traefik.http.routers.a-api.rule"] != "Host(`x`)" {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
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
				{Status: StatusFail, Subject: "router", Detail: "x"},
				{Status: StatusFail, Subject: "verify hook", Detail: "y", ExitCode: exitCode},
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
		// No Node.js check (divergence ledger row 17): Prerequisites is Git, Docker Engine, Docker Compose.
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
		if !contains(section(sections, "Registry")[0].Detail, "not created yet") {
			t.Fatalf("registry[0] = %+v", section(sections, "Registry")[0])
		}
		wantRouter := []string{"ok router", "ok routes"}
		gotRouter := statuses(sections, "Router")
		if len(gotRouter) != 2 || gotRouter[0] != wantRouter[0] || gotRouter[1] != wantRouter[1] {
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

	// TypeScript never prints this line: it is Go's own, for state a
	// TypeScript build left behind (docs/decisions/0001-port-to-go.md:177,
	// "plus doctor's line for TypeScript-era state"; divergence ledger row 22).
	t.Run("reports a control home a TypeScript build last used, and that the next command here upgrades it", func(t *testing.T) {
		controlHome := t.TempDir() + "/control"
		if err := os.MkdirAll(controlHome, 0o700); err != nil {
			t.Fatal(err)
		}
		// Empty, the way a TypeScript-created control home leaves it:
		// LegacyControlHome treats an empty marker as TypeScript-era, and a
		// Go-format one (files.ControlHomeFormat) as its own, already-upgraded one.
		if err := os.WriteFile(filepath.Join(controlHome, files.ControlHomeMarker), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		opts := testOpts(t, func(o *doctorOptions) { o.ControlHome = controlHome })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		got := section(sections, "Registry")
		if len(got) < 2 {
			t.Fatalf("registry = %+v, want the control-home check and the legacy-home line", got)
		}
		want := Check{Status: StatusOK, Subject: "control home", Detail: "was last used by a TypeScript build of berth; the next command here upgrades it"}
		if got[1] != want {
			t.Fatalf("got %+v, want %+v", got[1], want)
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
		// their relative order is not guaranteed — unlike TypeScript's own
		// Promise.all, whose two calls are issued synchronously in one tick.
		// ps (the routes check) always comes last, once both have resolved.
		if len(dockerArgs) != 3 || dockerArgs[2] != "ps" {
			t.Fatalf("dockerArgs = %v, want [version compose] (either order) then ps", dockerArgs)
		}
		firstTwo := map[string]bool{dockerArgs[0]: true, dockerArgs[1]: true}
		if !firstTwo["version"] || !firstTwo["compose"] {
			t.Fatalf("dockerArgs = %v, want [version compose] (either order) then ps", dockerArgs)
		}
	})

	t.Run("skips the router's own state, which is read through Compose, when Compose does not answer, and still reads routes", func(t *testing.T) {
		runner := newDoctorRunner(nil)
		runner.state.docker["compose"] = outcome{result: run.Result{Stderr: "docker: 'compose' is not a docker command.", ExitCode: 1}}
		fakeRouter := newFakeRouterSource()
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner; o.Router = fakeRouter })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := []Check{
			{Status: StatusSkip, Subject: "router", Detail: "needs Docker Compose"},
			{Status: StatusOK, Subject: "routes", Detail: "no running container is routed"},
		}
		got := section(sections, "Router")
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("router = %+v", got)
		}
		if fakeRouter.asked != 0 {
			t.Fatalf("asked = %d, want 0", fakeRouter.asked)
		}
	})

	// "reports Docker checks that could not even be made, and skips the
	// router rather than guessing" has no Go counterpart: run.Runner.Which
	// returns a plain bool and cannot fail the way TypeScript's own
	// which(), a real subprocess check, can reject. dockerChecks has
	// nothing to catch here.
	t.Run("reports Docker checks that could not even be made, and skips the router rather than guessing", func(t *testing.T) {
		t.Skip("run.Runner.Which returns a plain bool (internal/run/run.go) and cannot fail the way " +
			"TypeScript's own which() can reject; dockerChecks has nothing to catch (docs/port/divergences.md, row 20)")
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
		if len(gotRouter) != 1 || gotRouter[0] != (Check{Status: StatusSkip, Subject: "router", Detail: "needs Docker"}) {
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
		want := Check{Status: StatusFail, Subject: "Docker Engine",
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
			{Status: StatusFail, Subject: "Docker Engine", Detail: "`docker version --format {{.Server.Version}}` did not answer within 30 s"},
			{Status: StatusFail, Subject: "Docker Compose", Detail: "unexpected failure"},
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
		want := Check{Status: StatusFail, Subject: "Git", Detail: "`git` could not be started: it is not on PATH (ENOENT)."}
		if got := section(sections, "Prerequisites")[0]; got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}

		runner.state.gitVersion = outcome{result: run.Result{Stderr: "fatal: bad\n", ExitCode: 128}}
		sections, err = doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want = Check{Status: StatusFail, Subject: "Git", Detail: "`git --version` failed: fatal: bad"}
		if got := section(sections, "Prerequisites")[0]; got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	// "fails Node outside the engines range" has no Go counterpart: there is
	// no Node.js check at all (divergence ledger row 17).
	t.Run("fails Node outside the engines range", func(t *testing.T) {
		t.Skip("doctor drops the Node.js engines check entirely (docs/port/divergences.md, row 17)")
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
		got := section(sections, "Registry")[0]
		if got.Status != StatusFail || got.Subject != "control home" || !contains(got.Detail, "Refusing to use") {
			t.Fatalf("got %+v", got)
		}

		if err := os.Remove(filepath.Join(controlHome, "someone-elses.txt")); err != nil {
			t.Fatal(err)
		}
		// A marker with the Go build's own format, not empty: an empty one
		// is what a TypeScript-created control home leaves, which triggers
		// the separate "was last used by a TypeScript build" line (see
		// "reports a control home a TypeScript build last used..." above)
		// — a distinct scenario from the one this test is about.
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
		want := []string{"ok control home", "ok registrations", "fail unreadable record"}
		gotStatuses := statuses(sections, "Registry")
		if len(gotStatuses) != 3 {
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
		got := section(sections, "Registry")[0]
		// TypeScript's own detail carries Node's errno name, "ENOTDIR"; Go's
		// os package formats the same underlying error in its own words
		// ("not a directory") instead — the same accepted difference
		// docs/port/divergences.md row 5 already covers for an unexpected
		// file error's wording generally.
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
		want := []Check{{Status: StatusFail, Subject: "router", Detail: `BERTH_ROUTER_PORT must be a port number between 1 and 65535, not "x".`}}
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
			{Status: StatusFail, Subject: "router", Detail: "exploded"},
			{Status: StatusOK, Subject: "routes", Detail: "no running container is routed"},
		}
		got := section(sections, "Router")
		if len(got) != 2 || got[0] != wantRouter[0] || got[1] != wantRouter[1] {
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
	// cancellation included, into a failed Check. src/commands/doctor.ts:140-155
	// catches only DockerUnavailableError; everything else — Cancelled
	// among it — propagates. These four pin that for every docker call
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

	t.Run("lets a cancellation during the routes ps call end the command", func(t *testing.T) {
		runner := newDoctorRunner(nil)
		runner.state.docker["ps"] = outcome{err: cancelled(syscall.SIGINT)}
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner })
		_, err := doctorSections(context.Background(), opts)
		wantCancelled(t, err, "Operation cancelled by SIGINT.")
	})

	t.Run("lets a cancellation during the routes inspect call end the command", func(t *testing.T) {
		runner := newDoctorRunner(nil)
		runner.state.docker["ps"] = okOutcome("c1\n")
		runner.state.docker["inspect"] = outcome{err: cancelled(syscall.SIGINT)}
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner })
		_, err := doctorSections(context.Background(), opts)
		wantCancelled(t, err, "Operation cancelled by SIGINT.")
	})

	t.Run("fails two routers that claim the same rule, read from the routed containers", func(t *testing.T) {
		runner := newDoctorRunner(nil)
		runner.state.docker["ps"] = okOutcome("c1\nc2\n")
		runner.state.docker["inspect"] = okOutcome(
			"/berth-aaa-mail-1\t{\"traefik.http.routers.berth-aaa-mail.rule\":\"Host(`mail.main.acme.localhost`)\"}\n" +
				"/berth-bbb-api-1\t{\"traefik.http.routers.berth-bbb-api.rule\":\"Host(`mail.main.acme.localhost`)\"}\n")
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := Check{Status: StatusFail, Subject: "routes",
			Detail: "Host(`mail.main.acme.localhost`) is claimed by berth-aaa-mail-1, berth-bbb-api-1; requests for it may reach any of them"}
		if got := section(sections, "Router")[1]; got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		var psArgs []string
		var inspectSpec *run.Spec
		for _, call := range runner.state.callsSnapshot() {
			call := call
			if call.Command == "docker" && call.Args[0] == "ps" {
				psArgs = call.Args
			}
			if call.Command == "docker" && call.Args[0] == "inspect" {
				inspectSpec = &call
			}
		}
		want2 := []string{"ps", "-q", "--filter", "label=dev.berth.router=berth-router"}
		if len(psArgs) != len(want2) {
			t.Fatalf("ps args = %v, want %v", psArgs, want2)
		}
		for i := range want2 {
			if psArgs[i] != want2[i] {
				t.Fatalf("ps args = %v, want %v", psArgs, want2)
			}
		}
		if inspectSpec == nil || inspectSpec.Timeout != dockerProbeTimeout {
			t.Fatalf("inspect timeout = %v, want %v", inspectSpec, dockerProbeTimeout)
		}
	})

	t.Run("bounds a rule any container could have written before it reaches the report", func(t *testing.T) {
		runner := newDoctorRunner(nil)
		rule := "Host(`" + repeatString("a", 500) + "`)"
		runner.state.docker["ps"] = okOutcome("c1\nc2\n")
		runner.state.docker["inspect"] = okOutcome(
			"/one\t" + jsonLabels("one", rule) + "\n" + "/two\t" + jsonLabels("two", rule) + "\n")
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		detail := section(sections, "Router")[1].Detail
		want := string([]rune(rule)[:199]) + "… is claimed by one, two"
		if len(detail) < len(want) || detail[:len(want)] != want {
			t.Fatalf("detail = %q, want prefix %q", detail, want)
		}
	})

	t.Run("bounds the list of containers claiming a rule, however many there are", func(t *testing.T) {
		runner := newDoctorRunner(nil)
		var psLines, inspectLines string
		for i := 0; i < 60; i++ {
			name := fmt.Sprintf("claimant-%02d", i)
			psLines += name + "\n"
			inspectLines += "/" + name + "\t" + jsonLabels(name, "Host(`x`)") + "\n"
		}
		runner.state.docker["ps"] = okOutcome(psLines)
		runner.state.docker["inspect"] = okOutcome(inspectLines)
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner })
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

	t.Run("fails rather than half-reads an inspect that ran out of time part-way", func(t *testing.T) {
		runner := newDoctorRunner(nil)
		runner.state.docker["ps"] = okOutcome("c1\nc2\n")
		runner.state.docker["inspect"] = outcome{result: run.Result{Stdout: "/one\t" + jsonLabels("one", "Host(`a`)") + "\n", ExitCode: 1, TimedOut: true}}
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := Check{Status: StatusFail, Subject: "routes", Detail: "`docker inspect` did not answer within 30 s, so routes were not checked"}
		if got := section(sections, "Router")[1]; got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("fails rather than half-reads an inspect whose output could not all be captured", func(t *testing.T) {
		runner := newDoctorRunner(nil)
		runner.state.docker["ps"] = okOutcome("c1\n")
		runner.state.docker["inspect"] = outcome{result: run.Result{Stdout: "/one\t" + jsonLabels("one", "Host(`a`)") + "\n", ExitCode: 1, OutputOverflow: true}}
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := Check{Status: StatusFail, Subject: "routes", Detail: "`docker inspect` produced more output than can be read, so routes were not checked"}
		if got := section(sections, "Router")[1]; got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("counts routed containers when no rule is shared, and fails an inspect that answered nothing", func(t *testing.T) {
		runner := newDoctorRunner(nil)
		runner.state.docker["ps"] = okOutcome("c1\n")
		runner.state.docker["inspect"] = okOutcome("/berth-aaa-api-1\t" + jsonLabels("berth-aaa-api", "Host(`a`)") + "\n")
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := Check{Status: StatusOK, Subject: "routes", Detail: "1 routed container(s); no two routers share a rule"}
		if got := section(sections, "Router")[1]; got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}

		runner.state.docker["inspect"] = outcome{result: run.Result{Stderr: "Error: No such object: c1", ExitCode: 1}}
		sections, err = doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want = Check{Status: StatusFail, Subject: "routes", Detail: "Error: No such object: c1"}
		if got := section(sections, "Router")[1]; got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("reads the containers inspect did describe when another stopped in between", func(t *testing.T) {
		runner := newDoctorRunner(nil)
		runner.state.docker["ps"] = okOutcome("c1\nc2\n")
		runner.state.docker["inspect"] = outcome{result: run.Result{
			Stdout: "/berth-aaa-api-1\t" + jsonLabels("berth-aaa-api", "Host(`a`)") + "\n",
			Stderr: "Error: No such object: c2", ExitCode: 1,
		}}
		opts := testOpts(t, func(o *doctorOptions) { o.Runner = runner })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := Check{Status: StatusOK, Subject: "routes", Detail: "1 routed container(s); no two routers share a rule"}
		if got := section(sections, "Router")[1]; got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("fails a config file that does not load", func(t *testing.T) {
		opts := testOpts(t, func(o *doctorOptions) { o.Checkout = checkoutUnloadable(errors.New("berth.toml: bad")) })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := []Check{{Status: StatusFail, Subject: "berth.toml", Detail: "berth.toml: bad"}}
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

// jsonLabels is the JSON object parseRoutedContainers expects for a
// container's Docker labels, carrying one Traefik router rule.
func jsonLabels(routerName, rule string) string {
	return fmt.Sprintf(`{"traefik.http.routers.%s.rule":%q}`, routerName, rule)
}

/* ------------------------------------------------------ doctor in a checkout ---- */

func TestDoctorInACheckoutSections(t *testing.T) {
	// #19, row 25 in docs/port/divergences.md: doctor answers "would up
	// work?", so its berth.toml line fails on the same control character in
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
		want := []string{"ok berth.toml", "skip this worktree", "skip verify hook"}
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
		if checks[2].Detail != "needs a registered worktree; run 'berth up' first" {
			t.Fatalf("detail = %q", checks[2].Detail)
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
		if last != (Check{Status: StatusSkip, Subject: "verify hook", Detail: "none configured"}) {
			t.Fatalf("got %+v", last)
		}
	})

	t.Run("runs the verify hook with its output on stderr, and passes when it exits 0", func(t *testing.T) {
		f := newCheckoutFixture(t, "./verify")
		f.register("feature-login")
		sections, err := doctorSections(context.Background(), f.opts())
		if err != nil {
			t.Fatal(err)
		}
		checks := section(sections, "This checkout")
		want := []string{"ok berth.toml", "ok this worktree", "ok verify hook"}
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
		want := []string{"ok berth.toml", "ok this worktree", "fail verify hook"}
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
		if last != (Check{Status: StatusFail, Subject: "verify hook", Detail: "spawn went wrong"}) {
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
			{Status: StatusOK, Subject: brand.ConfigFile, Detail: f.loaded.File},
			{Status: StatusFail, Subject: "this worktree", Detail: "Command failed (git rev-parse): not a git repository"},
			{Status: StatusFail, Subject: "missing checkouts", Detail: "Command failed (git worktree list): not a git repository"},
			{Status: StatusSkip, Subject: "verify hook", Detail: "needs this worktree to be identified"},
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
			{Status: StatusOK, Subject: brand.ConfigFile, Detail: f.loaded.File},
			{Status: StatusFail, Subject: "this worktree", Detail: "no registry here"},
			{Status: StatusSkip, Subject: "verify hook", Detail: "none configured"},
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
		want := []string{"ok berth.toml", "ok this worktree", "fail missing checkouts", "ok verify hook"}
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
		want := Check{Status: StatusWarn, Subject: "feature-gone.acme.localhost", Detail: fmt.Sprintf(
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

	// silentUnreleased leaves scope taken and then unreleased — this port's
	// counterpart of TypeScript's silentLock, which the heartbeat design
	// needed (a directory made silent by backdating its mtime); the flock
	// design instead needs the flock actually let go while its record stays
	// unreleased (see internal/lock/recover.go's recoverPreviousHolder and
	// docs/port/divergences.md row 18).
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
		want := []Check{{Status: StatusOK, Subject: "locks", Detail: "none held"}}
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
	// not fail, and names no command (docs/port/divergences.md row 18). See
	// "reports an unreleased-but-gone worktree lock as recovered
	// automatically" below for the Go-accurate version of this scenario.
	t.Run("fails a silent worktree lock, and names the command that clears it safely", func(t *testing.T) {
		t.Skip("a silent lock self-heals the next time it is taken in the flock design, so doctor reports it ok, " +
			"not fail, and names no recovery command (docs/port/divergences.md, row 18); " +
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

	// legacyLockDir leaves a TypeScript build's lock directory beside scope's
	// target, with that build's holder record in the target itself when
	// command is set, and none when it is not.
	legacyLockDir := func(t *testing.T, scope lock.Scope, command string) {
		t.Helper()
		if err := os.MkdirAll(scope.Target+".lock", 0o700); err != nil {
			t.Fatal(err)
		}
		if command == "" {
			return
		}
		data, err := json.Marshal(map[string]any{"pid": deadTestPID, "host": "old-host", "command": command, "startedAt": "2026-09-01T00:00:00Z"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(scope.Target, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("warns of a TypeScript build's worktree lock, naming its holder and the command that clears it", func(t *testing.T) {
		controlHome := t.TempDir()
		registerLocksWorktree(t, controlHome, "main")
		operation, _ := lock.WorktreeScopes(controlHome, testRepository, testWorktreeID, "main", procid.Markers{})
		legacyLockDir(t, operation, "berth up")
		got := locksOf(t, controlHome)
		if len(got) != 1 || got[0].Status != StatusWarn || got[0].Subject != "main.acme.localhost" {
			t.Fatalf("got %+v", got)
		}
		want := fmt.Sprintf("taken by a TypeScript build of berth, \"berth up\" (PID %d on \"old-host\"); "+
			"this build cannot take it until that is gone. If no TypeScript build is still running here, "+
			"'berth worktree unlock %s' checks that it is gone and clears it", deadTestPID, testWorktreeID)
		if got[0].Detail != want {
			t.Fatalf("detail = %q, want %q", got[0].Detail, want)
		}
	})

	t.Run("warns of a TypeScript build's worktree lock that names no holder, with the rmdir that clears it, not unlock", func(t *testing.T) {
		controlHome := t.TempDir()
		registerLocksWorktree(t, controlHome, "main")
		operation, _ := lock.WorktreeScopes(controlHome, testRepository, testWorktreeID, "main", procid.Markers{})
		legacyLockDir(t, operation, "")
		got := locksOf(t, controlHome)
		if len(got) != 1 || got[0].Status != StatusWarn || got[0].Subject != "main.acme.localhost" {
			t.Fatalf("got %+v", got)
		}
		want := "a TypeScript build of berth left " + operation.Target + ".lock naming no holder; " +
			"this build cannot take it until that is gone. If no TypeScript build is still running here, " +
			"remove it by hand: `rmdir '" + operation.Target + ".lock'`"
		if got[0].Detail != want {
			t.Fatalf("detail = %q, want %q", got[0].Detail, want)
		}
	})

	t.Run("warns of a TypeScript build's registry lock that names no holder, with the rmdir that clears it", func(t *testing.T) {
		controlHome := t.TempDir()
		registryScope := lock.RegistryScope(controlHome)
		if err := os.MkdirAll(filepath.Dir(registryScope.Target), 0o700); err != nil {
			t.Fatal(err)
		}
		legacyLockDir(t, registryScope, "")
		got := locksOf(t, controlHome)
		if len(got) != 1 || got[0].Status != StatusWarn || got[0].Subject != "registry" {
			t.Fatalf("got %+v", got)
		}
		want := "a TypeScript build of berth left " + registryScope.Target + ".lock naming no holder; " +
			"this build cannot take it until that is gone. If no TypeScript build is still running here, " +
			"remove it by hand: `rmdir '" + registryScope.Target + ".lock'`"
		if got[0].Detail != want {
			t.Fatalf("detail = %q, want %q", got[0].Detail, want)
		}
	})

	// Pins the fix for #21: a TypeScript build's registry lock that names a
	// holder now points at `worktree unlock --registry`, the same as a
	// worktree lock's own line, instead of the rmdir that only the
	// no-holder-named case (above) still needs.
	t.Run("warns of a TypeScript build's registry lock, naming its holder and the command that clears it", func(t *testing.T) {
		controlHome := t.TempDir()
		registryScope := lock.RegistryScope(controlHome)
		if err := os.MkdirAll(filepath.Dir(registryScope.Target), 0o700); err != nil {
			t.Fatal(err)
		}
		legacyLockDir(t, registryScope, "berth up")
		got := locksOf(t, controlHome)
		if len(got) != 1 || got[0].Status != StatusWarn || got[0].Subject != "registry" {
			t.Fatalf("got %+v", got)
		}
		want := fmt.Sprintf("taken by a TypeScript build of berth, \"berth up\" (PID %d on \"old-host\"); "+
			"this build cannot take it until that is gone. If no TypeScript build is still running here, "+
			"'berth worktree unlock --registry' checks that it is gone and clears it", deadTestPID)
		if got[0].Detail != want {
			t.Fatalf("detail = %q, want %q", got[0].Detail, want)
		}
	})

	t.Run("warns of a TypeScript build's router lock, naming its holder and the command that clears it", func(t *testing.T) {
		controlHome := t.TempDir()
		routerScope := lock.Scope{
			Target: filepath.Join(t.TempDir(), "router-lock"), Description: "shared router",
			Markers: procid.Markers{Project: "berth-router"},
		}
		if err := os.MkdirAll(filepath.Dir(routerScope.Target), 0o700); err != nil {
			t.Fatal(err)
		}
		legacyLockDir(t, routerScope, "berth up")
		opts := testOpts(t, func(o *doctorOptions) { o.ControlHome = controlHome; o.RouterLockScope = &routerScope })
		sections, err := doctorSections(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		got := section(sections, "Locks")
		if len(got) != 1 || got[0].Status != StatusWarn || got[0].Subject != "router" {
			t.Fatalf("got %+v", got)
		}
		want := fmt.Sprintf("taken by a TypeScript build of berth, \"berth up\" (PID %d on \"old-host\"); "+
			"this build cannot take it until that is gone. If no TypeScript build is still running here, "+
			"'berth worktree unlock --router' checks that it is gone and clears it", deadTestPID)
		if got[0].Detail != want {
			t.Fatalf("detail = %q, want %q", got[0].Detail, want)
		}
	})

	// "is not misled by recovery mutexes, temporary files or directories
	// that are not a worktree's" exercises worktreeRecoveryLockTarget, a
	// `.recovery` mutex file the heartbeat design needed and the flock
	// design does not have at all (docs/port/divergences.md row 3: "no more
	// `.recovery` mutex"). The parts that still apply to Go — a `.tmp.NNN`
	// leftover and a non-UUID sibling directory, both of which
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
		want := []Check{{Status: StatusOK, Subject: "locks", Detail: "none held"}}
		if len(got) != 1 || got[0] != want[0] {
			t.Fatalf("got %+v", got)
		}
	})
}

// deadTestPID is a PID no test system running this suite is expected to
// have — the same choice internal/lock's own legacy_test.go and
// internal/commands/unlock_test.go make for the same reason.
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
