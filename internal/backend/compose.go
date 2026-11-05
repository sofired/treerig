// Package backend is the shipping backend: Docker Compose, driven through the
// `docker` CLI — never an SDK — which is what makes this layer portable.
//
// Ownership is the whole point. Every resource Compose creates for a worktree
// is stamped with three labels: <namespace>.managed, .repository-uuid and
// .worktree-uuid. Inventory finds resources by those labels, verifies the
// full tuple on each one, and refuses everything if any single resource
// fails. Reap verifies again before removing.
//
// Status reads the model and nothing else; Down needs no model at all — it
// acts on the project name, so a Compose file that no longer parses cannot
// stop you from stopping it.
package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/docker"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/stateformat"
	"github.com/sofired/treerig/internal/text"
	"github.com/sofired/treerig/internal/worktree"
)

// Docker-call timeouts (#14): apart from `up`'s own `--wait-timeout` flag
// (Options.WaitTimeoutSeconds), which bounds only `up --wait`'s
// health-check wait — not the request even reaching Docker — nothing else
// bounds a Docker call by default. Every introspection call — `compose
// config`, `compose ps`, and Inventory's own `ps`/`ls`/`inspect` per
// resource kind — is timed the same as doctor's own docker probes and the
// router's own precedent (internal/router/router.go): 30s. Reap's
// removal of one resource gets a minute, the same generous room the
// router's own `compose down` gets for a slow teardown — plenty even on
// slow storage for a
// single container, volume, network or image, which is a much smaller thing
// to remove than the router's whole three-container stack.
//
// `up` and this worktree's own `compose down` stay unbounded, deliberately:
// `up` may need to pull or build arbitrary images, which can legitimately
// take longer than any fixed cap. `compose down`'s own `-t`/`--timeout`
// defaults to unset, which sends the daemon no override at all, so each
// container's own `stop_grace_period` — set by the consumer's Compose file,
// not by this build — governs how long a graceful stop is allowed to take;
// a big stack with a deliberately generous grace period is exactly the
// "long-running by design" case a fixed timeout would fail while it is
// working as configured, not hung. This differs from the router's own
// `Down`, bounded to a minute: that stack is small, fixed, and
// owned by this build, not an arbitrary consumer's.
const (
	dockerProbeTimeout = 30 * time.Second
	removeTimeout      = 1 * time.Minute
)

// Timeouts overrides the runtime's per-call docker timeouts; a zero field
// keeps this port's own default. Production leaves it unset; tests use it to
// prove a hung `docker` is caught in milliseconds rather than seconds.
type Timeouts struct {
	// Probe bounds compose config, compose ps, and Inventory's own
	// ps/ls/inspect calls; default dockerProbeTimeout.
	Probe time.Duration
	// Remove bounds Reap's removal of one resource; default removeTimeout.
	Remove time.Duration
}

// withDefaults fills every unset (zero or negative) field with this
// package's own default.
func (t Timeouts) withDefaults() Timeouts {
	if t.Probe <= 0 {
		t.Probe = dockerProbeTimeout
	}
	if t.Remove <= 0 {
		t.Remove = removeTimeout
	}
	return t
}

// Compose's own labels.
const (
	ComposeProjectLabel = "com.docker.compose.project"
	ComposeServiceLabel = "com.docker.compose.service"
)

