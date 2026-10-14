package commandstest

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/stateformat"
	"github.com/sofired/berth/internal/worktree"
)

// Docker is a stateful stand-in for the `docker` CLI: it keeps containers,
// volumes, networks and images with their labels, and answers the list,
// inspect, remove and compose calls the Compose backend makes, so command
// tests drive the real backend end to end without a daemon.
type Docker struct {
	mu        sync.Mutex
	resources map[string]map[string]map[string]string // kind → id → labels
	inspected map[string]int
	names     map[string]string // container id → name
	// serviceConfig adds keys to a service's entry in `compose config`.
	serviceConfig map[string]map[string]any
	tampers       map[string]tamper
	next          int
	calls         []run.Spec
	// Services is what `compose config` reports the Compose file declares.
	Services []string
	// before runs once, ahead of the first docker call, outside the lock: the
	// seam a test uses to let a rival act after the target was resolved.
	before func()
	// removeNetworkExitCode/removeNetworkStderr make every `network rm`
	// call answer this way instead of succeeding, without deleting the
	// resource — FailNetworkRemove's own state, for a test pinning Reap's
	// own ErrNetworkKept tolerance (internal/backend/compose.go) and
	// RemoveWithContext's handling of it. Sticky, like
	// disconnectExitCode/disconnectStderr: a test clears it
	// (FailNetworkRemove(0, "")) to simulate the network clearing before a
	// retried removal.
	removeNetworkExitCode int
	removeNetworkStderr   string
	// upStderr, when set, makes `compose up` fail with it after upDuring
	// runs: FailUp's own state, for a test where a port is taken between
	// the check and the start.
	upStderr  string
	upDuring  func()
	upFailing bool

	// The rest models containers' state, networks and shared services; see
	// compose.go. All of it is zero for a test that does not use it.
	status       map[string]string               // container id → Docker state, "running" when absent
	health       map[string]string               // container id → health status
	attached     map[string]map[string][]string  // container id → network id → aliases
	mounts       map[string][]string             // container id → volume names
	mountPaths   map[string]string               // volume name → where its containers mount it
	bindings     map[string]map[string][]binding // container id → "1025/tcp" → bindings
	networkNames map[string]string               // network id → name
	// realistic makes `compose up` create what a real one would; Realistic.
	realistic bool
	// sharedServices is the shared Compose file's `compose config` services.
	sharedServices map[string]map[string]any
	// composeUp, when set, runs ahead of every `compose up`, outside the lock.
	composeUp func(project string)
	// errors injects a failure for a docker call whose joined args contain a key.
	errors []injected
}

// injected is a failure FailCall stages for matching calls.
type injected struct {
	contains string
	stderr   string
	code     int
	times    int
	// skip is how many matching calls pass before the failure starts.
	skip int
}

// binding is one published port of a container.
type binding struct{ ip, port string }

// FailUp makes every subsequent `docker compose up` run during, outside the
// fake's lock, then exit 1 with stderr: Docker's bind error, say, after a
// rival took a port.
func (d *Docker) FailUp(stderr string, during func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.upFailing, d.upStderr, d.upDuring = true, stderr, during
}

// SetServiceConfig adds key to service's entry in what `compose config`
// reports, such as network_mode or scale.
func (d *Docker) SetServiceConfig(service, key string, value any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.serviceConfig == nil {
		d.serviceConfig = map[string]map[string]any{}
	}
	if d.serviceConfig[service] == nil {
		d.serviceConfig[service] = map[string]any{}
	}
	d.serviceConfig[service][key] = value
}

// FailNetworkRemove makes every subsequent `docker network rm` call answer
// with exitCode and stderr instead of succeeding, leaving the network in
// place; exitCode 0 clears it, so a resource `network rm` is next asked to
// remove succeeds normally again.
func (d *Docker) FailNetworkRemove(exitCode int, stderr string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removeNetworkExitCode, d.removeNetworkStderr = exitCode, stderr
}

type tamper struct {
	after  int
	labels map[string]string
}

// NewDocker is an empty daemon whose Compose file declares services.
func NewDocker(services ...string) *Docker {
	return &Docker{
		resources: map[string]map[string]map[string]string{"container": {}, "volume": {}, "network": {}, "image": {}},
		inspected: map[string]int{},
		names:     map[string]string{},
		tampers:   map[string]tamper{},
		Services:  services,
		status:    map[string]string{}, health: map[string]string{},
		attached: map[string]map[string][]string{}, mounts: map[string][]string{}, mountPaths: map[string]string{},
		bindings: map[string]map[string][]binding{}, networkNames: map[string]string{},
		sharedServices: map[string]map[string]any{},
	}
}

