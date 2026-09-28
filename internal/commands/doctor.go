package commands

// `doctor` runs entirely on the flock lock layer. Read-only, start to
// finish: no lock is taken, nothing is written, and nothing is repaired.
// Every check ends as a line in the report — a check that fails to even run
// is reported as failed rather than ending the command, since the point of
// a diagnosis is to see everything that is wrong at once (see guarded) —
// except a cancellation, which ends the command outright (see guarded and
// Doctor).
//
// doctorSections builds every section from opts alone: every external
// dependency arrives through doctorOptions, so each one can be swapped for
// a fake in tests — see doctor_internal_test.go. Doctor itself is the thin
// part that builds doctorOptions from a real Env, prints the report, and
// turns its exit code into what the CLI reports.
//
// There is no Node.js version check: no Go runtime version applies to a Go
// build. The Locks section is built entirely on lock.Describe (see
// internal/lock's package comment): a live holder is an answer, not a
// problem, and an unreleased-but-gone one recovers itself the next time its
// lock is taken, so the wording here carries no heartbeat age and no
// "waits" clause — see lockCheck and lockChecks below for the specifics.
// run.Runner.Which is a plain PATH lookup with no failure mode of its own,
// so dockerChecks always reaches its own Engine and Compose probes rather
// than folding a `which` failure into one line.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/docker"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/hooks"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/procid"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/router"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/settings"
	"github.com/sofired/berth/internal/text"
	"github.com/sofired/berth/internal/worktree"
)

// gitMinimum is the oldest Git `git worktree list --porcelain -z` needs.
const gitMinimum = "2.36"

// composeMinimum is the oldest Compose the README promises to work with.
const composeMinimum = "2.21"

// dockerProbeTimeout is how long Docker gets to answer before it counts as unavailable.
const dockerProbeTimeout = 30 * time.Second

// CheckStatus is one check's verdict.
type CheckStatus string

const (
	StatusOK   CheckStatus = "ok"
	StatusWarn CheckStatus = "warn"
	StatusFail CheckStatus = "fail"
	StatusSkip CheckStatus = "skip"
)

// Check is one line of the doctor report.
type Check struct {
	Status  CheckStatus
	Subject string
	Detail  string
	// ExitCode is set for a failure that carries its own exit code — the
	// verify hook's — making it the command's.
	ExitCode *int
}

// Section is one area of the doctor report.
type Section struct {
	Title  string
	Checks []Check
}

// failureCheck turns an error a check could not even run past into a failed
// line for subject, carrying err's own message as the detail.
func failureCheck(subject string, err error) Check {
	return Check{Status: StatusFail, Subject: subject, Detail: err.Error()}
}

// guarded runs a group of checks; one that fails to even run becomes a
// failed line instead of ending the report — except a cancellation, which
// this re-raises, so it ends the whole command instead of becoming a line.
func guarded(subject string, run func() ([]Check, error)) ([]Check, error) {
	checks, err := run()
	if err != nil {
		var cancelled *clierr.Cancelled
		if errors.As(err, &cancelled) {
			return nil, err
		}
		return []Check{failureCheck(subject, err)}, nil
	}
	return checks, nil
}

/* ------------------------------------------------------------ versions ---- */

var versionPattern = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

type toolVersion struct {
	text  string
	parts [3]int
}

// readVersion is the first `major.minor[.patch]` in a tool's version output.
func readVersion(output string) (toolVersion, bool) {
	m := versionPattern.FindStringSubmatch(output)
	if m == nil {
		return toolVersion{}, false
	}
	var v toolVersion
	v.text = m[0]
	v.parts[0] = atoiOrZero(m[1])
	v.parts[1] = atoiOrZero(m[2])
	v.parts[2] = atoiOrZero(m[3])
	return v, true
}

// atoiOrZero parses s, a digit run readVersion already matched, as a
// non-negative integer; "" (an absent patch group) yields 0.
func atoiOrZero(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

// atLeast reports whether version is minimum or newer, comparing major,
// minor, then patch in order.
func atLeast(version, minimum [3]int) bool {
	for i := 0; i < 3; i++ {
		if version[i] != minimum[i] {
			return version[i] > minimum[i]
		}
	}
	return true
}

// versionCheck is a tool's version output against the oldest version that works.
func versionCheck(subject, output, minimum string) Check {
	version, ok := readVersion(output)
	floor, floorOK := readVersion(minimum)
	if !ok || !floorOK {
		return Check{Status: StatusFail, Subject: subject, Detail: fmt.Sprintf("could not read a version from %q", text.OneLine(output, 80))}
	}
	if atLeast(version.parts, floor.parts) {
		return Check{Status: StatusOK, Subject: subject, Detail: version.text}
	}
	return Check{Status: StatusFail, Subject: subject, Detail: fmt.Sprintf("%s; %s or newer is needed", version.text, minimum)}
}

/* ------------------------------------------------------- prerequisites ---- */

// gitCheck runs `git --version` and reports it as a version check; a
// non-zero exit is its own failed line, detailed with stderr, rather than a
// version to compare.
func gitCheck(ctx context.Context, runner run.Runner) (Check, error) {
	result, err := runner.Run(ctx, run.Spec{Command: "git", Args: []string{"--version"}, AllowFailure: true})
	if err != nil {
		return Check{}, err
	}
	if result.ExitCode != 0 {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = fmt.Sprintf("exit %d", result.ExitCode)
		}
		return Check{Status: StatusFail, Subject: "Git", Detail: fmt.Sprintf("`git --version` failed: %s", text.OneLine(detail, 0))}, nil
	}
	return versionCheck("Git", result.Stdout, gitMinimum), nil
}

// askDocker is one `docker` query, bounded in time; Docker being unavailable
// is its answer, not a propagated error — but only that: anything else,
// cancellation included, propagates so a Ctrl-C here ends the command
// rather than becoming a line. The successful result is returned for the
// caller to interpret further; nil means the returned Check is the answer.
func askDocker(ctx context.Context, runner run.Runner, subject string, args []string, timeout time.Duration) (Check, *run.Result, error) {
	result, err := docker.Run(ctx, runner, args, run.Spec{AllowFailure: true, Timeout: timeout})
	if err != nil {
		if docker.Unavailable(err) {
			return Check{Status: StatusFail, Subject: subject, Detail: err.Error()}, nil, nil
		}
		return Check{}, nil, err
	}
	if result.TimedOut {
		return Check{Status: StatusFail, Subject: subject, Detail: fmt.Sprintf(
			"`docker %s` did not answer within %d s", strings.Join(args, " "), int(timeout.Seconds()))}, nil, nil
	}
	if result.ExitCode != 0 {
		return Check{Status: StatusFail, Subject: subject, Detail: text.OneLine(
			docker.Said(result.Stderr, fmt.Sprintf("`docker %s` exited %d", strings.Join(args, " "), result.ExitCode)), 0)}, nil, nil
	}
	return Check{}, &result, nil
}

// dockerState is the Docker lines, and whether each half of Docker answered
// at all — what the Router section needs.
type dockerState struct {
	checks          []Check
	engineAnswered  bool
	composeAnswered bool
}