// Options configure one worktree runtime.
type Options struct {
	// ComposeFile is the absolute path to the consumer's Compose file. Its
	// directory is Compose's project directory, exactly as with `docker compose -f`.
	ComposeFile string
	// ProjectRoot is the directory containing the config file; it reaches Compose as TREERIG_ROOT.
	ProjectRoot string
	// ConfigFile is the absolute path of the loaded config, handed to Compose as TREERIG_CONFIG.
	ConfigFile     string
	ProjectName    string
	Domain         string
	ControlHome    string
	LabelNamespace string
	Runner         run.Runner
	Logger         logger.Logger
	// Environ is the environment Compose inherits; os.Environ() in production.
	Environ []string
	// URLVariables are the worktree's routed URLs as worktree.URLVariables
	// builds them from the same routes `treerig status` shows (#177):
	// TREERIG_URL and TREERIG_SERVICE_URL_<NAME>, handed to every Compose
	// invocation of the worktree's own project so compose.yaml can interpolate
	// them. Nil when the router's port could not be resolved: the variables are
	// then unset, and any inherited ones dropped, not left stale.
	URLVariables [][2]string
	// Routes are the services to route, from [service.<name>] in config.
	Routes []RouteSpec
	// SharedUse maps a service of the consumer's Compose file to the shared
	// services it uses (config's used-by, inverted); the override stamps each
	// such container with its shared-use labels (docs/design/routing.md,
	// decision 22) and changes nothing else about it.
	SharedUse map[string][]string
	// WaitTimeoutSeconds is how long `up --wait` waits for health before
	// failing; nil uses DefaultWaitTimeoutSeconds. A pointer so an explicit
	// 0 (unlike a plain zero-value int) is not mistaken for "unset".
	WaitTimeoutSeconds *int
	// Timeouts overrides this runtime's per-call docker timeouts (#14).
	Timeouts Timeouts
}

// Compose is the Compose runtime.
type Compose struct {
	options  Options
	timeouts Timeouts
}

// NewCompose makes a runtime.
func NewCompose(options Options) *Compose {
	return &Compose{options: options, timeouts: options.Timeouts.withDefaults()}
}

// project is the Compose project name a worktree's own resources are stamped
// and looked up with.
func (c *Compose) project(ref worktree.Ref) string {
	return layout.ComposeProjectName(ref.RepositoryID, ref.WorktreeID)
}

// environment is the same TREERIG_* vocabulary hooks receive, so one name means one thing:
// worktree.Environment, then Options.URLVariables (#177) over an inherited
// environment stripped of any URL variable of its own. The one place it is
// built, so every invocation of the project — up, its config reads, down,
// status — sees identical values for identical inputs.
func (c *Compose) environment(ref worktree.Ref) []string {
	source := worktree.EnvironmentSource{
		ProjectName: c.options.ProjectName, Domain: c.options.Domain, ControlHome: c.options.ControlHome,
		ConfigFile: c.options.ConfigFile, ProjectRoot: c.options.ProjectRoot,
	}
	extra := append(worktree.Variables(worktree.Environment(source, ref)), "COMPOSE_PROJECT_NAME="+c.project(ref))
	extra = append(extra, worktree.Variables(c.options.URLVariables)...)
	inherited := make([]string, 0, len(c.options.Environ))
	for _, entry := range c.options.Environ {
		name, _, _ := strings.Cut(entry, "=")
		if !worktree.IsURLVariable(name) {
			inherited = append(inherited, entry)
		}
	}
	return docker.ComposeEnvironment(inherited, extra...)
}

// composeArgs is the full `docker compose ...` command line compose() builds
// for args and files — factored out so a caller that must report a failure
// with run.Failure after a call already made (Down, Reap) can name the exact
// command line docker.Run used, rather than a reconstruction that could
// drift from it (found in review of #55).
func (c *Compose) composeArgs(ref worktree.Ref, args []string, files []string) []string {
	full := []string{"compose", "--project-name", c.project(ref)}
	if files != nil {
		full = append(full, "--project-directory", filepath.Dir(c.options.ComposeFile))
		for _, file := range files {
			full = append(full, "-f", file)
		}
	}
	return append(full, args...)
}

// compose is one Compose invocation. Files are passed only when a verb needs
// the model; the project directory is then the consumer file's own directory.
func (c *Compose) compose(ctx context.Context, ref worktree.Ref, args []string, files []string, spec run.Spec) (run.Result, error) {
	spec.Env = docker.Merge(c.environment(ref), spec.Env...)
	return docker.Run(ctx, c.options.Runner, c.composeArgs(ref, args, files), spec)
}

// model is the consumer's Compose file as Compose itself resolves it. Only
// the service names are needed here.
type model struct {
	Services map[string]json.RawMessage `json:"services"`
}