// Ownership is the three ownership labels, the format label and Compose's
// own project label, as `up` stamps them.
func Ownership(ref worktree.Ref, namespace string) map[string]string {
	return map[string]string{
		brand.ManagedLabel(namespace):    "true",
		brand.RepositoryLabel(namespace): ref.RepositoryID,
		brand.WorktreeLabel(namespace):   ref.WorktreeID,
		brand.FormatLabel(namespace):     strconv.Itoa(stateformat.DockerLabel),
		"com.docker.compose.project":     layout.ComposeProjectName(ref.RepositoryID, ref.WorktreeID),
	}
}

// Up is what a worktree's `up` leaves behind, as MemoryRuntime has it: one
// running container per service and one data volume, all owned. It returns
// the new ids in inventory order.
func (d *Docker) Up(ref worktree.Ref, namespace string, services ...string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ids []string
	for _, service := range services {
		labels := Ownership(ref, namespace)
		labels["com.docker.compose.service"] = service
		ids = append(ids, d.add("container", labels))
	}
	ids = append(ids, d.add("volume", Ownership(ref, namespace)))
	return ids
}

// Publish adds a running container of ref's service that publishes host port
// on loopback, with the ownership labels ref's Up gives it, and returns its
// id. The fake answers `ps --filter publish=<port>` from the "publish" label
// it sets.
func (d *Docker) Publish(ref worktree.Ref, namespace, service, name string, port int) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	labels := Ownership(ref, namespace)
	labels["com.docker.compose.service"] = service
	labels["publish"] = strconv.Itoa(port)
	id := d.add("container", labels)
	d.names[id] = name
	return id
}

// PublishForeign adds a running container that publishes host port and is
// none of berth's: it carries no ownership labels.
func (d *Docker) PublishForeign(name string, port int) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := d.add("container", map[string]string{"publish": strconv.Itoa(port)})
	d.names[id] = name
	return id
}

// PublishManaged adds a running container that publishes host port and
// carries berth's ownership labels for some other worktree.
func (d *Docker) PublishManaged(other worktree.Ref, namespace, name string, port int) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	labels := Ownership(other, namespace)
	labels["publish"] = strconv.Itoa(port)
	id := d.add("container", labels)
	d.names[id] = name
	return id
}

// Add puts one resource in the daemon with exactly these labels.
func (d *Docker) Add(kind string, labels map[string]string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.add(kind, labels)
}

// add stores a resource under kind with labels and returns its generated id,
// without acquiring the lock — callers hold it already.
func (d *Docker) add(kind string, labels map[string]string) string {
	d.next++
	id := fmt.Sprintf("%s%02d", kind[:1], d.next)
	d.resources[kind][id] = labels
	return id
}

// Labels are a resource's labels, for tests to alter; nil when it is gone.
func (d *Docker) Labels(kind, id string) map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.resources[kind][id]
}

// TamperAfter swaps a resource's labels once it has been inspected n times:
// a resource that verified when inventoried and no longer does at reap time.
func (d *Docker) TamperAfter(id string, n int, labels map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tampers[id] = tamper{after: n, labels: labels}
}

// BeforeFirstCall runs f once, ahead of the next docker call.
func (d *Docker) BeforeFirstCall(f func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.before = f
}

