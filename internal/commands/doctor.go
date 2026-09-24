package commands

// `doctor`, ported from src/commands/doctor.ts onto the flock lock layer.
// Read-only, start to finish: no lock is taken, nothing is written, and
// nothing is repaired. Every check ends as a line in the report — a check
// that fails to even run is reported as failed rather than ending the
// command, since the point of a diagnosis is to see everything that is
// wrong at once (see guarded) — except a cancellation, which ends the
// command the way it does in TypeScript (see guarded and Doctor).
//
// doctorSections mirrors src/commands/doctor.ts's own doctor(): every
// external dependency arrives through doctorOptions, so it can be tested the
// way the TypeScript suite's own DoctorRunner and RouterSource fakes test
// doctor() — see doctor_internal_test.go. Doctor itself is the thin part
// that builds doctorOptions from a real Env, prints the report, and turns
// its exit code into what the CLI reports.
//
// The Node.js version check has no Go counterpart and is dropped outright
// (divergence ledger row 17): there is no Go runtime version for a Go build
// to be running under. The Locks section, and the wording a stuck holder's
// line uses, are Go's own in the specific, narrow ways row 18 records: see
// internal/lock's package comment and docs/port/divergences.md rows 3, 18, 20
// and 22 (20 being run.Runner.Which's own inability to fail the way
// TypeScript's does; 22 being the Registry section's own line for a control
// home a TypeScript build last used, planned from the start of WP6).

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

	"github.com/sofired/berth/internal/backend"
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

func failureCheck(subject string, err error) Check {
	return Check{Status: StatusFail, Subject: subject, Detail: err.Error()}
}

// guarded runs a group of checks; one that fails to even run becomes a
// failed line instead of ending the report — except a cancellation, which
// this re-raises exactly as src/commands/doctor.ts's own guarded() does, so
// it ends the whole command instead of becoming a line.
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