// readModel runs `docker compose config` and decodes just enough of it —
// the service names — to report status; a document Compose does not shape
// the way model expects is refused rather than partially read.
func (c *Compose) readModel(ctx context.Context, ref worktree.Ref) (model, error) {
	file := c.options.ComposeFile
	// --no-interpolate is the one `config` flag the documented floor (Compose
	// 2.21) is sure to have; --no-path-resolution and --no-env-resolution came
	// later, so a file's env_file entries are still read.
	stdout, err := c.composeConfigJSON(ctx, ref, "--no-interpolate")
	var unavailable *docker.UnavailableError
	if err != nil && ctx.Err() == nil && !errors.As(err, &unavailable) {
		// Some Compose releases reject a value left uninterpolated in a typed
		// field (a port, a number); the file may be fine as `up` reads it, so
		// ask once more the usual way. The first call's benefit, a ${VAR:?}
		// check that does not fire, stays when it succeeds.
		stdout, err = c.composeConfigJSON(ctx, ref)
	}
	if err != nil {
		return model{}, err
	}
	var raw any
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		return model{}, clierr.Newf("Compose returned invalid JSON for %s.", file)
	}
	var parsed model
	document, ok := raw.(map[string]any)
	if !ok {
		return model{}, clierr.Newf("Compose returned an unrecognized document for %s.", file)
	}
	if services, present := document["services"]; present {
		table, ok := services.(map[string]any)
		if !ok {
			return model{}, clierr.Newf("Compose returned an unrecognized document for %s.", file)
		}
		parsed.Services = map[string]json.RawMessage{}
		for name, service := range table {
			if _, ok := service.(map[string]any); !ok {
				return model{}, clierr.Newf("Compose returned an unrecognized document for %s.", file)
			}
			encoded, _ := json.Marshal(service)
			parsed.Services[name] = encoded
		}
	}
	return parsed, nil
}

// activeEndpoints matches Compose's refusal to remove a network something is
// still attached to.
var activeEndpoints = regexp.MustCompile(`(?i)has active endpoints`)

// IsActiveEndpoints reports whether err is Compose's refusal to remove a
// network something is still attached to, as the failed command reported it.
func IsActiveEndpoints(err error) bool {
	return err != nil && activeEndpoints.MatchString(err.Error())
}

// Down stops the project; data is kept. Deliberately carries no Timeout
// (#14): see this file's own timeouts comment for why. Tolerates Compose's
// own refusal to remove a network something is still attached to as the
// expected outcome, not a failure. Only that one, specific outcome is
// intercepted: AllowFailure lets this read the result instead of Finish
// rejecting it automatically, so every other failure — a genuine Compose
// refusal, a timeout, a signal, an output overflow — must still be reported
// through run.Failure, the same wording and exit code (124 on a timeout) any
// other bounded call gets, rather than a bespoke message that silently
// dropped all of it.
func (c *Compose) Down(ctx context.Context, ref worktree.Ref) error {
	args := []string{"down", "--remove-orphans"}
	spec := run.Spec{AllowFailure: true, Output: run.Tee}
	result, err := c.compose(ctx, ref, args, nil, spec)
	if err != nil {
		return err
	}
	if result.ExitCode == 0 {
		return nil
	}
	if !result.TimedOut && !result.OutputOverflow && result.Signal == 0 && activeEndpoints.MatchString(result.Stderr) {
		c.options.Logger.Info(fmt.Sprintf("A network of %s is kept while something else is attached to it.", c.project(ref)))
		return nil
	}
	return run.Failure(run.Spec{Command: "docker", Args: c.composeArgs(ref, args, nil), Timeout: spec.Timeout}, result)
}

// LogTail is how many lines of each container's history Logs shows before it
// follows: enough to see how the services just came up, not a long-running
// project's whole past.
const LogTail = 100

// Logs follows the logs of every container of the worktree's project:
// `docker compose logs --follow --tail 100`, through the project name alone,
// as Down does. The follower is run Interactive: output goes straight to
// this process's own streams, nothing is captured (a follow has no end to
// bound a capture by), and the follower gets its own process group, which
// the runner drains whenever Logs returns, so nothing it started outlives the
// call.
//
// Whether Compose's follower ends when the project's containers go away
// varies: on Compose 5.5.0 it has been seen to keep waiting for new
// containers when a `restart: always` service had restarted under it (it
// depends on where in the restart cycle the containers were removed), and
// otherwise to exit 0 once they are gone; Compose 2.38.2 always exits. So the
// caller cannot count on it ending, or on it staying. The Result is
// returned whatever the follower's exit, for the caller to judge: Logs knows
// nothing of what ending means. A follower this process cancelled by a signal
// comes back as that cancellation's error, with no Result.
func (c *Compose) Logs(ctx context.Context, ref worktree.Ref) (run.Result, error) {
	args := []string{"logs", "--follow", "--tail", strconv.Itoa(LogTail)}
	return c.compose(ctx, ref, args, nil, run.Spec{AllowFailure: true, Output: run.Interactive, Label: "log follower"})
}