// dockerChecks asks Docker Engine and Docker Compose together, so a Docker
// that answers neither costs one timeout rather than two. Its returned error
// is only ever a propagate-worthy one — a cancellation reaching either
// probe — since askDocker itself already turns Docker being unavailable into
// a Check; prerequisitesSection folds anything else that still reaches it
// into one failed "Docker" line.
func dockerChecks(ctx context.Context, runner run.Runner, timeout time.Duration) (dockerState, error) {
	if !runner.Which("docker") {
		return dockerState{checks: []Check{
			{Status: StatusFail, Subject: "Docker Engine", Detail: "the `docker` command is not on PATH; is Docker installed?"},
			{Status: StatusSkip, Subject: "Docker Compose", Detail: "needs the `docker` command"},
		}}, nil
	}
	var engineCheck, composeCheck Check
	var engineResult, composeResult *run.Result
	var engineErr, composeErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		engineCheck, engineResult, engineErr = askDocker(ctx, runner, "Docker Engine", []string{"version", "--format", "{{.Server.Version}}"}, timeout)
	}()
	go func() {
		defer wg.Done()
		composeCheck, composeResult, composeErr = askDocker(ctx, runner, "Docker Compose", []string{"compose", "version", "--short"}, timeout)
	}()
	wg.Wait()
	if engineErr != nil {
		return dockerState{}, engineErr
	}
	if composeErr != nil {
		return dockerState{}, composeErr
	}

	checks := make([]Check, 0, 2)
	if engineResult != nil {
		checks = append(checks, Check{Status: StatusOK, Subject: "Docker Engine",
			Detail: fmt.Sprintf("%s, daemon answering", text.OneLine(strings.TrimSpace(engineResult.Stdout), 80))})
	} else {
		checks = append(checks, engineCheck)
	}
	if composeResult != nil {
		checks = append(checks, versionCheck("Docker Compose", composeResult.Stdout, composeMinimum))
	} else {
		checks = append(checks, composeCheck)
	}
	return dockerState{checks: checks, engineAnswered: engineResult != nil, composeAnswered: composeResult != nil}, nil
}

// prerequisitesSection is the "Prerequisites" section: Git, then Docker
// Engine and Docker Compose. Docker is probed first, and an error from its
// probe becomes one failed "Docker" line rather than ending the report,
// unless it is a cancellation. Also returns the dockerState the Router
// section needs to know whether Docker answered at all.
func prerequisitesSection(ctx context.Context, opts doctorOptions) ([]Check, dockerState, error) {
	state, err := dockerChecks(ctx, opts.Runner, opts.dockerTimeout())
	if err != nil {
		var cancelled *clierr.Cancelled
		if errors.As(err, &cancelled) {
			return nil, dockerState{}, err
		}
		state = dockerState{checks: []Check{failureCheck("Docker", err)}}
	}
	checks, err := guarded("Git", func() ([]Check, error) {
		check, err := gitCheck(ctx, opts.Runner)
		if err != nil {
			return nil, err
		}
		return []Check{check}, nil
	})
	if err != nil {
		return nil, dockerState{}, err
	}
	checks = append(checks, state.checks...)
	return checks, state, nil
}

/* ------------------------------------------------------------ registry ---- */

// controlHomeCheck reports the control home: ok noting it will be created by
// the first `up` when it does not exist yet, ok naming its path when it
// exists and passes files.AssertAdoptableControlHome, or that assertion's
// own error otherwise. userHome is the caller's own resolved home
// directory, handed straight through to AssertAdoptableControlHome.
func controlHomeCheck(controlHome, userHome string) (Check, error) {
	exists, err := files.PathExists(controlHome)
	if err != nil {
		return Check{}, err
	}
	if !exists {
		return Check{Status: StatusOK, Subject: "control home", Detail: fmt.Sprintf("%s (not created yet; the first up creates it)", controlHome)}, nil
	}
	if err := files.AssertAdoptableControlHome(controlHome, userHome); err != nil {
		return Check{}, err
	}
	return Check{Status: StatusOK, Subject: "control home", Detail: controlHome}, nil
}