func atoiOrZero(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

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
// is its answer, not a propagated error — but only that: src/commands/doctor.ts:140-155
// catches DockerUnavailableError alone and rethrows everything else,
// cancellation included, so a Ctrl-C here ends the command rather than
// becoming a line. The successful result is returned for the caller to
// interpret further; nil means the returned Check is the answer.
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
// into one failed "Docker" line, matching src/commands/doctor.ts's own
// top-level try/catch around dockerChecks (doctor.ts:496-501).
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

func controlHomeCheck(controlHome string) (Check, error) {
	exists, err := files.PathExists(controlHome)
	if err != nil {
		return Check{}, err
	}
	if !exists {
		return Check{Status: StatusOK, Subject: "control home", Detail: fmt.Sprintf("%s (not created yet; the first up creates it)", controlHome)}, nil
	}
	if err := files.AssertAdoptableControlHome(controlHome); err != nil {
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

func registrySection(controlHome, commandName string) ([]Check, error) {
	checks, err := guarded("control home", func() ([]Check, error) {
		check, err := controlHomeCheck(controlHome)
		if err != nil {
			return nil, err
		}
		out := []Check{check}
		legacy, err := files.LegacyControlHome(controlHome)
		if err != nil {
			return nil, err
		}
		if legacy {
			out = append(out, Check{Status: StatusOK, Subject: "control home", Detail: fmt.Sprintf(
				"was last used by a TypeScript build of %s; the next command here upgrades it", brand.Name)})
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	registrations, err := guarded("registrations", func() ([]Check, error) {
		scan, err := registry.ScanRegistrations(controlHome)
		if err != nil {
			return nil, err
		}
		return registryChecks(scan, commandName), nil
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
// gone, which recovery alone cannot get past. A worktree lock needs none:
// `worktree unlock --holder-unverifiable` is that escape hatch. The
// registry and router locks have no `unlock` of their own (#21, until
// WP8), so lockChecks passes each of them the one that exists today:
// removing the sidecar record by hand once its holder, and everything it
// started, is confirmed gone.
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
			detail += fmt.Sprintf(". If that also refuses it as unverifiable, make sure the holder, and everything it started, "+
				"is gone, then remove it by hand: `rm %s`", text.ShellQuote(handRemoval))
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
// around, unlike src/operation-lock.ts's worktreeRecoveryLockTarget: the
// flock design has no such file (docs/port/divergences.md row 3).
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
// internal/lock's package comment), so it needs no advice beyond that,
// unlike TypeScript's own heartbeat design — except the registry and router
// locks' hand-removal fallback (see lockCheck).
func lockChecks(controlHome string, routerLockScope *lock.Scope) ([]Check, error) {
	scan, err := registry.ScanRegistrations(controlHome)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]registry.RegisteredWorktree, len(scan.Records))
	for _, record := range scan.Records {
		byKey[record.RepositoryID+"/"+record.WorktreeID] = record
	}

	// Subjects match heldLocks in src/commands/doctor.ts exactly: "registry"
	// and "router" (short — not their longer lock.Scope.Description), a
	// worktree's own hostname (or "worktree <uuid>" before it has one) for
	// its operation lock, and that plus " (development session)" for its
	// development lock.
	var checks []Check
	registryScope := lock.RegistryScope(controlHome)
	if check, ok := lockCheck("registry", registryScope, registryScope.Target+".holder"); ok {
		checks = append(checks, check)
	}
	if routerLockScope != nil {
		if check, ok := lockCheck("router", *routerLockScope, routerLockScope.Target+".holder"); ok {
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
		// Development, then operation — worktreeLocks' own order in
		// src/operation-lock.ts, which heldLocks iterates unchanged.
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

// doctorRouterSource is RouterSource from src/commands/doctor.ts: what the
// Router section needs of the shared router. A real router.Router satisfies
// it through realRouterSource; tests supply their own, the way the
// TypeScript suite's routerSource() fake does.
type doctorRouterSource interface {
	Status(ctx context.Context) (router.RouterStatus, error)
	Network() string
}

// realRouterSource adapts a real *router.Router — whose Network is a plain
// field, not a method — to doctorRouterSource.
type realRouterSource struct{ svc *router.Router }

func (r realRouterSource) Status(ctx context.Context) (router.RouterStatus, error) {
	return r.svc.Status(ctx)
}
func (r realRouterSource) Network() string { return r.svc.Network }

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

type routedContainer struct {
	Name   string
	Labels map[string]string
}

// parseRoutedContainers parses `docker inspect --format
// '{{.Name}}\t{{json .Config.Labels}}'`, one container per line.
func parseRoutedContainers(stdout string) []routedContainer {
	var containers []routedContainer
	for _, line := range strings.Split(stdout, "\n") {
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			continue
		}
		var labels map[string]any
		if err := json.Unmarshal([]byte(line[tab+1:]), &labels); err != nil || labels == nil {
			continue
		}
		strLabels := make(map[string]string, len(labels))
		for key, value := range labels {
			if s, ok := value.(string); ok {
				strLabels[key] = s
			}
		}
		containers = append(containers, routedContainer{Name: strings.TrimPrefix(line[:tab], "/"), Labels: strLabels})
	}
	return containers
}

var routerRulePattern = regexp.MustCompile(`^traefik\.http\.routers\.([^.]+)\.rule$`)

// routeConflicts is every rule more than one claimant routes, each with the
// containers behind them. Replicas of one Compose service are one claimant:
// they share a router name and the project and service Compose stamped on them.
func routeConflicts(containers []routedContainer) map[string][]string {
	type claim struct {
		claimants  map[string]bool
		containers map[string]bool
	}
	byRule := map[string]*claim{}
	for _, container := range containers {
		project, service := container.Labels[backend.ComposeProjectLabel], container.Labels[backend.ComposeServiceLabel]
		for key, rule := range container.Labels {
			m := routerRulePattern.FindStringSubmatch(key)
			if m == nil {
				continue
			}
			entry, ok := byRule[rule]
			if !ok {
				entry = &claim{claimants: map[string]bool{}, containers: map[string]bool{}}
				byRule[rule] = entry
			}
			claimKey, _ := json.Marshal([]string{m[1], project, service})
			entry.claimants[string(claimKey)] = true
			entry.containers[container.Name] = true
		}
	}
	conflicts := map[string][]string{}
	for rule, entry := range byRule {
		if len(entry.claimants) <= 1 {
			continue
		}
		names := make([]string, 0, len(entry.containers))
		for name := range entry.containers {
			names = append(names, name)
		}
		sort.Strings(names)
		conflicts[rule] = names
	}
	return conflicts
}

// routeChecks is every running container the router would route, read from
// Docker rather than from any registry.
// routeChecks is every running container the router would route, read from
// Docker rather than from any registry. Unlike askDocker's own calls, the
// inspect step below is not wrapped in its own Docker-unavailable catch,
// matching src/commands/doctor.ts:376-406 exactly: routerSection's own
// guarded("routes", ...) is what turns whatever this returns into a line —
// or, for a cancellation, lets it end the command — the same way TypeScript
// leaves that one call for its own caller's guarded() to catch.
func routeChecks(ctx context.Context, runner run.Runner, network string, timeout time.Duration) ([]Check, error) {
	listedCheck, listed, err := askDocker(ctx, runner, "routes", []string{"ps", "-q", "--filter", "label=" + router.RoutedByLabel + "=" + network}, timeout)
	if err != nil {
		return nil, err
	}
	if listed == nil {
		return []Check{listedCheck}, nil
	}
	ids := strings.Fields(strings.TrimSpace(listed.Stdout))
	if len(ids) == 0 {
		return []Check{{Status: StatusOK, Subject: "routes", Detail: "no running container is routed"}}, nil
	}

	args := append([]string{"inspect", "--format", "{{.Name}}\t{{json .Config.Labels}}"}, ids...)
	inspected, err := docker.Run(ctx, runner, args, run.Spec{AllowFailure: true, Timeout: timeout})
	if err != nil {
		return nil, err
	}
	if inspected.OutputOverflow {
		return []Check{{Status: StatusFail, Subject: "routes", Detail: "`docker inspect` produced more output than can be read, so routes were not checked"}}, nil
	}
	if inspected.TimedOut {
		return []Check{{Status: StatusFail, Subject: "routes", Detail: fmt.Sprintf(
			"`docker inspect` did not answer within %d s, so routes were not checked", int(timeout.Seconds()))}}, nil
	}
	containers := parseRoutedContainers(inspected.Stdout)
	if len(containers) == 0 && inspected.ExitCode != 0 {
		return []Check{{Status: StatusFail, Subject: "routes", Detail: text.OneLine(
			docker.Said(inspected.Stderr, fmt.Sprintf("`docker inspect` exited %d", inspected.ExitCode)), 0)}}, nil
	}

	conflicts := routeConflicts(containers)
	if len(conflicts) == 0 {
		return []Check{{Status: StatusOK, Subject: "routes", Detail: fmt.Sprintf(
			"%d routed container(s); no two routers share a rule", len(containers))}}, nil
	}
	rules := make([]string, 0, len(conflicts))
	for rule := range conflicts {
		rules = append(rules, rule)
	}
	sort.Strings(rules)
	checks := make([]Check, len(rules))
	for i, rule := range rules {
		checks[i] = Check{Status: StatusFail, Subject: "routes", Detail: fmt.Sprintf(
			"%s is claimed by %s; requests for it may reach any of them",
			text.OneLine(rule, 200), text.OneLine(strings.Join(conflicts[rule], ", "), 200))}
	}
	return checks, nil
}

func routerSection(ctx context.Context, opts doctorOptions, state dockerState) ([]Check, error) {
	if opts.RouterErr != nil {
		return []Check{{Status: StatusFail, Subject: "router", Detail: opts.RouterErr.Error()}}, nil
	}
	if !state.engineAnswered {
		return []Check{{Status: StatusSkip, Subject: "router", Detail: "needs Docker"}}, nil
	}
	var checks []Check
	if state.composeAnswered {
		routerChecks, err := guarded("router", func() ([]Check, error) {
			status, err := opts.Router.Status(ctx)
			if err != nil {
				return nil, err
			}
			return routerStatusChecks(status), nil
		})
		if err != nil {
			return nil, err
		}
		checks = append(checks, routerChecks...)
	} else {
		checks = append(checks, Check{Status: StatusSkip, Subject: "router", Detail: "needs Docker Compose"})
	}
	routes, err := guarded("routes", func() ([]Check, error) {
		return routeChecks(ctx, opts.Runner, opts.Router.Network(), opts.dockerTimeout())
	})
	if err != nil {
		return nil, err
	}
	return append(checks, routes...), nil
}

/* ------------------------------------------------------------ checkout ---- */

// checkoutSource is CheckoutSource from src/commands/doctor.ts: where the
// checkout section comes from — no config at all, one that does not load, or
// one that does.
type checkoutSource struct {
	kind         string // "none", "unloadable", or "loaded"
	searchedFrom string
	unloadable   error
	loaded       *config.Loaded
}

func checkoutNone(searchedFrom string) checkoutSource {
	return checkoutSource{kind: "none", searchedFrom: searchedFrom}
}
func checkoutUnloadable(err error) checkoutSource {
	return checkoutSource{kind: "unloadable", unloadable: err}
}
func checkoutIsLoaded(loaded *config.Loaded) checkoutSource {
	return checkoutSource{kind: "loaded", loaded: loaded}
}

// checkoutRegistry is CheckoutRegistry from src/commands/doctor.ts:
// Pick<WorktreeRegistry, 'current' | 'pruneCandidates'>. *registry.Registry
// satisfies it without declaring so.
type checkoutRegistry interface {
	Current(ctx context.Context) (*registry.WorktreeRegistration, error)
	PruneCandidates(ctx context.Context) ([]registry.RegisteredWorktree, error)
}

// findCheckout is findCheckout from src/commands/doctor.ts: the config the
// other commands would use here, without failing when there is none.
// explicit is --config; "" means the nearest berth.toml above cwd.
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

// checkoutChecks is this worktree, registered or not, and its verify hook.
// Each step runs on its own: a checkout Git cannot identify still has a
// config worth reporting, and its verify hook still deserves a line saying
// why it did not run — except a cancellation identifying the worktree, or
// during the verify hook itself, either of which ends the whole command
// (see guarded and checkoutChecks' two direct callers of reg.Current/RunHook below).
func checkoutChecks(ctx context.Context, opts doctorOptions, loaded *config.Loaded) ([]Check, error) {
	checks := []Check{{Status: StatusOK, Subject: brand.ConfigFile, Detail: loaded.File}}

	var reg checkoutRegistry
	var regErr error
	if opts.RegistryFor != nil {
		reg, regErr = opts.RegistryFor(loaded)
	} else {
		reg = registry.New(registry.Options{
			ControlHome: opts.ControlHome, Root: loaded.Root, Domain: loaded.Config.Project.Domain,
			LabelNamespace: loaded.Config.LabelNamespace(), Runner: opts.Runner, Logger: opts.Logger,
			CommandName: opts.CommandName, Invocation: opts.Invocation,
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
		// Its output goes to stderr: stdout is this report.
		_, hookErr := hooks.RunHook(ctx, config.Verify, hooks.HookContext{
			Source: worktree.EnvironmentSource{
				ProjectName: loaded.Config.Project.Name, Domain: registration.Domain,
				ControlHome: opts.ControlHome, ConfigFile: loaded.File, ProjectRoot: loaded.Root,
			},
			Ref: ref, Hooks: loaded.Config.Hooks, Logger: opts.Logger, Runner: opts.Runner, Environ: opts.Environ,
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
// it failed, as the hook contract promises; otherwise 1.
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

// doctorOptions is DoctorOptions from src/commands/doctor.ts: every external
// dependency doctorSections needs, injectable so tests can control each one
// exactly the way the TypeScript suite's own DoctorRunner and RouterSource
// fakes do (see doctor_internal_test.go).
type doctorOptions struct {
	CommandName string
	ControlHome string
	Runner      run.Runner
	Logger      logger.Logger
	// Router is the shared router; nil when RouterErr says why one could
	// not even be described (a bad port in the environment).
	Router    doctorRouterSource
	RouterErr error
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

func (o doctorOptions) dockerTimeout() time.Duration {
	if o.DockerProbeTimeout > 0 {
		return o.DockerProbeTimeout
	}
	return dockerProbeTimeout
}

// doctorSections is doctor from src/commands/doctor.ts: every section, in
// its order, built from opts alone. A cancellation anywhere in it — Git,
// Docker, the router, identifying this worktree, or its verify hook —
// aborts immediately, before anything is printed, the way TypeScript's own
// doctor() lets one propagate out of its guarded() calls.
func doctorSections(ctx context.Context, opts doctorOptions) ([]Section, error) {
	prerequisiteChecks, state, err := prerequisitesSection(ctx, opts)
	if err != nil {
		return nil, err
	}
	registryChecks, err := registrySection(opts.ControlHome, opts.CommandName)
	if err != nil {
		return nil, err
	}
	locksChecks, err := guarded("locks", func() ([]Check, error) { return lockChecks(opts.ControlHome, opts.RouterLockScope) })
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

// buildDoctorOptions is doctorOptions built from a real Env: findCheckout is
// resolved first (env.ConfigPath, or the nearest berth.toml above env.Cwd),
// then the shared router — or why one could not even be described.
func buildDoctorOptions(env *Env, controlHome string) (doctorOptions, error) {
	checkout, err := findCheckout(env.ConfigPath, env.Cwd)
	if err != nil {
		return doctorOptions{}, err
	}
	opts := doctorOptions{
		CommandName: env.CommandName, ControlHome: controlHome, Runner: env.Runner, Logger: env.Logger,
		Checkout: checkout, Invocation: env.Invocation, Environ: env.Environ,
	}
	svc, _, err := newRouterService(env)
	if err != nil {
		opts.RouterErr = err
		return opts, nil
	}
	opts.Router = realRouterSource{svc: svc}
	if routerScope, err := lock.RouterScope(env.Getenv, svc.ProjectName); err == nil {
		opts.RouterLockScope = &routerScope
	}
	return opts, nil
}

// Doctor is `doctor`. Its report goes to stdout, one line per check; its
// summary goes to stderr. The returned exit code is 0 when nothing failed.
func Doctor(ctx context.Context, env *Env) (int, error) {
	controlHome, err := layout.ResolveControlHome(env.Getenv, "")
	if err != nil {
		return 0, err
	}
	opts, err := buildDoctorOptions(env, controlHome)
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