// HasContainers reports whether the worktree's project has any container at
// all, running or not: `docker compose ps --all --quiet`, bounded like the
// other probes.
func (c *Compose) HasContainers(ctx context.Context, ref worktree.Ref) (bool, error) {
	result, err := c.compose(ctx, ref, []string{"ps", "--all", "--quiet"}, nil, run.Spec{AllowFailure: true, Timeout: c.timeouts.Probe})
	if err != nil {
		return false, err
	}
	if result.ExitCode != 0 {
		return false, clierr.Newf("Compose could not list containers:\n%s", firstNonEmpty(strings.TrimSpace(result.Stderr), strings.TrimSpace(result.Stdout)))
	}
	return strings.TrimSpace(result.Stdout) != "", nil
}

// Status lists the containers of every declared service, in service-name
// order, one entry per container (a scaled service appears once per replica),
// and a NotCreated entry for a declared service that has none, the state after
// `down`. worktree.Summarize collapses replicas for a display.
func (c *Compose) Status(ctx context.Context, ref worktree.Ref) ([]worktree.ServiceStatus, error) {
	config, err := c.readModel(ctx, ref)
	if err != nil {
		return nil, err
	}
	result, err := c.compose(ctx, ref, []string{"ps", "--format", "json", "--all"}, nil, run.Spec{AllowFailure: true, Timeout: c.timeouts.Probe})
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, clierr.Newf("Compose could not list services:\n%s", firstNonEmpty(strings.TrimSpace(result.Stderr), strings.TrimSpace(result.Stdout)))
	}
	records, err := parsePsOutput(result.Stdout)
	if err != nil {
		return nil, err
	}
	if err := c.completeExits(ctx, records); err != nil {
		return nil, err
	}
	seen := map[string][]worktree.ServiceStatus{}
	for _, record := range records {
		status := worktree.ContainerStatus(record.State, record.Health, record.ExitCode, record.ExitKnown, record.OOMKilled, record.OOMRead)
		status.Service = record.Service
		// Docker's own word for a state it has no name for is printed later;
		// it is cleaned here too, as defence in depth.
		status.Detail = text.Printable(status.Detail)
		seen[record.Service] = append(seen[record.Service], status)
	}
	names := make([]string, 0, len(config.Services))
	for name := range config.Services {
		names = append(names, name)
	}
	// JavaScript's default sort: treerig lists service names in this order.
	slices.SortFunc(names, text.CompareUTF16)
	statuses := make([]worktree.ServiceStatus, 0, len(names))
	for _, name := range names {
		if containers, ok := seen[name]; ok {
			statuses = append(statuses, containers...)
		} else {
			statuses = append(statuses, worktree.ServiceStatus{Service: name, State: worktree.NotCreated})
		}
	}
	return statuses, nil
}

// inspectConcurrency bounds how many containers completeExits inspects at once.
const inspectConcurrency = 8

// completeExits asks Docker about each exited container whose story Compose
// leaves open: one that reported no exit code, and one that exited 137, which
// is what a stop, a reboot or a daemon restart leaves and also what the
// out-of-memory killer leaves; only Docker says which. The inspects run
// concurrently under one shared deadline (the probe timeout), so a worktree
// with many such containers is no slower than one. A container that cannot be
// inspected keeps what Compose said, and an exit 137 whose flag could not be
// read is then unknown, never assumed to be a stop. Only a cancellation is an
// error.
func (c *Compose) completeExits(ctx context.Context, records []psRecord) error {
	deadline, cancel := context.WithTimeout(ctx, c.timeouts.Probe)
	defer cancel()
	var group errgroup.Group
	group.SetLimit(inspectConcurrency)
	for i := range records {
		record := &records[i]
		if !strings.EqualFold(record.State, "exited") || record.ID == "" || (record.ExitKnown && record.ExitCode != 137) {
			continue
		}
		group.Go(func() error {
			container, found, err := docker.InspectContainer(deadline, c.options.Runner, record.ID, c.timeouts.Probe)
			if err == nil && found {
				record.ExitCode, record.ExitKnown, record.OOMKilled, record.OOMRead = container.ExitCode, true, container.OOMKilled, true
			}
			return nil
		})
	}
	_ = group.Wait()
	return ctx.Err()
}

