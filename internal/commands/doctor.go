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
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/docker"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/hooks"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/ports"
	"github.com/sofired/treerig/internal/procid"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/router"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/settings"
	"github.com/sofired/treerig/internal/stateformat"
	"github.com/sofired/treerig/internal/text"
	"github.com/sofired/treerig/internal/worktree"
)

// gitMinimum is the oldest Git `git worktree list --porcelain -z` needs.
const gitMinimum = "2.36"

// composeMinimum is the oldest Compose docs/getting-started.md promises to work with.
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
	// ID is a short dotted slug naming the kind of check, the same in every
	// run whatever the outcome; it is what `doctor --json` consumers key on,
	// where Subject is display text. An ID names the kind of thing checked and
	// starts with its section's ID; it may repeat, once per item, and then
	// Instance says which item.
	ID string
	// Instance is the durable key of the item a per-item check is about (a
	// worktree ID, a host, a rule, a record's file name); empty for a check
	// that is not per item.
	Instance string
	Status   CheckStatus
	Subject  string
	Detail   string
	// ExitCode is set for a failure that carries its own exit code — the
	// verify hook's — making it the command's.
	ExitCode *int
}

// Section is one area of the doctor report.
type Section struct {
	// ID is the section's stable slug; Title is display text.
	ID     string
	Title  string
	Checks []Check
}

// failureCheck turns an error a check could not even run past into a failed
// line for subject, carrying err's own message as the detail.
func failureCheck(id, subject string, err error) Check {
	return Check{ID: id, Status: StatusFail, Subject: subject, Detail: err.Error()}
}

// recordInstance is the instance of an unreadable registry record: the
// repository directory and the file, "<repository>/<file>", which is unique
// across stores where the file name alone (a worktree ID) is not. A path
// without a repository directory above it is just its file name.
func recordInstance(file string) string {
	repository := filepath.Base(filepath.Dir(filepath.Dir(file)))
	if repository == "." || repository == string(filepath.Separator) {
		return filepath.Base(file)
	}
	return path.Join(repository, filepath.Base(file))
}

// routeClaimPattern is the rule treerig writes for a route (backend.RouteRule):
// a host, and optionally a path prefix.
var routeClaimPattern = regexp.MustCompile("^Host\\(`([^`]*)`\\)(?: && PathPrefix\\(`([^`]*)`\\))?$")

// ruleClaim is what a routing rule claims in the form users know, "<host>"
// or "<host><path>". A rule treerig did not write is its own claim, verbatim.
func ruleClaim(rule string) string {
	match := routeClaimPattern.FindStringSubmatch(rule)
	if match == nil {
		return rule
	}
	return match[1] + match[2]
}