// registryChecks is what the registry says about itself: records that
// cannot be read, and hostnames that more than one worktree claims —
// including the index's own, which no worktree can share with it.
func registryChecks(scan registry.RegistryScan, commandName string) []Check {
	count := "none yet"
	if len(scan.Records) > 0 {
		count = fmt.Sprintf("%d readable", len(scan.Records))
	}
	checks := []Check{{Status: StatusOK, Subject: "registrations", Detail: count}}
	for _, problem := range scan.Problems {
		checks = append(checks, Check{Status: StatusFail, Subject: "unreadable record", Detail: problem.Reason})
	}

	hosts := make([]string, 0, len(scan.Conflicts))
	for host := range scan.Conflicts {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	for _, host := range hosts {
		claimants := scan.Conflicts[host]
		ids := make([]string, len(claimants))
		for i, record := range claimants {
			ids[i] = record.WorktreeID
		}
		checks = append(checks, Check{Status: StatusFail, Subject: host, Detail: fmt.Sprintf(
			"registered to %d worktrees (%s); requests for it may reach any of them that is running. Remove all but one with '%s worktree remove <uuid>'.",
			len(claimants), text.OneLine(strings.Join(ids, ", "), 200), commandName)})
	}

	for _, record := range scan.Records {
		if layout.WorktreeHost(record.Slug, record.Domain) != router.IndexHost {
			continue
		}
		checks = append(checks, Check{Status: StatusFail, Subject: router.IndexHost, Detail: fmt.Sprintf(
			"is the checkout index's own address, and worktree %s is registered to it. Set another domain in that checkout's %s and run '%s up' there.",
			record.WorktreeID, brand.ConfigFile, commandName)})
	}
	return checks
}

// registrySection is the "Registry" section: the berth root (#80), the
// control home check, the router's own state directory, and registryChecks
// over a fresh scan of every registration.
func registrySection(opts doctorOptions) ([]Check, error) {
	// Neither can fail — both are already-resolved, already-checked paths,
	// no I/O of their own — so unlike control home and registrations below,
	// neither needs its own guarded() call; a person who set only
	// BERTH_CONTROL_HOME can still see where the rest of the berth root
	// lives (the router's own state; a future settings.toml, #79).
	checks := []Check{{Status: StatusOK, Subject: "berth root", Detail: opts.Home}}
	controlHomeChecks, err := guarded("control home", func() ([]Check, error) {
		check, err := controlHomeCheck(opts.ControlHome, opts.UserHome)
		if err != nil {
			return nil, err
		}
		return []Check{check}, nil
	})
	if err != nil {
		return nil, err
	}
	checks = append(checks, controlHomeChecks...)
	checks = append(checks, Check{Status: StatusOK, Subject: "router state", Detail: opts.RouterStateDir})
	registrations, err := guarded("registrations", func() ([]Check, error) {
		scan, err := registry.ScanRegistrations(opts.ControlHome)
		if err != nil {
			return nil, err
		}
		return registryChecks(scan, opts.CommandName), nil
	})
	if err != nil {
		return nil, err
	}
	return append(checks, registrations...), nil
}

/* --------------------------------------------------------------- locks ---- */

// lockCheck is a read-only look at one lock, built entirely on lock.Describe.
// ok is false when there is nothing worth a line: the lock was never taken,
// or it was taken and cleanly released.
//
// handRemoval is the advice to fall back on if an unreleased lock's next
// acquirer refuses it too — its holder judged unverifiable rather than
// gone, which recovery alone cannot get past — a full sentence fragment
// slotted straight after "If that also refuses it as unverifiable, ". A
// worktree lock needs none: `worktree unlock --holder-unverifiable` is that
// escape hatch. The registry and router locks now have the same escape
// hatch, `worktree unlock --registry|--router --holder-unverifiable` (#21),
// so lockChecks passes each of them that instead of the hand removal this
// build once had no better answer than.
func lockCheck(subject string, scope lock.Scope, handRemoval string) (check Check, ok bool) {
	described, err := lock.Describe(scope, nil)
	if err != nil {
		return Check{Status: StatusFail, Subject: subject, Detail: fmt.Sprintf("could not be checked: %s", err)}, true
	}
	if described.State == lock.Held {
		return Check{Status: StatusOK, Subject: subject, Detail: "held by " + lock.HolderLabel(described.Record)}, true
	}
	if described.Record != nil && described.Record.ReleasedAt == nil {
		detail := fmt.Sprintf("unreleased by %s; recovered automatically the next time this lock is taken", lock.HolderLabel(described.Record))
		if handRemoval != "" {
			detail += fmt.Sprintf(". If that also refuses it as unverifiable, %s", handRemoval)
		}
		return Check{Status: StatusOK, Subject: subject, Detail: detail}, true
	}
	return Check{}, false
}

// lockedWorktree is one worktree lock.Describe should look at, found by
// scanning this control home's worktree lock directory rather than the
// registry, so an orphan — a first `up` that died before it registered — is
// still seen.
type lockedWorktree struct {
	RepositoryID string
	WorktreeID   string
}

// readDirIfExists is os.ReadDir that answers no entries, not an error, for a
// directory that does not exist yet.
func readDirIfExists(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return entries, nil
}

// lockedWorktreeIDs scans this control home's worktree locks for every
// worktree that has one, whether or not it is registered. A non-UUID
// repository directory, or a non-UUID entry inside one (a `.tmp.NNN`
// left by a write in progress, say), never matches uuidPattern and is
// silently skipped — there is no separate recovery-mutex file to skip
// around: flock(2) needs no sidecar mutex the way the heartbeat design
// once did.
func lockedWorktreeIDs(controlHome string) ([]lockedWorktree, error) {
	worktreesDir := layout.ControlLayout(controlHome).LocksDir + "/worktrees"
	repositories, err := readDirIfExists(worktreesDir)
	if err != nil {
		return nil, err
	}
	var found []lockedWorktree
	for _, repository := range repositories {
		if !repository.IsDir() || !uuidPattern.MatchString(repository.Name()) {
			continue
		}
		entries, err := readDirIfExists(worktreesDir + "/" + repository.Name())
		if err != nil {
			return nil, err
		}
		ids := map[string]bool{}
		for _, entry := range entries {
			id := worktreeIDOf(entry.Name())
			if uuidPattern.MatchString(id) {
				ids[id] = true
			}
		}
		sorted := make([]string, 0, len(ids))
		for id := range ids {
			sorted = append(sorted, id)
		}
		sort.Strings(sorted)
		for _, id := range sorted {
			found = append(found, lockedWorktree{RepositoryID: repository.Name(), WorktreeID: id})
		}
	}
	return found, nil
}

// lockChecks is who holds what, read without taking or touching anything: a
// live holder is an answer, not a problem; an unreleased-but-gone one is
// recovered automatically the next time its lock is taken (see
// internal/lock's package comment), so it needs no advice beyond that —
// except the registry and router locks' own --holder-unverifiable fallback
// (see lockCheck), now that `worktree unlock --registry`/`--router` can
// clear them too (#21).
func lockChecks(controlHome string, routerLockScope *lock.Scope, commandName string) ([]Check, error) {
	scan, err := registry.ScanRegistrations(controlHome)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]registry.RegisteredWorktree, len(scan.Records))
	for _, record := range scan.Records {
		byKey[record.RepositoryID+"/"+record.WorktreeID] = record
	}

	// Subjects: "registry" and "router" (short — not their longer
	// lock.Scope.Description), a worktree's own hostname (or "worktree
	// <uuid>" before it has one) for its operation lock, and that plus
	// " (development session)" for its development lock.
	var checks []Check
	registryScope := lock.RegistryScope(controlHome)
	registryHandRemoval := fmt.Sprintf("run '%s worktree unlock --registry --holder-unverifiable' once you have made sure its holder is gone", commandName)
	if check, ok := lockCheck("registry", registryScope, registryHandRemoval); ok {
		checks = append(checks, check)
	}
	if routerLockScope != nil {
		routerHandRemoval := fmt.Sprintf(
			"run '%s worktree unlock --router --holder-unverifiable' once you have made sure its holder, and everything it started, is gone", commandName)
		if check, ok := lockCheck("router", *routerLockScope, routerHandRemoval); ok {
			checks = append(checks, check)
		}
	}

	ids, err := lockedWorktreeIDs(controlHome)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		name := id.WorktreeID
		shown := "worktree " + id.WorktreeID
		if record, ok := byKey[id.RepositoryID+"/"+id.WorktreeID]; ok {
			name = record.Slug
			shown = layout.WorktreeHost(record.Slug, record.Domain)
		}
		operation, development := lock.WorktreeScopes(controlHome, id.RepositoryID, id.WorktreeID, name, procid.Markers{})
		// Development, then operation.
		if check, ok := lockCheck(shown+" (development session)", development, ""); ok {
			checks = append(checks, check)
		}
		if check, ok := lockCheck(shown, operation, ""); ok {
			checks = append(checks, check)
		}
	}

	if len(checks) == 0 {
		return []Check{{Status: StatusOK, Subject: "locks", Detail: "none held"}}, nil
	}
	return checks, nil
}

/* -------------------------------------------------------------- router ---- */

// doctorRouterSource is what the Router section needs of the shared router.
// A real router.Router satisfies it through realRouterSource; tests supply
// their own fake.
type doctorRouterSource interface {
	Status(ctx context.Context) (router.RouterStatus, error)
	Network() string
	// Owner reports which berth root started the running router (#82); see
	// routerOwnerCheck.
	Owner(ctx context.Context) (router.Ownership, error)
	// PublishedPort is the host port the running router publishes, and
	// whether one is running at all (#79); see routerPortCheck.
	PublishedPort(ctx context.Context) (port int, running bool, err error)
}

// realRouterSource adapts a real *router.Router — whose Network is a plain
// field, not a method — to doctorRouterSource.
type realRouterSource struct{ svc *router.Router }

// Status delegates to the wrapped router.Router's own Status.
func (r realRouterSource) Status(ctx context.Context) (router.RouterStatus, error) {
	return r.svc.Status(ctx)
}

// PublishedPort delegates to the wrapped router.Router's own PublishedPort.
func (r realRouterSource) PublishedPort(ctx context.Context) (int, bool, error) {
	return r.svc.PublishedPort(ctx)
}

// Network returns the wrapped router.Router's Network field.
func (r realRouterSource) Network() string { return r.svc.Network }

// Owner delegates to the wrapped router.Router's own Owner.
func (r realRouterSource) Owner(ctx context.Context) (router.Ownership, error) {
	return r.svc.Owner(ctx)
}

// routerStatusChecks is the router in the words `router status` uses; not
// running is an answer, not a problem.
func routerStatusChecks(status router.RouterStatus) []Check {
	report := router.StatusReport(status)
	if !status.Reachable {
		checks := make([]Check, len(report.Errors))
		for i, e := range report.Errors {
			checks[i] = Check{Status: StatusFail, Subject: "router", Detail: e}
		}
		return checks
	}
	if !report.Failed {
		return []Check{{Status: StatusOK, Subject: "router", Detail: strings.Join(report.Output, "; ")}}
	}
	checks := make([]Check, len(status.Problems))
	for i, problem := range status.Problems {
		checks[i] = Check{Status: StatusFail, Subject: "router", Detail: problem}
	}
	return checks
}

// fragmentRule is one router name and the rule it claims, read from one
// dynamic-configuration fragment's decoded document — router.FragmentDocument's
// own fields are exported even though its nested types are not, so this
// needs no accessor from internal/router.
type fragmentRule struct {
	name string
	rule string
}