// psRecord is one container of `docker compose ps --format json`.
type psRecord struct {
	// ID is the container's ID, when Compose reports it.
	ID      string
	Service string
	State   string
	Health  string
	// ExitCode is how the container ended; ExitKnown says Compose gave one.
	ExitCode  int
	ExitKnown bool
	// OOMKilled and OOMRead are filled in by completeExits, never by Compose:
	// OOMRead says Docker was asked and answered.
	OOMKilled bool
	OOMRead   bool
}

// parsePsOutput reads `docker compose ps --format json`: one document per
// line since Compose 2.21, the documented floor, and a single document — an
// array, or null for none — before that. Output nobody can parse is refused
// rather than read as "no service has a container": an empty list is a complete
// answer, garbage is not.
func parsePsOutput(stdout string) ([]psRecord, error) {
	output := strings.TrimSpace(stdout)
	if output == "" {
		return nil, nil
	}
	unreadable := func() error {
		shown := output
		if text.JSLen(shown) > 400 {
			shown = text.TruncateUTF16(shown, 400) + "\n…"
		}
		return clierr.Newf("Compose returned service state in a form this version of %s cannot read. "+
			"Run `docker compose version` and report it with the output below:\n%s", brand.Name, shown)
	}
	record := func(candidate any) (psRecord, error) {
		object, ok := candidate.(map[string]any)
		if !ok {
			return psRecord{}, unreadable()
		}
		service, serviceOK := object["Service"].(string)
		state, stateOK := object["State"].(string)
		if !serviceOK || !stateOK {
			return psRecord{}, unreadable()
		}
		health := ""
		if raw, present := object["Health"]; present {
			value, ok := raw.(string)
			if !ok {
				return psRecord{}, unreadable()
			}
			health = value
		}
		parsed := psRecord{Service: service, State: state, Health: health}
		if id, ok := object["ID"].(string); ok {
			parsed.ID = id
		}
		if raw, present := object["ExitCode"]; present {
			value, ok := raw.(float64)
			if !ok || value != float64(int(value)) {
				return psRecord{}, unreadable()
			}
			parsed.ExitCode, parsed.ExitKnown = int(value), true
		}
		return parsed, nil
	}

	var whole any
	decoder := json.NewDecoder(bytes.NewReader([]byte(output)))
	if err := decoder.Decode(&whole); err == nil && !decoder.More() {
		switch value := whole.(type) {
		case nil:
			return nil, nil
		case []any:
			records := make([]psRecord, 0, len(value))
			for _, item := range value {
				parsed, err := record(item)
				if err != nil {
					return nil, err
				}
				records = append(records, parsed)
			}
			return records, nil
		default:
			parsed, err := record(value)
			if err != nil {
				return nil, err
			}
			return []psRecord{parsed}, nil
		}
	}
	// One document per line; every non-blank line must be a record.
	var records []psRecord
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var candidate any
		if err := json.Unmarshal([]byte(line), &candidate); err != nil {
			return nil, unreadable()
		}
		parsed, err := record(candidate)
		if err != nil {
			return nil, err
		}
		records = append(records, parsed)
	}
	return records, nil
}

// resourceType is one kind of Docker resource, in removal order.
type resourceType struct {
	kind           string
	listArguments  []string
	inspectCommand []string
	removeArgs     []string
	// labelPrefix is where Docker keeps labels in the inspect document for this kind.
	labelPrefix string
}

