package router

// The shared router, one per machine, used by every worktree of every
// repository. It binds one loopback port and routes by hostname; worktree
// services reach it by joining the shared network and carrying labels the
// Compose runtime generates for them. Nothing here is per-project, which is
// why its settings come from the environment rather than from berth.toml.
//
// Ported from src/router/router.ts. Unlike there, locking is not this
// package's job: the caller acquires the router's lock.Scope through
// lifecycle.Hold (taken alone) before calling Ensure, the way every other
// mutation in this build holds its lock around the action rather than the
// action holding it around itself.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/checkoutindex"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/docker"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/text"
)

// Docker-call timeouts (#14): the TypeScript build never set any on the
// router's own calls. containerID and inspect are quick introspection,
// timed the same as doctor.ts's own docker inspect probes
// (DOCKER_PROBE_TIMEOUT_MS). start and Down have no TypeScript precedent, so
// these are this port's own sensible choices: starting the router may pull
// three images on a bare machine and wait for every health check, hence the
// generous 5 minutes; lock.RouterTimeout is sized to cover Ensure's worst
// case in terms of these three constants, so a waiter's patience is never
// shorter than what a first start may need — see its own comment. Stopping
// it is ordinarily fast; a minute is generous room for a slow network
// teardown without leaving a hung `docker` unnoticed for long.
const (
	dockerProbeTimeout = 30 * time.Second
	startTimeout       = 5 * time.Minute
	downTimeout        = 1 * time.Minute
)

// Timeouts overrides the router's per-call timeouts; a zero field keeps this
// port's own default. Production leaves it unset; tests use it to prove a
// hung `docker` is caught in milliseconds rather than minutes.
type Timeouts struct {
	// Probe bounds containerID and inspect; default dockerProbeTimeout.
	Probe time.Duration
	// Start bounds `compose up`; default startTimeout.
	Start time.Duration
	// Down bounds `compose down`; default downTimeout.
	Down time.Duration
}

// withDefaults fills every unset (zero or negative) field with this
// package's own default.
func (t Timeouts) withDefaults() Timeouts {
	if t.Probe <= 0 {
		t.Probe = dockerProbeTimeout
	}
	if t.Start <= 0 {
		t.Start = startTimeout
	}
	if t.Down <= 0 {
		t.Down = downTimeout
	}
	return t
}

// notRunning is Verify's answer when every container is gone.
const notRunning = "the router is not running"

// dockerName is what Docker accepts for a project or network name; nothing
// else may reach a Traefik predicate, since the network name is interpolated
// into it.
var dockerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// DefaultName is the shared router's default Compose project name; the
// network defaults to it too.
const DefaultName = brand.Name + "-router"

// ResolveProjectName is the shared router's Compose project name when
// nothing more specific overrides it — DefaultName, today the only value
// this build ever resolves it to (see Options.ProjectName's own doc
// comment). New calls this for its own default rather than naming
// DefaultName directly, and so does internal/commands's routerLockScope,
// for the two callers with no Router of their own to ask (newCheckoutIndex,
// commands.UnlockGlobal, #21): one shared function, so a lock scope's
// project-name marker and the real router's own ProjectName can never
// independently drift onto two different defaults.
func ResolveProjectName() string { return DefaultName }

// PlaceholderPage returns the bytes of the placeholder index page Ensure
// writes before the index container's first `compose up --wait`, because
// that container's health check fetches it (router.ts:198-201 in the
// TypeScript build, ported as write() below). Tests may supply a stub.
type PlaceholderPage func(now time.Time) ([]byte, error)

// defaultPlaceholderPage is the checkout index's own placeholder:
// renderIndexHtml(placeholderSnapshot(now)), as the TypeScript build writes.
func defaultPlaceholderPage(now time.Time) ([]byte, error) {
	return []byte(checkoutindex.PlaceholderHTML(now)), nil
}

// Options configure a Router.
type Options struct {
	// State is the router's own on-disk layout (layout.RouterState).
	State layout.RouterLayout
	// Port is the loopback port to publish.
	Port           int
	Runner         run.Runner
	Logger         logger.Logger
	LabelNamespace string
	// ProjectName defaults to ResolveProjectName() (DefaultName);
	// overridable so tests can run a router beside a developer's real one.
	// internal/commands's routerLockScope resolves the same default for its
	// two callers with no Router built yet to ask (#21) — an override given
	// here reaches New, but not those, which is why nothing in this build
	// overrides it today.
	ProjectName string
	// Network defaults to ProjectName.
	Network string
	// Environ is the environment Compose inherits; os.Environ() in production.
	Environ []string
	// Invocation names this process to whoever waits on the router lock.
	Invocation string
	// CommandName is the name the CLI was invoked as; appears in advice.
	CommandName string
	// Page returns the placeholder index page; defaults to defaultPlaceholderPage.
	// SettingsPath names the settings file (#79) in the message a router
	// that fails to start prints, beside BERTH_ROUTER_PORT; "" names it
	// generically, as brand.SettingsFile.
	SettingsPath string
	Page         PlaceholderPage
	// Timeouts overrides this router's per-call docker timeouts (#14).
	Timeouts Timeouts
	// Home is the berth root (BERTH_HOME) this Router's state was resolved
	// from — stamped on both containers as RouterHomeLabel and checked by
	// Ensure before it ever touches a router another berth root started
	// (#82). "" (never set, a test mostly) matches only another Router
	// that also left it unset, so nothing here changes for a caller that
	// does not know its own berth root.
	Home string
}

// Router is what the rest of the tool needs to know about the shared router.
type Router struct {
	Port        int
	Network     string
	IndexURL    string
	ProjectName string

	state          layout.RouterLayout
	settingsPath   string
	labelNamespace string
	runner         run.Runner
	logger         logger.Logger
	environ        []string
	invocation     string
	commandName    string
	page           PlaceholderPage
	timeouts       Timeouts
	home           string
}

// New builds a Router. The network name is interpolated into Traefik's
// discovery predicate, so a project or network name that could rewrite it is refused.
func New(options Options) (*Router, error) {
	projectName := options.ProjectName
	if projectName == "" {
		projectName = ResolveProjectName()
	}
	if !dockerName.MatchString(projectName) {
		return nil, clierr.Newf("Invalid router project name: %s.", projectName)
	}
	network := options.Network
	if network == "" {
		network = projectName
	}
	if !dockerName.MatchString(network) {
		return nil, clierr.Newf("Invalid router network name: %s.", network)
	}
	labelNamespace := options.LabelNamespace
	if labelNamespace == "" {
		labelNamespace = brand.LabelNamespace
	}
	page := options.Page
	if page == nil {
		page = defaultPlaceholderPage
	}
	return &Router{
		Port:           options.Port,
		Network:        network,
		IndexURL:       IndexURL(options.Port),
		ProjectName:    projectName,
		state:          options.State,
		labelNamespace: labelNamespace,
		runner:         options.Runner,
		logger:         options.Logger,
		environ:        options.Environ,
		invocation:     options.Invocation,
		commandName:    options.CommandName,
		settingsPath:   options.SettingsPath,
		page:           page,
		timeouts:       options.Timeouts.withDefaults(),
		home:           options.Home,
	}, nil
}

// commandNameOr is r.commandName, falling back to the brand name when it was
// never given, so advice always names something to run.
func (r *Router) commandNameOr() string {
	if r.commandName != "" {
		return r.commandName
	}
	return brand.Name
}

// componentLabel is the label naming which service a container is.
func (r *Router) componentLabel() string { return r.labelNamespace + ".component" }

// protocolLabelName is the label naming a container's router protocol.
func (r *Router) protocolLabelName() string {
	return r.labelNamespace + ".router-protocol"
}

// compose runs one `docker compose` call against this router's own stack
// file and project directory.
func (r *Router) compose(ctx context.Context, args []string, spec run.Spec) (run.Result, error) {
	full := append([]string{
		"compose",
		"--project-directory", r.state.StateDir,
		"--project-name", r.ProjectName,
		"-f", r.state.ComposeFile,
	}, args...)
	spec.Env = docker.ComposeEnvironment(r.environ)
	return docker.Run(ctx, r.runner, full, spec)
}