// dynamicNoun names the shared router's dynamic-configuration directory in
// a refusal — router.FragmentPath's own boundary, mirrored here (found in
// review, round 2 of #55) since readFragmentRules needs the same check the
// writers already apply, and internal/router's own copy is unexported.
const dynamicNoun = "the shared router's dynamic-configuration directory"

// readFragmentRules decodes one dynamic-configuration fragment's own HTTP
// routers into their names and rules. Refuses the same way the writers do
// (files.AssertSafeFilePathNamed) before ever reading path — a fragment
// filename is only ever a plain file router.ListFragments itself wrote or
// found already there, but doctor never trusted that on the write side
// either, and a doctor that follows a symlink under dynamicDir out to an
// arbitrary file would read it and report its contents as "conflicting"
// text, not just fail to decode it. A fragment that cannot be safely
// resolved, read, or decoded is skipped rather than failing the whole
// check: validating a fragment's own JSON shape is not doctor's job, and a
// corrupted or unsafe one is not silently treated as a conflict with
// anything either.
func readFragmentRules(dynamicDir, path string) []fragmentRule {
	if err := files.AssertSafeFilePathNamed(dynamicDir, path, dynamicNoun); err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc router.FragmentDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}
	rules := make([]fragmentRule, 0, len(doc.HTTP.Routers))
	for name, entry := range doc.HTTP.Routers {
		rules = append(rules, fragmentRule{name: name, rule: entry.Rule})
	}
	return rules
}

// fragmentIdentity is a route fragment's own <repository-uuid>.
// <worktree-uuid>.<service> triple, resolved against byKey (lockChecks'
// own map, keyed by "<repositoryID>/<worktreeID>") the same way the Locks
// section already resolves a lock's holder to a name: the registered
// worktree's own routed hostname plus the service, when a record exists,
// or the raw identity — nothing else to show — when it does not (removed,
// or never registered by this control home). fragmentIdentity never fails:
// a route conflict is worth reporting whether or not the registry can name
// its claimants.
func fragmentIdentity(byKey map[string]registry.RegisteredWorktree, fragment router.FragmentInfo) string {
	if record, ok := byKey[fragment.RepositoryID+"/"+fragment.WorktreeID]; ok {
		return layout.WorktreeHost(record.Slug, record.Domain) + " (" + fragment.Service + ")"
	}
	return fragment.RepositoryID + "." + fragment.WorktreeID + "." + fragment.Service
}

// routeConflicts is every rule more than one fragment's router claims, each
// with the claiming fragments' own identities (fragmentIdentity). This
// replaces the pre-#55 mechanism of reading routed containers' own Traefik
// labels: Traefik's Docker-provider discovery is gone, so a fragment file
// is now the only thing that can claim a rule at all. dynamicDir is the
// boundary readFragmentRules checks every fragment path against.
func routeConflicts(dynamicDir string, fragments []router.FragmentInfo, byKey map[string]registry.RegisteredWorktree) map[string][]string {
	type claim struct {
		claimants map[[2]string]bool
		names     map[string]bool
	}
	byRule := map[string]*claim{}
	for _, fragment := range fragments {
		identity := fragmentIdentity(byKey, fragment)
		for _, r := range readFragmentRules(dynamicDir, fragment.Path) {
			entry, ok := byRule[r.rule]
			if !ok {
				entry = &claim{claimants: map[[2]string]bool{}, names: map[string]bool{}}
				byRule[r.rule] = entry
			}
			entry.claimants[[2]string{r.name, identity}] = true
			entry.names[identity] = true
		}
	}
	conflicts := map[string][]string{}
	for rule, entry := range byRule {
		if len(entry.claimants) <= 1 {
			continue
		}
		names := make([]string, 0, len(entry.names))
		for name := range entry.names {
			names = append(names, name)
		}
		sort.Strings(names)
		conflicts[rule] = names
	}
	return conflicts
}

// orphanedFragmentsCheck warns — never fails, and never removes anything,
// matching doctor's own "it takes no lock, writes nothing, and repairs
// nothing" invariant — when fragments exist whose worktree's own router
// network is gone (router.OrphanedFragments, the same definition the
// router's own Ensure uses to actually remove them). nil, nil means nothing
// worth a line.
func orphanedFragmentsCheck(ctx context.Context, runner run.Runner, timeout time.Duration, routerNetwork, commandName string, fragments []router.FragmentInfo) (*Check, error) {
	if len(fragments) == 0 {
		return nil, nil
	}
	networks, err := router.LabelledWorktreeNetworks(ctx, runner, timeout, routerNetwork)
	if err != nil {
		return nil, err
	}
	orphaned := router.OrphanedFragments(fragments, networks)
	if len(orphaned) == 0 {
		return nil, nil
	}
	type worktreeKey struct{ repositoryID, worktreeID string }
	worktrees := map[worktreeKey]bool{}
	for _, fragment := range orphaned {
		worktrees[worktreeKey{fragment.RepositoryID, fragment.WorktreeID}] = true
	}
	fragmentPlural, worktreePlural, networkPlural, networkVerb := "s", "s", "s", "are"
	if len(orphaned) == 1 {
		fragmentPlural = ""
	}
	if len(worktrees) == 1 {
		worktreePlural, networkPlural, networkVerb = "", "", "is"
	}
	return &Check{Status: StatusWarn, Subject: "routes", Detail: fmt.Sprintf(
		"%d route fragment%s for %d worktree%s whose router network%s %s gone; the next '%s up' or '%s router up' removes them",
		len(orphaned), fragmentPlural, len(worktrees), worktreePlural, networkPlural, networkVerb, commandName, commandName)}, nil
}

// routeChecks reads every dynamic-configuration fragment under the router's
// own state directory (router.ListFragments) rather than any container's
// own labels — Traefik's Docker-provider discovery is gone (#55), so a
// fragment is the only thing that can claim a route at all now — reports
// any rule two fragments both claim, the same conflict the pre-#55 version
// reported from container labels, and warns (never fails or repairs) about
// fragments an orphaned worktree left behind.
func routeChecks(ctx context.Context, runner run.Runner, state layout.RouterLayout, routerNetwork, controlHome, commandName string, timeout time.Duration) ([]Check, error) {
	fragments, err := router.ListFragments(state)
	if err != nil {
		return nil, err
	}
	var checks []Check
	if len(fragments) == 0 {
		checks = append(checks, Check{Status: StatusOK, Subject: "routes", Detail: "no route fragments"})
	} else {
		scan, err := registry.ScanRegistrations(controlHome)
		if err != nil {
			return nil, err
		}
		byKey := make(map[string]registry.RegisteredWorktree, len(scan.Records))
		for _, record := range scan.Records {
			byKey[record.RepositoryID+"/"+record.WorktreeID] = record
		}
		conflicts := routeConflicts(state.DynamicDir, fragments, byKey)
		if len(conflicts) == 0 {
			plural := "s"
			if len(fragments) == 1 {
				plural = ""
			}
			checks = append(checks, Check{Status: StatusOK, Subject: "routes", Detail: fmt.Sprintf(
				"%d route fragment%s; no two routers share a rule", len(fragments), plural)})
		} else {
			rules := make([]string, 0, len(conflicts))
			for rule := range conflicts {
				rules = append(rules, rule)
			}
			sort.Strings(rules)
			for _, rule := range rules {
				checks = append(checks, Check{Status: StatusFail, Subject: "routes", Detail: fmt.Sprintf(
					"%s is claimed by %s; requests for it may reach any of them",
					text.OneLine(rule, 200), text.OneLine(strings.Join(conflicts[rule], ", "), 200))})
			}
		}
	}
	// The orphan check is advisory: a failure to list networks becomes its
	// own failed line rather than replacing the conflict lines above.
	orphaned, err := orphanedFragmentsCheck(ctx, runner, timeout, routerNetwork, commandName, fragments)
	if err != nil {
		var cancelled *clierr.Cancelled
		if errors.As(err, &cancelled) {
			return nil, err
		}
		checks = append(checks, failureCheck("routes", err))
	} else if orphaned != nil {
		checks = append(checks, *orphaned)
	}
	return checks, nil
}