// resourceTypes is in removal order: containers hold volumes and networks; images last.
var resourceTypes = []resourceType{
	{kind: "container", listArguments: []string{"ps", "-aq"}, inspectCommand: []string{"inspect"}, removeArgs: []string{"rm", "--force"}, labelPrefix: ".Config"},
	{kind: "volume", listArguments: []string{"volume", "ls", "-q"}, inspectCommand: []string{"volume", "inspect"}, removeArgs: []string{"volume", "rm"}},
	{kind: "network", listArguments: []string{"network", "ls", "-q"}, inspectCommand: []string{"network", "inspect"}, removeArgs: []string{"network", "rm"}},
	{kind: "image", listArguments: []string{"image", "ls", "-q"}, inspectCommand: []string{"image", "inspect"}, removeArgs: []string{"image", "rm"}, labelPrefix: ".Config"},
}

// list is every kind's ID matching all of filters, deduplicated and sorted:
// the raw material Inventory verifies before trusting any of it.
func (c *Compose) list(ctx context.Context, kind resourceType, filters []string) ([]string, error) {
	args := slices.Clone(kind.listArguments)
	for _, filter := range filters {
		args = append(args, "--filter", "label="+filter)
	}
	result, err := docker.Run(ctx, c.options.Runner, args, run.Spec{Timeout: c.timeouts.Probe})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var ids []string
	for _, id := range strings.Fields(result.Stdout) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

// verify is one inspect per resource, reading the three ownership labels,
// the format label and the Compose service name together. Any mismatch is a
// refusal; a resource whose labels cannot be read is treated the same way.
func (c *Compose) verify(ctx context.Context, ref worktree.Ref, kind resourceType, id string) (string, error) {
	namespace := c.options.LabelNamespace
	label := func(name string) string { return fmt.Sprintf(`{{ index %s.Labels "%s" }}`, kind.labelPrefix, name) }
	format := strings.Join([]string{
		label(brand.ManagedLabel(namespace)),
		label(ComposeProjectLabel),
		label(brand.RepositoryLabel(namespace)),
		label(brand.WorktreeLabel(namespace)),
		label(ComposeServiceLabel),
		label(brand.FormatLabel(namespace)),
	}, "\t")
	args := append(slices.Clone(kind.inspectCommand), "--format", format, id)
	result, err := docker.Run(ctx, c.options.Runner, args, run.Spec{AllowFailure: true, Timeout: c.timeouts.Probe})
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", clierr.Newf("Refusing to touch %s %s: its labels could not be inspected.", kind.kind, id)
	}
	fields := strings.Split(strings.TrimSpace(result.Stdout), "\t")
	field := func(i int) string {
		if i < len(fields) {
			return fields[i]
		}
		return ""
	}
	managed, project, repositoryID, worktreeID, service, format := field(0), field(1), field(2), field(3), field(4), field(5)
	// Compose's own project label is not promised on images: its bake
	// builder leaves it off, while our build.labels always land. An image may
	// lack it; one that carries it must carry ours.
	projectOK := project == c.project(ref) || (kind.kind == "image" && project == "")
	if managed != "true" || !projectOK || repositoryID != ref.RepositoryID || worktreeID != ref.WorktreeID {
		return "", clierr.Newf("Refusing to touch %s %s: its ownership labels do not match this worktree.", kind.kind, id)
	}
	// A volume is proven by the ownership labels alone (see ownershipLabels).
	if kind.kind == "volume" {
		return service, nil
	}
	// The ownership labels above prove ownership only in the format they
	// were written in. A resource of a newer format may mean them
	// differently, and one with no format label is not one this build
	// stamped: neither is proven owned, so neither is touched
	// (docs/design/state.md).
	switch verdict, found := stateformat.Label(format, stateformat.DockerLabel); verdict {
	case stateformat.Current:
	case stateformat.FromNewer:
		return "", stateformat.Newer("touch", stateformat.DescribeResource(kind.kind, id, service), found, stateformat.DockerLabel)
	default:
		return "", clierr.Newf("Refusing to touch %s: %s", stateformat.DescribeResource(kind.kind, id, service), stateformat.Unproven(kind.kind, id, brand.FormatLabel(namespace), "it is this worktree's"))
	}
	return service, nil
}