// stateNoun is this package's root for files.*Named's outside-of-root
// refusal: the state directory is not a control home, so it must not be
// called one if that refusal ever actually fires (today it cannot: every
// path secureState touches is built as a child of state.StateDir).
const stateNoun = "the shared router's state directory"

// SecureState is secureState, exported so a caller can secure the state
// directory before taking the router's lock: lock.New creates <StateDir>/lock
// and its holder record the moment it is acquired, so the directory must
// already be safe by then. commands.RouterUp calls this before routerHold;
// Ensure calls the same check again as its own first step, since it must
// not depend on every caller remembering to call this first.
func SecureState(state layout.RouterLayout) error {
	return secureState(state)
}

// secureState creates and locks down the router's state directory: owner-only
// except the two directories a container mounts, which cannot be 0700
// without CAP_DAC_OVERRIDE (see internal/files's package comment for why the
// same exception applies to a control home's router directories). This
// package owns it because, as of protocol 4, that directory is no longer
// part of any control home (#12, #13).
func secureState(state layout.RouterLayout) error {
	if err := secureDirectories(state.StateDir, []string{state.StateDir}, 0o700); err != nil {
		return err
	}
	if err := secureDirectories(state.StateDir, []string{state.DynamicDir, state.IndexDir}, 0o755); err != nil {
		return err
	}
	for _, file := range []string{state.LockPath, state.IndexFile, state.ComposeFile, state.ProtocolFile} {
		if err := files.AssertSafeFilePathNamed(state.StateDir, file, stateNoun); err != nil {
			return err
		}
	}
	return nil
}

// secureDirectories mirrors files.go's own createSecureDirectories exactly,
// one directory at a time: checked before anything touches it, created,
// checked again — closing the time-of-check-to-time-of-use window a symlink
// planted between the two checks could otherwise use — and chmod'd only
// once both checks have passed. A planted symlink at root (or at any dir) is
// refused by the first check, before MkdirAll or Chmod ever runs, so nothing
// in whatever it points to is created or has its mode changed.
func secureDirectories(root string, dirs []string, mode os.FileMode) error {
	for _, dir := range dirs {
		if err := files.AssertSafeDirectoryPathNamed(root, dir, stateNoun); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, mode); err != nil {
			return err
		}
		if err := files.AssertSafeDirectoryPathNamed(root, dir, stateNoun); err != nil {
			return err
		}
		if err := os.Chmod(dir, mode); err != nil {
			return err
		}
	}
	return nil
}

// write writes the stack document, the checkout index's own dynamic-
// configuration fragment, and makes sure the directories it mounts exist and
// serve.
func (r *Router) write(now time.Time) error {
	if err := secureState(r.state); err != nil {
		return err
	}
	// The index container's health check fetches the page, so one must exist
	// before `up --wait`.
	exists, err := files.PathExists(r.state.IndexFile)
	if err != nil {
		return err
	}
	if !exists {
		page, err := r.page(now)
		if err != nil {
			return err
		}
		if err := files.AtomicWrite(r.state.IndexFile, page, 0o644); err != nil {
			return err
		}
	}
	document := Stack(StackOptions{
		ProjectName:    r.ProjectName,
		Network:        r.Network,
		Port:           r.Port,
		DynamicDir:     r.state.DynamicDir,
		IndexDir:       r.state.IndexDir,
		LabelNamespace: r.labelNamespace,
		Home:           r.home,
	})
	// JSON is YAML; Compose reads it as such.
	if err := files.AtomicJSON(r.state.ComposeFile, document); err != nil {
		return err
	}
	// #55: Traefik's Docker-provider discovery is gone, so the index
	// container's own IndexRouteLabels — still stamped on it for
	// documentation — route nothing on their own any more. The index needs
	// the same kind of file-provider fragment a worktree's own routed
	// service gets, just not scoped to any worktree: see WriteIndexFragment.
	return WriteIndexFragment(r.state, r.ProjectName)
}

// containerID is by project name alone, with no stack file: this runs
// before the file exists, and a failure then can only mean Docker itself — a
// daemon that is not running, a socket this user cannot open, a `docker`
// that is not installed — never a stopped router. Compose given a project
// name and no file looks for none, so the consumer's own compose file in the
// current directory is never loaded; keep it that way (no cwd, no -f). "" means no container.
func (r *Router) containerID(ctx context.Context, service string) (string, error) {
	result, err := docker.Run(ctx, r.runner, []string{
		"compose", "--project-name", r.ProjectName, "ps", "--all", "--quiet", service,
	}, run.Spec{AllowFailure: true, Env: docker.ComposeEnvironment(r.environ), Timeout: r.timeouts.Probe})
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", &docker.UnavailableError{Detail: docker.Said(result.Stderr,
			fmt.Sprintf("`docker compose ps` exited %d without saying why", result.ExitCode))}
	}
	first, _, _ := strings.Cut(strings.TrimSpace(result.Stdout), "\n")
	return first, nil
}

// labelledWorktreeNetworks is LabelledWorktreeNetworks for this router's
// own network. Shared by Verify's network-membership check, Ensure's own
// reconnection step and its orphaned-fragment reconciliation, so the three
// can never independently drift onto different answers for "every network
// this router owns."
func (r *Router) labelledWorktreeNetworks(ctx context.Context) ([]string, error) {
	return LabelledWorktreeNetworks(ctx, r.runner, r.timeouts.Probe, r.Network)
}