// addressPoolBridgeWarning is how many existing bridge-driver networks
// (Docker's own built-in "bridge" excluded) doctor warns at. Since #55 every
// routed worktree that is up owns one more bridge network, each carved from
// the daemon's default address pools. Docker exposes no way to read those
// pools' configuration, and doctor writes nothing — so no throwaway
// `docker network create` probe — so this counts what already exists.
//
// Docker's built-in default pools are fifteen /16s (172.17.0.0 through
// 172.31.0.0) and sixteen /20s carved from 192.168.0.0/16: about 31
// networks, docker0 taking one, before `compose up` fails with "could not
// find an available, non-overlapping IPv4 address pool among the defaults".
// 20 warns with roughly a third of that left. It is a heuristic: a daemon
// with its own default-address-pools, or other networks' explicit subnets,
// moves the real limit either way; the threshold is the owner's to tune.
const addressPoolBridgeWarning = 20

// addressPoolsCheck warns when the number of existing bridge-driver
// networks suggests Docker's default address pools may be close to
// exhausted — see addressPoolBridgeWarning's own comment for how
// approximate this is. commandName names this build's own invocation in the
// warning's advisory text (found in review of #55), matching every other
// doctor advisory's own quoted-command form (orphanedFragmentsCheck,
// registrySection, checkoutSections) rather than a hardcoded "up".
func addressPoolsCheck(ctx context.Context, runner run.Runner, commandName string, timeout time.Duration) (Check, error) {
	check, result, err := askDocker(ctx, runner, "address pools", []string{
		"network", "ls", "--filter", "driver=bridge", "--format", "{{.Name}}",
	}, timeout)
	if err != nil {
		return Check{}, err
	}
	if result == nil {
		return check, nil
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(result.Stdout), "\n") {
		if name := strings.TrimSpace(line); name != "" && name != "bridge" {
			count++
		}
	}
	if count >= addressPoolBridgeWarning {
		return Check{Status: StatusWarn, Subject: "address pools", Detail: fmt.Sprintf(
			"%d bridge networks exist; with Docker's default address pools (about 31) a later '%s up' may fail to "+
				"create its network. Remove networks nothing uses with `docker network prune`, or give the "+
				"Docker daemon smaller or more `default-address-pools`.", count, commandName)}, nil
	}
	plural := "s"
	if count == 1 {
		plural = ""
	}
	return Check{Status: StatusOK, Subject: "address pools", Detail: fmt.Sprintf("%d bridge network%s", count, plural)}, nil
}

// routerOwnerCheck is doctor's own read-only report of which berth root
// started the running router (#82): nil, nil when nothing is running —
// Status already says so, and there is no owner to report. A running
// container with no ownership label at all is reported as foreign, exactly
// like one whose label names another root, matching Router.Ensure's own
// checkOwnership; one whose label names this berth root is ok.
func routerOwnerCheck(ctx context.Context, svc doctorRouterSource, home, commandName string) (*Check, error) {
	owner, err := svc.Owner(ctx)
	if err != nil {
		return nil, err
	}
	if !owner.Running {
		return nil, nil
	}
	if !owner.Labelled {
		return &Check{Status: StatusFail, Subject: "router owner", Detail: fmt.Sprintf(
			"not owned by this berth root; this one's '%s up' or '%s router up' refuses it until '%s router down' hands it over",
			commandName, commandName, commandName)}, nil
	}
	if owner.Home == home {
		return &Check{Status: StatusOK, Subject: "router owner", Detail: "this berth root (" + home + ")"}, nil
	}
	return &Check{Status: StatusFail, Subject: "router owner", Detail: fmt.Sprintf(
		"a different berth root (%s); this one's '%s up' or '%s router up' refuses it until '%s router down' hands it over",
		owner.Home, commandName, commandName, commandName)}, nil
}

// routerSection is the "Router" section: opts.RouterErr's own line if the
// router could not even be described, and one skip if Docker Engine did not
// answer. Otherwise it is the router's status, plus routerOwnerCheck's own
// report of which berth root started it (#82), or a skip when Docker
// Compose did not answer, since both need Compose, followed by routeChecks
// and addressPoolsCheck, which need only the Engine, and last the port that
// was resolved and where it came from (#79) — which needs nothing, though
// comparing it with the port the running router publishes needs Compose,
// so without Compose that line states the resolution alone.
func routerSection(ctx context.Context, opts doctorOptions, state dockerState) ([]Check, error) {
	if opts.RouterErr != nil {
		return []Check{{Status: StatusFail, Subject: "router", Detail: opts.RouterErr.Error()}}, nil
	}
	if !state.engineAnswered {
		// One skip and nothing else: the port resolution (#79) needs no
		// Docker, but a Go-only line here would also widen the section's
		// subject column, so it waits for #46. Pinned byte for byte by the
		// golden tests.
		return []Check{{Status: StatusSkip, Subject: "router", Detail: "needs Docker"}}, nil
	}
	var checks []Check
	if state.composeAnswered {
		routerChecks, err := guarded("router", func() ([]Check, error) {
			status, err := opts.Router.Status(ctx)
			if err != nil {
				return nil, err
			}
			result := routerStatusChecks(status)
			owner, err := routerOwnerCheck(ctx, opts.Router, opts.Home, opts.CommandName)
			if err != nil {
				return nil, err
			}
			if owner != nil {
				result = append(result, *owner)
			}
			return result, nil
		})
		if err != nil {
			return nil, err
		}
		checks = append(checks, routerChecks...)
	} else {
		checks = append(checks, Check{Status: StatusSkip, Subject: "router", Detail: "needs Docker Compose"})
	}
	routes, err := guarded("routes", func() ([]Check, error) {
		return routeChecks(ctx, opts.Runner, opts.RouterState, opts.Router.Network(), opts.ControlHome, opts.CommandName, opts.dockerTimeout())
	})
	if err != nil {
		return nil, err
	}
	checks = append(checks, routes...)
	pools, err := guarded("address pools", func() ([]Check, error) {
		check, err := addressPoolsCheck(ctx, opts.Runner, opts.CommandName, opts.dockerTimeout())
		if err != nil {
			return nil, err
		}
		return []Check{check}, nil
	})
	if err != nil {
		return nil, err
	}
	checks = append(checks, pools...)
	port, err := guarded("router port", func() ([]Check, error) {
		// The resolution needs nothing; comparing it with what the running
		// router publishes asks Compose for the container, so without Compose
		// the line says only where the port came from — the same skip the
		// status check above makes, not a failure.
		if !state.composeAnswered {
			return []Check{{Status: StatusOK, Subject: "router port", Detail: opts.RouterPort.Describe()}}, nil
		}
		check, err := routerPortCheck(ctx, opts)
		if err != nil {
			return nil, err
		}
		return []Check{check}, nil
	})
	if err != nil {
		return nil, err
	}
	return append(checks, port...), nil
}