// Inventory is everything this worktree owns, in removal order, every item
// verified. Resources are found two ways — by Compose project and by
// ownership labels — so a resource with the right owner but a mangled
// project label is still found, and then refused by verification rather than
// orphaned. Every resource is verified before any is returned, so a caller
// that reaps the result cannot remove half and then hit a refusal.
func (c *Compose) Inventory(ctx context.Context, ref worktree.Ref) ([]worktree.OwnedResource, error) {
	namespace := c.options.LabelNamespace
	ownership := []string{
		brand.ManagedLabel(namespace) + "=true",
		brand.RepositoryLabel(namespace) + "=" + ref.RepositoryID,
		brand.WorktreeLabel(namespace) + "=" + ref.WorktreeID,
	}
	var owned []worktree.OwnedResource
	for _, kind := range resourceTypes {
		// Built images are found by ownership alone: Compose stamps its project
		// label on them only on some build paths, and a project-only match would
		// sweep in images built before the override stamped them.
		var byProject []string
		if kind.kind != "image" {
			ids, err := c.list(ctx, kind, []string{ComposeProjectLabel + "=" + c.project(ref)})
			if err != nil {
				return nil, err
			}
			byProject = ids
		}
		byOwnership, err := c.list(ctx, kind, ownership)
		if err != nil {
			return nil, err
		}
		ids := slices.Concat(byProject, byOwnership)
		slices.Sort(ids)
		for _, id := range slices.Compact(ids) {
			service, err := c.verify(ctx, ref, kind, id)
			if err != nil {
				return nil, err
			}
			owned = append(owned, worktree.OwnedResource{Kind: kind.kind, ID: id, Service: service})
		}
	}
	return owned, nil
}

// ErrNetworkKept is Reap's own signal that a network resource was left in
// place rather than actually removed: Compose's "has active endpoints"
// refusal, tolerated the same way Down already tolerates it, rather than
// failed. Always wrapped (`fmt.Errorf("%w: …", ErrNetworkKept)` or similar),
// never returned bare, so a caller checks with errors.Is against whatever
// context Reap's own error adds. Reap must not report success for a
// resource that is not actually gone, or RemoveWithContext would go on to
// delete the worktree's home, registration and identity marker over a
// network still sitting on the daemon — see RemoveWithContext's own
// handling of this error.
var ErrNetworkKept = errors.New("network still has active endpoints; left in place")

// Reap verifies a resource again and removes it. A network's own removal
// tolerates Compose's "has active endpoints" refusal the same way Down
// already does, reporting ErrNetworkKept instead of succeeding: something
// else is still attached to the network, and the caller must not treat the
// worktree as gone while a network it owns sits on the daemon.
func (c *Compose) Reap(ctx context.Context, ref worktree.Ref, resource worktree.OwnedResource) error {
	index := slices.IndexFunc(resourceTypes, func(kind resourceType) bool { return kind.kind == resource.Kind })
	if index < 0 {
		return clierr.Newf("Unknown resource kind: %s", resource.Kind)
	}
	kind := resourceTypes[index]
	if _, err := c.verify(ctx, ref, kind, resource.ID); err != nil {
		return err
	}
	args := append(slices.Clone(kind.removeArgs), resource.ID)
	spec := run.Spec{AllowFailure: true, Timeout: c.timeouts.Remove}
	result, err := docker.Run(ctx, c.options.Runner, args, spec)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		// Only the tolerated network case is intercepted here (found in
		// review, round 2 of #55) — and only when the failure is Compose's
		// own clean refusal, not a timeout, signal or output overflow
		// dressed up as one: AllowFailure lets this read the result instead
		// of Finish rejecting it automatically, so every other failure must
		// still be reported through run.Failure, the same wording and exit
		// code (124 on a timeout) any other bounded call gets.
		if kind.kind == "network" && !result.TimedOut && !result.OutputOverflow && result.Signal == 0 && activeEndpoints.MatchString(result.Stderr) {
			c.options.Logger.Info(fmt.Sprintf("A network of %s is kept while something else is attached to it.", c.project(ref)))
			return fmt.Errorf("network %s: %w", resource.ID, ErrNetworkKept)
		}
		return run.Failure(run.Spec{Command: "docker", Args: args, Timeout: spec.Timeout}, result)
	}
	c.options.Logger.Info(fmt.Sprintf("Removed %s %s.", resource.Kind, resource.ID))
	return nil
}

// firstNonEmpty is the first non-empty value, or "" if every one is empty.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
