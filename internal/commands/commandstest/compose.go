package commandstest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/worktree"
)

// This file is the Docker fake's model of the parts of a real daemon the
// shared-services tests need: a container's state, the networks it is
// attached to (which `network connect` and `disconnect` change), `docker
// inspect` of containers, networks and volumes as JSON, and — once Realistic
// is on — `compose up` creating what a real one would from the override file
// treerig wrote.

// Realistic makes `compose up` for a worktree create its containers, volumes
// and default network from the override treerig wrote (labels and all), and
// `compose down` remove a network only when nothing is attached to it, as a
// real daemon does. Without it `compose up` is a no-op, as it always was for
// the tests that stage their own resources.
func (d *Docker) Realistic() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.realistic = true
}

// SetSharedService sets the entry `compose config` reports for a service of
// the shared Compose file, such as {"image": "mailpit", "depends_on": …}.
func (d *Docker) SetSharedService(name string, entry map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sharedServices[name] = entry
}

// BeforeComposeUp runs f ahead of every `compose up`, with the Compose
// project it is for, outside the fake's lock: the seam a test uses to let a
// rival act, or to count starts.
func (d *Docker) BeforeComposeUp(f func(project string)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.composeUp = f
}

// FailCall makes the next times docker calls whose arguments, joined with
// spaces, contain contains answer with stderr and code instead of running; a
// negative times makes it permanent.
func (d *Docker) FailCall(contains, stderr string, code, times int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.errors = append(d.errors, injected{contains: contains, stderr: stderr, code: code, times: times})
}

// FailCallAfter is FailCall that lets the first skip matching calls through.
func (d *Docker) FailCallAfter(contains string, skip int, stderr string, code, times int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.errors = append(d.errors, injected{contains: contains, stderr: stderr, code: code, times: times, skip: skip})
}

// ClearFailures removes every FailCall.
func (d *Docker) ClearFailures() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.errors = nil
}

// SetStatus sets a container's Docker state (exited, paused, …).
func (d *Docker) SetStatus(id, status string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status[id] = status
}

// SetHealth sets a container's health check result.
func (d *Docker) SetHealth(id, health string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.health[id] = health
}

// SetName names a container.
func (d *Docker) SetName(id, name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.names[id] = name
}

// AddNetwork adds a network with a name and labels, returning its ID.
func (d *Docker) AddNetwork(name string, labels map[string]string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := d.add("network", labels)
	d.networkNames[id] = name
	return id
}

// Attach attaches a container to a network, as `docker network connect` does.
func (d *Docker) Attach(containerID, networkID string, aliases ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.attach(containerID, networkID, aliases)
}

// Detach detaches a container from a network, as `docker network
// disconnect` does, and reports an error when it was not attached.
func (d *Docker) Detach(containerID, networkID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, on := d.attached[containerID][networkID]; !on {
		return fmt.Errorf("container %s is not attached to network %s", containerID, networkID)
	}
	delete(d.attached[containerID], networkID)
	return nil
}