// routerPortCheck reports the router port doctor resolved and where it came
// from (#79), and warns when the running router publishes a different one:
// the next `up` or `router up` moves it, and URLs already open keep the old
// port until then. Nothing running, or a port that cannot be read off the
// container, is just the resolution itself.
func routerPortCheck(ctx context.Context, opts doctorOptions) (Check, error) {
	described := opts.RouterPort.Describe()
	published, running, err := opts.Router.PublishedPort(ctx)
	if err != nil {
		return Check{}, err
	}
	if running && published != 0 && published != opts.RouterPort.Port {
		return Check{Status: StatusWarn, Subject: "router port", Detail: fmt.Sprintf(
			"%s, but the running router publishes %d; the next '%s up' or '%s router up' moves it, and URLs already open keep the old port until then",
			described, published, opts.CommandName, opts.CommandName)}, nil
	}
	return Check{Status: StatusOK, Subject: "router port", Detail: described}, nil
}

/* ------------------------------------------------------------ checkout ---- */

// checkoutSource is where the checkout section comes from — no config at
// all, one that does not load, or one that does.
type checkoutSource struct {
	kind         string // "none", "unloadable", or "loaded"
	searchedFrom string
	unloadable   error
	loaded       *config.Loaded
}

// checkoutNone is a checkoutSource for no berth.toml found searching upward
// from searchedFrom.
func checkoutNone(searchedFrom string) checkoutSource {
	return checkoutSource{kind: "none", searchedFrom: searchedFrom}
}

// checkoutUnloadable is a checkoutSource for a berth.toml that was found but
// failed to load, carrying the loader's own error.
func checkoutUnloadable(err error) checkoutSource {
	return checkoutSource{kind: "unloadable", unloadable: err}
}

// checkoutIsLoaded is a checkoutSource for a berth.toml that loaded cleanly.
func checkoutIsLoaded(loaded *config.Loaded) checkoutSource {
	return checkoutSource{kind: "loaded", loaded: loaded}
}

// checkoutRegistry is the two registry.Registry methods checkoutChecks
// needs. *registry.Registry satisfies it without declaring so.
type checkoutRegistry interface {
	Current(ctx context.Context) (*registry.WorktreeRegistration, error)
	PruneCandidates(ctx context.Context) ([]registry.RegisteredWorktree, error)
}

// findCheckout is the config the other commands would use here, without
// failing when there is none. explicit is --config; "" means the nearest
// berth.toml above cwd.
func findCheckout(explicit, cwd string) (checkoutSource, error) {
	file := explicit
	if file == "" {
		found, ok, err := config.FindConfigFile(cwd)
		if err != nil {
			return checkoutSource{}, err
		}
		if !ok {
			return checkoutNone(cwd), nil
		}
		file = found
	}
	loaded, err := config.Load(file)
	if err != nil {
		return checkoutUnloadable(err), nil
	}
	return checkoutIsLoaded(loaded), nil
}

// checkoutFileCheck is the first line of "This checkout": ok when loaded
// parsed cleanly (the base loader is lenient — see internal/config/start.go),
// fail when config.CheckStart finds a control character in a
// [service.index] label or purpose (#19), with the exact wording
// `config validate` itself would refuse with. doctor answers "would up work?", so this is a
// failed check, not a warning: `up` against this same file refuses outright.
// A multi-line message like this one prints on the section's single line
// the same way an unrecognized-key or other multi-line schema error already
// does in the "unloadable" case above — formatDoctorReport's text.Printable
// turns each embedded newline into a space.
func checkoutFileCheck(loaded *config.Loaded) Check {
	if err := config.CheckStart(loaded); err != nil {
		return Check{Status: StatusFail, Subject: brand.ConfigFile, Detail: err.Error()}
	}
	return Check{Status: StatusOK, Subject: brand.ConfigFile, Detail: loaded.File}
}

// hookResolutionChecks is one line per configured hook (config.Hooks.Configured,
// schema order: prepare, seed, verify, backup, restore), each subject
// "<stage> hook file" — distinct from "verify hook" below, the actual run,
// so the two never read as one repeated line — and never one for a hook
// that is not configured at all, matching `config validate`'s own silence
// about them. Each line predicts, without spawning anything, exactly what
// RunHook's own Runner would find trying to start it: hooks.ResolveHook
// shares RunHook's own command splitting and path resolution, and
// run.PredictStart underneath it shares osrun's own existence and
// permission checks — so this can never drift from what `up` or `doctor`'s
// own verify-hook run below actually does. A hook file that would not start
// names the exit code the runner would give, 127 for missing or 126 for
// present but not executable or not a valid executable format, in the
// line's own Detail — never in the Check's own ExitCode, and so never
// doctor's own exit code either: the README promises doctor exits with the
// verify hook's own code only when that hook actually ran and failed, and 1
// for any other failure, a predicted one included (see doctorExitCode).
//
// Every [commands] entry follows the hooks, in file order, as
// "<name> command file" (#94): hooks.ResolveCommand shares `berth run`'s own
// resolution the same way, and a broken one reads and counts identically.
//
// This runs whether or not this worktree is registered: unlike the verify
// hook's own run below, resolving a hook needs no environment built from a
// registration, so it is the only doctor check that can catch a typo in
// prepare, seed, backup or restore — or in verify, before a first `up` ever
// registers this worktree — matching the motivation for this check (#105).
func hookResolutionChecks(loaded *config.Loaded, environ []string) []Check {
	var checks []Check
	for _, stage := range loaded.Config.Hooks.Configured() {
		resolution, _, err := hooks.ResolveHook(loaded.Config.Hooks, stage, loaded.Root, environ)
		checks = append(checks, resolutionCheck(string(stage)+" hook file", resolution, err))
	}
	for _, entry := range loaded.Config.Commands {
		resolution, err := hooks.ResolveCommand(entry.Name, entry.Command, loaded.Root, environ)
		checks = append(checks, resolutionCheck(entry.Name+" command file", resolution, err))
	}
	return checks
}

// resolutionCheck is the line for one hook or command file: ok naming what it
// resolves to, a fail naming the exit code a real run would give when it
// would not start, or the failure itself for anything else (an empty
// command).
func resolutionCheck(subject string, resolution hooks.Resolution, err error) Check {
	if err == nil {
		return Check{Status: StatusOK, Subject: subject, Detail: fmt.Sprintf("resolves to %s", resolution.Command)}
	}
	var startErr *run.StartError
	if errors.As(err, &startErr) {
		return Check{Status: StatusFail, Subject: subject, Detail: fmt.Sprintf(
			"would not start (exit %d): `%s` %s", startErr.ExitCode, resolution.Command, startErr.Detail)}
	}
	return failureCheck(subject, err)
}

