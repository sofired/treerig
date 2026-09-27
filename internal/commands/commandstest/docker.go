package commandstest

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/run"
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
	tampers   map[string]tamper
	next      int
	calls     []run.Spec
	// Services is what `compose config` reports the Compose file declares.
	Services []string
	// before runs once, ahead of the first docker call, outside the lock: the
	// seam a test uses to let a rival act after the target was resolved.
	before func()
	// disconnectExitCode/disconnectStderr make every `network disconnect`
	// call answer this way instead of succeeding — FailNetworkDisconnect's
	// own state, for a test pinning unrouteWorktree's warn-and-continue
	// path (internal/commands/router.go).
	disconnectExitCode int
	disconnectStderr   string
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
}

// FailNetworkDisconnect makes every subsequent `docker network disconnect`
// call answer with exitCode and stderr instead of succeeding.
func (d *Docker) FailNetworkDisconnect(exitCode int, stderr string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disconnectExitCode, d.disconnectStderr = exitCode, stderr
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
		tampers:   map[string]tamper{},
		Services:  services,
	}
}

// Ownership is the three ownership labels plus Compose's own, as `up` stamps them.
func Ownership(ref worktree.Ref, namespace string) map[string]string {
	return map[string]string{
		brand.ManagedLabel(namespace):    "true",
		brand.RepositoryLabel(namespace): ref.RepositoryID,
		brand.WorktreeLabel(namespace):   ref.WorktreeID,
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

// Handle answers one docker call.
func (d *Docker) Handle(_ context.Context, spec run.Spec) (run.Result, error) {
	d.mu.Lock()
	before := d.before
	d.before = nil
	d.mu.Unlock()
	if before != nil {
		before()
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, spec)
	args := spec.Args
	ok := func(stdout string) (run.Result, error) { return run.Finish(spec, run.Result{Stdout: stdout}) }

	if len(args) > 0 && args[0] == "compose" {
		project := after(args, "--project-name")
		switch {
		case slices.Contains(args, "config"):
			services := map[string]any{}
			for _, name := range d.Services {
				services[name] = map[string]any{"image": "busybox"}
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

	if len(args) > 1 && args[0] == "network" && args[1] == "disconnect" && d.disconnectExitCode != 0 {
		return run.Finish(spec, run.Result{Stderr: d.disconnectStderr, ExitCode: d.disconnectExitCode})
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
		for _, match := range labelName.FindAllStringSubmatch(after(args, "--format"), -1) {
			values = append(values, labels[match[1]])
		}
		return ok(strings.Join(values, "\t"))
	case slices.Contains(args, "rm"):
		if kind == "network" && d.removeNetworkExitCode != 0 {
			return run.Finish(spec, run.Result{Stderr: d.removeNetworkStderr, ExitCode: d.removeNetworkExitCode})
		}
		id := args[len(args)-1]
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