// Attachments are the IDs of the networks a container is attached to, sorted.
func (d *Docker) Attachments(containerID string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ids []string
	for id := range d.attached[containerID] {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Aliases are the aliases a container has on a network.
func (d *Docker) Aliases(containerID, networkID string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.attached[containerID][networkID])
}

// Exists reports whether a resource is still in the daemon.
func (d *Docker) Exists(kind, id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.resources[kind][id]
	return ok
}

// Find lists the IDs of kind whose labels include every pair in labels.
func (d *Docker) Find(kind string, labels map[string]string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ids []string
	for _, id := range sortedKeys(d.resources[kind]) {
		match := true
		for name, value := range labels {
			match = match && d.resources[kind][id][name] == value
		}
		if match {
			ids = append(ids, id)
		}
	}
	return ids
}

// SetLabels replaces a resource's labels.
func (d *Docker) SetLabels(kind, id string, labels map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.resources[kind][id] = labels
}

// Count is how many docker calls so far have joined arguments containing sub.
func (d *Docker) Count(sub string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, spec := range d.calls {
		if strings.Contains(strings.Join(spec.Args, " "), sub) {
			n++
		}
	}
	return n
}

// statusOf is a container's Docker state; running unless a test said otherwise.
func (d *Docker) statusOf(id string) string {
	if status, ok := d.status[id]; ok {
		return status
	}
	return "running"
}

// attach records a network attachment.
func (d *Docker) attach(containerID, networkID string, aliases []string) {
	if d.attached[containerID] == nil {
		d.attached[containerID] = map[string][]string{}
	}
	d.attached[containerID][networkID] = aliases
}

// endpoints is how many containers are attached to a network.
func (d *Docker) endpoints(networkID string) int {
	n := 0
	for _, networks := range d.attached {
		if _, ok := networks[networkID]; ok {
			n++
		}
	}
	return n
}

// forget drops a removed container's attachments, mounts and state.
func (d *Docker) forget(id string) {
	delete(d.attached, id)
	delete(d.mounts, id)
	delete(d.status, id)
	delete(d.health, id)
	delete(d.bindings, id)
}

// networkByRef is a network's ID given an ID or a name.
func (d *Docker) networkByRef(ref string) (string, bool) {
	if _, ok := d.resources["network"][ref]; ok {
		return ref, true
	}
	for id, name := range d.networkNames {
		if name == ref {
			if _, ok := d.resources["network"][id]; ok {
				return id, true
			}
		}
	}
	return "", false
}

// fail is a docker call that exited 1 with stderr.
func fail(spec run.Spec, stderr string) run.Result {
	result, _ := run.Finish(spec, run.Result{Stderr: stderr, ExitCode: 1})
	return result
}

// done is a docker call that printed stdout and exited 0.
func done(spec run.Spec, stdout string) run.Result {
	result, _ := run.Finish(spec, run.Result{Stdout: stdout})
	return result
}

// handleEngine answers the calls this file models: inspect of containers,
// networks and volumes, network connect and disconnect, and stop. It reports
// false for any other call, which the rest of the fake answers.
func (d *Docker) handleEngine(spec run.Spec) (run.Result, bool) {
	args := spec.Args
	// An inspect with --format is the legacy template path.
	templated := slices.Contains(args, "--format")
	switch {
	case !templated && len(args) >= 3 && args[0] == "container" && args[1] == "inspect":
		id := args[2]
		labels, present := d.resources["container"][id]
		if !present {
			return fail(spec, "Error response from daemon: No such container: "+id), true
		}
		d.inspected[id]++
		if change, ok := d.tampers[id]; ok && d.inspected[id] > change.after {
			labels = change.labels
			d.resources["container"][id] = labels
		}
		return done(spec, d.containerDocument(id, labels)), true
	case !templated && len(args) >= 3 && args[0] == "network" && args[1] == "inspect":
		id, present := d.networkByRef(args[2])
		if !present {
			return fail(spec, "Error response from daemon: network "+args[2]+" not found"), true
		}
		attached := map[string]any{}
		for container, networks := range d.attached {
			if _, ok := networks[id]; ok {
				attached[container] = map[string]any{"Name": d.names[container]}
			}
		}
		return done(spec, mustJSON([]any{map[string]any{
			"Id": id, "Name": d.networkNames[id], "Labels": d.resources["network"][id], "Containers": attached,
		}})), true
	case !templated && len(args) >= 3 && args[0] == "volume" && args[1] == "inspect":
		labels, present := d.resources["volume"][args[2]]
		if !present {
			return fail(spec, "Error response from daemon: get "+args[2]+": no such volume"), true
		}
		return done(spec, mustJSON([]any{map[string]any{"Name": args[2], "Labels": labels}})), true
	case len(args) >= 4 && args[0] == "network" && args[1] == "connect":
		alias := after(args, "--alias")
		container, network := args[len(args)-1], args[len(args)-2]
		if _, present := d.resources["container"][container]; !present {
			// Not a container this model knows: the legacy answer applies.
			return run.Result{}, false
		}
		id, present := d.networkByRef(network)
		if !present {
			return fail(spec, "Error response from daemon: network "+network+" not found"), true
		}
		if _, already := d.attached[container][id]; already {
			return fail(spec, fmt.Sprintf("Error response from daemon: endpoint with name %s already exists in network %s", d.names[container], d.networkNames[id])), true
		}
		var aliases []string
		if alias != "" {
			aliases = []string{alias}
		}
		d.attach(container, id, aliases)
		return done(spec, ""), true
	case len(args) >= 4 && args[0] == "network" && args[1] == "disconnect":
		container, network := args[len(args)-1], args[len(args)-2]
		if _, present := d.resources["container"][container]; !present {
			// Not a container this model knows: the legacy answer applies.
			return run.Result{}, false
		}
		id, present := d.networkByRef(network)
		if !present {
			return fail(spec, "Error response from daemon: network "+network+" not found"), true
		}
		if _, on := d.attached[container][id]; !on {
			return fail(spec, fmt.Sprintf("Error response from daemon: container %s is not connected to network %s", container, d.networkNames[id])), true
		}
		delete(d.attached[container], id)
		return done(spec, ""), true
	case len(args) == 2 && args[0] == "stop":
		if _, present := d.resources["container"][args[1]]; !present {
			return fail(spec, "Error response from daemon: No such container: "+args[1]), true
		}
		d.status[args[1]] = "exited"
		return done(spec, ""), true
	}
	return run.Result{}, false
}

// mustJSON encodes v or panics: the fake only encodes what it built.
func mustJSON(v any) string {
	encoded, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// containerDocument is `docker container inspect` for one container.
func (d *Docker) containerDocument(id string, labels map[string]string) string {
	networks := map[string]any{}
	for networkID, aliases := range d.attached[id] {
		networks[d.networkNames[networkID]] = map[string]any{"NetworkID": networkID, "Aliases": aliases}
	}
	ports := map[string]any{}
	for port, bindings := range d.bindings[id] {
		var list []any
		for _, b := range bindings {
			list = append(list, map[string]any{"HostIp": b.ip, "HostPort": b.port})
		}
		ports[port] = list
	}
	var mounts []any
	for _, name := range d.mounts[id] {
		mounts = append(mounts, map[string]any{"Type": "volume", "Name": name, "Destination": d.mountPaths[name]})
	}
	state := map[string]any{"Status": d.statusOf(id)}
	if health, ok := d.health[id]; ok {
		state["Health"] = map[string]any{"Status": health}
	}
	return mustJSON([]any{map[string]any{
		"Id": id, "Name": "/" + d.names[id], "State": state,
		"Config":          map[string]any{"Labels": labels},
		"Mounts":          mounts,
		"NetworkSettings": map[string]any{"Networks": networks, "Ports": ports},
	}})
}

// overrideFile is the last -f file of a compose call, decoded: the override
// treerig wrote, which is JSON.
func overrideFile(args []string) map[string]any {
	var file string
	for i, arg := range args {
		if arg == "-f" && i+1 < len(args) {
			file = args[i+1]
		}
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var document map[string]any
	if json.Unmarshal(data, &document) != nil {
		return nil
	}
	return document
}

// overrideLabels is the labels the override gives a service, volume or network.
func overrideLabels(document map[string]any, section, name string) map[string]string {
	labels := map[string]string{}
	entries, _ := document[section].(map[string]any)
	entry, _ := entries[name].(map[string]any)
	raw, _ := entry["labels"].(map[string]any)
	for key, value := range raw {
		labels[key] = fmt.Sprint(value)
	}
	return labels
}

// positional are the services named at the end of a `compose up`.
func positional(args []string) []string {
	var services []string
	skip := false
	seenUp := false
	for _, arg := range args {
		switch {
		case !seenUp:
			seenUp = arg == "up"
		case skip:
			skip = false
		case arg == "--wait-timeout":
			skip = true
		case strings.HasPrefix(arg, "-"):
		default:
			services = append(services, arg)
		}
	}
	return services
}

// Placeholders a shared service entry may hold (SetSharedService), which
// `compose config` resolves as Compose would from the --project-directory it
// is given and the PWD of its environment, so a test can read one file from two
// checkouts: ProjectDirectory is the project directory, ProjectGrandparent its
// grandparent (what `../../` names: the directory holding a checkout whose
// shared file is one directory deep), WorkingDirectory the PWD variable.
const (
	ProjectDirectory   = "@PROJECT_DIRECTORY@"
	ProjectGrandparent = "@PROJECT_GRANDPARENT@"
	WorkingDirectory   = "@PWD@"
)

// resolveSharedPlaceholders replaces the placeholders in the JSON of a shared
// `compose config` answer with the values the call's arguments and
// environment give.
func resolveSharedPlaceholders(printed string, spec run.Spec) string {
	directory := after(spec.Args, "--project-directory")
	pwd := ""
	for _, entry := range spec.Env {
		if value, found := strings.CutPrefix(entry, "PWD="); found {
			pwd = value
		}
	}
	return strings.NewReplacer(
		ProjectGrandparent, filepath.Dir(filepath.Dir(directory)), ProjectDirectory, directory, WorkingDirectory, pwd,
	).Replace(printed)
}

// sharedProject reports whether project is a repository's shared project.
func sharedProject(project string) bool { return strings.HasPrefix(project, brand.SharedProjectPrefix) }

// handleCompose answers `compose config` and `compose up` for the shared
// project, and `compose up` and `down` for a worktree when Realistic.
func (d *Docker) handleCompose(spec run.Spec, project string) (run.Result, bool) {
	args := spec.Args
	switch {
	case sharedProject(project) && slices.Contains(args, "config"):
		services := map[string]any{}
		for name, entry := range d.sharedServices {
			services[name] = entry
		}
		volumes := map[string]any{}
		for _, entry := range d.sharedServices {
			for _, mount := range volumeMounts(entry) {
				volumes[mount] = map[string]any{"name": project + "_" + mount}
			}
		}
		printed := mustJSON(map[string]any{
			"name": project, "services": services, "volumes": volumes,
			"networks": map[string]any{"default": map[string]any{"name": project + "_default"}},
		})
		return done(spec, resolveSharedPlaceholders(printed, spec)), true
	case sharedProject(project) && slices.Contains(args, "up"):
		if d.upFailing {
			return run.Result{}, false
		}
		return d.composeUpShared(spec, project), true
	case d.realistic && slices.Contains(args, "up") && !d.upFailing:
		return d.composeUpWorktree(spec, project), true
	case d.realistic && slices.Contains(args, "down"):
		for id, labels := range d.resources["container"] {
			if labels["com.docker.compose.project"] == project {
				d.forget(id)
				delete(d.resources["container"], id)
			}
		}
		stderr := ""
		for id, labels := range d.resources["network"] {
			if labels["com.docker.compose.project"] != project {
				continue
			}
			if d.endpoints(id) > 0 {
				stderr += fmt.Sprintf(" Network %s Resource is still in use\n", d.networkNames[id])
				continue
			}
			delete(d.resources["network"], id)
		}
		result, _ := run.Finish(spec, run.Result{Stderr: stderr})
		return result, true
	}
	return run.Result{}, false
}

// volumeMounts are the named volumes a service entry mounts.
func volumeMounts(entry map[string]any) []string {
	var names []string
	mounts, _ := entry["volumes"].([]any)
	for _, mount := range mounts {
		fields, _ := mount.(map[string]any)
		if fields["type"] == "volume" {
			if source, ok := fields["source"].(string); ok {
				names = append(names, source)
			}
		}
	}
	return names
}

// closureOf is service and everything it names in depends_on, in the shared file.
func (d *Docker) closureOf(service string) []string {
	seen := map[string]bool{}
	var order []string
	var visit func(string)
	visit = func(name string) {
		if seen[name] {
			return
		}
		entry, ok := d.sharedServices[name]
		if !ok {
			return
		}
		seen[name] = true
		dependencies, _ := entry["depends_on"].(map[string]any)
		var names []string
		for dependency := range dependencies {
			names = append(names, dependency)
		}
		sort.Strings(names)
		for _, dependency := range names {
			visit(dependency)
		}
		order = append(order, name)
	}
	visit(service)
	return order
}

// composeUpShared is `compose up` for the shared project: every requested
// service, and with no --no-deps what it depends on, is created from the
// override's labels and ports unless a container of it exists. An existing one
// is started if it was stopped and otherwise left alone; with --force-recreate
// it is replaced by a new container that keeps the old one's volumes, named
// and anonymous alike, as Compose does.
func (d *Docker) composeUpShared(spec run.Spec, project string) run.Result {
	override := overrideFile(spec.Args)
	requested := positional(spec.Args)
	force := slices.Contains(spec.Args, "--force-recreate")
	noDeps := slices.Contains(spec.Args, "--no-deps")
	if len(requested) == 0 {
		for name := range d.sharedServices {
			requested = append(requested, name)
		}
		sort.Strings(requested)
	}
	var wanted []string
	for _, name := range requested {
		if _, ok := d.sharedServices[name]; !ok {
			return fail(spec, "no such service: "+name)
		}
		members := []string{name}
		if !noDeps {
			members = d.closureOf(name)
		}
		for _, member := range members {
			if !slices.Contains(wanted, member) {
				wanted = append(wanted, member)
			}
		}
	}
	network := d.ensureProjectNetwork(project, "default", overrideLabels(override, "networks", "default"))
	for _, service := range wanted {
		var kept []string
		if existing := d.containerOf(project, service); existing != "" {
			if !force {
				if status := d.statusOf(existing); status != "running" {
					d.status[existing] = "running"
				}
				continue
			}
			kept = d.mounts[existing]
			delete(d.names, existing)
			d.forget(existing)
			delete(d.resources["container"], existing)
		}
		name := project + "-" + service + "-1"
		for id, taken := range d.names {
			if taken == name {
				if _, alive := d.resources["container"][id]; alive {
					return fail(spec, fmt.Sprintf("Error response from daemon: Conflict. The container name \"/%s\" is already in use by container \"%s\". You have to remove (or rename) that container to be able to reuse that name.", name, id))
				}
			}
		}
		labels := overrideLabels(override, "services", service)
		labels["com.docker.compose.project"] = project
		labels["com.docker.compose.service"] = service
		id := d.add("container", labels)
		d.names[id] = name
		d.attach(id, network, []string{service})
		entry, _ := override["services"].(map[string]any)[service].(map[string]any)
		ports, _ := entry["ports"].([]any)
		for _, raw := range ports {
			port, _ := raw.(map[string]any)
			key := fmt.Sprintf("%v/tcp", port["target"])
			if d.bindings[id] == nil {
				d.bindings[id] = map[string][]binding{}
			}
			d.bindings[id][key] = append(d.bindings[id][key], binding{ip: fmt.Sprint(port["host_ip"]), port: fmt.Sprint(port["published"])})
		}
		if kept != nil {
			d.mounts[id] = kept
			continue
		}
		mounts, _ := d.sharedServices[service]["volumes"].([]any)
		for _, raw := range mounts {
			mount, _ := raw.(map[string]any)
			source, _ := mount["source"].(string)
			target, _ := mount["target"].(string)
			if mount["type"] != "volume" {
				continue
			}
			if source == "" {
				d.next++
				anonymous := fmt.Sprintf("anon%02d", d.next)
				d.resources["volume"][anonymous] = map[string]string{"com.docker.volume.anonymous": ""}
				d.mountPaths[anonymous] = target
				d.mounts[id] = append(d.mounts[id], anonymous)
				continue
			}
			full := project + "_" + source
			if _, ok := d.resources["volume"][full]; !ok {
				volumeLabels := overrideLabels(override, "volumes", source)
				volumeLabels["com.docker.compose.project"] = project
				d.resources["volume"][full] = volumeLabels
			}
			d.mountPaths[full] = target
			d.mounts[id] = append(d.mounts[id], full)
		}
	}
	return done(spec, "")
}

// containerOf is the ID of the container of a Compose project's service, or "".
func (d *Docker) containerOf(project, service string) string {
	for _, id := range sortedKeys(d.resources["container"]) {
		labels := d.resources["container"][id]
		if labels["com.docker.compose.project"] == project && labels["com.docker.compose.service"] == service {
			return id
		}
	}
	return ""
}

// ensureProjectNetwork is the ID of a Compose project's network key,
// created with labels when it does not exist.
func (d *Docker) ensureProjectNetwork(project, key string, labels map[string]string) string {
	name := project + "_" + key
	for id, existing := range d.networkNames {
		if existing == name {
			if _, alive := d.resources["network"][id]; alive {
				return id
			}
		}
	}
	labels["com.docker.compose.project"] = project
	id := d.add("network", labels)
	d.networkNames[id] = name
	return id
}

// composeUpWorktree is `compose up` for a worktree once Realistic: one
// container per service the Compose file declares, labelled as the override
// says, on the project's default network.
func (d *Docker) composeUpWorktree(spec run.Spec, project string) run.Result {
	override := overrideFile(spec.Args)
	network := d.ensureProjectNetwork(project, "default", overrideLabels(override, "networks", "default"))
	requested := positional(spec.Args)
	for _, service := range d.Services {
		if len(requested) > 0 && !slices.Contains(requested, service) {
			continue
		}
		if d.containerOf(project, service) != "" {
			continue
		}
		labels := overrideLabels(override, "services", service)
		labels["com.docker.compose.project"] = project
		labels["com.docker.compose.service"] = service
		id := d.add("container", labels)
		d.names[id] = project + "-" + service + "-1"
		d.attach(id, network, []string{service})
	}
	return done(spec, "")
}

// WorktreeDefaultNetwork is the ID of ref's default network in a Realistic
// daemon, or "" before its `up`.
func (d *Docker) WorktreeDefaultNetwork(ref worktree.Ref) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	name := layout.ComposeProjectName(ref.RepositoryID, ref.WorktreeID) + "_default"
	for id, existing := range d.networkNames {
		if existing == name {
			if _, alive := d.resources["network"][id]; alive {
				return id
			}
		}
	}
	return ""
}

// ContainerNamed is the ID of the live container with this name, or "".
func (d *Docker) ContainerNamed(name string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, taken := range d.names {
		if taken == name {
			if _, alive := d.resources["container"][id]; alive {
				return id
			}
		}
	}
	return ""
}

// Published are the host ports a container publishes for a container port, as "ip:port".
func (d *Docker) Published(id string, containerPort int) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, b := range d.bindings[id][fmt.Sprintf("%d/tcp", containerPort)] {
		out = append(out, b.ip+":"+b.port)
	}
	return out
}