// checkoutChecks is this worktree, registered or not, and its verify hook.
// Each step runs on its own: a checkout Git cannot identify still has a
// config worth reporting, and its verify hook still deserves a line saying
// why it did not run — except a cancellation identifying the worktree, or
// during the verify hook itself, either of which ends the whole command
// (see guarded and checkoutChecks' two direct callers of reg.Current/RunHook below).
func checkoutChecks(ctx context.Context, opts doctorOptions, loaded *config.Loaded) ([]Check, error) {
	checks := []Check{checkoutFileCheck(loaded)}

	var reg checkoutRegistry
	var regErr error
	if opts.RegistryFor != nil {
		reg, regErr = opts.RegistryFor(loaded)
	} else {
		reg = registry.New(registry.Options{
			ControlHome: opts.ControlHome, Root: loaded.Root, Domain: loaded.Config.Project.Domain,
			LabelNamespace: loaded.Config.LabelNamespace(), Runner: opts.Runner, Logger: opts.Logger,
			CommandName: opts.CommandName, Invocation: opts.Invocation, UserHome: opts.UserHome,
		})
	}

	identified := true
	var registration *registry.WorktreeRegistration
	if regErr != nil {
		identified = false
		checks = append(checks, failureCheck("this worktree", regErr))
	} else {
		var err error
		registration, err = reg.Current(ctx)
		if err != nil {
			var cancelled *clierr.Cancelled
			if errors.As(err, &cancelled) {
				return nil, err
			}
			identified = false
			checks = append(checks, failureCheck("this worktree", err))
		} else if registration != nil {
			checks = append(checks, Check{Status: StatusOK, Subject: "this worktree",
				Detail: "registered as " + layout.WorktreeHost(registration.Slug, registration.Domain)})
		} else {
			checks = append(checks, Check{Status: StatusSkip, Subject: "this worktree",
				Detail: fmt.Sprintf("not registered; '%s up' registers it", opts.CommandName)})
		}
	}

	if regErr == nil {
		missing, err := guarded("missing checkouts", func() ([]Check, error) {
			candidates, err := reg.PruneCandidates(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]Check, len(candidates))
			for i, record := range candidates {
				out[i] = Check{Status: StatusWarn, Subject: layout.WorktreeHost(record.Slug, record.Domain), Detail: fmt.Sprintf(
					"registered, but Git no longer lists its checkout. Remove what it left with '%s worktree remove %s'.",
					opts.CommandName, record.WorktreeID)}
			}
			return out, nil
		})
		if err != nil {
			return nil, err
		}
		checks = append(checks, missing...)
	}

	checks = append(checks, hookResolutionChecks(loaded, opts.Environ)...)

	command := loaded.Config.Hooks.Verify
	switch {
	case command == nil:
		checks = append(checks, Check{Status: StatusSkip, Subject: "verify hook", Detail: "none configured"})
	case !identified:
		checks = append(checks, Check{Status: StatusSkip, Subject: "verify hook", Detail: "needs this worktree to be identified"})
	case registration == nil:
		checks = append(checks, Check{Status: StatusSkip, Subject: "verify hook",
			Detail: fmt.Sprintf("needs a registered worktree; run '%s up' first", opts.CommandName)})
	default:
		ref := worktree.Ref{RepositoryID: registration.RepositoryID, WorktreeID: registration.WorktreeID, Slug: registration.Slug}
		// No routes when opts.RouterErr != nil: opts.RouterPort is then
		// unresolved (its zero value), and routing on port 0 would hand the
		// hook a wrong :0 URL rather than no URL at all.
		var verifyRoutes []worktree.ServiceRoute
		if opts.RouterErr == nil {
			verifyRoutes = worktree.Routes(loaded.Config.Service, ref.Slug, registration.Domain, opts.RouterPort.Port)
		}
		// Its output goes to stderr: stdout is this report.
		_, hookErr := hooks.RunHook(ctx, config.Verify, hooks.HookContext{
			Source: worktree.EnvironmentSource{
				ProjectName: loaded.Config.Project.Name, Domain: registration.Domain,
				ControlHome: opts.ControlHome, ConfigFile: loaded.File, ProjectRoot: loaded.Root,
			},
			Ref: ref, Hooks: loaded.Config.Hooks, Logger: opts.Logger, Runner: opts.Runner, Environ: opts.Environ,
			Routes: verifyRoutes, // BERTH_URL, BERTH_SERVICE_URL_<NAME> (#28)
		}, hooks.RunOptions{Output: hooks.Stderr})
		if hookErr != nil {
			var cancelled *clierr.Cancelled
			if errors.As(hookErr, &cancelled) {
				return nil, hookErr
			}
			var hookFailure *clierr.Error
			if errors.As(hookErr, &hookFailure) {
				checks = append(checks, Check{Status: StatusFail, Subject: "verify hook", Detail: hookFailure.Message, ExitCode: &hookFailure.ExitCode})
			} else {
				checks = append(checks, failureCheck("verify hook", hookErr))
			}
		} else {
			checks = append(checks, Check{Status: StatusOK, Subject: "verify hook", Detail: fmt.Sprintf("%s exited 0", *command)})
		}
	}
	return checks, nil
}

// thisCheckoutSection is the "This checkout" section: a skip when no
// berth.toml was found, a failed line naming the load error when one was
// found but would not load, otherwise checkoutChecks over the loaded config.
func thisCheckoutSection(ctx context.Context, opts doctorOptions) ([]Check, error) {
	switch opts.Checkout.kind {
	case "none":
		return []Check{{Status: StatusSkip, Subject: brand.ConfigFile, Detail: fmt.Sprintf(
			"none in %s or above; run '%s doctor' in a checkout to check it and its verify hook", opts.Checkout.searchedFrom, opts.CommandName)}}, nil
	case "unloadable":
		return []Check{{Status: StatusFail, Subject: brand.ConfigFile, Detail: opts.Checkout.unloadable.Error()}}, nil
	default:
		return guarded("this worktree", func() ([]Check, error) { return checkoutChecks(ctx, opts, opts.Checkout.loaded) })
	}
}

/* -------------------------------------------------------------- report ---- */

// doctorExitCode is 0 when nothing failed; the verify hook's own code when
// it ran and failed, as the hook contract and the README promise; 1 for any
// other failure. Only the verify hook's own run (checkoutChecks' RunHook
// call) ever sets a Check's ExitCode today — a hook-file resolution failure
// (hookResolutionChecks) does not, on purpose: its predicted 127 or 126 is
// what the runner would give if this hook were the one actually run, not
// what doctor itself exits with, so it stays in the line's own Detail and
// never overrides or races the verify hook's real, observed code here.
func doctorExitCode(sections []Section) int {
	var failures []Check
	for _, section := range sections {
		for _, check := range section.Checks {
			if check.Status == StatusFail {
				failures = append(failures, check)
			}
		}
	}
	if len(failures) == 0 {
		return 0
	}
	for _, failure := range failures {
		if failure.ExitCode != nil && *failure.ExitCode != 0 {
			return *failure.ExitCode
		}
	}
	return 1
}

// tally counts every check across sections by status, always returning all
// four statuses, zero-filled, even when none occurred.
func tally(sections []Section) map[CheckStatus]int {
	counts := map[CheckStatus]int{StatusOK: 0, StatusWarn: 0, StatusFail: 0, StatusSkip: 0}
	for _, section := range sections {
		for _, check := range section.Checks {
			counts[check.Status]++
		}
	}
	return counts
}

// formatDoctorReport is the report, one section per area and one aligned
// line per check, every borrowed string made printable.
func formatDoctorReport(sections []Section) []string {
	var lines []string
	for _, section := range sections {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, text.Printable(section.Title))
		subjects := make([]string, len(section.Checks))
		width := 0
		for i, check := range section.Checks {
			subjects[i] = text.Printable(check.Subject)
			if l := text.JSLen(subjects[i]); l > width {
				width = l
			}
		}
		for i, check := range section.Checks {
			lines = append(lines, fmt.Sprintf("  %s  %s  %s",
				text.PadEnd(string(check.Status), 4), text.PadEnd(subjects[i], width), text.Printable(check.Detail)))
		}
	}
	return lines
}

type doctorSummaryLine struct {
	level   string // "error" or "info"
	message string
}

// doctorSummary is the one line on stderr that says how it went, and how
// loudly: a failure is an error line, as every failing verb's last word is.
func doctorSummary(sections []Section) doctorSummaryLine {
	counts := tally(sections)
	if counts[StatusFail] > 0 {
		return doctorSummaryLine{level: "error", message: fmt.Sprintf("%d check(s) failed.", counts[StatusFail])}
	}
	var notes []string
	if counts[StatusWarn] > 0 {
		notes = append(notes, fmt.Sprintf("%d warning(s)", counts[StatusWarn]))
	}
	if counts[StatusSkip] > 0 {
		notes = append(notes, fmt.Sprintf("%d skipped", counts[StatusSkip]))
	}
	message := "No checks failed."
	if len(notes) > 0 {
		message = fmt.Sprintf("No checks failed (%s).", strings.Join(notes, ", "))
	}
	return doctorSummaryLine{level: "info", message: message}
}