// Owned lists the resources still carrying ref's ownership under namespace.
func (d *Docker) Owned(ref worktree.Ref, namespace string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ids []string
	for _, kind := range []string{"container", "volume", "network", "image"} {
		for _, id := range sortedKeys(d.resources[kind]) {
			labels := d.resources[kind][id]
			if labels[brand.WorktreeLabel(namespace)] == ref.WorktreeID && labels[brand.RepositoryLabel(namespace)] == ref.RepositoryID {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// Calls is every docker call so far, rendered "arg arg…".
func (d *Docker) Calls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	lines := make([]string, len(d.calls))
	for i, spec := range d.calls {
		lines[i] = strings.Join(spec.Args, " ")
	}
	return lines
}

var labelName = regexp.MustCompile(`"([^"]+)"`)

// formatToken is one {{ … }} action of an inspect --format template.
var formatToken = regexp.MustCompile(`\{\{[^}]*\}\}`)

// Handle answers one docker call.
func (d *Docker) Handle(_ context.Context, spec run.Spec) (run.Result, error) {
	d.mu.Lock()
	before := d.before
	d.before = nil
	d.mu.Unlock()
	if before != nil {
		before()
	}

	if len(spec.Args) > 0 && spec.Args[0] == "compose" && slices.Contains(spec.Args, "up") {
		d.mu.Lock()
		failing, during, hook := d.upFailing, d.upDuring, d.composeUp
		d.mu.Unlock()
		if hook != nil {
			hook(after(spec.Args, "--project-name"))
		}
		if failing && during != nil {
			during()
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, spec)
	args := spec.Args
	ok := func(stdout string) (run.Result, error) { return run.Finish(spec, run.Result{Stdout: stdout}) }

	// The engine facts mode detection reads: a native, rootful Linux Engine
	// reached over its default socket.
	if len(args) > 0 && args[0] == "info" {
		return ok(`{"Name":"cc-worker","OperatingSystem":"Ubuntu 24.04.4 LTS","OSType":"linux",` +
			`"SecurityOptions":["name=apparmor","name=seccomp,profile=builtin","name=cgroupns"]}`)
	}
	if len(args) > 1 && args[0] == "context" && args[1] == "inspect" {
		return ok("unix:///var/run/docker.sock\n")
	}

	joined := strings.Join(args, " ")
	for i := range d.errors {
		if failure := &d.errors[i]; failure.times != 0 && strings.Contains(joined, failure.contains) {
			if failure.skip > 0 {
				failure.skip--
				continue
			}
			if failure.times > 0 {
				failure.times--
			}
			return run.Finish(spec, run.Result{Stderr: failure.stderr, ExitCode: failure.code})
		}
	}
	if result, handled := d.handleEngine(spec); handled {
		return result, nil
	}

	if len(args) > 0 && args[0] == "compose" {
		project := after(args, "--project-name")
		if result, handled := d.handleCompose(spec, project); handled {
			return result, nil
		}
		switch {
		case slices.Contains(args, "config"):
			services := map[string]any{}
			for _, name := range d.Services {
				entry := map[string]any{"image": "busybox"}
				for key, value := range d.serviceConfig[name] {
					entry[key] = value
				}
				services[name] = entry
			}
			encoded, _ := json.Marshal(map[string]any{"name": project, "services": services})
			return ok(string(encoded))
		case slices.Contains(args, "ps"):
			var lines []string
			for _, id := range sortedKeys(d.resources["container"]) {
				labels := d.resources["container"][id]
				if labels["com.docker.compose.project"] == project {
					encoded, _ := json.Marshal(map[string]any{"Service": labels["com.docker.compose.service"], "State": "running", "Health": ""})
					lines = append(lines, string(encoded))
				}
			}
			return ok(strings.Join(lines, "\n"))
		case slices.Contains(args, "up") && d.upFailing:
			return run.Finish(spec, run.Result{Stderr: d.upStderr, ExitCode: 1})
		case slices.Contains(args, "down"):
			// Containers go; named volumes are left for reap, as Compose leaves them.
			for id, labels := range d.resources["container"] {
				if labels["com.docker.compose.project"] == project {
					delete(d.resources["container"], id)
				}
			}
			return ok("")
		}
		return ok("")
	}

	kind := map[string]string{"ps": "container", "volume": "volume", "network": "network", "image": "image", "inspect": "container", "rm": "container"}[args[0]]
	switch {
	case slices.Contains(args, "--filter"):
		var filters [][2]string
		for i, arg := range args {
			if arg == "--filter" && i+1 < len(args) {
				pair := strings.TrimPrefix(args[i+1], "label=")
				name, value, _ := strings.Cut(pair, "=")
				filters = append(filters, [2]string{name, value})
			}
		}
		var ids []string
		for _, id := range sortedKeys(d.resources[kind]) {
			labels := d.resources[kind][id]
			if kind == "container" && args[0] == "ps" && !slices.Contains(args, "-a") && !slices.Contains(args, "-aq") && d.statusOf(id) != "running" {
				continue
			}
			matches := true
			for _, filter := range filters {
				matches = matches && labels[filter[0]] == filter[1]
			}
			if matches {
				ids = append(ids, id)
			}
		}
		return ok(strings.Join(ids, "\n"))
	case slices.Contains(args, "--format"):
		id := args[len(args)-1]
		labels, present := d.resources[kind][id]
		if !present {
			return run.Finish(spec, run.Result{Stderr: "Error: No such object: " + id, ExitCode: 1})
		}
		d.inspected[id]++
		if change, ok := d.tampers[id]; ok && d.inspected[id] > change.after {
			labels = change.labels
			d.resources[kind][id] = labels
		}
		var values []string
		for _, token := range formatToken.FindAllString(after(args, "--format"), -1) {
			if strings.Contains(token, ".Name") {
				values = append(values, "/"+d.names[id])
				continue
			}
			for _, match := range labelName.FindAllStringSubmatch(token, -1) {
				values = append(values, labels[match[1]])
			}
		}
		return ok(strings.Join(values, "\t"))
	case slices.Contains(args, "rm"):
		if kind == "network" && d.removeNetworkExitCode != 0 {
			return run.Finish(spec, run.Result{Stderr: d.removeNetworkStderr, ExitCode: d.removeNetworkExitCode})
		}
		id := args[len(args)-1]
		if kind == "network" && d.endpoints(id) > 0 {
			return run.Finish(spec, run.Result{Stderr: "Error response from daemon: error while removing network: network " + d.networkNames[id] + " has active endpoints", ExitCode: 1})
		}
		if kind == "container" {
			d.forget(id)
		}
		delete(d.resources[kind], id)
		return ok("")
	}
	return ok("")
}

// after returns the value following flag in args, or "" when flag is absent
// or is the last argument.
func after(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// sortedKeys returns m's keys in a deterministic order, so listings such as
// Owned and the compose ps/filter handlers don't vary between runs.
func sortedKeys(m map[string]map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
