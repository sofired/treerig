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
	// ProjectName defaults to DefaultName; overridable so tests can run a
	// router beside a developer's real one.
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
	Page PlaceholderPage
	// Timeouts overrides this router's per-call docker timeouts (#14).
	Timeouts Timeouts
}

// Router is what the rest of the tool needs to know about the shared router.
type Router struct {
	Port        int
	Network     string
	IndexURL    string
	ProjectName string

	state          layout.RouterLayout
	labelNamespace string
	runner         run.Runner
	logger         logger.Logger
	environ        []string
	invocation     string
	commandName    string
	page           PlaceholderPage
	timeouts       Timeouts
}

// New builds a Router. The network name is interpolated into Traefik's
// discovery predicate, so a project or network name that could rewrite it is refused.
func New(options Options) (*Router, error) {
	projectName := options.ProjectName
	if projectName == "" {
		projectName = DefaultName
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
		page:           page,
		timeouts:       options.Timeouts.withDefaults(),
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

// write writes the stack document and makes sure the directories it mounts
// exist and serve.
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
	})
	// JSON is YAML; Compose reads it as such.
	return files.AtomicJSON(r.state.ComposeFile, document)
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

// noteUnroutedAfterUpgrade is said only after the upgrade is a fact: a
// router from before the routed-by label discovered every container with
// Traefik labels; the new one discovers only containers labelled for it, and
// a label cannot be added to a container already running. Each worktree's
// next `up` recreates its own.
func (r *Router) noteUnroutedAfterUpgrade(previous int, hadHistory bool) {
	if !hadHistory || previous >= RoutedBySinceProtocol {
		return
	}
	r.logger.Warn(fmt.Sprintf("Worktrees already up elsewhere stay unrouted until their next '%s up'.", r.commandNameOr()))
}

/* ---- verification: every property the stack asked for, checked on the running container */

// commonProblems is every check shared by all three services: ownership,
// the component and protocol labels, and the hardening every container carries.
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
func (r *Router) routerProblems(inspected ContainerInspect) []string {
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
	if !sameSet(networks, []string{r.Network, r.ProjectName + "_" + DockerAPINetwork}) {
		joined := strings.Join(networks, ", ")
		if joined == "" {
			joined = "(none)"
		}
		problems = append(problems, fmt.Sprintf("router: attached to networks %s instead of the shared and proxy networks", joined))
	}
	if strings.Join(inspected.Config.Cmd, "\x00") != strings.Join(RouterCommand(r.Network), "\x00") {
		problems = append(problems, "router: command line differs")
	}
	if inspected.Config.Image != RouterImage {
		problems = append(problems, "router: image is not the pinned digest")
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

// dockerAPIProblems is every way the docker-api proxy container can differ
// from what the stack asked for: its API access, network, image,
// capabilities and mount, on top of commonProblems.
func (r *Router) dockerAPIProblems(inspected ContainerInspect) []string {
	problems := r.commonProblems(inspected, DockerAPIService)
	environment := map[string]string{}
	for _, entry := range inspected.Config.Env {
		name, value, _ := strings.Cut(entry, "=")
		environment[name] = value
	}
	var wrong []string
	for name, expected := range DockerAPIAccess() {
		if environment[name] != expected {
			wrong = append(wrong, name)
		}
	}
	if len(wrong) > 0 {
		sort.Strings(wrong)
		problems = append(problems, fmt.Sprintf("docker-api: API access differs (%s)", strings.Join(wrong, ", ")))
	}
	networks := sortedNetworkKeys(inspected.NetworkSettings.Networks)
	if !sameSet(networks, []string{r.ProjectName + "_" + DockerAPINetwork}) {
		problems = append(problems, "docker-api: attached to a network other than the internal proxy network")
	}
	if inspected.Config.Image != DockerAPIImage {
		problems = append(problems, "docker-api: image is not the pinned digest")
	}
	if len(inspected.HostConfig.CapAdd) != 0 {
		problems = append(problems, "docker-api: has added capabilities")
	}
	for _, bindings := range inspected.NetworkSettings.Ports {
		if len(bindings) > 0 {
			problems = append(problems, "docker-api: publishes a port")
			break
		}
	}
	var socket *InspectMount
	for i, mount := range inspected.Mounts {
		if mount.Destination == DockerSocket {
			socket = &inspected.Mounts[i]
			break
		}
	}
	if len(inspected.Mounts) != 1 || socket == nil || socket.Type != "bind" || socket.Source != DockerSocket || socket.RW {
		problems = append(problems, "docker-api: mounts are not exactly the read-only Docker socket")
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
	services := []string{RouterService, DockerAPIService, IndexService}
	ids := make([]string, len(services))
	for i, service := range services {
		id, err := r.containerID(ctx, service)
		if err != nil {
			return nil, err
		}
		ids[i] = id
	}
	// All three gone is a router that is not running. One or two gone is a
	// router that is still bound to its port and still routing, with a hole
	// in it — reporting that as "not running" would tell an operator the
	// opposite of what is true.
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
	if inspected[0] == nil || inspected[1] == nil || inspected[2] == nil {
		return []string{"the router could not be inspected"}, nil
	}
	var problems []string
	problems = append(problems, r.routerProblems(*inspected[0])...)
	problems = append(problems, r.dockerAPIProblems(*inspected[1])...)
	problems = append(problems, r.indexProblems(*inspected[2])...)
	return problems, nil
}

// Ensure is idempotent: starts the router if needed, verifies it, and fails
// loudly if it cannot be made right. The caller holds the router's lock
// around this call (see lock.RouterScope and lifecycle.Requests.Router).
//
// secureState runs first, before previousProtocol's own possible write of
// the protocol record: a symlinked state directory must be refused before
// anything — this call included — ever touches whatever it points to.
func (r *Router) Ensure(ctx context.Context) error {
	if err := secureState(r.state); err != nil {
		return err
	}
	previous, hadHistory, err := r.previousProtocol(ctx)
	if err != nil {
		return err
	}
	if err := r.write(time.Now()); err != nil {
		return err
	}
	if err := r.start(ctx, nil); err != nil {
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
		remaining, err := r.Verify(ctx)
		if err != nil {
			return err
		}
		if len(remaining) > 0 {
			return clierr.Newf("The shared router could not be brought to its expected configuration:\n  %s", strings.Join(remaining, "\n  "))
		}
	}
	// The warning is about what Verify just established, so it is said even
	// if recording the new protocol fails.
	r.noteUnroutedAfterUpgrade(previous, hadHistory)
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
		return clierr.Newf(
			"The shared router failed to start. Make sure 127.0.0.1:%d is free, or choose another port with %s_ROUTER_PORT.\n%s",
			r.Port, brand.EnvPrefix, detail)
	}
	return nil
}

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
	result, err := docker.Run(ctx, r.runner, []string{
		"compose", "--project-name", r.ProjectName, "down", "--remove-orphans",
	}, run.Spec{AllowFailure: true, Output: run.Tee, Env: docker.ComposeEnvironment(r.environ), Timeout: r.timeouts.Down})
	if err != nil {
		return err
	}
	if result.ExitCode == 0 {
		return nil
	}
	if activeEndpoints.MatchString(result.Stderr) {
		r.logger.Info(fmt.Sprintf("Network %s is kept while worktree services are attached to it.", r.Network))
		return nil
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
	Cmd    []string
	Env    []string
	Image  string
	Labels map[string]string
}

// InspectHostConfig is `docker inspect`'s .HostConfig.
type InspectHostConfig struct {
	CapAdd         []string
	CapDrop        []string
	ReadonlyRootfs bool
	SecurityOpt    []string
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