/* --------------------------------------------------------------- entry ---- */

// doctorOptions is every external dependency doctorSections needs,
// injectable so tests can control each one via fakes (see
// doctor_internal_test.go).
type doctorOptions struct {
	CommandName string
	ControlHome string
	// Home is the checked berth root (BERTH_HOME, #80): reported alongside
	// ControlHome and RouterStateDir in the Registry section, so someone
	// who set only BERTH_CONTROL_HOME can still see where the rest of it
	// (the router's own state, and a future settings.toml, #79) lives.
	Home string
	// RouterStateDir is layout.RouterStateFrom(Home).StateDir, reported
	// even when Router/RouterErr says the router itself could not be
	// described (a bad port does not change where its state directory is).
	RouterStateDir string
	// RouterPort is the router port resolved for this berth root and where
	// it came from (#79): BERTH_ROUTER_PORT, settings.toml, or the default.
	// Reported as its own line, against what the running router publishes.
	RouterPort settings.PortResolution
	// UserHome is the caller's own resolved home directory, handed to
	// controlHomeCheck's own files.AssertAdoptableControlHome.
	UserHome string
	Runner   run.Runner
	Logger   logger.Logger
	// Router is the shared router; nil when RouterErr says why one could
	// not even be described (a bad port in the environment).
	Router    doctorRouterSource
	RouterErr error
	// RouterState is the shared router's own on-disk layout, for
	// routeChecks — reading its dynamic-configuration fragments needs no
	// running router and no lock, matching doctor's own "reads, never
	// writes" invariant.
	RouterState layout.RouterLayout
	// RouterLockScope is the shared router's own lock, for the Locks
	// section; nil skips that line.
	RouterLockScope *lock.Scope
	Checkout        checkoutSource
	// RegistryFor overrides the registry checkoutChecks builds from a
	// loaded config; nil builds the real one. Its own error — a registry
	// that could not even be built, not one that answered with an error —
	// is reported as "this worktree" failing to identify.
	RegistryFor func(loaded *config.Loaded) (checkoutRegistry, error)
	Invocation  string
	Environ     []string
	// DockerProbeTimeout overrides dockerProbeTimeout; zero keeps the default.
	DockerProbeTimeout time.Duration
}

// dockerTimeout is o.DockerProbeTimeout when set, otherwise
// dockerProbeTimeout.
func (o doctorOptions) dockerTimeout() time.Duration {
	if o.DockerProbeTimeout > 0 {
		return o.DockerProbeTimeout
	}
	return dockerProbeTimeout
}

// doctorSections builds every section, in order, from opts alone. A
// cancellation anywhere in it — Git, Docker, the router, identifying this
// worktree, or its verify hook — aborts immediately, before anything is
// printed, by propagating out of guarded()'s calls.
func doctorSections(ctx context.Context, opts doctorOptions) ([]Section, error) {
	prerequisiteChecks, state, err := prerequisitesSection(ctx, opts)
	if err != nil {
		return nil, err
	}
	registryChecks, err := registrySection(opts)
	if err != nil {
		return nil, err
	}
	locksChecks, err := guarded("locks", func() ([]Check, error) { return lockChecks(opts.ControlHome, opts.RouterLockScope, opts.CommandName) })
	if err != nil {
		return nil, err
	}
	routerChecksResult, err := routerSection(ctx, opts, state)
	if err != nil {
		return nil, err
	}
	checkoutChecksResult, err := thisCheckoutSection(ctx, opts)
	if err != nil {
		return nil, err
	}
	return []Section{
		{Title: "Prerequisites", Checks: prerequisiteChecks},
		{Title: "Registry", Checks: registryChecks},
		{Title: "Locks", Checks: locksChecks},
		{Title: "Router", Checks: routerChecksResult},
		{Title: "This checkout", Checks: checkoutChecksResult},
	}, nil
}

// doctorOptionsFor is doctorOptions built from env and an already-checked
// berth root (home, userHome — commands.checkedHome), against svc as the
// shared router and state as its own resolved layout — or svcErr, why one
// could not even be described, when svc is nil. buildDoctorOptions calls
// this with the default shared router routerServiceFrom resolves from
// home; the real-daemon tests' own DoctorSections (export_integration_test.go)
// call it with a router of their own name, port and state instead, the same
// seam SharedRouter gives `up`, so the two can never drift apart.
func doctorOptionsFor(env *Env, home, userHome string, svc *router.Router, state layout.RouterLayout, resolved settings.PortResolution, svcErr error) (doctorOptions, error) {
	checkout, err := findCheckout(env.ConfigPath, env.Cwd)
	if err != nil {
		return doctorOptions{}, err
	}
	controlHome, err := layout.ControlHomeFrom(home, env.Getenv, "")
	if err != nil {
		return doctorOptions{}, err
	}
	opts := doctorOptions{
		CommandName: env.CommandName, ControlHome: controlHome, Home: home,
		RouterStateDir: layout.RouterStateDirFrom(home), UserHome: userHome,
		Runner: env.Runner, Logger: env.Logger,
		Checkout: checkout, Invocation: env.Invocation, Environ: env.Environ,
	}
	if svcErr != nil {
		opts.RouterErr = svcErr
		return opts, nil
	}
	if svc == nil {
		// Only a test seam (DoctorSections in export_integration_test.go)
		// can reach this: buildDoctorOptions always pairs a nil svc with
		// the error routerServiceFrom returned, caught above.
		return doctorOptions{}, fmt.Errorf("doctorOptionsFor: no router and no error describing why")
	}
	opts.Router = realRouterSource{svc: svc}
	// RouterState is the shared router's own on-disk layout, for
	// routeChecks — reading its dynamic-configuration fragments needs no
	// running router and no lock, matching doctor's own "reads, never
	// writes" invariant (#55).
	opts.RouterState = state
	// The resolution routerServiceFrom made for svc's own port (#79),
	// handed through rather than made again, so the line doctor prints can
	// never disagree with the port the router it inspects was built for.
	opts.RouterPort = resolved
	routerScope := routerLockScope(state, svc.ProjectName)
	opts.RouterLockScope = &routerScope
	return opts, nil
}

// buildDoctorOptions is doctorOptions built from a real Env and an
// already-checked berth root (Doctor's own single checkedHome call):
// findCheckout is resolved first (env.ConfigPath, or the nearest berth.toml
// above env.Cwd), then the shared router — or why one could not even be
// described.
func buildDoctorOptions(env *Env, home, userHome string) (doctorOptions, error) {
	svc, state, resolved, err := routerServiceFrom(env, home)
	return doctorOptionsFor(env, home, userHome, svc, state, resolved, err)
}

// Doctor is `doctor`. Its report goes to stdout, one line per check; its
// summary goes to stderr. The returned exit code is 0 when nothing failed.
func Doctor(ctx context.Context, env *Env) (int, error) {
	home, userHome, err := checkedHome(env.Getenv)
	if err != nil {
		return 0, err
	}
	opts, err := buildDoctorOptions(env, home, userHome)
	if err != nil {
		return 0, err
	}
	sections, err := doctorSections(ctx, opts)
	if err != nil {
		return 0, err
	}

	for _, line := range formatDoctorReport(sections) {
		env.Logger.Output(text.Printable(line))
	}
	summary := doctorSummary(sections)
	if summary.level == "error" {
		env.Logger.Error(summary.message)
	} else {
		env.Logger.Info(summary.message)
	}
	return doctorExitCode(sections), nil
}
