package router

// The shared router, one per machine, used by every worktree of every
// repository. It serves one loopback port and routes by hostname to
// loopback targets (stack.go). Nothing here is per-project, which is why its
// settings come from the environment and settings.toml rather than from
// berth.toml.
//
// Locking is not this package's job: the caller acquires the router's
// lock.Scope through lifecycle.Hold (taken alone) before calling Ensure,
// the way every other mutation holds its lock around the action rather
// than the action holding it around itself.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/checkoutindex"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/docker"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/ports"
	"github.com/sofired/berth/internal/run"
)

// Docker-call timeouts (#14): containerID and inspect are quick
// introspection, timed the same as doctor's own docker inspect probes.
// start and Down get their own sensible choices: starting the router may
// pull images on a bare machine and wait for every health check, hence the
// generous 5 minutes; lock.RouterTimeout is sized to cover Ensure's worst
// case in terms of these constants, so a waiter's patience is never shorter
// than what a first start may need — see its own comment. Stopping it is
// ordinarily fast; a minute is generous room without leaving a hung
// `docker` unnoticed for long.
const (
	dockerProbeTimeout = 30 * time.Second
	startTimeout       = 5 * time.Minute
	downTimeout        = 1 * time.Minute
)

// logTailLines is how many of the router's last log lines a failed start shows.
const logTailLines = 20

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

// Problem is one way the running router is not as it should be: Message is
// the text `router status` and `doctor` print, and Code names the difference
// in a fixed vocabulary that scripts may key on ("<service>.<what>", for
// example "router.not-healthy"). The set of codes may grow; a Code never
// changes meaning. Within one status no Code appears twice.
type Problem struct {
	Code    string
	Message string
	// Log is the container's last log lines, for a container that is not
	// running or not healthy; "" otherwise or when they cannot be read.
	Log string
}

// Codes for the problems that concern no single container, and for a router
// that is gone. The service part of a code is "router", "index" or "stack"
// ("stack" covers failures that concern both containers).
const (
	codeAbsent = "router.absent"
	// CodeUnreachable is the code of the one problem an unreachable status
	// carries: Docker itself could not be asked.
	CodeUnreachable = "stack.unreachable"
)

// notRunning is the status message when every container is gone.
const notRunning = "the router is not running"

// dockerName is what Docker accepts for a project name.
var dockerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// DefaultName is the shared router's default Compose project name.
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
// that container's health check fetches it (see write() below). Tests may
// supply a stub.
type PlaceholderPage func(now time.Time) ([]byte, error)

// defaultPlaceholderPage is the checkout index's own placeholder.
func defaultPlaceholderPage(now time.Time) ([]byte, error) {
	return []byte(checkoutindex.PlaceholderHTML(now)), nil
}

// Options configure a Router.
type Options struct {
	// State is the router's own on-disk layout (layout.RouterState).
	State layout.RouterLayout
	// Port is the loopback port the proxy serves routes on.
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
	// ControlHome is the control home whose registrations the router reads,
	// without the registry lock, to keep its own ports clear of theirs and
	// to repair route files (decision 9).
	ControlHome string
	// Registrations looks up one worktree's record for route repair; nil
	// skips the repair.
	Registrations Registrations
	// PortRange is where the index and ping ports are allocated from; the
	// zero value means layout.DefaultPortRange.
	PortRange layout.PortRange
	// Probe decides whether a port is free, for the allocation and for the
	// check before starting; nil asks the operating system. Tests set a fake
	// so they bind nothing.
	Probe ports.Probe
	// ModeOverride is settings.toml's [router] mode ("host", "bridge" or ""
	// to detect it from `docker info`).
	ModeOverride string
	// DockerHost is DOCKER_HOST, passed in by the entry point; "" asks the
	// current Docker context for its endpoint.
	DockerHost string
	// ClientOS is runtime.GOOS, passed in by the entry point: it takes part
	// in mode detection and picks which tool a message suggests for finding
	// the process holding a port.
	ClientOS string
}

// Router is what the rest of the tool needs to know about the shared router.
type Router struct {
	Port        int
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
	controlHome    string
	registrations  Registrations
	portRange      layout.PortRange
	probe          ports.Probe
	modeOverride   string
	dockerHost     string
	clientOS       string
}