// LabelledWorktreeNetworks lists, by name, every Docker network labelled
// RoutedByLabel=routerNetwork: one worktree's dedicated router network
// (layout.WorktreeRouterNetwork) per entry — never a fixed set, since a
// worktree network is created only by its own `up` and removed by its own
// `down` or `worktree remove`. The label is in the brand's own namespace
// whatever a project's label-namespace says, which is what lets this answer
// without any project config: doctor calls it too.
//
// --format alone, not with --quiet: which of the two wins when both are
// given differs between Docker CLI versions, and IDs would never match the
// names Verify compares against.
func LabelledWorktreeNetworks(ctx context.Context, runner run.Runner, timeout time.Duration, routerNetwork string) ([]string, error) {
	result, err := docker.Run(ctx, runner, []string{
		"network", "ls",
		"--filter", "label=" + RoutedByLabel + "=" + routerNetwork,
		"--format", "{{.Name}}",
	}, run.Spec{AllowFailure: true, Timeout: timeout})
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, &docker.UnavailableError{Detail: docker.Said(result.Stderr,
			fmt.Sprintf("`docker network ls` exited %d without saying why", result.ExitCode))}
	}
	var names []string
	for _, line := range strings.Split(result.Stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// networkConnectTolerated matches Docker's refusal to connect a container
// already attached to the network: an idempotent connect, from
// ConnectNetwork's point of view.
var networkConnectTolerated = regexp.MustCompile(`(?i)already exists in network|is already connected to network`)

// networkGoneOnReconnect matches Docker's refusal to connect to a network
// that no longer exists at all — tolerated only by reconnectWorktreeNetworks
// (see its own comment), never by ConnectNetwork generally: up.go's own
// direct call for a worktree's newly created network must still fail
// loudly if that network is unexpectedly missing, since there the override
// should always have created it first.
var networkGoneOnReconnect = regexp.MustCompile(`(?i)no such network|network .* not found`)

// networkDisconnectTolerated matches Docker's refusal to disconnect a
// container that was never attached, or a network or container that no
// longer exists: every one of them "nothing to disconnect", from
// DisconnectNetwork's point of view.
var networkDisconnectTolerated = regexp.MustCompile(`(?i)is not connected to (any )?network|no such network|no such container`)

// ConnectNetwork attaches the router container to network, tolerating an
// already-attached pair as success (Docker's own `network connect` is not
// otherwise idempotent) and a router that is not currently running as
// success too — there is nothing to connect until the router itself comes
// up, and Ensure's own reconnectWorktreeNetworks reconnects every labelled
// network on its next run regardless.
//
// `up` calls this after Ensure has already returned and released the
// router's lock — by design: a slow worktree `compose up` must not hold the
// router lock the whole time it runs (see up.go's own comment on
// registering and waiting for the router unlocked). That leaves a narrow
// window: if a different process force-recreates the router between Ensure
// returning and this call, the container id resolved here can belong to a
// container that is about to disappear, and this connect is then wasted.
// This is deliberately not fixed by holding the router lock across a
// worktree's whole `up` — it is self-healing instead: the next Ensure
// (any later `up` or `router up`) finds the labelled network unattached
// again and reconnects it via reconnectWorktreeNetworks.
func (r *Router) ConnectNetwork(ctx context.Context, network string) error {
	id, err := r.containerID(ctx, RouterService)
	if err != nil {
		return err
	}
	result, err := r.connectNetwork(ctx, network, id)
	if err != nil {
		return err
	}
	if result.ExitCode == 0 || networkConnectTolerated.MatchString(result.Stderr) {
		return nil
	}
	return clierr.Newf("Could not connect the shared router to network %s:\n%s",
		network, docker.Said(result.Stderr, strings.TrimSpace(result.Stdout)))
}

// connectNetwork is `docker network connect`'s raw outcome, factored out of
// ConnectNetwork so reconnectWorktreeNetworks can inspect result.Stderr
// itself and tolerate one more refusal ConnectNetwork does not (see
// networkGoneOnReconnect), rather than matching against a caller's already
// clierr-formatted message. id empty (the router is not running) answers a
// clean no-op, matching ConnectNetwork's own "nothing to connect" tolerance.
func (r *Router) connectNetwork(ctx context.Context, network, id string) (run.Result, error) {
	if id == "" {
		return run.Result{}, nil
	}
	return docker.Run(ctx, r.runner, []string{"network", "connect", network, id},
		run.Spec{AllowFailure: true, Timeout: r.timeouts.Probe})
}

// DisconnectNetwork detaches the router container from network, tolerating
// there being nothing to disconnect — the router not running, the network
// already gone, or the endpoint already gone — as success. The mirror of
// ConnectNetwork; `down` and `worktree remove` call it before their own
// worktree's network can be removed (see mutate.go and remove.go for why
// the order matters and how a real failure here is handled).
func (r *Router) DisconnectNetwork(ctx context.Context, network string) error {
	id, err := r.containerID(ctx, RouterService)
	if err != nil {
		return err
	}
	if id == "" {
		return nil
	}
	result, err := docker.Run(ctx, r.runner, []string{"network", "disconnect", network, id},
		run.Spec{AllowFailure: true, Timeout: r.timeouts.Probe})
	if err != nil {
		return err
	}
	if result.ExitCode == 0 || networkDisconnectTolerated.MatchString(result.Stderr) {
		return nil
	}
	return clierr.Newf("Could not disconnect the shared router from network %s:\n%s",
		network, docker.Said(result.Stderr, strings.TrimSpace(result.Stdout)))
}

// reconnectWorktreeNetworks reconnects the router to every worktree network
// labelled as its own. Runs unconditionally on every Ensure, not only after
// a recreate: a router that never lost an attachment finds every connect
// already-attached and so a fast no-op; a router that was just
// force-recreated — which drops every attachment `compose up` itself did
// not create — gets every worktree network back.
func (r *Router) reconnectWorktreeNetworks(ctx context.Context) error {
	networks, err := r.labelledWorktreeNetworks(ctx)
	if err != nil {
		return err
	}
	id, err := r.containerID(ctx, RouterService)
	if err != nil {
		return err
	}
	for _, network := range networks {
		result, err := r.connectNetwork(ctx, network, id)
		if err != nil {
			return err
		}
		if result.ExitCode == 0 || networkConnectTolerated.MatchString(result.Stderr) {
			continue
		}
		// A network this loop just listed as labelled can already be gone
		// by the time this specific connect runs (found in review, round 2
		// of #55): a concurrent `down` or `worktree remove` may have torn
		// it down between labelledWorktreeNetworks reading it and here —
		// its own RoutedByLabel persists until removal actually finishes
		// (see RoutedByLabel's own comment), so the label list is never
		// more than a snapshot. Nothing to do: a gone network has nothing
		// left to route through it either, and the next Ensure's own next
		// listing will simply not see it again. Tolerated only here, not by
		// ConnectNetwork generally — see networkGoneOnReconnect's own
		// comment for why.
		if networkGoneOnReconnect.MatchString(result.Stderr) {
			continue
		}
		return clierr.Newf("Could not connect the shared router to network %s:\n%s",
			network, docker.Said(result.Stderr, strings.TrimSpace(result.Stdout)))
	}
	return nil
}

// reconcileOrphanedFragments removes every dynamic-configuration fragment
// OrphanedFragments finds: its worktree's own router network is gone, so
// nothing the fragment names is reachable any more — torn down by a `down`
// or `worktree remove` that could not also remove the fragment, or by hand.
// doctor reports the very same condition without ever removing anything
// (see its own routes check); this is the repair half of that same story.
func (r *Router) reconcileOrphanedFragments(ctx context.Context) error {
	fragments, err := ListFragments(r.state)
	if err != nil {
		return err
	}
	if len(fragments) == 0 {
		return nil
	}
	networks, err := r.labelledWorktreeNetworks(ctx)
	if err != nil {
		return err
	}
	type worktreeKey struct{ repositoryID, worktreeID string }
	removed := map[worktreeKey]bool{}
	for _, fragment := range OrphanedFragments(fragments, networks) {
		key := worktreeKey{fragment.RepositoryID, fragment.WorktreeID}
		if removed[key] {
			continue
		}
		removed[key] = true
		if err := RemoveWorktreeFragments(r.state, fragment.RepositoryID, fragment.WorktreeID); err != nil {
			return err
		}
	}
	return nil
}

// OrphanedFragments is every fragment whose worktree's own router network
// (layout.WorktreeRouterNetwork) is not among networks, the router's
// labelled worktree networks (LabelledWorktreeNetworks). The network, not a
// container, is the proof of life: its name is derived from the fragment's
// own IDs and its label is in the brand's fixed namespace, so the check
// holds for a project that overrides its label-namespace — whose containers
// carry only that project's own ownership labels — and needs no registry,
// which is per control home while this directory is machine-wide (see
// internal/layout's RouterStateDir comment).
// A fragment is only reachable through that network anyway, so a fragment
// without one routes nowhere. Pure; shared by Ensure's repair and doctor's
// report, so the two cannot drift onto different definitions of "orphaned".
func OrphanedFragments(fragments []FragmentInfo, networks []string) []FragmentInfo {
	live := make(map[string]bool, len(networks))
	for _, network := range networks {
		live[network] = true
	}
	var orphaned []FragmentInfo
	for _, fragment := range fragments {
		if !live[layout.WorktreeRouterNetwork(fragment.RepositoryID, fragment.WorktreeID)] {
			orphaned = append(orphaned, fragment)
		}
	}
	return orphaned
}

// networkNotFoundTolerated matches Docker's refusal to remove a network
// that is already gone: nothing left to clean up, from
// removeLegacyDockerAPINetwork's point of view.
var networkNotFoundTolerated = regexp.MustCompile(`(?i)no such network|not found`)

// removeLegacyDockerAPINetwork best-effort removes the "docker-api" network
// a router below FileProviderSinceProtocol declared with no explicit name
// (stack.go's own pre-protocol-5 internalNetwork carried none), found by the
// project and network-key labels Compose itself always stamps rather than a
// guessed name — the literal string Compose derived depends on which
// separator ("_" or "-") the Compose version in use at the time chose.
// Nothing else in this build reads or writes either label; they exist only
// so this one cleanup can find what it is looking for without guessing.
// Ensure calls it on every run, ungated, so a failed cleanup is retried on
// the next run — see its caller.
func (r *Router) removeLegacyDockerAPINetwork(ctx context.Context) error {
	listed, err := docker.Run(ctx, r.runner, []string{
		"network", "ls",
		"--filter", "label=com.docker.compose.project=" + r.ProjectName,
		"--filter", "label=com.docker.compose.network=docker-api",
		"--format", "{{.ID}}",
	}, run.Spec{AllowFailure: true, Timeout: r.timeouts.Probe})
	if err != nil {
		return err
	}
	if listed.ExitCode != 0 {
		return &docker.UnavailableError{Detail: docker.Said(listed.Stderr,
			fmt.Sprintf("`docker network ls` exited %d without saying why", listed.ExitCode))}
	}
	for _, id := range strings.Fields(listed.Stdout) {
		result, err := docker.Run(ctx, r.runner, []string{"network", "rm", id}, run.Spec{AllowFailure: true, Timeout: r.timeouts.Probe})
		if err != nil {
			return err
		}
		if result.ExitCode == 0 || networkNotFoundTolerated.MatchString(result.Stderr) {
			continue
		}
		return clierr.Newf("Could not remove network %s:\n%s", id, docker.Said(result.Stderr, strings.TrimSpace(result.Stdout)))
	}
	return nil
}

// inspect is `docker inspect` for one container, parsed as ContainerInspect.
// Returns nil, nil rather than an error when it cannot be read — gone, not
// yet started, or an unparseable answer — since none of that is fatal to a
// caller that is only checking whether the container matches what was asked for.
func (r *Router) inspect(ctx context.Context, container string) (*ContainerInspect, error) {
	result, err := docker.Run(ctx, r.runner, []string{"inspect", container}, run.Spec{AllowFailure: true, Timeout: r.timeouts.Probe})
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, nil
	}
	var parsed []ContainerInspect
	if err := json.Unmarshal([]byte(result.Stdout), &parsed); err != nil || len(parsed) == 0 {
		return nil, nil
	}
	return &parsed[0], nil
}

// jsMaxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER (2^53 - 1).
const jsMaxSafeInteger = 1<<53 - 1

// toProtocol parses text as a protocol number the way the TypeScript build's
// own toProtocol does: digits only ("0x10" and "1e3" are corruption, not
// protocols), and a safe, non-negative integer of at least 1.
func toProtocol(text string) (int, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, false
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] < '0' || trimmed[i] > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseUint(trimmed, 10, 64)
	if err != nil || value > jsMaxSafeInteger || value < 1 {
		return 0, false
	}
	return int(value), true
}

// errnoNames are the errno symbols this package's own read failures might
// plausibly carry, the way Node's error.code names them.
var errnoNames = map[syscall.Errno]string{
	syscall.EACCES:  "EACCES",
	syscall.EPERM:   "EPERM",
	syscall.EISDIR:  "EISDIR",
	syscall.ENOTDIR: "ENOTDIR",
	syscall.ELOOP:   "ELOOP",
}

// errnoName is err's errno symbol (from errnoNames), or its own message when
// it does not wrap a plain errno.
func errnoName(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if name, ok := errnoNames[errno]; ok {
			return name
		}
		return errno.Error()
	}
	return err.Error()
}

// readOptional is the file's text, or "", false when it is simply not there.
func readOptional(file string) (string, bool, error) {
	data, err := os.ReadFile(file)
	if err == nil {
		return string(data), true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	return "", false, clierr.Newf("%s could not be read (%s). Fix its permissions, or remove it and run this again.",
		file, errnoName(err))
}

// recordedProtocol is what this control home last brought up and verified,
// if it has recorded anything legible.
func (r *Router) recordedProtocol() (int, bool, error) {
	text, present, err := readOptional(r.state.ProtocolFile)
	if err != nil {
		return 0, false, err
	}
	if !present {
		return 0, false, nil
	}
	recorded, ok := toProtocol(text)
	if !ok {
		r.logger.Warn(fmt.Sprintf(
			"Ignoring %s: it does not name a router protocol. This run takes the previous protocol from the "+
				"router itself, if it is still there.",
			r.state.ProtocolFile))
	}
	return recorded, ok, nil
}

// protocolOf is the protocol label of the router container, running or not,
// if it carries one.
func (r *Router) protocolOf(inspected ContainerInspect) (int, bool, error) {
	value, present := inspected.Config.Labels[r.protocolLabelName()]
	if !present {
		return 0, false, nil
	}
	protocol, ok := toProtocol(value)
	if !ok {
		return 0, false, clierr.Newf(
			"The shared router carries an invalid protocol label: %s. Remove the router with '%s router down' and run this again.",
			value, r.commandNameOr())
	}
	return protocol, true, nil
}

// previousProtocol settles what protocol the router was at before this run:
// refuses a downgrade, announces an upgrade, and records what it found so a
// failed start cannot erase it. Returns that protocol, or ok=false when this
// machine has no router history at all.
func (r *Router) previousProtocol(ctx context.Context) (previous int, ok bool, err error) {
	recorded, recordedOK, err := r.recordedProtocol()
	if err != nil {
		return 0, false, err
	}
	id, err := r.containerID(ctx, RouterService)
	if err != nil {
		return 0, false, err
	}
	var container *ContainerInspect
	if id != "" {
		if container, err = r.inspect(ctx, id); err != nil {
			return 0, false, err
		}
		if container == nil {
			return 0, false, clierr.New("The shared router exists but could not be inspected.")
		}
	}

	var running int
	var runningOK, serving bool
	if container != nil {
		if running, runningOK, err = r.protocolOf(*container); err != nil {
			return 0, false, err
		}
		// `ps --all` also finds a container that was created and never
		// started — a failed `up`'s leftover, which has served nothing.
		serving = container.State.Status == "running"
	}

	command := r.commandNameOr()
	alsoRecorded := ""
	if recordedOK && recorded > Protocol {
		alsoRecorded = fmt.Sprintf(" and this control home's record with `rm %s`", text.ShellQuote(r.state.ProtocolFile))
	}
	// Refuse on the newest evidence there is, each case naming the remedy
	// that actually frees the router. The router is machine-wide while the
	// record is per control home, so either can be the newer one.
	if runningOK && running > Protocol {
		return 0, false, clierr.Newf(
			"The shared router is at protocol %d; this version speaks %d. Upgrade rather than downgrading the "+
				"router the other worktrees on this machine expect, or hand the router to this one: remove it with '%s router down'%s.",
			running, Protocol, command, alsoRecorded)
	}
	if recordedOK && recorded > Protocol {
		return 0, false, clierr.Newf(
			"This control home last recorded the shared router at protocol %d; this version speaks %d. "+
				"Run that version again, or remove the record with `rm %s` to hand the router to this one.",
			recorded, Protocol, text.ShellQuote(r.state.ProtocolFile))
	}

	// Announce on the oldest evidence. A record ahead of an older running
	// router means another control home downgraded it; a record behind a
	// container is a failed start's leftover.
	evidence := make([]int, 0, 2)
	if recordedOK {
		evidence = append(evidence, recorded)
	}
	if runningOK {
		evidence = append(evidence, running)
	}
	if len(evidence) == 0 {
		return 0, false, nil
	}
	previous = slices.Min(evidence)

	// Made durable before anything overwrites the evidence: write() is about
	// to stamp the new protocol into the stack file, and a start that fails
	// afterwards leaves a container carrying it too.
	if !recordedOK || recorded != previous {
		if err := files.AtomicWrite(r.state.ProtocolFile, []byte(strconv.Itoa(previous)+"\n"), 0o600); err != nil {
			return 0, false, clierr.Newf(
				"Could not record the router protocol in %s: %s. Fix its permissions, or remove it and run this again.",
				r.state.ProtocolFile, err)
		}
	}
	if previous < Protocol {
		if serving && running >= Protocol {
			r.logger.Info(fmt.Sprintf("The shared router is already at protocol %d, but this control home last saw %d.", Protocol, previous))
		} else {
			r.logger.Info(fmt.Sprintf("Upgrading the shared router from protocol %d to %d.", previous, Protocol))
		}
	}
	return previous, true, nil
}

// FileProviderSinceProtocol is the protocol that moved routing from
// Traefik's Docker-provider discovery to file-provider fragments berth
// itself writes and removes, on the worktree's own dedicated network (#55).
// It subsumes RoutedBySinceProtocol: a router older than protocol 5 either
// discovered every container Traefik could see (older than
// RoutedBySinceProtocol) or discovered only containers carrying the
// routed-by label (RoutedBySinceProtocol through 4) — either way, a
// worktree last brought up under an older protocol carries no fragment and
// is not attached to its own per-worktree network, so it is unrouted until
// its next `up` just the same. One threshold, one warning: see
// noteUnroutedAfterUpgrade.
const FileProviderSinceProtocol = 5

// noteUnroutedAfterUpgrade is said only after the upgrade is a fact: a
// router from before FileProviderSinceProtocol routed a worktree by
// discovering its containers, one way or another; the new one routes
// nothing a worktree's own `up` has not written a file-provider fragment
// and joined to its per-worktree network for. Each worktree's next `up`
// does both.
func (r *Router) noteUnroutedAfterUpgrade(previous int, hadHistory bool) {
	if !hadHistory || previous >= FileProviderSinceProtocol {
		return
	}
	r.logger.Warn(fmt.Sprintf("Worktrees already up elsewhere stay unrouted until their next '%s up'.", r.commandNameOr()))
}

/* ---- verification: every property the stack asked for, checked on the running container */

// hostNamespaceModes are the HostConfig fields a container shares with the
// host when set to "host" rather than left to its own namespace: PID, IPC,
// network and UTS. Any one of them turns off the isolation the README claims
// for the router stack, so commonProblems checks all four the same way.
func hostNamespaceModes(hc InspectHostConfig) []string {
	return []string{hc.PidMode, hc.IpcMode, hc.NetworkMode, hc.UTSMode}
}

// unconfined reports whether opt is SecurityOpt's own "<profile>=unconfined"
// entry for profile ("seccomp" or "apparmor"), matching Docker's own
// lowercase spelling.
func unconfined(opt, profile string) bool {
	return strings.EqualFold(opt, profile+"=unconfined")
}

// commonProblems is every check shared by all three services: ownership,
// the component and protocol labels, and the hardening every container
// carries — including, as of #55/#74, that none of it can be bypassed:
// not privileged, no shared host namespace, no devices, and neither
// seccomp nor AppArmor left unconfined.
func (r *Router) commonProblems(inspected ContainerInspect, service string) []string {
	var problems []string
	labels := inspected.Config.Labels
	if labels["com.docker.compose.project"] != r.ProjectName || labels["com.docker.compose.service"] != service {
		problems = append(problems, service+": not the container Compose started for this project")
	}
	if labels[r.componentLabel()] != service {
		problems = append(problems, service+": component label")
	}
	if labels[r.protocolLabelName()] != strconv.Itoa(Protocol) {
		problems = append(problems, service+": protocol label")
	}
	if inspected.State.Status != "running" {
		problems = append(problems, service+": not running")
	}
	if inspected.State.Health.Status != "healthy" {
		problems = append(problems, service+": not healthy")
	}
	if !inspected.HostConfig.ReadonlyRootfs {
		problems = append(problems, service+": root filesystem is writable")
	}
	if !slices.Contains(inspected.HostConfig.SecurityOpt, "no-new-privileges:true") {
		problems = append(problems, service+": no-new-privileges is not set")
	}
	if !slices.Contains(inspected.HostConfig.CapDrop, "ALL") {
		problems = append(problems, service+": capabilities are not dropped")
	}
	if inspected.HostConfig.Privileged {
		problems = append(problems, service+": privileged")
	}
	if slices.Contains(hostNamespaceModes(inspected.HostConfig), "host") {
		problems = append(problems, service+": shares a host namespace")
	}
	if len(inspected.HostConfig.Devices) > 0 {
		problems = append(problems, service+": devices are attached")
	}
	for _, opt := range inspected.HostConfig.SecurityOpt {
		if unconfined(opt, "seccomp") {
			problems = append(problems, service+": seccomp is unconfined")
		}
		if unconfined(opt, "apparmor") {
			problems = append(problems, service+": apparmor is unconfined")
		}
	}
	return problems
}

// sameSet reports whether actual and expected hold the same strings,
// ignoring order and duplicates the way a JavaScript sorted-join comparison
// treats them (a repeated entry counts once either way, since it lands on
// the same joined position).
func sameSet(actual, expected []string) bool {
	a := slices.Clone(actual)
	e := slices.Clone(expected)
	sort.Strings(a)
	sort.Strings(e)
	return slices.Equal(a, e)
}

// sortedPortKeys is otherPorts' and networks' own key order, sorted for a
// determinism encoding/json's map iteration does not give: a real fixture
// never carries more than one, so this never diverges from the TypeScript
// build's JSON-insertion order in anything a test exercises.
func sortedPortKeys(ports map[string][]InspectPortBinding) []string {
	keys := make([]string, 0, len(ports))
	for key := range ports {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// sortedNetworkKeys is networks' own keys, sorted for the same reason
// sortedPortKeys is.
func sortedNetworkKeys(networks map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(networks))
	for key := range networks {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// routerProblems is every way the router container can differ from what the
// stack asked for: its port binding, networks, command line, image,
// capabilities and mount, on top of commonProblems.
func (r *Router) routerProblems(ctx context.Context, inspected ContainerInspect) []string {
	problems := r.commonProblems(inspected, RouterService)
	bindings := inspected.NetworkSettings.Ports["80/tcp"]
	if len(bindings) != 1 || bindings[0].HostIP != "127.0.0.1" || bindings[0].HostPort != strconv.Itoa(r.Port) {
		problems = append(problems, fmt.Sprintf("router: port 80 is not published only on 127.0.0.1:%d", r.Port))
	}
	var otherPorts []string
	for _, port := range sortedPortKeys(inspected.NetworkSettings.Ports) {
		if port != "80/tcp" && len(inspected.NetworkSettings.Ports[port]) > 0 {
			otherPorts = append(otherPorts, port)
		}
	}
	if len(otherPorts) > 0 {
		problems = append(problems, fmt.Sprintf("router: publishes unexpected ports (%s)", strings.Join(otherPorts, ", ")))
	}
	networks := sortedNetworkKeys(inspected.NetworkSettings.Networks)
	expected, err := r.labelledWorktreeNetworks(ctx)
	if err != nil {
		problems = append(problems, fmt.Sprintf("router: could not list this router's worktree networks: %s", err))
	} else {
		expected = append(expected, r.Network)
		// Only an attached network outside expected, or a missing r.Network
		// itself, is a real problem (found in review, round 2 of #55): a
		// labelled worktree network that exists but is not attached yet —
		// the window between a worktree's own `up` creating and labelling
		// it and this router's next Ensure calling ConnectNetwork, or the
		// window during `down`/`worktree remove`'s teardown before the
		// network itself is actually gone — is not a stack problem.
		// Ensure's own reconnectWorktreeNetworks (run before Verify, every
		// time) is what closes the first gap; treating either as drift here
		// would make Ensure force-recreate the shared router over a race it
		// cannot win by recreating, and fail doctor/status for the same
		// non-problem. sameSet's exact-membership comparison, still right
		// for the index's own single fixed network below, is wrong here.
		var foreign []string
		for _, network := range networks {
			if !slices.Contains(expected, network) {
				foreign = append(foreign, network)
			}
		}
		if len(foreign) > 0 || !slices.Contains(networks, r.Network) {
			joined := strings.Join(networks, ", ")
			if joined == "" {
				joined = "(none)"
			}
			wantJoined := strings.Join(expected, ", ")
			problems = append(problems, fmt.Sprintf("router: attached to networks %s instead of %s", joined, wantJoined))
		}
	}
	if strings.Join(inspected.Config.Cmd, "\x00") != strings.Join(RouterCommand(), "\x00") {
		problems = append(problems, "router: command line differs")
	}
	if inspected.Config.Image != RouterImage {
		problems = append(problems, "router: image is not the pinned digest")
	}
	if !slices.Equal(inspected.Config.Entrypoint, RouterImageEntrypoint) {
		problems = append(problems, "router: entrypoint differs from the pinned image")
	}
	if !sameSet(inspected.HostConfig.CapAdd, []string{"CAP_NET_BIND_SERVICE"}) {
		problems = append(problems, "router: capabilities beyond NET_BIND_SERVICE")
	}
	var dynamic *InspectMount
	for i, mount := range inspected.Mounts {
		if mount.Destination == DynamicConfigMount {
			dynamic = &inspected.Mounts[i]
			break
		}
	}
	if len(inspected.Mounts) != 1 || dynamic == nil || dynamic.Type != "bind" || dynamic.Source != r.state.DynamicDir || dynamic.RW {
		problems = append(problems, "router: mounts are not exactly the read-only dynamic-configuration directory")
	}
	return problems
}

// indexProblems is every way the index container can differ from what the
// stack asked for: its image, command, routing labels, network, capabilities
// and mount, on top of commonProblems.
func (r *Router) indexProblems(inspected ContainerInspect) []string {
	problems := r.commonProblems(inspected, IndexService)
	if inspected.Config.Image != IndexImage {
		problems = append(problems, "index: image is not the pinned digest")
	}
	if !slices.Equal(inspected.Config.Entrypoint, IndexImageEntrypoint) {
		problems = append(problems, "index: entrypoint differs from the pinned image")
	}
	if strings.Join(inspected.Config.Cmd, "\x00") != strings.Join(IndexCommand, "\x00") {
		problems = append(problems, "index: command line differs")
	}
	for _, entry := range IndexRouteLabels(r.Network) {
		if inspected.Config.Labels[entry.key] != entry.value {
			problems = append(problems, "index: routing labels differ")
			break
		}
	}
	networks := sortedNetworkKeys(inspected.NetworkSettings.Networks)
	if !sameSet(networks, []string{r.Network}) {
		problems = append(problems, "index: attached to a network other than the shared one")
	}
	for _, bindings := range inspected.NetworkSettings.Ports {
		if len(bindings) > 0 {
			problems = append(problems, "index: publishes a port")
			break
		}
	}
	if len(inspected.HostConfig.CapAdd) != 0 {
		problems = append(problems, "index: has added capabilities")
	}
	var served *InspectMount
	for i, mount := range inspected.Mounts {
		if mount.Destination == IndexMount {
			served = &inspected.Mounts[i]
			break
		}
	}
	if len(inspected.Mounts) != 1 || served == nil || served.Type != "bind" || served.Source != r.state.IndexDir || served.RW {
		problems = append(problems, "index: mounts are not exactly the read-only index directory")
	}
	return problems
}

// Verify is everything that differs between the running router and what this
// version would start. Returns a *docker.UnavailableError when Docker cannot say.
func (r *Router) Verify(ctx context.Context) ([]string, error) {
	services := []string{RouterService, IndexService}
	ids := make([]string, len(services))
	for i, service := range services {
		id, err := r.containerID(ctx, service)
		if err != nil {
			return nil, err
		}
		ids[i] = id
	}
	// Both gone is a router that is not running. One gone is a router that
	// is still bound to its port and still routing, with a hole in it —
	// reporting that as "not running" would tell an operator the opposite of
	// what is true.
	gone := true
	for _, id := range ids {
		if id != "" {
			gone = false
			break
		}
	}
	if gone {
		return []string{notRunning}, nil
	}
	var missing []string
	for i, id := range ids {
		if id == "" {
			missing = append(missing, services[i]+": container is missing")
		}
	}
	if len(missing) > 0 {
		return missing, nil
	}
	inspected := make([]*ContainerInspect, len(ids))
	for i, id := range ids {
		doc, err := r.inspect(ctx, id)
		if err != nil {
			return nil, err
		}
		inspected[i] = doc
	}
	if inspected[0] == nil || inspected[1] == nil {
		return []string{"the router could not be inspected"}, nil
	}
	var problems []string
	problems = append(problems, r.routerProblems(ctx, *inspected[0])...)
	problems = append(problems, r.indexProblems(*inspected[1])...)
	return problems, nil
}

// Ownership is what the running router's own container says about which
// berth root's Ensure most recently started or recreated it (#82).
type Ownership struct {
	// Running is false when the router container does not exist, or exists
	// but is not running; Labelled and Home are then meaningless — a router
	// serving nobody is nothing to own.
	Running bool
	// Labelled is false when the running container carries no
	// RouterHomeLabel at all (or an empty one): a router started by a build
	// from before this label existed. A container that cannot be inspected
	// is an error from Owner, never an unlabelled router. Home is ""
	// in that case; Ensure treats it as adoptable rather than foreign (see
	// checkOwnership), the same way an unparseable protocol label falls
	// through to other evidence rather than refusing outright.
	Labelled bool
	// Home is the berth root RouterHomeLabel recorded on the running
	// container, when Labelled.
	Home string
}

// Owner reports Ownership for the running router, read from the router
// container's own RouterHomeLabel — never the index container's copy of the
// same label, since Stack always stamps both together and Ensure never
// recreates one without the other.
func (r *Router) Owner(ctx context.Context) (Ownership, error) {
	id, err := r.containerID(ctx, RouterService)
	if err != nil {
		return Ownership{}, err
	}
	if id == "" {
		return Ownership{}, nil
	}
	inspected, err := r.inspect(ctx, id)
	if err != nil {
		return Ownership{}, err
	}
	if inspected == nil {
		// Unparseable or unreadable: not "no label" but "no answer", and an
		// owner that cannot be read is not one to adopt over — the same
		// refusal previousProtocol makes of the same container a moment
		// later, so a transient inspect failure never turns a foreign router
		// into an adoptable one.
		return Ownership{}, clierr.New("The shared router exists but could not be inspected.")
	}
	// A container that exists but is not running serves nobody, so there is
	// nothing to protect: the refusal is for a router in use (containerID
	// lists stopped containers too, as previousProtocol needs it to). It
	// reads as no router, and the next start takes it over the way a fresh
	// start would.
	if inspected.State.Status != "running" {
		return Ownership{}, nil
	}
	// An empty value counts as no label: Stack stamps "" for a caller with
	// no berth root to give (a test's own router, mainly), and an empty
	// owner is nobody's to protect the router from.
	home, present := inspected.Config.Labels[RouterHomeLabel]
	if !present || home == "" {
		return Ownership{Running: true}, nil
	}
	return Ownership{Running: true, Labelled: true, Home: home}, nil
}

// checkOwnership refuses to touch a running router that belongs to a
// different berth root (#82): before this check, two berth roots each held
// their own router lock over their own state but drove the same
// machine-wide container, so a second root's Ensure force-recreated
// whatever the first had just started, with no warning at all
// (see docs/design/locks.md). A router
// carrying no ownership label predates RouterHomeLabel altogether — there is
// no foreign owner to protect it from — so it is adopted instead, logged
// rather than silently taken over. The log line also says what follows:
// the label is part of the container's own config, so the plain `compose
// up` Ensure runs next recreates the container once to carry it, before
// Verify ever looks — a recreate that would otherwise have no line of its
// own, since it is Compose's own reconciliation, not Verify's.
func (r *Router) checkOwnership(ctx context.Context) error {
	owner, err := r.Owner(ctx)
	if err != nil {
		return err
	}
	if !owner.Running {
		return nil
	}
	if !owner.Labelled {
		r.logger.Info("The running router carries no berth-root ownership label; it predates this check. Adopting it: this start recreates it once to record this berth root as its owner.")
		return nil
	}
	if owner.Home == r.home {
		return nil
	}
	return clierr.Newf(
		"The shared router belongs to a different berth root (%s). Hand it to this one: stop it with '%s router down', then run this again.",
		owner.Home, r.commandNameOr())
}

// Ensure is idempotent: starts the router if needed, verifies it, and fails
// loudly if it cannot be made right. The caller holds the router's lock
// around this call (see internal/commands' routerLockScope and
// lifecycle.Requests.Router). As of #55 it also reconnects every labelled
// per-worktree router network (reconnectWorktreeNetworks) and removes
// fragments whose worktree's router network is gone
// (reconcileOrphanedFragments).
//
// secureState runs first, before previousProtocol's own possible write of
// the protocol record: a symlinked state directory must be refused before
// anything — this call included — ever touches whatever it points to.
// checkOwnership runs next, before previousProtocol reads or writes
// anything else: a router belonging to a different berth root is refused
// outright rather than force-recreated (#82).
func (r *Router) Ensure(ctx context.Context) error {
	if err := secureState(r.state); err != nil {
		return err
	}
	// Before previousProtocol: a router belonging to a different berth root
	// must be refused outright, not read for its protocol history — that
	// history is not this berth root's to trust or overwrite (#82).
	if err := r.checkOwnership(ctx); err != nil {
		return err
	}
	previous, hadHistory, err := r.previousProtocol(ctx)
	if err != nil {
		return err
	}
	// What port a router already running publishes, read before anything
	// changes it, so a port change can be announced once it is a fact
	// (notePortChange, #79). Best effort: an error here is the same class
	// previousProtocol just handled, and a notice is not worth failing for.
	priorPort, wasRunning, _ := r.publishedPort(ctx)
	if err := r.write(time.Now()); err != nil {
		return err
	}
	if err := r.start(ctx, nil); err != nil {
		return err
	}
	// Runs before Verify, not after: Verify's own network-membership check
	// (routerProblems) now expects the router attached to exactly this
	// router's own network plus every currently labelled per-worktree
	// network, and `compose up` above only ever reconnects networks the
	// compose file itself declares — never one a worktree's own `up`
	// created and labelled separately. Reconnecting first is what makes
	// that expectation actually satisfiable the moment a new worktree
	// network appears, rather than reporting it as a problem worth a
	// recreate (which would not fix it) on every single Ensure until the
	// worktree's own `up` happens to call ConnectNetwork again. A router
	// that never lost an attachment finds this a fast no-op (Docker's own
	// connect is idempotent, tolerated the same way ConnectNetwork always
	// tolerates it); one that was just force-recreated by a concurrent
	// process — which drops every attachment `compose up` itself did not
	// create — gets every worktree network back before anything judges it.
	if err := r.reconnectWorktreeNetworks(ctx); err != nil {
		return err
	}
	problems, err := r.Verify(ctx)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		r.logger.Warn(fmt.Sprintf("The shared router is not configured as expected (%s); recreating it once.", strings.Join(problems, "; ")))
		if err := r.start(ctx, []string{"--force-recreate"}); err != nil {
			return err
		}
		// A force-recreate drops every attachment `compose up` itself did
		// not create, including every worktree network just reconnected
		// above — reconnect again before re-verifying, for the same reason.
		if err := r.reconnectWorktreeNetworks(ctx); err != nil {
			return err
		}
		remaining, err := r.Verify(ctx)
		if err != nil {
			return err
		}
		if len(remaining) > 0 {
			return clierr.Newf("The shared router could not be brought to its expected configuration:\n  %s", strings.Join(remaining, "\n  "))
		}
	}
	// A router recreated across the protocol-5 boundary leaves its
	// old "docker-api" network behind: `compose up --remove-orphans` above
	// already dropped the now-undeclared *container*, but Compose never
	// prunes a network just because the current file no longer declares
	// it. Run unconditionally on every Ensure, the same as
	// reconcileOrphanedFragments below, not gated on hadHistory && previous
	// < FileProviderSinceProtocol (found in review, round 3 of #55: a first
	// cut's own gate made a transient failure here permanent — this Ensure
	// still writes Protocol to the record a few lines down regardless of
	// this call's own error, so the very next Ensure would have read
	// previous == Protocol and never tried again, leaking the network
	// forever over one bad `docker network ls`). removeLegacyDockerAPINetwork
	// is a cheap, label-filtered lookup that tolerates finding nothing, the
	// same shape as reconcileOrphanedFragments's own self-healing best
	// effort — so nothing is lost by asking every time instead of once.
	if err := r.removeLegacyDockerAPINetwork(ctx); err != nil {
		if ctx.Err() != nil {
			return err
		}
		r.logger.Warn(fmt.Sprintf("Could not remove the retired docker-api network: %s", err))
	}
	// Best effort: fragments that cannot be reconciled this run (a transient
	// `docker network ls` failure, an unreadable fragment) must not turn an
	// otherwise healthy router start into a failure, and nothing is removed
	// unless the network list was read. A cancellation is not such a
	// failure: it still ends the command. doctor reports the same condition
	// without ever repairing it; this is Ensure's repair half.
	if err := r.reconcileOrphanedFragments(ctx); err != nil {
		if ctx.Err() != nil {
			return err
		}
		r.logger.Warn(fmt.Sprintf("Could not reconcile orphaned route fragments: %s", err))
	}
	// The warning is about what Verify just established, so it is said even
	// if recording the new protocol fails.
	r.noteUnroutedAfterUpgrade(previous, hadHistory)
	r.notePortChange(priorPort, wasRunning)
	if !hadHistory || previous != Protocol {
		// A record that will not write costs a repeated announcement next
		// time, not a router: never fail a verified `up` over it.
		if err := files.AtomicWrite(r.state.ProtocolFile, []byte(strconv.Itoa(Protocol)+"\n"), 0o600); err != nil {
			r.logger.Warn(fmt.Sprintf("Could not record the router protocol in %s: %s", r.state.ProtocolFile, err))
		}
	}
	return nil
}

// start runs `compose up --detach --wait --remove-orphans`, plus extra
// (`--force-recreate` on a recreate), reporting a failed start in terms of the
// port most likely to blame.
func (r *Router) start(ctx context.Context, extra []string) error {
	args := append([]string{"up", "--detach", "--wait", "--remove-orphans"}, extra...)
	result, err := r.compose(ctx, args, run.Spec{AllowFailure: true, Timeout: r.timeouts.Start})
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(result.Stdout)
		}
		settingsFile := r.settingsPath
		if settingsFile == "" {
			settingsFile = brand.SettingsFile
		}
		// Said only when the port is one a rootless daemon could never publish
		// and the failure reads as a refusal rather than a busy port: an
		// "address already in use" on port 80 is somebody else's server, not a
		// missing capability.
		rootless := ""
		if r.Port < 1024 && rootlessRefusal.MatchString(detail) {
			rootless = " If your Docker daemon runs rootless, it cannot publish a port below 1024 at all; pick one above 1023."
		}
		return clierr.Newf(
			"The shared router failed to start. Make sure 127.0.0.1:%d is free, or choose another port with %s_ROUTER_PORT or [router] port in %s.%s\n%s",
			r.Port, brand.EnvPrefix, settingsFile, rootless, detail)
	}
	return nil
}

// publishedPort reports the host port the running router publishes for its
// own port 80, read off its container: 0 when it publishes none that can be
// read, and running false when there is no container at all. A container
// that cannot be inspected counts as running with an unknown port — the
// same "no evidence either way" previousProtocol settles for.
func (r *Router) publishedPort(ctx context.Context) (port int, running bool, err error) {
	id, err := r.containerID(ctx, RouterService)
	if err != nil {
		return 0, false, err
	}
	if id == "" {
		return 0, false, nil
	}
	inspected, err := r.inspect(ctx, id)
	if err != nil {
		return 0, false, err
	}
	if inspected == nil {
		return 0, true, nil
	}
	for _, binding := range inspected.NetworkSettings.Ports["80/tcp"] {
		if n, err := strconv.Atoi(binding.HostPort); err == nil && n > 0 {
			return n, true, nil
		}
	}
	return 0, true, nil
}

// PublishedPort is publishedPort for callers outside this package: doctor
// compares it with the port it resolved (#79) and warns when they differ.
func (r *Router) PublishedPort(ctx context.Context) (port int, running bool, err error) {
	return r.publishedPort(ctx)
}

// notePortChange is said once the router is verified on r.Port: a router
// that was running on another port before this Ensure has just been
// recreated on the new one (Verify's own port check made that happen), so
// URLs already open — the index page's included — still name the old port.
func (r *Router) notePortChange(priorPort int, wasRunning bool) {
	if !wasRunning || priorPort == 0 || priorPort == r.Port {
		return
	}
	r.logger.Info(fmt.Sprintf("The shared router moved from port %d to %d; URLs already open, the index page included, still use the old port.", priorPort, r.Port))
}

// rootlessRefusal matches the way a daemon that cannot bind a privileged
// port says so — a permission failure, never a busy port.
var rootlessRefusal = regexp.MustCompile(`(?i)permission denied|operation not permitted|not permitted|cannot expose privileged port`)

// RouterStatus is what `router status` says, and where.
type RouterStatus struct {
	// Reachable is false when Docker itself could not be asked; Running is
	// then unknown and reported false.
	Reachable bool
	Running   bool
	// Problems is empty when the running router is exactly what this version would start.
	Problems []string
	Port     int
	Network  string
}

// Status reports the router's status, translating a Docker-unavailable
// Verify failure into an unreachable status rather than propagating it.
func (r *Router) Status(ctx context.Context) (RouterStatus, error) {
	problems, err := r.Verify(ctx)
	if err != nil {
		var unavailable *docker.UnavailableError
		if !errors.As(err, &unavailable) {
			return RouterStatus{}, err
		}
		return RouterStatus{Reachable: false, Running: false, Problems: []string{err.Error()}, Port: r.Port, Network: r.Network}, nil
	}
	return RouterStatus{Reachable: true, Running: !slices.Contains(problems, notRunning), Problems: problems, Port: r.Port, Network: r.Network}, nil
}

// Report is what `router status` says, and where.
type Report struct {
	Output []string
	Errors []string
	Failed bool
}

// StatusReport is routerStatusReport ported: `running` only means a
// container exists, so a stack whose containers have exited or gone
// unhealthy must not read as running on the one stream a script is told to
// read; the reasons go to errors, and they are what makes the report failed.
func StatusReport(status RouterStatus) Report {
	where := fmt.Sprintf("127.0.0.1:%d, network %s", status.Port, status.Network)
	if !status.Reachable {
		joined := strings.TrimSuffix(strings.Join(status.Problems, "; "), ".")
		return Report{Output: []string{}, Errors: []string{"Router state unknown: " + joined}, Failed: true}
	}
	// verify() answers [notRunning] and nothing else when every container is
	// gone, so there is no other reason to drop here.
	if !status.Running {
		return Report{Output: []string{fmt.Sprintf("not running (would use %s)", where)}, Errors: []string{}, Failed: false}
	}
	output := "running on " + where
	if len(status.Problems) > 0 {
		output = fmt.Sprintf("degraded on %s (%d problem(s) reported on stderr)", where, len(status.Problems))
	}
	errs := make([]string, len(status.Problems))
	for i, problem := range status.Problems {
		errs[i] = "Router problem: " + problem
	}
	return Report{Output: []string{output}, Errors: errs, Failed: len(status.Problems) > 0}
}

// activeEndpoints matches Compose's refusal to remove a network something is
// still attached to.
var activeEndpoints = regexp.MustCompile(`(?i)has active endpoints`)

// Down stops the router. The shared network stays while any worktree service
// is still attached to it; Compose reports that as a failure, and here it is
// the expected outcome.
func (r *Router) Down(ctx context.Context) error {
	// By project name alone, as containerID reads it: the router is
	// machine-wide, so it must be removable from a control home that never
	// wrote a stack file — the one a refusal tells the user to run this
	// from. --remove-orphans no longer sweeps orphans without a model to
	// compare against; it stays because it still catches one-off `compose
	// run` containers of the project.
	args := []string{"compose", "--project-name", r.ProjectName, "down", "--remove-orphans"}
	spec := run.Spec{AllowFailure: true, Output: run.Tee, Env: docker.ComposeEnvironment(r.environ), Timeout: r.timeouts.Down}
	result, err := docker.Run(ctx, r.runner, args, spec)
	if err != nil {
		return err
	}
	if result.ExitCode == 0 {
		return nil
	}
	// Only the tolerated network case is intercepted here, and only when
	// the failure is Compose's own clean refusal, not a timeout, signal or
	// output overflow dressed up as one (found in review, round 3 of #55,
	// the same bug class already fixed in internal/backend's Down and
	// Reap): AllowFailure lets this read the result instead of Finish
	// rejecting it automatically.
	if !result.TimedOut && !result.OutputOverflow && result.Signal == 0 && activeEndpoints.MatchString(result.Stderr) {
		r.logger.Info(fmt.Sprintf("Network %s is kept while worktree services are attached to it.", r.Network))
		return nil
	}
	// A timeout, signal or output overflow is reported through run.Failure,
	// the same wording and exit code (124 on a timeout) any other bounded
	// call gets — a hand-built message here would silently drop all of
	// that, the same bug class as internal/backend's Down and Reap. A
	// plain Compose refusal keeps src/router/router.ts's own ported
	// wording instead ("The shared router did not stop cleanly:\n…",
	// `down()`): TypeScript has no timeout concept for this call at all, so
	// there is nothing to diverge from for that one case, and the bytes
	// should match exactly.
	if result.TimedOut || result.OutputOverflow || result.Signal != 0 {
		return run.Failure(run.Spec{Command: "docker", Args: args, Timeout: spec.Timeout}, result)
	}
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	return clierr.Newf("The shared router did not stop cleanly:\n%s", detail)
}

/* ------------------------------------------------------------- inspect shapes */

// ContainerInspect is what `docker inspect` says, restricted to the fields
// Verify reads. Every field is a plain value, not a pointer: a container
// missing a section (never seen from a real daemon, only from a hand-built
// test fixture) reads the same as one whose section is present but empty,
// matching the TypeScript build's `?? {}` fallbacks.
type ContainerInspect struct {
	Config          InspectConfig
	HostConfig      InspectHostConfig
	Mounts          []InspectMount
	NetworkSettings InspectNetworkSettings
	State           InspectState
}

// InspectConfig is `docker inspect`'s .Config.
type InspectConfig struct {
	Cmd        []string
	Entrypoint []string
	Env        []string
	Image      string
	Labels     map[string]string
}

// InspectHostConfig is `docker inspect`'s .HostConfig.
type InspectHostConfig struct {
	CapAdd         []string
	CapDrop        []string
	Devices        []InspectDevice
	IpcMode        string
	NetworkMode    string
	PidMode        string
	Privileged     bool
	ReadonlyRootfs bool
	SecurityOpt    []string
	UTSMode        string
}

// InspectDevice is one entry of `docker inspect`'s .HostConfig.Devices. The
// verifier only asks whether any are present, so its own fields (host path,
// container path, cgroup permissions) are not needed here.
type InspectDevice struct {
	PathOnHost string
}

// InspectMount is one entry of `docker inspect`'s .Mounts.
type InspectMount struct {
	Destination string
	RW          bool
	Source      string
	Type        string
}

// InspectPortBinding is one published port's host binding.
type InspectPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string
}

// InspectNetworkSettings is `docker inspect`'s .NetworkSettings.
type InspectNetworkSettings struct {
	Networks map[string]json.RawMessage
	Ports    map[string][]InspectPortBinding
}

// InspectState is `docker inspect`'s .State.
type InspectState struct {
	Health InspectHealth
	Status string
}

// InspectHealth is `docker inspect`'s .State.Health.
type InspectHealth struct {
	Status string
}