// guarded runs a group of checks; one that fails to even run becomes a
// failed line instead of ending the report — except a cancellation, which
// this re-raises, so it ends the whole command instead of becoming a line.
func guarded(id, subject string, run func() ([]Check, error)) ([]Check, error) {
	checks, err := run()
	if err != nil {
		var cancelled *clierr.Cancelled
		if errors.As(err, &cancelled) {
			return nil, err
		}
		return []Check{failureCheck(id, subject, err)}, nil
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
func versionCheck(id, subject, output, minimum string) Check {
	version, ok := readVersion(output)
	floor, floorOK := readVersion(minimum)
	if !ok || !floorOK {
		return Check{ID: id, Status: StatusFail, Subject: subject, Detail: fmt.Sprintf("could not read a version from %q", text.OneLine(output, 80))}
	}
	if atLeast(version.parts, floor.parts) {
		return Check{ID: id, Status: StatusOK, Subject: subject, Detail: version.text}
	}
	return Check{ID: id, Status: StatusFail, Subject: subject, Detail: fmt.Sprintf("%s; %s or newer is needed", version.text, minimum)}
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
		return Check{Status: StatusFail, ID: "prerequisites.git", Subject: "Git", Detail: fmt.Sprintf("`git --version` failed: %s", text.OneLine(detail, 0))}, nil
	}
	return versionCheck("prerequisites.git", "Git", result.Stdout, gitMinimum), nil
}

// askDocker is one `docker` query, bounded in time; Docker being unavailable
// is its answer, not a propagated error — but only that: anything else,
// cancellation included, propagates so a Ctrl-C here ends the command
// rather than becoming a line. The successful result is returned for the
// caller to interpret further; nil means the returned Check is the answer.
func askDocker(ctx context.Context, runner run.Runner, id, subject string, args []string, timeout time.Duration) (Check, *run.Result, error) {
	result, err := docker.Run(ctx, runner, args, run.Spec{AllowFailure: true, Timeout: timeout})
	if err != nil {
		if docker.Unavailable(err) {
			return Check{ID: id, Status: StatusFail, Subject: subject, Detail: err.Error()}, nil, nil
		}
		return Check{}, nil, err
	}
	if result.TimedOut {
		return Check{ID: id, Status: StatusFail, Subject: subject, Detail: fmt.Sprintf(
			"`docker %s` did not answer within %d s", strings.Join(args, " "), int(timeout.Seconds()))}, nil, nil
	}
	if result.ExitCode != 0 {
		return Check{ID: id, Status: StatusFail, Subject: subject, Detail: text.OneLine(
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
			{Status: StatusFail, ID: "prerequisites.docker.engine", Subject: "Docker Engine", Detail: "the `docker` command is not on PATH; is Docker installed?"},
			{Status: StatusSkip, ID: "prerequisites.docker.compose", Subject: "Docker Compose", Detail: "needs the `docker` command"},
		}}, nil
	}
	var engineCheck, composeCheck Check
	var engineResult, composeResult *run.Result
	var engineErr, composeErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		engineCheck, engineResult, engineErr = askDocker(ctx, runner, "prerequisites.docker.engine", "Docker Engine", []string{"version", "--format", "{{.Server.Version}}"}, timeout)
	}()
	go func() {
		defer wg.Done()
		composeCheck, composeResult, composeErr = askDocker(ctx, runner, "prerequisites.docker.compose", "Docker Compose", []string{"compose", "version", "--short"}, timeout)
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
		checks = append(checks, Check{Status: StatusOK, ID: "prerequisites.docker.engine", Subject: "Docker Engine",
			Detail: fmt.Sprintf("%s, daemon answering", text.OneLine(strings.TrimSpace(engineResult.Stdout), 80))})
	} else {
		checks = append(checks, engineCheck)
	}
	if composeResult != nil {
		checks = append(checks, versionCheck("prerequisites.docker.compose", "Docker Compose", composeResult.Stdout, composeMinimum))
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
		state = dockerState{checks: []Check{failureCheck("prerequisites.docker.engine", "Docker", err)}}
	}
	checks, err := guarded("prerequisites.git", "Git", func() ([]Check, error) {
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
		return Check{Status: StatusOK, ID: "registry.control-home", Subject: "control home", Detail: fmt.Sprintf("%s (not created yet; the first up creates it)", controlHome)}, nil
	}
	if err := files.AssertAdoptableControlHome(controlHome, userHome); err != nil {
		return Check{}, err
	}
	// A control home a newer treerig made fails here with the upgrade
	// instruction, the same refusal every other command gives it
	// (docs/design/state.md).
	if err := files.AssertControlHomeFormat(controlHome); err != nil {
		return Check{}, err
	}
	return Check{Status: StatusOK, ID: "registry.control-home", Subject: "control home", Detail: controlHome}, nil
}

// registryChecks is what the registry says about itself: records that
// cannot be read, and hostnames that more than one worktree claims —
// including the index's own, which no worktree can share with it.
func registryChecks(scan registry.RegistryScan, commandName string) []Check {
	count := "none yet"
	if len(scan.Records) > 0 {
		count = fmt.Sprintf("%d readable", len(scan.Records))
	}
	checks := []Check{{Status: StatusOK, ID: "registry.registrations", Subject: "registrations", Detail: count}}
	for _, problem := range scan.Problems {
		checks = append(checks, Check{Status: StatusFail, ID: "registry.unreadable-record", Instance: recordInstance(problem.File), Subject: "unreadable record", Detail: problem.Reason})
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
		checks = append(checks, Check{Status: StatusFail, ID: "registry.host-conflict", Instance: host, Subject: host, Detail: fmt.Sprintf(
			"registered to %d worktrees (%s); requests for it may reach any of them that is running. Remove all but one with '%s worktree remove <uuid>'.",
			len(claimants), text.OneLine(strings.Join(ids, ", "), 200), commandName)})
	}

	for _, conflict := range registry.PortConflicts(scan.Records) {
		claims := make([]string, len(conflict.Claims))
		for i, claim := range conflict.Claims {
			claims[i] = fmt.Sprintf("%s of %s", claim.Service, layout.WorktreeHost(claim.Worktree.Slug, claim.Worktree.Domain))
		}
		checks = append(checks, Check{Status: StatusFail, ID: "registry.port-conflict", Instance: strconv.Itoa(conflict.Port),
			Subject: fmt.Sprintf("port %d", conflict.Port), Detail: fmt.Sprintf(
				"recorded for %s; only one can publish it. Run '%s up --reallocate-port <service>' for all but one of those services, each in the checkout that records it.",
				text.OneLine(strings.Join(claims, ", "), 200), commandName)})
	}

	for _, record := range scan.Records {
		if layout.WorktreeHost(record.Slug, record.Domain) != router.IndexHost {
			continue
		}
		checks = append(checks, Check{Status: StatusFail, ID: "registry.index-host", Instance: record.WorktreeID, Subject: router.IndexHost, Detail: fmt.Sprintf(
			"is the checkout index's own address, and worktree %s is registered to it. Set another domain in that checkout's %s and run '%s up' there.",
			record.WorktreeID, brand.ConfigFile, commandName)})
	}
	return checks
}

// registrySection is the "Registry" section: the treerig root (#80), the
// control home check, the router's own state directory, and registryChecks
// over a fresh scan of every registration.
func registrySection(opts doctorOptions) ([]Check, error) {
	// Neither can fail — both are already-resolved, already-checked paths,
	// no I/O of their own — so unlike control home and registrations below,
	// neither needs its own guarded() call; a person who set only
	// TREERIG_CONTROL_HOME can still see where the rest of the treerig root
	// lives (the router's own state; a future settings.toml, #79).
	checks := []Check{{Status: StatusOK, ID: "registry.treerig-root", Subject: "treerig root", Detail: opts.Home}}
	controlHomeChecks, err := guarded("registry.control-home", "control home", func() ([]Check, error) {
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
	routerStateChecks, err := guarded("registry.router-state", "router state", func() ([]Check, error) {
		// The state file is read, not just named: a file that is unreadable or
		// written by a newer treerig is a failed line carrying the same message
		// every command that needs the router's ports would give.
		if opts.RouterState.StateFile != "" {
			if _, _, err := router.ReadState(opts.RouterState); err != nil {
				return nil, err
			}
		}
		return []Check{{Status: StatusOK, ID: "registry.router-state", Subject: "router state", Detail: opts.RouterStateDir}}, nil
	})
	if err != nil {
		return nil, err
	}
	checks = append(checks, routerStateChecks...)
	for _, check := range controlHomeChecks {
		if check.Status == StatusFail {
			// Nothing under a control home treerig refuses is read: a scan would
			// answer "none yet" about records it never looked at.
			return append(checks, Check{Status: StatusSkip, ID: "registry.registrations", Subject: "registrations", Detail: "control home not readable"}), nil
		}
	}
	registrations, err := guarded("registry.registrations", "registrations", func() ([]Check, error) {
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
func lockCheck(id, subject string, scope lock.Scope, handRemoval string) (check Check, ok bool) {
	described, err := lock.Describe(scope, nil)
	if err != nil {
		return Check{ID: id, Status: StatusFail, Subject: subject, Detail: fmt.Sprintf("could not be checked: %s", err)}, true
	}
	if described.Record != nil && described.Record.NewerFormat() != "" {
		// Held or not, a holder this build cannot identify is not recovered
		// automatically: it is refused until upgraded past or cleared, like any
		// unverifiable one.
		state := "unreleased"
		if described.State == lock.Held {
			state = "held"
		}
		return Check{ID: id, Status: StatusWarn, Subject: subject,
			Detail: state + ", and its holder record is " + stateformat.Detail(described.Record.NewerFormat(), stateformat.LockRecord)}, true
	}
	if described.State == lock.Held {
		return Check{ID: id, Status: StatusOK, Subject: subject, Detail: "held by " + lock.HolderLabel(described.Record)}, true
	}
	if described.Record != nil && described.Record.ReleasedAt == nil {
		detail := fmt.Sprintf("unreleased by %s; recovered automatically the next time this lock is taken", lock.HolderLabel(described.Record))
		if handRemoval != "" {
			detail += fmt.Sprintf(". If that also refuses it as unverifiable, %s", handRemoval)
		}
		return Check{ID: id, Status: StatusOK, Subject: subject, Detail: detail}, true
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
	if check, ok := lockCheck("locks.registry", "registry", registryScope, registryHandRemoval); ok {
		checks = append(checks, check)
	}
	if routerLockScope != nil {
		routerHandRemoval := fmt.Sprintf(
			"run '%s worktree unlock --router --holder-unverifiable' once you have made sure its holder, and everything it started, is gone", commandName)
		if check, ok := lockCheck("locks.router", "router", *routerLockScope, routerHandRemoval); ok {
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
		if check, ok := lockCheck("locks.worktree.development", shown+" (development session)", development, ""); ok {
			check.Instance = id.WorktreeID
			checks = append(checks, check)
		}
		if check, ok := lockCheck("locks.worktree.operation", shown, operation, ""); ok {
			check.Instance = id.WorktreeID
			checks = append(checks, check)
		}
	}

	sharedLocks, err := sharedLockChecks(controlHome, repositoryNames(scan.Records), commandName)
	if err != nil {
		return nil, err
	}
	checks = append(checks, sharedLocks...)

	if len(checks) == 0 {
		return []Check{{Status: StatusOK, ID: "locks.none", Subject: "locks", Detail: "none held"}}, nil
	}
	return checks, nil
}

/* -------------------------------------------------------------- router ---- */

// doctorRouterSource is what the Router section needs of the shared router.
// A real router.Router satisfies it through realRouterSource; tests supply
// their own fake.
type doctorRouterSource interface {
	Status(ctx context.Context) (router.RouterStatus, error)
	// Owner reports which treerig root started the running router (#82); see
	// routerOwnerCheck.
	Owner(ctx context.Context) (router.Ownership, error)
	// RunningPort is the loopback port the running router serves, and
	// whether one is running at all (#79); see routerPortCheck.
	RunningPort(ctx context.Context) (port int, running bool, err error)
	// ModeChoice is the mode this build would start the router in beside
	// what detection alone would pick, or why it would refuse; see
	// routerModeCheck.
	ModeChoice(ctx context.Context) (router.ModeChoice, error)
	// CheckPorts lists the ports the router needs that something else
	// holds; see routerPortsCheck.
	CheckPorts(ctx context.Context) ([]router.PortConflict, error)
}

// realRouterSource adapts a real *router.Router to doctorRouterSource.
type realRouterSource struct{ svc *router.Router }

// Status delegates to the wrapped router.Router's own Status.
func (r realRouterSource) Status(ctx context.Context) (router.RouterStatus, error) {
	return r.svc.Status(ctx)
}

// RunningPort delegates to the wrapped router.Router's own RunningPort.
func (r realRouterSource) RunningPort(ctx context.Context) (int, bool, error) {
	return r.svc.RunningPort(ctx)
}

// ModeChoice delegates to the wrapped router.Router's own ModeChoice.
func (r realRouterSource) ModeChoice(ctx context.Context) (router.ModeChoice, error) {
	return r.svc.ModeChoice(ctx)
}

// CheckPorts delegates to the wrapped router.Router's own CheckPorts.
func (r realRouterSource) CheckPorts(ctx context.Context) ([]router.PortConflict, error) {
	return r.svc.CheckPorts(ctx)
}

// Owner delegates to the wrapped router.Router's own Owner.
func (r realRouterSource) Owner(ctx context.Context) (router.Ownership, error) {
	return r.svc.Owner(ctx)
}

// routerStatusChecks is the router in the words `router status` uses; not
// running is an answer, not a problem. A degraded router is one failed line
// per problem, each keyed by the problem's code (router.Problem).
func routerStatusChecks(status router.RouterStatus) []Check {
	report := router.StatusReport(status)
	if !status.Reachable {
		checks := make([]Check, len(report.Errors))
		for i, e := range report.Errors {
			checks[i] = Check{Status: StatusFail, ID: "router.container", Instance: router.CodeUnreachable, Subject: "router", Detail: e}
		}
		return checks
	}
	if !report.Failed {
		return []Check{{Status: StatusOK, ID: "router.container", Subject: "router", Detail: strings.Join(report.Output, "; ")}}
	}
	// One line per problem, as `router status` prints them, each keyed by the
	// problem's code (unique within a status), so (id, instance) is a key.
	checks := make([]Check, len(status.Problems))
	for i, problem := range status.Problems {
		detail := problem.Message
		if problem.Log != "" {
			detail += "; last log lines: " + text.OneLine(strings.ReplaceAll(problem.Log, "\n", " | "), 300)
		}
		checks[i] = Check{Status: StatusFail, ID: "router.container", Instance: problem.Code, Subject: "router", Detail: detail}
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
	name := fragment.Service
	if fragment.HostRoute {
		name = "host route " + name
	}
	if record, ok := byKey[fragment.RepositoryID+"/"+fragment.WorktreeID]; ok {
		return layout.WorktreeHost(record.Slug, record.Domain) + " (" + name + ")"
	}
	return fragment.RepositoryID + "." + fragment.WorktreeID + "." + name
}

// routeConflicts is every rule more than one fragment's router claims, each
// with the claiming fragments' own identities (fragmentIdentity). This
// replaces the pre-#55 mechanism of reading routed containers' own Traefik
// labels: Traefik's Docker-provider discovery is gone, so a fragment file
// is now the only thing that can claim a rule at all. dynamicDir is the
// boundary readFragmentRules checks every fragment path against.
func routeConflicts(dynamicDir string, fragments []router.FragmentInfo, byKey map[string]registry.RegisteredWorktree) map[string][]string {
	claims := make([]routeClaim, len(fragments))
	for i, fragment := range fragments {
		claims[i] = routeClaim{identity: fragmentIdentity(byKey, fragment), path: fragment.Path}
	}
	return conflictsOf(dynamicDir, claims)
}

// routeClaim is one route file's claim on its rules: the file, and how a
// report names whoever wrote it.
type routeClaim struct {
	identity string
	path     string
}

// sharedClaims are the shared services' route files as claims, each named by
// its service and, when a worktree of its repository is registered, that
// worktree's hostname; otherwise by the repository's ID.
func sharedClaims(fragments []router.SharedFragmentInfo, byKey map[string]registry.RegisteredWorktree) []routeClaim {
	claims := make([]routeClaim, len(fragments))
	for i, fragment := range fragments {
		owner := "repository " + fragment.RepositoryID
		for _, record := range byKey {
			if record.RepositoryID == fragment.RepositoryID {
				owner = "the repository of the worktree " + layout.WorktreeHost(record.Slug, record.Domain)
				break
			}
		}
		claims[i] = routeClaim{identity: fragment.Name + " (shared service of " + owner + ")", path: fragment.Path}
	}
	return claims
}

// conflictsOf is every rule more than one claim's router claims, with the
// claiming identities, sorted.
func conflictsOf(dynamicDir string, all []routeClaim) map[string][]string {
	type claim struct {
		claimants map[[2]string]bool
		names     map[string]bool
	}
	byRule := map[string]*claim{}
	for _, fragment := range all {
		identity := fragment.identity
		for _, r := range readFragmentRules(dynamicDir, fragment.path) {
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

// routeChecks reads every dynamic-configuration fragment under the router's
// own state directory (router.ListFragments) rather than any container's
// own labels — Traefik's Docker-provider discovery is gone (#55), so a
// fragment is the only thing that can claim a route at all now — reports
// any rule two fragments both claim, the same conflict the pre-#55 version
// reported from container labels, and warns (never fails or repairs) about
// fragments an orphaned worktree left behind.
func routeChecks(state layout.RouterLayout, controlHome, commandName string) ([]Check, error) {
	fragments, err := router.ListFragments(state)
	if err != nil {
		return nil, err
	}
	hostFragments, err := router.ListHostFragments(state)
	if err != nil {
		return nil, err
	}
	// Host routes (#6) claim rules of their own, so they can conflict with a
	// service's, a shared service's, or another worktree's host route: all
	// of them are claims in the one check below.
	fragments = append(fragments, hostFragments...)
	sharedFragments, err := router.ListSharedFragments(state)
	if err != nil {
		return nil, err
	}
	var checks []Check
	if len(fragments)+len(sharedFragments) == 0 {
		checks = append(checks, Check{Status: StatusOK, ID: "router.routes", Subject: "routes", Detail: "no route files"})
	} else {
		scan, err := registry.ScanRegistrations(controlHome)
		if err != nil {
			return nil, err
		}
		byKey := make(map[string]registry.RegisteredWorktree, len(scan.Records))
		for _, record := range scan.Records {
			byKey[record.RepositoryID+"/"+record.WorktreeID] = record
		}
		claims := make([]routeClaim, 0, len(fragments)+len(sharedFragments))
		for _, fragment := range fragments {
			claims = append(claims, routeClaim{identity: fragmentIdentity(byKey, fragment), path: fragment.Path})
		}
		claims = append(claims, sharedClaims(sharedFragments, byKey)...)
		conflicts := conflictsOf(state.DynamicDir, claims)
		if len(conflicts) == 0 {
			plural := "s"
			if len(claims) == 1 {
				plural = ""
			}
			checks = append(checks, Check{Status: StatusOK, ID: "router.routes", Subject: "routes", Detail: fmt.Sprintf(
				"%d route file%s; no two routers share a rule", len(claims), plural)})
		} else {
			rules := make([]string, 0, len(conflicts))
			for rule := range conflicts {
				rules = append(rules, rule)
			}
			sort.Strings(rules)
			for _, rule := range rules {
				checks = append(checks, Check{Status: StatusFail, ID: "router.route-conflict", Instance: ruleClaim(rule), Subject: "routes", Detail: fmt.Sprintf(
					"%s is claimed by %s; requests for it may reach any of them",
					text.OneLine(rule, 200), text.OneLine(strings.Join(conflicts[rule], ", "), 200))})
			}
		}
	}
	// The orphan check is advisory: a failure to read it becomes its own
	// failed line rather than replacing the conflict lines above.
	orphaned, err := orphanedRoutesChecks(state, controlHome, commandName)
	if err != nil {
		var cancelled *clierr.Cancelled
		if errors.As(err, &cancelled) {
			return nil, err
		}
		checks = append(checks, failureCheck("router.orphaned-routes", "routes", err))
	} else {
		checks = append(checks, orphaned...)
	}
	return checks, nil
}

// unmatchedDetail describes route files no registration of this control home
// matches, with the pronouns agreeing with their count, and how to delete
// them: nothing removes them automatically.
func unmatchedDetail(names []string, why, dynamicDir string, shared bool) string {
	// A shared service's route file is judged against its record, not a
	// registration, so a list that holds one says both.
	noun, owners, ownersGone := "registration", "worktree", "worktrees"
	if shared {
		noun, owners, ownersGone = "registration or shared-service record", "worktree or shared service", "worktrees or shared services"
	}
	if len(names) == 1 {
		return fmt.Sprintf("1 route file matches no %s of this control home (%s); %s; nothing removes it. If its %s is gone, delete it from %s",
			noun, text.OneLine(names[0], 300), why, owners, dynamicDir)
	}
	return fmt.Sprintf("%d route files match no %s of this control home (%s); %s; nothing removes them. If their %s are gone, delete them from %s",
		len(names), noun, text.OneLine(strings.Join(names, ", "), 300), why, ownersGone, dynamicDir)
}

// orphanedRoutesChecks warns — never fails, and never removes anything,
// matching doctor's own "it takes no lock, writes nothing, and repairs
// nothing" invariant — about route files that disagree with a registration
// (router.FindRouteProblems, the same definition the router's own Ensure uses
// to repair them). Files that match no record of this control home get a line
// of their own (instance "unmatched", or "other-home" for a repository it does
// not know): the router never removes them, because they may belong to another
// control home, so it says how to. An empty result means nothing worth a line.
func orphanedRoutesChecks(state layout.RouterLayout, controlHome, commandName string) ([]Check, error) {
	problems, err := router.FindRouteProblems(state, controlHomeRegistrations{controlHome: controlHome})
	if err != nil {
		return nil, err
	}
	// Shared services' route files are judged against the shared records the
	// same way (decision 9), and fall into the same three buckets.
	sharedProblems, err := router.FindSharedRouteProblems(state, controlHomeRegistrations{controlHome: controlHome})
	if err != nil {
		return nil, err
	}
	var repairable, stale, foreign []string
	for _, problem := range problems {
		switch {
		case problem.Repairable():
			repairable = append(repairable, problem.Describe())
		case problem.Kind == router.RouteUnknownRepository:
			foreign = append(foreign, filepath.Base(problem.Fragment.Path))
		default:
			stale = append(stale, filepath.Base(problem.Fragment.Path))
		}
	}
	var staleShared, foreignShared, repairableShared bool
	for _, problem := range sharedProblems {
		switch {
		case problem.Repairable():
			repairable = append(repairable, problem.Describe())
			repairableShared = true
		case problem.Kind == router.RouteUnknownRepository:
			foreign = append(foreign, filepath.Base(problem.Fragment.Path))
			foreignShared = true
		default:
			stale = append(stale, filepath.Base(problem.Fragment.Path))
			staleShared = true
		}
	}
	var checks []Check
	if len(repairable) > 0 {
		// A shared service's file is judged against its record, so a list
		// that holds one says so.
		plural, single := "the registrations", "its registration"
		if repairableShared {
			plural, single = "their registrations or shared-service records", "its registration or shared-service record"
		}
		detail := fmt.Sprintf("%d route files disagree with %s (%s); the next '%s up' or '%s router up' repairs them",
			len(repairable), plural, text.OneLine(strings.Join(repairable, "; "), 300), commandName, commandName)
		if len(repairable) == 1 {
			detail = fmt.Sprintf("1 route file disagrees with %s (%s); the next '%s up' or '%s router up' repairs it",
				single, text.OneLine(repairable[0], 300), commandName, commandName)
		}
		checks = append(checks, Check{Status: StatusWarn, ID: "router.orphaned-routes", Subject: "routes", Detail: detail})
	}
	// Both unmatched kinds are left alone, because the router's state is per
	// treerig root while registries are per control home. A file of a worktree
	// this home knows the repository of is likely stale (a warning); one of a
	// repository it does not know is likely another control home's (a notice).
	if len(stale) > 0 {
		staleWhy := "the repository is known here but no registration holds the worktree, so this file is probably stale"
		if staleShared {
			staleWhy = "the repository is known here but no registration or shared-service record holds it, so this file is probably stale"
		}
		checks = append(checks, Check{Status: StatusWarn, ID: "router.orphaned-routes", Instance: "unmatched", Subject: "routes",
			Detail: unmatchedDetail(stale, staleWhy, state.DynamicDir, staleShared)})
	}
	if len(foreign) > 0 {
		checks = append(checks, Check{Status: StatusOK, ID: "router.orphaned-routes", Instance: "other-home", Subject: "routes",
			Detail: unmatchedDetail(foreign, "this control home does not know the repository, so this file probably belongs to another control home", state.DynamicDir, foreignShared)})
	}
	return checks, nil
}

// routerOwnerCheck is doctor's own read-only report of which treerig root
// started the running router (#82): nil, nil when nothing is running —
// Status already says so, and there is no owner to report. A running
// container with no ownership label at all is reported as foreign, exactly
// like one whose label names another root, matching Router.Ensure's own
// checkOwnership; one whose label names this treerig root is ok.
func routerOwnerCheck(ctx context.Context, svc doctorRouterSource, home, commandName string) (*Check, error) {
	owner, err := svc.Owner(ctx)
	if err != nil {
		return nil, err
	}
	if !owner.Running {
		return nil, nil
	}
	if !owner.Labelled {
		return &Check{Status: StatusFail, ID: "router.owner", Subject: "router owner", Detail: fmt.Sprintf(
			"not owned by this treerig root; this one's '%s up' or '%s router up' refuses it until '%s router down' hands it over",
			commandName, commandName, commandName)}, nil
	}
	if owner.Home == home {
		return &Check{Status: StatusOK, ID: "router.owner", Subject: "router owner", Detail: "this treerig root (" + home + ")"}, nil
	}
	return &Check{Status: StatusFail, ID: "router.owner", Subject: "router owner", Detail: fmt.Sprintf(
		"a different treerig root (%s); this one's '%s up' or '%s router up' refuses it until '%s router down' hands it over",
		owner.Home, commandName, commandName, commandName)}, nil
}

// routerSection is the "Router" section: opts.RouterErr's own line if the
// router could not even be described, and one skip if Docker Engine did not
// answer. Otherwise it is the router's status, plus routerOwnerCheck's own
// report of which treerig root started it (#82), or a skip when Docker
// Compose did not answer, since both need Compose. Then the mode the proxy
// runs in and would run in (needs only the Engine), that it answers on
// loopback alone, routeChecks (which need no Docker), a probe of each route
// through the running proxy, and last the port that was resolved and where
// it came from (#79) — which needs nothing, though comparing it with the
// port the running router serves needs Compose, so without Compose that
// line states the resolution alone.
func routerSection(ctx context.Context, opts doctorOptions, state dockerState) ([]Check, error) {
	if opts.RouterErr != nil {
		return []Check{{Status: StatusFail, ID: "router.container", Subject: "router", Detail: opts.RouterErr.Error()}}, nil
	}
	if !state.engineAnswered {
		// One skip and nothing else: the port resolution (#79) needs no
		// Docker, but a Go-only line here would also widen the section's
		// subject column, so it waits for #46. Pinned byte for byte by the
		// golden tests.
		return []Check{{Status: StatusSkip, ID: "router.container", Subject: "router", Detail: "needs Docker"}}, nil
	}
	var checks []Check
	var status *router.RouterStatus
	if state.composeAnswered {
		routerChecks, err := guarded("router.container", "router", func() ([]Check, error) {
			answered, err := opts.Router.Status(ctx)
			if err != nil {
				return nil, err
			}
			status = &answered
			result := routerStatusChecks(answered)
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
		checks = append(checks, Check{Status: StatusSkip, ID: "router.container", Subject: "router", Detail: "needs Docker Compose"})
	}
	mode, err := guarded("router.mode", "router mode", func() ([]Check, error) {
		check, err := routerModeCheck(ctx, opts, status)
		if err != nil {
			return nil, err
		}
		return []Check{check}, nil
	})
	if err != nil {
		return nil, err
	}
	checks = append(checks, mode...)
	portChecks, err := guarded("router.ports", "router ports", func() ([]Check, error) {
		check, err := routerPortsCheck(ctx, opts, status)
		if err != nil || check == nil {
			return nil, err
		}
		return []Check{*check}, nil
	})
	if err != nil {
		return nil, err
	}
	checks = append(checks, portChecks...)
	// The running router's port when it says one, else the resolved one.
	servedPort := opts.RouterPort.Port
	if status != nil && status.RunningPort != 0 {
		servedPort = status.RunningPort
	}
	running := status != nil && status.Running
	if running {
		bind, err := guarded("router.bind", "router bind", func() ([]Check, error) {
			check, err := routerBindCheck(ctx, opts, servedPort)
			if err != nil || check == nil {
				return nil, err
			}
			return []Check{*check}, nil
		})
		if err != nil {
			return nil, err
		}
		checks = append(checks, bind...)
	}
	routes, err := guarded("router.routes", "routes", func() ([]Check, error) {
		return routeChecks(opts.RouterState, opts.ControlHome, opts.CommandName)
	})
	if err != nil {
		return nil, err
	}
	checks = append(checks, routes...)
	if running {
		probes, err := guarded("router.route-probe", "routes", func() ([]Check, error) {
			return routeProbeChecks(ctx, opts, servedPort)
		})
		if err != nil {
			return nil, err
		}
		checks = append(checks, probes...)
	}
	port, err := guarded("router.port", "router port", func() ([]Check, error) {
		// The resolution needs nothing; comparing it with what the running
		// router serves asks Compose for the container, so without Compose
		// the line says only where the port came from — the same skip the
		// status check above makes, not a failure.
		if !state.composeAnswered {
			return []Check{{Status: StatusOK, ID: "router.port", Subject: "router port", Detail: opts.RouterPort.Describe()}}, nil
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
// from (#79), and warns when the running router serves a different one:
// the next `up` or `router up` moves it, and URLs already open keep the old
// port until then. Nothing running, or a port that cannot be read off the
// container, is just the resolution itself.
func routerPortCheck(ctx context.Context, opts doctorOptions) (Check, error) {
	described := opts.RouterPort.Describe()
	served, running, err := opts.Router.RunningPort(ctx)
	if err != nil {
		return Check{}, err
	}
	if running && served != 0 && served != opts.RouterPort.Port {
		return Check{Status: StatusWarn, ID: "router.port", Subject: "router port", Detail: fmt.Sprintf(
			"%s, but the running router serves %d; the next '%s up' or '%s router up' moves it, and URLs already open keep the old port until then",
			described, served, opts.CommandName, opts.CommandName)}, nil
	}
	return Check{Status: StatusOK, ID: "router.port", Subject: "router port", Detail: described}, nil
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

// checkoutNone is a checkoutSource for no treerig.toml found searching upward
// from searchedFrom.
func checkoutNone(searchedFrom string) checkoutSource {
	return checkoutSource{kind: "none", searchedFrom: searchedFrom}
}

// checkoutUnloadable is a checkoutSource for a treerig.toml that was found but
// failed to load, carrying the loader's own error.
func checkoutUnloadable(err error) checkoutSource {
	return checkoutSource{kind: "unloadable", unloadable: err}
}

// checkoutIsLoaded is a checkoutSource for a treerig.toml that loaded cleanly.
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
// treerig.toml above cwd.
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
// [service.<name>.index] label or purpose (#19), with the exact wording
// `config validate` itself would refuse with. doctor answers "would up work?", so this is a
// failed check, not a warning: `up` against this same file refuses outright.
// A multi-line message like this one prints on the section's single line
// the same way an unrecognized-key or other multi-line schema error already
// does in the "unloadable" case above — formatDoctorReport's text.Printable
// turns each embedded newline into a space.
func checkoutFileCheck(loaded *config.Loaded) Check {
	if err := config.CheckStart(loaded); err != nil {
		return Check{Status: StatusFail, ID: "checkout.config", Subject: brand.ConfigFile, Detail: err.Error()}
	}
	return Check{Status: StatusOK, ID: "checkout.config", Subject: brand.ConfigFile, Detail: loaded.File}
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
// doctor's own exit code either: docs/doctor.md promises doctor exits with the
// verify hook's own code only when that hook actually ran and failed, and 1
// for any other failure, a predicted one included (see doctorExitCode).
//
// Every [commands] entry follows the hooks, in file order, as
// "<name> command file" (#94): hooks.ResolveCommand shares `treerig run`'s own
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
		checks = append(checks, resolutionCheck("checkout.hooks.resolve", string(stage), string(stage)+" hook file", resolution, err))
	}
	for _, entry := range loaded.Config.Commands {
		resolution, err := hooks.ResolveCommand(entry.Name, entry.Command, loaded.Root, environ)
		checks = append(checks, resolutionCheck("checkout.commands.resolve", entry.Name, entry.Name+" command file", resolution, err))
	}
	return checks
}

// resolutionCheck is the line for one hook or command file, under id with
// instance naming the stage or command: ok naming what it
// resolves to, a fail naming the exit code a real run would give when it
// would not start, or the failure itself for anything else (an empty
// command).
func resolutionCheck(id, instance, subject string, resolution hooks.Resolution, err error) Check {
	if err == nil {
		return Check{ID: id, Instance: instance, Status: StatusOK, Subject: subject, Detail: fmt.Sprintf("resolves to %s", resolution.Command)}
	}
	var startErr *run.StartError
	if errors.As(err, &startErr) {
		return Check{ID: id, Instance: instance, Status: StatusFail, Subject: subject, Detail: fmt.Sprintf(
			"would not start (exit %d): `%s` %s", startErr.ExitCode, resolution.Command, startErr.Detail)}
	}
	failed := failureCheck(id, subject, err)
	failed.Instance = instance
	return failed
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
	identityFailure := ""
	var registration *registry.WorktreeRegistration
	if regErr != nil {
		identified = false
		checks = append(checks, failureCheck("checkout.worktree", "this worktree", regErr))
	} else {
		var err error
		registration, err = reg.Current(ctx)
		if err != nil {
			var cancelled *clierr.Cancelled
			if errors.As(err, &cancelled) {
				return nil, err
			}
			identified = false
			identityFailure = err.Error()
			checks = append(checks, failureCheck("checkout.worktree", "this worktree", err))
		} else if registration != nil {
			checks = append(checks, Check{Status: StatusOK, ID: "checkout.worktree", Subject: "this worktree",
				Detail: "registered as " + layout.WorktreeHost(registration.Slug, registration.Domain)})
		} else {
			checks = append(checks, Check{Status: StatusSkip, ID: "checkout.worktree", Subject: "this worktree",
				Detail: fmt.Sprintf("not registered; '%s up' registers it", opts.CommandName)})
		}
	}

	if regErr == nil {
		missing, err := guarded("checkout.missing-scan", "missing checkouts", func() ([]Check, error) {
			candidates, err := reg.PruneCandidates(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]Check, len(candidates))
			for i, record := range candidates {
				out[i] = Check{ID: "checkout.missing", Instance: record.WorktreeID, Status: StatusWarn, Subject: layout.WorktreeHost(record.Slug, record.Domain), Detail: fmt.Sprintf(
					"registered, but Git no longer lists its checkout. Remove what it left with '%s worktree remove %s'.",
					opts.CommandName, record.WorktreeID)}
			}
			return out, nil
		})
		if err != nil {
			return nil, err
		}
		// The scan reads every checkout's identity markers, this one's
		// included, so a marker this build cannot read fails it with the very
		// message already given above. That is reported once.
		for i, check := range missing {
			if !identified && check.Status == StatusFail && check.Detail == identityFailure {
				missing[i] = Check{Status: StatusSkip, ID: check.ID, Subject: check.Subject, Detail: "needs this worktree to be identified"}
			}
		}
		checks = append(checks, missing...)
	}

	if identified && registration != nil && len(loaded.Config.Service) > 0 {
		portLines, err := loopbackPortChecks(ctx, opts, loaded, registration)
		if err != nil {
			return nil, err
		}
		checks = append(checks, portLines...)
	}

	if identified && registration != nil && len(registration.HostRoutes) > 0 {
		checks = append(checks, hostRouteChecks(ctx, opts, registration)...)
	}

	checks = append(checks, hookResolutionChecks(loaded, opts.Environ)...)

	command := loaded.Config.Hooks.Verify
	switch {
	case command == nil:
		checks = append(checks, Check{Status: StatusSkip, ID: "checkout.hooks.verify", Subject: "verify hook", Detail: "none configured"})
	case !identified:
		checks = append(checks, Check{Status: StatusSkip, ID: "checkout.hooks.verify", Subject: "verify hook", Detail: "needs this worktree to be identified"})
	case registration == nil:
		checks = append(checks, Check{Status: StatusSkip, ID: "checkout.hooks.verify", Subject: "verify hook",
			Detail: fmt.Sprintf("needs a registered worktree; run '%s up' first", opts.CommandName)})
	default:
		ref := worktree.Ref{RepositoryID: registration.RepositoryID, WorktreeID: registration.WorktreeID, Slug: registration.Slug}
		// No routes when opts.RouterErr != nil: opts.RouterPort is then
		// unresolved (its zero value), and routing on port 0 would hand the
		// hook a wrong :0 URL rather than no URL at all.
		var verifyRoutes []worktree.ServiceRoute
		if opts.RouterErr == nil {
			verifyRoutes = routesOf(loaded.Config, ref.Slug, registration.Domain, opts.RouterPort.Port, registration.Ports, registration.HostRoutes).All()
		}
		// Its output goes to stderr: stdout is this report.
		_, hookErr := hooks.RunHook(ctx, config.Verify, hooks.HookContext{
			Source: worktree.EnvironmentSource{
				ProjectName: loaded.Config.Project.Name, Domain: registration.Domain,
				ControlHome: opts.ControlHome, ConfigFile: loaded.File, ProjectRoot: loaded.Root,
			},
			Ref: ref, Hooks: loaded.Config.Hooks, Logger: opts.Logger, Runner: opts.Runner, Environ: opts.Environ,
			ComposeFile: composeFilePath(loaded),
			// TREERIG_URL, TREERIG_SERVICE_URL_<NAME> (#28), TREERIG_SERVICE_PORT_<NAME> (#119)
			Routes: verifyRoutes,
			// TREERIG_SHARED_ADDR_<NAME>_<PORT>
			Shared: sharedAddrsFor(opts.ControlHome, registration.RepositoryID, loaded.Config),
			// TREERIG_SHARED_URL_<NAME>, only with a resolved router port, as above
			SharedURLs: sharedVerifyURLs(opts, registration.RepositoryID, loaded.Config),
		}, hooks.RunOptions{Output: hooks.Stderr})
		if hookErr != nil {
			var cancelled *clierr.Cancelled
			if errors.As(hookErr, &cancelled) {
				return nil, hookErr
			}
			var hookFailure *clierr.Error
			if errors.As(hookErr, &hookFailure) {
				checks = append(checks, Check{Status: StatusFail, ID: "checkout.hooks.verify", Subject: "verify hook", Detail: hookFailure.Message, ExitCode: &hookFailure.ExitCode})
			} else {
				checks = append(checks, failureCheck("checkout.hooks.verify", "verify hook", hookErr))
			}
		} else {
			checks = append(checks, Check{Status: StatusOK, ID: "checkout.hooks.verify", Subject: "verify hook", Detail: fmt.Sprintf("%s exited 0", *command)})
		}
	}
	return checks, nil
}

// thisCheckoutSection is the "This checkout" section: a skip when no
// treerig.toml was found, a failed line naming the load error when one was
// found but would not load, otherwise checkoutChecks over the loaded config.
func thisCheckoutSection(ctx context.Context, opts doctorOptions) ([]Check, error) {
	switch opts.Checkout.kind {
	case "none":
		return []Check{{Status: StatusSkip, ID: "checkout.config", Subject: brand.ConfigFile, Detail: fmt.Sprintf(
			"none in %s or above; run '%s doctor' in a checkout to check it and its verify hook", opts.Checkout.searchedFrom, opts.CommandName)}}, nil
	case "unloadable":
		return []Check{{Status: StatusFail, ID: "checkout.config", Subject: brand.ConfigFile, Detail: opts.Checkout.unloadable.Error()}}, nil
	default:
		return guarded("checkout.worktree", "this worktree", func() ([]Check, error) { return checkoutChecks(ctx, opts, opts.Checkout.loaded) })
	}
}

/* -------------------------------------------------------------- report ---- */

// doctorExitCode is 0 when nothing failed; the verify hook's own code when
// it ran and failed, as the hook contract and docs/doctor.md promise; 1 for any
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
	// Home is the checked treerig root (TREERIG_HOME, #80): reported alongside
	// ControlHome and RouterStateDir in the Registry section, so someone
	// who set only TREERIG_CONTROL_HOME can still see where the rest of it
	// (the router's own state, and a future settings.toml, #79) lives.
	Home string
	// RouterStateDir is layout.RouterStateFrom(Home).StateDir, reported
	// even when Router/RouterErr says the router itself could not be
	// described (a bad port does not change where its state directory is).
	RouterStateDir string
	// RouterPort is the router port resolved for this treerig root and where
	// it came from (#79): TREERIG_ROUTER_PORT, settings.toml, or the default.
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
	// PortProbe decides whether an allocated loopback port is free; nil asks
	// the operating system (#119).
	PortProbe ports.Probe
	// Listeners says which loopback addresses a host route's port answers
	// on; nil dials (ports.DialListener).
	Listeners ports.ListenerProbe
	// OS is Env.OS, for the tool a message suggests.
	OS string
	// RouteClient sends the route probes (decision 13); nil skips them.
	RouteClient routeClient
	// RouteProbeBudget and RouteProbeInterval override routeProbeBudget and
	// routeProbeInterval; zero keeps the default. Tests shorten them.
	RouteProbeBudget   time.Duration
	RouteProbeInterval time.Duration
	// LANProbe reports which non-loopback addresses answer on the router's
	// port; nil skips the check.
	LANProbe lanProbe
	// DockerAnswered is set by doctorSections from its own Docker probes:
	// true when both the daemon and Compose answered. The checks that ask
	// Docker who holds a port, or what a Compose file publishes, are
	// skipped, or say they could not tell, when it is false.
	DockerAnswered bool
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
	locksChecks, err := guarded("locks.scan", "locks", func() ([]Check, error) { return lockChecks(opts.ControlHome, opts.RouterLockScope, opts.CommandName) })
	if err != nil {
		return nil, err
	}
	routerChecksResult, err := routerSection(ctx, opts, state)
	if err != nil {
		return nil, err
	}
	opts.DockerAnswered = state.engineAnswered && state.composeAnswered
	checkoutChecksResult, err := thisCheckoutSection(ctx, opts)
	if err != nil {
		return nil, err
	}
	sharedChecksResult, err := sharedSection(ctx, opts)
	if err != nil {
		return nil, err
	}
	sections := []Section{
		{ID: "prerequisites", Title: "Prerequisites", Checks: prerequisiteChecks},
		{ID: "registry", Title: "Registry", Checks: registryChecks},
		{ID: "locks", Title: "Locks", Checks: locksChecks},
		{ID: "router", Title: "Router", Checks: routerChecksResult},
		{ID: "checkout", Title: "This checkout", Checks: checkoutChecksResult},
	}
	// Only when treerig.toml declares shared services or Docker still holds
	// some of this repository's.
	if len(sharedChecksResult) > 0 {
		sections = append(sections, Section{ID: "shared", Title: "Shared services", Checks: sharedChecksResult})
	}
	return sections, nil
}

// doctorOptionsFor is doctorOptions built from env and an already-checked
// treerig root (home, userHome — commands.checkedHome), against svc as the
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
		Runner: env.Runner, Logger: env.Logger, OS: env.OS,
		Checkout: checkout, Invocation: env.Invocation, Environ: env.Environ,
		Listeners: env.Listeners,
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
	opts.RouteClient = &http.Client{
		Timeout: routeRequestTimeout,
		// A redirect is an answer; following it would probe somewhere else.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	opts.LANProbe = lanAnswering
	return opts, nil
}

// buildDoctorOptions is doctorOptions built from a real Env and an
// already-checked treerig root (Doctor's own single checkedHome call):
// findCheckout is resolved first (env.ConfigPath, or the nearest treerig.toml
// above env.Cwd), then the shared router — or why one could not even be
// described.
func buildDoctorOptions(env *Env, home, userHome string) (doctorOptions, error) {
	svc, state, resolved, err := routerServiceFrom(env, home)
	return doctorOptionsFor(env, home, userHome, svc, state, resolved, err)
}

// DoctorReport is `doctor --json`'s document. OK is true when no check
// failed; ExitCode is what the command exits with, 0 exactly when OK.
type DoctorReport struct {
	OK       bool                  `json:"ok"`
	ExitCode int                   `json:"exitCode"`
	Counts   DoctorCounts          `json:"counts"`
	Sections []DoctorReportSection `json:"sections"`
}

// DoctorCounts is how many checks ended in each status.
type DoctorCounts struct {
	OK   int `json:"ok"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
	Skip int `json:"skip"`
}

// DoctorReportSection is one area of the report, in report order. ID is the
// stable key; Title is display text.
type DoctorReportSection struct {
	ID     string              `json:"id"`
	Title  string              `json:"title"`
	Checks []DoctorReportCheck `json:"checks"`
}

// DoctorReportCheck is one check. ID is the stable kind, "<section>.<kind>";
// it repeats once per item for a per-item kind, and Instance is then the
// item's durable key. Subject and Detail are display text, always present. HookExitCode is present
// only for a failed verify hook, and is the code doctor then exits with.
type DoctorReportCheck struct {
	ID           string `json:"id"`
	Instance     string `json:"instance,omitempty"`
	Status       string `json:"status"`
	Subject      string `json:"subject"`
	Detail       string `json:"detail"`
	HookExitCode *int   `json:"hookExitCode,omitempty"`
}

// doctorReport maps sections onto the JSON document; code is
// doctorExitCode(sections).
func doctorReport(sections []Section, code int) DoctorReport {
	counts := tally(sections)
	report := DoctorReport{
		OK: code == 0, ExitCode: code,
		Counts:   DoctorCounts{OK: counts[StatusOK], Warn: counts[StatusWarn], Fail: counts[StatusFail], Skip: counts[StatusSkip]},
		Sections: make([]DoctorReportSection, 0, len(sections)),
	}
	for _, section := range sections {
		checks := make([]DoctorReportCheck, 0, len(section.Checks))
		for _, check := range section.Checks {
			checks = append(checks, DoctorReportCheck{
				ID: check.ID, Instance: check.Instance, Status: string(check.Status), Subject: check.Subject, Detail: check.Detail, HookExitCode: check.ExitCode,
			})
		}
		report.Sections = append(report.Sections, DoctorReportSection{ID: section.ID, Title: section.Title, Checks: checks})
	}
	return report
}

// Doctor is `doctor`. Its report goes to stdout, one line per check (or, with
// options.JSON, one DoctorReport document); its summary goes to stderr in
// both forms. The returned exit code is 0 when nothing failed.
func Doctor(ctx context.Context, env *Env, options OutputOptions) (int, error) {
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

	code := doctorExitCode(sections)
	if options.JSON {
		out, err := text.SafeJSON(doctorReport(sections, code))
		if err != nil {
			return 0, err
		}
		env.Logger.Output(out)
	} else {
		for _, line := range formatDoctorReport(sections) {
			env.Logger.Output(text.Printable(line))
		}
	}
	summary := doctorSummary(sections)
	if summary.level == "error" {
		env.Logger.Error(summary.message)
	} else {
		env.Logger.Info(summary.message)
	}
	return code, nil
}

// sharedVerifyURLs are the shared services' routed URLs the verify hook gets:
// none when the router's port could not be resolved, for the reason the
// service routes above are left out.
func sharedVerifyURLs(opts doctorOptions, repositoryID string, cfg config.Config) []worktree.SharedURL {
	if opts.RouterErr != nil {
		return nil
	}
	return sharedURLsFor(opts.ControlHome, repositoryID, cfg, opts.RouterPort.Port, opts.Logger)
}