// New builds a Router. The project name is a Compose project name, so one
// Docker would not accept is refused.
func New(options Options) (*Router, error) {
	projectName := options.ProjectName
	if projectName == "" {
		projectName = ResolveProjectName()
	}
	if !dockerName.MatchString(projectName) {
		return nil, clierr.Newf("Invalid router project name: %s.", projectName)
	}
	labelNamespace := options.LabelNamespace
	if labelNamespace == "" {
		labelNamespace = brand.LabelNamespace
	}
	page := options.Page
	if page == nil {
		page = defaultPlaceholderPage
	}
	portRange := options.PortRange
	if portRange == (layout.PortRange{}) {
		portRange = layout.DefaultPortRange
	}
	return &Router{
		Port:           options.Port,
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
		controlHome:    options.ControlHome,
		registrations:  options.Registrations,
		portRange:      portRange,
		probe:          options.Probe,
		modeOverride:   options.ModeOverride,
		dockerHost:     options.DockerHost,
		clientOS:       options.ClientOS,
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

// settingsFile is the settings file's name for a message.
func (r *Router) settingsFile() string {
	if r.settingsPath != "" {
		return r.settingsPath
	}
	return brand.SettingsFile
}

// protocolLabelName is the label naming a container's router protocol.
func (r *Router) protocolLabelName() string {
	return r.labelNamespace + ".router-protocol"
}

// rangeProbe is the probe that picks ports from the allocation range.
func (r *Router) rangeProbe() ports.Probe {
	if r.probe != nil {
		return r.probe
	}
	return ports.LoopbackProbe{}
}

// startProbe is the probe that decides whether port is free before the
// router starts on it. A privileged port cannot be bind-probed by an
// unprivileged user, so it is only dialled.
func (r *Router) startProbe(port int) ports.Probe {
	switch {
	case r.probe != nil:
		return r.probe
	case port < privilegedPortLimit:
		return ports.DialProbe{}
	default:
		return ports.LoopbackProbe{}
	}
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
// package owns it because that directory is no part of any control home
// (#12, #13).
func secureState(state layout.RouterLayout) error {
	if err := secureDirectories(state.StateDir, []string{state.StateDir}, 0o700); err != nil {
		return err
	}
	if err := secureDirectories(state.StateDir, []string{state.DynamicDir, state.IndexDir}, 0o755); err != nil {
		return err
	}
	for _, file := range []string{state.LockPath, state.IndexFile, state.ComposeFile, state.StateFile} {
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
func (r *Router) write(now time.Time, options StackOptions) error {
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
	// JSON is YAML; Compose reads it as such.
	if err := files.AtomicJSON(r.state.ComposeFile, Stack(options)); err != nil {
		return err
	}
	// The index is routed like any service: by a file the file provider
	// reads, to the loopback port the index container is published on.
	return WriteIndexFragment(r.state, options.IndexPort)
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

// readEngine reads `docker info` and the endpoint the client talks to.
func (r *Router) readEngine(ctx context.Context) (Engine, error) {
	result, err := docker.Run(ctx, r.runner, []string{"info", "--format", "{{json .}}"}, run.Spec{AllowFailure: true, Timeout: r.timeouts.Probe})
	if err != nil {
		return Engine{}, err
	}
	if result.ExitCode != 0 {
		return Engine{}, &docker.UnavailableError{Detail: docker.Said(result.Stderr,
			fmt.Sprintf("`docker info` exited %d without saying why", result.ExitCode))}
	}
	engine, err := ParseEngineInfo(result.Stdout)
	if err != nil {
		return Engine{}, err
	}
	engine.ClientOS = r.clientOS
	engine.Endpoint = r.dockerHost
	if engine.Endpoint == "" {
		endpoint, err := docker.Run(ctx, r.runner, []string{"context", "inspect", "--format", "{{.Endpoints.docker.Host}}"},
			run.Spec{AllowFailure: true, Timeout: r.timeouts.Probe})
		if err != nil {
			return Engine{}, err
		}
		engine.Endpoint = strings.TrimSpace(endpoint.Stdout)
		if endpoint.ExitCode != 0 || engine.Endpoint == "" {
			return Engine{}, clierr.Newf("Could not tell which Docker endpoint %s is talking to: %s",
				r.commandNameOr(), docker.Said(endpoint.Stderr, "`docker context inspect` named none"))
		}
	}
	return engine, nil
}

// detectMode reads the daemon and decides the proxy's mode (DetectMode).
func (r *Router) detectMode(ctx context.Context) (Mode, error) {
	engine, err := r.readEngine(ctx)
	if err != nil {
		return "", err
	}
	return DetectMode(engine, r.modeOverride, r.settingsFile())
}

// DetectMode is the mode this build would start the proxy in on the
// daemon it is talking to, or why it refuses to (docs/design/routing.md,
// decision 1). doctor reports it beside the running router's own mode.
func (r *Router) DetectMode(ctx context.Context) (Mode, error) {
	return r.detectMode(ctx)
}

// ModeChoice is the proxy's mode as detection and settings.toml settle it.
type ModeChoice struct {
	// Mode is the mode a start would use.
	Mode Mode
	// Override is settings.toml's [router] mode, "" when unset.
	Override Mode
	// Detected is the mode `docker info` alone would pick; "" when detection
	// would refuse (DetectErr says why).
	Detected Mode
	// DetectErr is why detection alone would refuse; nil when it would not.
	DetectErr error
}

// ModeChoice reads the daemon once and reports the mode a start would use
// beside what detection alone would pick, so doctor can say that the
// override is in effect and when it disagrees. An error means even the
// override cannot be applied (a remote or rootless daemon).
func (r *Router) ModeChoice(ctx context.Context) (ModeChoice, error) {
	engine, err := r.readEngine(ctx)
	if err != nil {
		return ModeChoice{}, err
	}
	choice := ModeChoice{Override: Mode(r.modeOverride)}
	choice.Detected, choice.DetectErr = DetectMode(engine, "", r.settingsFile())
	if choice.Override == "" {
		if choice.DetectErr != nil {
			return ModeChoice{}, choice.DetectErr
		}
		choice.Mode = choice.Detected
		return choice, nil
	}
	choice.Mode, err = DetectMode(engine, r.modeOverride, r.settingsFile())
	if err != nil {
		return ModeChoice{}, err
	}
	return choice, nil
}

// jsMaxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER (2^53 - 1).
const jsMaxSafeInteger = 1<<53 - 1

// toProtocol parses text as a protocol number: digits only ("0x10" and
// "1e3" are corruption, not protocols), and a safe, non-negative integer of
// at least 1.
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

// routerRead is one read of the router container: whether there is one, and
// what `docker inspect` says about it. Ensure reads it once and derives the
// ownership check, the protocol check and the port it already serves from it.
type routerRead struct {
	// present is false when there is no router container at all.
	present bool
	// inspected is nil when the container exists but could not be inspected.
	inspected *ContainerInspect
}

// readRouter finds the router container and inspects it.
func (r *Router) readRouter(ctx context.Context) (routerRead, error) {
	id, err := r.containerID(ctx, RouterService)
	if err != nil {
		return routerRead{}, err
	}
	if id == "" {
		return routerRead{}, nil
	}
	inspected, err := r.inspect(ctx, id)
	if err != nil {
		return routerRead{}, err
	}
	return routerRead{present: true, inspected: inspected}, nil
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

// protocolMismatch says, in the advice Ensure refuses with, how a running
// router's layout differs from this build's: "" when the layout matches, or
// the router is not running (a stopped one serves nobody, so it is replaced
// like any other stopped router). A router speaking a newer protocol would be
// silently downgraded by recreating it; one speaking an older one, or none,
// has a layout this build does not share, which nothing here converts.
func (r *Router) protocolMismatch(inspected ContainerInspect) (string, error) {
	if inspected.State.Status != "running" {
		return "", nil
	}
	running, ok, err := r.protocolOf(inspected)
	if err != nil {
		return "", err
	}
	switch {
	case ok && running > Protocol:
		return fmt.Sprintf(
			"The shared router is at protocol %d; this version speaks %d. Upgrade rather than downgrading the "+
				"router the other worktrees on this machine expect, or hand the router to this one: remove it with '%s router down'.",
			running, Protocol, r.commandNameOr()), nil
	case !ok || running < Protocol:
		return fmt.Sprintf(
			"The running shared router has a different layout from this version's (it routes through per-worktree networks). "+
				"Run '%s router down' first, then run this again.", r.commandNameOr()), nil
	}
	return "", nil
}

// checkProtocol refuses to touch a running router whose layout this build
// does not share (protocolMismatch). Keeps no record of its own: the label
// on the container is the only truth.
func (r *Router) checkProtocol(read routerRead) error {
	if !read.present {
		return nil
	}
	if read.inspected == nil {
		return clierr.New("The shared router exists but could not be inspected.")
	}
	message, err := r.protocolMismatch(*read.inspected)
	if err != nil {
		return err
	}
	if message != "" {
		return clierr.New(message)
	}
	return nil
}

// Ownership is what the running router's own container says about which
// berth root's Ensure most recently started or recreated it (#82).
type Ownership struct {
	// Running is false when the router container does not exist, or exists
	// but is not running; Labelled and Home are then meaningless — a router
	// serving nobody is nothing to own.
	Running bool
	// Labelled is false only when the running container carries no
	// RouterHomeLabel key at all. A container that cannot be inspected is an
	// error from Owner, never an unlabelled router. Home is "" in that case;
	// checkOwnership treats an unlabelled router as foreign, exactly as it
	// treats one labelled for a different berth root.
	Labelled bool
	// Home is the berth root RouterHomeLabel recorded on the running
	// container, when Labelled — including "", a caller with no berth root
	// to give (a test's own router, mainly).
	Home string
}

// Owner reports Ownership for the running router, read from the router
// container's own RouterHomeLabel — never the index container's copy of the
// same label, since Stack always stamps both together and Ensure never
// recreates one without the other.
func (r *Router) Owner(ctx context.Context) (Ownership, error) {
	read, err := r.readRouter(ctx)
	if err != nil {
		return Ownership{}, err
	}
	return ownershipOf(read)
}

// ownershipOf reads the ownership off one read of the router container.
func ownershipOf(read routerRead) (Ownership, error) {
	if !read.present {
		return Ownership{}, nil
	}
	if read.inspected == nil {
		// Unparseable or unreadable: not "no label" but "no answer", and an
		// owner that cannot be read is not one to treat as foreign or as
		// this berth root's own, so a transient inspect failure never turns
		// a foreign router into a takeable one.
		return Ownership{}, clierr.New("The shared router exists but could not be inspected.")
	}
	// A container that exists but is not running serves nobody, so there is
	// nothing to protect: the refusal is for a router in use (containerID
	// lists stopped containers too, so this still finds one). It reads as
	// no router, and the next start takes it over the way a fresh start
	// would.
	if read.inspected.State.Status != "running" {
		return Ownership{}, nil
	}
	// The key's mere presence is what "labelled" means: Stack always writes
	// it, empty or not (a caller with no berth root to give — a test's own
	// router, mainly — gets an empty value, which still matches an equally
	// homeless r.home). Only a missing key is truly unlabelled.
	home, present := read.inspected.Config.Labels[RouterHomeLabel]
	if !present {
		return Ownership{Running: true}, nil
	}
	return Ownership{Running: true, Labelled: true, Home: home}, nil
}

// checkOwnership refuses to touch a running router that is not this berth
// root's own (#82): two berth roots each hold their own router lock over
// their own state but drive the same machine-wide container, so a second
// root's Ensure would otherwise force-recreate whatever the first had just
// started, with no warning at all (see docs/design/locks.md). A router
// carrying no ownership label at all is treated exactly like one labelled
// for a different berth root — this berth did not start it, so it is not
// this berth's to recreate.
func (r *Router) checkOwnership(read routerRead) error {
	owner, err := ownershipOf(read)
	if err != nil {
		return err
	}
	if !owner.Running {
		return nil
	}
	if !owner.Labelled {
		return clierr.Newf(
			"The shared router is not owned by this berth root. Hand it to this one: stop it with '%s router down', then run this again.",
			r.commandNameOr())
	}
	if owner.Home == r.home {
		return nil
	}
	return clierr.Newf(
		"The shared router belongs to a different berth root (%s). Hand it to this one: stop it with '%s router down', then run this again.",
		owner.Home, r.commandNameOr())
}

// Ensure is idempotent: starts the router if needed and fails loudly if it
// cannot be made right. The caller holds the router's lock around this call
// (see internal/commands' routerLockScope and lifecycle.Requests.Router).
//
// secureState runs first: a symlinked state directory must be refused
// before anything — this call included — ever touches whatever it points
// to. checkOwnership and checkProtocol run next, before anything else
// touches the router: a router belonging to a different berth root, or
// speaking a protocol this build does not share, is refused outright rather
// than force-recreated (#82). Then the mode is detected, the index and ping
// ports are allocated if they are not yet, and the ports the router needs
// are checked free before Compose is asked to start anything. Compose
// recreates the containers when their configuration changed — a different
// mode or port among it — so nothing else decides that. Last, route files
// that no longer match a registration are repaired.
func (r *Router) Ensure(ctx context.Context) error {
	if err := secureState(r.state); err != nil {
		return err
	}
	read, err := r.readRouter(ctx)
	if err != nil {
		return err
	}
	if err := r.checkOwnership(read); err != nil {
		return err
	}
	if err := r.checkProtocol(read); err != nil {
		return err
	}
	mode, err := r.detectMode(ctx)
	if err != nil {
		return err
	}
	state, err := r.ensureState(ctx, mode)
	if err != nil {
		return err
	}
	// What port a router already running serves, from the same read, so a
	// port change can be announced once it is a fact (notePortChange, #79).
	priorPort, wasRunning := 0, false
	if read.inspected != nil {
		prior := runningFrom(*read.inspected)
		priorPort, wasRunning = prior.Port, prior.Up
	}
	options := StackOptions{
		Mode:           mode,
		Port:           r.Port,
		IndexPort:      state.IndexPort,
		PingPort:       state.PingPort,
		DynamicDir:     r.state.DynamicDir,
		IndexDir:       r.state.IndexDir,
		LabelNamespace: r.labelNamespace,
		Home:           r.home,
	}
	if err := r.checkPortsFree(ctx, read, options); err != nil {
		return err
	}
	if err := r.write(time.Now(), options); err != nil {
		return err
	}
	if err := r.start(ctx); err != nil {
		return err
	}
	// Best effort: route files that cannot be reconciled this run (an
	// unreadable record) must not turn an otherwise healthy router start
	// into a failure. A cancellation is not such a failure: it still ends
	// the command. doctor reports the same condition without ever repairing
	// it; this is Ensure's repair half.
	if err := r.repairRoutes(ctx); err != nil {
		if ctx.Err() != nil {
			return err
		}
		r.logger.Warn(fmt.Sprintf("Could not reconcile route files: %s", err))
	}
	r.notePortChange(priorPort, wasRunning)
	return nil
}

// repairRoutes is RepairRoutes over this router's own state, reporting what
// it repaired; a Router built without Registrations repairs nothing.
func (r *Router) repairRoutes(ctx context.Context) error {
	if r.registrations == nil {
		return nil
	}
	repaired, err := RepairRoutes(ctx, r.state, r.registrations)
	for _, problem := range repaired {
		r.logger.Info(fmt.Sprintf("Repaired a route file: %s", problem.Describe()))
	}
	return err
}

// start runs `compose up --detach --wait --remove-orphans`. A failed start
// is reported in terms of the port most likely to blame, followed by the
// containers' last log lines: a port taken after the check above otherwise
// crash-loops the proxy with Compose reporting only "unhealthy".
func (r *Router) start(ctx context.Context) error {
	result, err := r.compose(ctx, []string{"up", "--detach", "--wait", "--remove-orphans"}, run.Spec{AllowFailure: true, Timeout: r.timeouts.Start})
	if err != nil {
		return err
	}
	if result.ExitCode == 0 {
		return nil
	}
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	message := fmt.Sprintf(
		"The shared router failed to start. Make sure 127.0.0.1:%d is free, or choose another port with %s_ROUTER_PORT or [router] port in %s.\n%s",
		r.Port, brand.EnvPrefix, r.settingsFile(), detail)
	if tail := r.lastLogs(ctx); tail != "" {
		message += fmt.Sprintf("\nLast log lines:\n%s", tail)
	}
	return clierr.New(message)
}

// lastLogs is the last lines the stack's containers logged, best effort: ""
// when they cannot be read. A bounded `compose logs` of its own, so a hung
// daemon cannot hold up the failure report.
func (r *Router) lastLogs(ctx context.Context, services ...string) string {
	if ctx.Err() != nil {
		return ""
	}
	args := append([]string{"logs", "--no-color", "--tail", strconv.Itoa(logTailLines)}, services...)
	result, err := r.compose(ctx, args, run.Spec{AllowFailure: true, Timeout: r.timeouts.Probe})
	if err != nil || result.ExitCode != 0 {
		return ""
	}
	return strings.TrimRight(result.Stdout, "\n")
}

// portsOf are the ports one running container holds: its published ones (the
// index, and the proxy in bridge mode) and, for a host-network proxy, the
// addresses of its entrypoints.
func portsOf(inspected ContainerInspect, own map[int]bool) {
	for _, bindings := range inspected.NetworkSettings.Ports {
		for _, binding := range bindings {
			if port, err := strconv.Atoi(binding.HostPort); err == nil {
				own[port] = true
			}
		}
	}
	if inspected.HostConfig.NetworkMode != "host" {
		return
	}
	for _, argument := range inspected.Config.Cmd {
		if _, address, ok := strings.Cut(argument, ".address="); ok {
			if _, port, found := strings.Cut(address, ":"); found {
				if n, err := strconv.Atoi(port); err == nil {
					own[n] = true
				}
			}
		}
	}
}

// ownPorts are the ports the stack's own running containers hold, so a port
// the router already serves is never mistaken for a conflict when it is
// started again. A container that is not running holds nothing. read is the
// router container, already read by the caller.
func (r *Router) ownPorts(ctx context.Context, read routerRead) (map[int]bool, error) {
	own := map[int]bool{}
	if read.inspected != nil && read.inspected.State.Status == "running" {
		portsOf(*read.inspected, own)
	}
	id, err := r.containerID(ctx, IndexService)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return own, nil
	}
	inspected, err := r.inspect(ctx, id)
	if err != nil {
		return nil, err
	}
	if inspected != nil && inspected.State.Status == "running" {
		portsOf(*inspected, own)
	}
	return own, nil
}

// PortConflict is a port the stack needs that something else holds.
type PortConflict struct {
	Port int
	// What names the port's role: "the router", "the checkout index" or
	// "the router's ping".
	What string
	// Holders names what holds it: containers, else "another process" with
	// the command that finds it.
	Holders string
	// Container is true when the holders are containers.
	Container bool
	// Remedy says how to move the port.
	Remedy string
	// Movable is true for a port the router allocated (state.json) and so can
	// be moved by removing that file; false for the router's own port, which
	// the user chose.
	Movable bool
}

// Describe is the conflict as one line of a refusal.
func (c PortConflict) Describe() string {
	holders := c.Holders
	if c.Container {
		holders += fmt.Sprintf(" (find it with: docker ps --filter publish=%d)", c.Port)
	}
	if c.Movable {
		return fmt.Sprintf("127.0.0.1:%d (%s) is held by %s; %s", c.Port, c.What, holders, c.Remedy)
	}
	return fmt.Sprintf("127.0.0.1:%d (%s) is held by %s; stop it, or %s", c.Port, c.What, holders, c.Remedy)
}

// portConflicts are the ports options needs that something other than this
// stack's own running containers holds.
func (r *Router) portConflicts(ctx context.Context, read routerRead, options StackOptions) ([]PortConflict, error) {
	own, err := r.ownPorts(ctx, read)
	if err != nil {
		return nil, err
	}
	stateRemedy := fmt.Sprintf("remove %s so the next start picks another port", r.state.StateFile)
	needs := []PortConflict{
		{Port: options.Port, What: "the router", Remedy: fmt.Sprintf("choose another port with %s_ROUTER_PORT or [router] port in %s", brand.EnvPrefix, r.settingsFile())},
	}
	if options.IndexPort > 0 {
		needs = append(needs, PortConflict{Port: options.IndexPort, What: "the checkout index", Remedy: stateRemedy, Movable: true})
	}
	if options.Mode == ModeHost && options.PingPort > 0 {
		needs = append(needs, PortConflict{Port: options.PingPort, What: "the router's ping", Remedy: stateRemedy, Movable: true})
	}
	var conflicts []PortConflict
	for _, need := range needs {
		if own[need.Port] || r.startProbe(need.Port).Free(ctx, need.Port) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		need.Holders, need.Container, err = r.holders(ctx, need.Port)
		if err != nil {
			return nil, err
		}
		conflicts = append(conflicts, need)
	}
	return conflicts, nil
}

// checkPortsFree refuses to start while the port the router serves, the
// index's, or (in host mode) the ping's is held by something other than this
// stack's own running containers, naming the holder. Docker's own bind
// error, or a crash-looping proxy, says nothing useful.
func (r *Router) checkPortsFree(ctx context.Context, read routerRead, options StackOptions) error {
	conflicts, err := r.portConflicts(ctx, read, options)
	if err != nil || len(conflicts) == 0 {
		return err
	}
	lines := make([]string, len(conflicts))
	for i, conflict := range conflicts {
		lines[i] = "  " + conflict.Describe()
	}
	return clierr.Newf("Cannot start the shared router: a port it needs is in use.\n%s", strings.Join(lines, "\n"))
}

// CheckPorts is the read-only form of the check Ensure makes before it
// starts: the ports this build would need, in the mode it would use, that
// something other than the stack's own running containers holds. It
// allocates and writes nothing; a port the router has not yet been given
// (no state file: a first run) is not checked, and the index and ping ports
// are not allocated yet. doctor reports it while the router is absent or
// unhealthy.
func (r *Router) CheckPorts(ctx context.Context) ([]PortConflict, error) {
	mode, err := r.detectMode(ctx)
	if err != nil {
		return nil, err
	}
	state, _, err := ReadState(r.state)
	if err != nil {
		return nil, err
	}
	read, err := r.readRouter(ctx)
	if err != nil {
		return nil, err
	}
	return r.portConflicts(ctx, read, StackOptions{Mode: mode, Port: r.Port, IndexPort: state.IndexPort, PingPort: state.PingPort})
}

// holders names what holds port: the containers publishing it, else
// "another process" with the command that finds it. A failure to ask Docker
// only costs the names.
func (r *Router) holders(ctx context.Context, port int) (string, bool, error) {
	result, err := docker.Run(ctx, r.runner, []string{"ps", "--filter", "publish=" + strconv.Itoa(port), "--format", "{{.Names}}"},
		run.Spec{AllowFailure: true, Timeout: r.timeouts.Probe})
	if err != nil {
		if ctx.Err() != nil {
			return "", false, err
		}
		// Only the names are lost.
		result = run.Result{}
	}
	var names []string
	for _, name := range strings.Fields(result.Stdout) {
		names = append(names, "container "+name)
	}
	if len(names) == 0 {
		return fmt.Sprintf("another process (%s)", ports.LookupHint(r.clientOS, port)), false, nil
	}
	return strings.Join(names, " and "), true, nil
}

// Running is what the router's container says about itself.
type Running struct {
	// Present is false when there is no router container at all.
	Present bool
	// Up is true when the container is running.
	Up bool
	// Mode is the mode label; "" when the container carries none.
	Mode Mode
	// Port is the port label, 0 when it carries none or cannot be read.
	Port int
}

// Running reads the router container's state and labels.
func (r *Router) Running(ctx context.Context) (Running, error) {
	read, err := r.readRouter(ctx)
	if err != nil {
		return Running{}, err
	}
	if !read.present {
		return Running{}, nil
	}
	if read.inspected == nil {
		// An inspect failure is no evidence either way.
		return Running{Present: true, Up: true}, nil
	}
	return runningFrom(*read.inspected), nil
}

// runningFrom reads a Running off an inspected router container.
func runningFrom(inspected ContainerInspect) Running {
	running := Running{Present: true, Up: inspected.State.Status == "running", Mode: Mode(inspected.Config.Labels[RouterModeLabel])}
	if port, err := strconv.Atoi(inspected.Config.Labels[RouterPortLabel]); err == nil && port > 0 {
		running.Port = port
	}
	return running
}

// RunningPort reports the loopback port the running router serves, read off
// its container's label: 0 when it carries none, and running false when
// there is no container at all. A container that cannot be inspected counts
// as running with an unknown port, since an inspect failure is no evidence
// either way. doctor compares it with the port it resolved (#79).
func (r *Router) RunningPort(ctx context.Context) (port int, running bool, err error) {
	state, err := r.Running(ctx)
	if err != nil {
		return 0, false, err
	}
	return state.Port, state.Present, nil
}

// notePortChange is said once the router is started on r.Port: a router
// that was running on another port before this Ensure has just been
// recreated on the new one, so URLs already open — the index page's
// included — still name the old port.
func (r *Router) notePortChange(priorPort int, wasRunning bool) {
	if !wasRunning || priorPort == 0 || priorPort == r.Port {
		return
	}
	r.logger.Info(fmt.Sprintf("The shared router moved from port %d to %d; URLs already open, the index page included, still use the old port.", priorPort, r.Port))
}

// RouterStatus is what `router status` says, and where.
type RouterStatus struct {
	// Reachable is false when Docker itself could not be asked; Running is
	// then unknown and reported false.
	Reachable bool
	Running   bool
	// Problems is empty when the running router is up and healthy. An
	// unreachable status has exactly one, coded "stack.unreachable".
	Problems []Problem
	// Port is the port this build would serve routes on.
	Port int
	// RunningPort and Mode are what the running router's container says: 0
	// and "" when it is not running or carries no such label.
	RunningPort int
	Mode        Mode
}

// Messages are the Message of each of Problems, in order.
func (s RouterStatus) Messages() []string {
	messages := make([]string, len(s.Problems))
	for i, problem := range s.Problems {
		messages[i] = problem.Message
	}
	return messages
}

// Status reports the router's status, translating a Docker-unavailable
// failure into an unreachable status rather than propagating it.
func (r *Router) Status(ctx context.Context) (RouterStatus, error) {
	problems, running, err := r.statusProblems(ctx)
	if err != nil {
		var unavailable *docker.UnavailableError
		if !errors.As(err, &unavailable) {
			return RouterStatus{}, err
		}
		return RouterStatus{Reachable: false, Running: false, Problems: []Problem{{Code: CodeUnreachable, Message: err.Error()}}, Port: r.Port}, nil
	}
	up := !slices.ContainsFunc(problems, func(problem Problem) bool { return problem.Code == codeAbsent })
	return RouterStatus{Reachable: true, Running: up, Problems: problems, Port: r.Port, RunningPort: running.Port, Mode: running.Mode}, nil
}

// statusProblems is every way the stack is not up and healthy, as problems
// with codes, plus what the router container says about itself. It checks
// what Compose and Docker report, not the container's configuration:
// Compose recreates a container whose configuration changed, so there is no
// drift to compare.
func (r *Router) statusProblems(ctx context.Context) ([]Problem, Running, error) {
	services := []string{RouterService, IndexService}
	ids := make([]string, len(services))
	for i, service := range services {
		id, err := r.containerID(ctx, service)
		if err != nil {
			return nil, Running{}, err
		}
		ids[i] = id
	}
	// Both gone is a router that is not running. One gone is a router that
	// is still bound to its port and still routing, with a hole in it —
	// reporting that as "not running" would tell an operator the opposite of
	// what is true.
	if ids[0] == "" && ids[1] == "" {
		return []Problem{{Code: codeAbsent, Message: notRunning}}, Running{}, nil
	}
	var missing []Problem
	for i, id := range ids {
		if id == "" {
			missing = append(missing, Problem{Code: services[i] + ".missing", Message: services[i] + ": container is missing"})
		}
	}
	if len(missing) > 0 {
		return missing, Running{}, nil
	}
	inspected := make([]*ContainerInspect, len(ids))
	for i, id := range ids {
		doc, err := r.inspect(ctx, id)
		if err != nil {
			return nil, Running{}, err
		}
		inspected[i] = doc
	}
	if inspected[0] == nil || inspected[1] == nil {
		return []Problem{{Code: "stack.inspect", Message: "the router could not be inspected"}}, Running{}, nil
	}
	var problems []Problem
	for i, service := range services {
		var unwell []Problem
		if inspected[i].State.Status != "running" {
			unwell = append(unwell, Problem{Code: service + ".not-running", Message: service + ": not running"})
		}
		if inspected[i].State.Health.Status != "healthy" {
			unwell = append(unwell, Problem{Code: service + ".not-healthy", Message: service + ": not healthy"})
		}
		if len(unwell) > 0 {
			// One read of the container's log serves both problems.
			tail := r.lastLogs(ctx, service)
			for _, problem := range unwell {
				problem.Log = tail
				problems = append(problems, problem)
			}
		}
	}
	// A running router whose layout this build does not share is reported with
	// the advice Ensure refuses with. No migration: it is stopped and started.
	mismatch, err := r.protocolMismatch(*inspected[0])
	if err != nil {
		return nil, Running{}, err
	}
	if mismatch != "" {
		problems = append(problems, Problem{Code: "router.protocol", Message: mismatch})
	}
	return problems, runningFrom(*inspected[0]), nil
}

// Report is what `router status` says, and where.
type Report struct {
	Output []string
	Errors []string
	Failed bool
}

// StatusReport builds the router's status report: `running` only means a
// container exists, so a stack whose containers have exited or gone
// unhealthy must not read as running on the one stream a script is told to
// read; the reasons go to errors, and they are what makes the report failed.
func StatusReport(status RouterStatus) Report {
	if !status.Reachable {
		joined := strings.TrimSuffix(strings.Join(status.Messages(), "; "), ".")
		return Report{Output: []string{}, Errors: []string{"Router state unknown: " + joined}, Failed: true}
	}
	// The status answers a not-running problem and nothing else when every
	// container is gone, so there is no other reason to drop here.
	if !status.Running {
		return Report{Output: []string{fmt.Sprintf("not running (would use 127.0.0.1:%d)", status.Port)}, Errors: []string{}, Failed: false}
	}
	port := status.RunningPort
	if port == 0 {
		port = status.Port
	}
	mode := "mode unknown"
	if status.Mode != "" {
		mode = string(status.Mode) + " mode"
	}
	where := fmt.Sprintf("127.0.0.1:%d, %s", port, mode)
	output := "running on " + where
	if len(status.Problems) > 0 {
		output = fmt.Sprintf("degraded on %s (%d problem(s) reported on stderr)", where, len(status.Problems))
	}
	errs := make([]string, len(status.Problems))
	for i, problem := range status.Problems {
		errs[i] = "Router problem: " + problem.Message
		if problem.Log != "" {
			errs[i] += "\n  Last log lines:\n    " + strings.ReplaceAll(problem.Log, "\n", "\n    ")
		}
	}
	return Report{Output: []string{output}, Errors: errs, Failed: len(status.Problems) > 0}
}

// Down stops the router. It acts on the Compose project by name alone, so
// the router is removable from a berth root that never wrote a stack file —
// the one a refusal tells the user to run this from. --remove-orphans stays
// because it still catches one-off `compose run` containers of the project.
func (r *Router) Down(ctx context.Context) error {
	args := []string{"compose", "--project-name", r.ProjectName, "down", "--remove-orphans"}
	spec := run.Spec{AllowFailure: true, Output: run.Tee, Env: docker.ComposeEnvironment(r.environ), Timeout: r.timeouts.Down}
	result, err := docker.Run(ctx, r.runner, args, spec)
	if err != nil {
		return err
	}
	if result.ExitCode == 0 {
		return nil
	}
	// A timeout, signal or output overflow is reported through run.Failure,
	// the same wording and exit code (124 on a timeout) any other bounded
	// call gets — a hand-built message here would silently drop all of
	// that. A plain Compose refusal instead gets its own wording ("The
	// shared router did not stop cleanly:\n…").
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
// the router reads. Every field is a plain value, not a pointer: a container
// missing a section (never seen from a real daemon, only from a hand-built
// test fixture) reads the same as one whose section is present but empty.
type ContainerInspect struct {
	Config          InspectConfig
	HostConfig      InspectHostConfig
	NetworkSettings InspectNetworkSettings
	State           InspectState
}

// InspectConfig is `docker inspect`'s .Config.
type InspectConfig struct {
	Cmd    []string
	Labels map[string]string
}

// InspectHostConfig is `docker inspect`'s .HostConfig.
type InspectHostConfig struct {
	NetworkMode string
}

// InspectPortBinding is one published port's host binding.
type InspectPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string
}

// InspectNetworkSettings is `docker inspect`'s .NetworkSettings.
type InspectNetworkSettings struct {
	Ports map[string][]InspectPortBinding
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
