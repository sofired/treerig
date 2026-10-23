package commands

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/sofired/treerig/internal/backend"
	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/docker"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/ports"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/router"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/shared"
	"github.com/sofired/treerig/internal/text"
	"github.com/sofired/treerig/internal/worktree"
)

// The Shared services section of `doctor` (docs/design/routing.md, "Doctor"):
// read-only, like the rest of it. It appears only when treerig.toml declares
// shared services or Docker still holds shared resources of this repository.

// sharedSection is the Shared services section's checks; none means the
// section is left out. It ignores a doctorOptions that carries a registry
// override, which is a test double for the checkout section and has no Git
// behind it to name a repository.
func sharedSection(ctx context.Context, opts doctorOptions) ([]Check, error) {
	if opts.Checkout.kind != "loaded" || opts.RegistryFor != nil {
		return nil, nil
	}
	loaded := opts.Checkout.loaded
	declared := loaded.Config.SharedServices()
	if len(declared) == 0 && !opts.DockerAnswered {
		return nil, nil
	}
	return guarded("shared.scan", "shared services", func() ([]Check, error) {
		identities := registry.NewIdentityService(loaded.Root, opts.Runner)
		repositoryID, present, err := identities.RepositoryID(ctx, loaded.Root)
		if err != nil {
			if len(declared) == 0 {
				return nil, nil
			}
			return nil, err
		}
		if !present {
			if len(declared) == 0 {
				return nil, nil
			}
			return []Check{{Status: StatusSkip, ID: "shared.services", Subject: "shared services",
				Detail: fmt.Sprintf("this repository has never been brought up; '%s up' starts them", opts.CommandName)}}, nil
		}
		return sharedChecks(ctx, opts, loaded, repositoryID)
	})
}

// sharedDoctor is what the shared checks share.
type sharedDoctor struct {
	opts         doctorOptions
	loaded       *config.Loaded
	repositoryID string
	namespace    string
	stack        *shared.Stack
	store        *registry.Store
	records      map[string]registry.SharedRecord
}

// sharedChecks is every line of the Shared services section for repositoryID.
func sharedChecks(ctx context.Context, opts doctorOptions, loaded *config.Loaded, repositoryID string) ([]Check, error) {
	d := &sharedDoctor{opts: opts, loaded: loaded, repositoryID: repositoryID, namespace: loaded.Config.LabelNamespace()}
	roots := []string{loaded.Root}
	d.stack = shared.New(shared.Options{
		RepositoryID: repositoryID, LabelNamespace: d.namespace, ControlHome: opts.ControlHome,
		ComposeFile: sharedFileIfAny(loaded), ProjectName: loaded.Config.Project.Name, WorktreeRoots: roots, Declared: sharedNames(loaded.Config.SharedServices()),
		Environ: opts.Environ, Runner: opts.Runner, Logger: opts.Logger,
	})
	d.store = registry.NewStore(opts.ControlHome, repositoryID)
	records, problems, err := d.store.SharedRecords()
	if err != nil {
		return nil, err
	}
	d.records = make(map[string]registry.SharedRecord, len(records))
	for _, record := range records {
		d.records[record.Name] = record
	}

	// Nothing declared: the section exists only if the repository still has
	// shared containers or records, and a listing that fails is not a reason
	// to report anything about a feature this repository does not use.
	if len(loaded.Config.SharedServices()) == 0 && len(records) == 0 && len(problems) == 0 {
		containers, _, err := d.stack.Verified(ctx)
		if err != nil || len(containers) == 0 {
			return nil, nil
		}
	}

	var checks []Check
	for _, problem := range problems {
		checks = append(checks, Check{Status: StatusFail, ID: "shared.unreadable-record", Instance: recordInstance(problem.File),
			Subject: "shared-service record " + recordInstance(problem.File), Detail: unreadableRecordDetail(problem)})
	}
	if !opts.DockerAnswered {
		return append(checks, Check{Status: StatusSkip, ID: "shared.services", Subject: "shared services", Detail: "Docker is not answering, so their state is unknown"}), nil
	}

	declaredNames := sharedNames(loaded.Config.SharedServices())
	instances, unproven, err := d.stack.Statuses(ctx, declaredNames)
	if err != nil {
		return append(checks, failureCheck("shared.services", "shared services", err)), nil
	}
	for _, problem := range unproven {
		checks = append(checks, Check{Status: StatusWarn, ID: "shared.orphan", Instance: problem.ID, Subject: "shared container " + problem.ID,
			Detail: "carries the shared labels and cannot be proven the stack's, so treerig will not touch it: " + text.OneLine(problem.Error(), 300)})
	}
	definition, definitionErr := d.readDefinition(ctx)
	if definitionErr != nil {
		checks = append(checks, Check{Status: StatusFail, ID: "shared.definition", Subject: "shared file", Detail: text.OneLine(definitionErr.Error(), 400)})
	}

	for _, service := range loaded.Config.SharedServices() {
		instance := instances[service.Name]
		checks = append(checks, d.serviceCheck(service, instance, definition, definitionErr == nil))
		checks = append(checks, d.portChecks(ctx, service, instance)...)
		checks = append(checks, d.routeCheck(service, instance)...)
	}

	registration, regErr := d.registration(ctx)
	if regErr == nil && registration != nil {
		connection, err := d.connectionChecks(ctx, registration, instances)
		if err != nil {
			return nil, err
		}
		checks = append(checks, connection...)
	}

	attachments, err := d.attachmentChecks(ctx)
	if err != nil {
		return nil, err
	}
	checks = append(checks, attachments...)

	orphans, err := d.orphanChecks(ctx, declaredNames)
	if err != nil {
		return nil, err
	}
	checks = append(checks, orphans...)
	return checks, nil
}

// sharedFileIfAny is the shared Compose file, or "" when none is configured.
func sharedFileIfAny(loaded *config.Loaded) string {
	if loaded.Config.Shared == nil || loaded.Config.Shared.File == "" {
		return ""
	}
	return sharedFilePath(loaded)
}

// readDefinition reads the shared file, for hashing and to check it can be
// shared at all.
func (d *sharedDoctor) readDefinition(ctx context.Context) (shared.Definition, error) {
	if d.loaded.Config.Shared == nil || d.loaded.Config.Shared.File == "" {
		return shared.Definition{}, nil
	}
	return d.stack.ReadDefinition(ctx)
}

// registration is this checkout's registration, or nil.
func (d *sharedDoctor) registration(ctx context.Context) (*registry.WorktreeRegistration, error) {
	reg := registry.New(registry.Options{
		ControlHome: d.opts.ControlHome, Root: d.loaded.Root, Domain: d.loaded.Config.Project.Domain,
		LabelNamespace: d.namespace, Runner: d.opts.Runner, Logger: d.opts.Logger,
		CommandName: d.opts.CommandName, Invocation: d.opts.Invocation, UserHome: d.opts.UserHome,
	})
	return reg.Current(ctx)
}

// hostOf names the worktree that started an instance for a line.
func (d *sharedDoctor) hostOf(worktreeID string) string {
	record, err := d.store.Find(worktreeID)
	if err != nil || record == nil {
		return "a removed worktree"
	}
	return layout.WorktreeHost(record.Slug, record.Domain)
}

// serviceCheck is one declared service's `shared.service` line: its state,
// and for a running one whether its definition is the one this worktree's
// file hashes to. A stopped container is a warning: with a restart policy of
// unless-stopped in the shared file Docker brings it back after a reboot, and
// without one `shared up` does.
func (d *sharedDoctor) serviceCheck(service config.SharedService, instance shared.Instance, definition shared.Definition, haveDefinition bool) Check {
	name := service.Name
	check := Check{ID: "shared.service", Instance: name, Subject: "shared service " + name}
	switch instance.State {
	case worktree.NotCreated:
		check.Status = StatusSkip
		check.Detail = fmt.Sprintf("not running; '%s up' starts it when a worktree uses it, or '%s shared up' does", d.opts.CommandName, d.opts.CommandName)
		return check
	case worktree.Stopped, worktree.Exited:
		check.Status = StatusWarn
		stopped := "stopped"
		if instance.State == worktree.Exited {
			stopped = fmt.Sprintf("exited (code %d)", instance.ExitCode)
		}
		check.Detail = fmt.Sprintf("its container is %s; '%s shared up' starts it again (restart: unless-stopped in the shared file brings it back after a reboot)", stopped, d.opts.CommandName)
		return check
	case worktree.Unknown:
		check.Status = StatusWarn
		check.Detail = text.OneLine(instance.Detail, 300)
		return check
	}
	check.Status = StatusOK
	check.Detail = string(instance.State)
	switch instance.State {
	case worktree.Unhealthy:
		check.Status = StatusWarn
		check.Detail = "running, but its health check fails"
	case worktree.Starting:
		check.Detail = "starting (its health check has not passed yet)"
	case worktree.Restarting:
		check.Status = StatusWarn
		check.Detail = "restarting"
	}
	if haveDefinition && definition.Has(name) {
		if expected, err := definition.Hash(name); err == nil && expected != instance.Definition {
			check.Status = StatusWarn
			check.Detail = fmt.Sprintf(
				"%s with a different definition than this worktree's (started by %s). '%s shared up --recreate %s' replaces it and keeps its data",
				check.Detail, d.hostOf(instance.DefinedBy), d.opts.CommandName, name)
		}
	}
	return check
}

// portChecks are one service's `shared.port` lines: a declared container
// port with no pinned host port, a pinned port the running container does not
// publish, and a pinned port something else holds while the service is down.
func (d *sharedDoctor) portChecks(ctx context.Context, service config.SharedService, instance shared.Instance) []Check {
	name := service.Name
	record, hasRecord := d.records[name]
	if len(service.ContainerPorts()) == 0 {
		return nil
	}
	if !hasRecord {
		if instance.Live() {
			return []Check{{Status: StatusWarn, ID: "shared.record", Instance: name, Subject: "shared service " + name + " record",
				Detail: fmt.Sprintf("running, but this control home has no record of its pinned ports. '%s shared up --recreate %s' pins new ones", d.opts.CommandName, name)}}
		}
		return nil
	}
	probe := d.opts.PortProbe
	if probe == nil {
		probe = ports.LoopbackProbe{}
	}
	var checks []Check
	for _, container := range service.ContainerPorts() {
		subject := fmt.Sprintf("shared service %s port %d", name, container)
		instanceKey := fmt.Sprintf("%s:%d", name, container)
		host := record.HostPort(container)
		switch {
		case host == 0:
			checks = append(checks, Check{Status: StatusWarn, ID: "shared.port", Instance: instanceKey, Subject: subject,
				Detail: fmt.Sprintf("declared, but no host port is pinned for it. '%s shared down %s' and '%s shared up' pin one", d.opts.CommandName, name, d.opts.CommandName)})
		case instance.Live():
			published := instance.Container.Ports[strconv.Itoa(container)+"/tcp"]
			ok := false
			for _, binding := range published {
				ok = ok || (binding.HostIP == shared.LoopbackHost && binding.HostPort == strconv.Itoa(host))
			}
			if ok {
				checks = append(checks, Check{Status: StatusOK, ID: "shared.port", Instance: instanceKey, Subject: subject, Detail: loopbackAddr(host) + " is published"})
			} else {
				checks = append(checks, Check{Status: StatusWarn, ID: "shared.port", Instance: instanceKey, Subject: subject,
					Detail: fmt.Sprintf("%s is pinned, but the running container does not publish it. '%s shared up --recreate %s' recreates it on the pinned port",
						loopbackAddr(host), d.opts.CommandName, name)})
			}
		case probe.Free(ctx, host):
			checks = append(checks, Check{Status: StatusOK, ID: "shared.port", Instance: instanceKey, Subject: subject, Detail: loopbackAddr(host) + " is free"})
		default:
			checks = append(checks, Check{Status: StatusWarn, ID: "shared.port", Instance: instanceKey, Subject: subject,
				Detail: fmt.Sprintf("%s is in use by another process, so %s cannot start on it. Stop that process, or run '%s shared up --reallocate-port %s:%d'",
					loopbackAddr(host), name, d.opts.CommandName, name, container)})
		}
	}
	return checks
}

// routeCheck is a routed service's `shared.route` line. A running instance
// whose record routes a web UI should have its route file: ok when it does, a
// warning when it does not. A service that is not running should have none
// (`shared down` removes it): a file left by a container stopped or removed
// any other way is a warning, since the hostname answers 502, and no file
// gets no line (an error looking for it is a failed check, not an absent file). A file that exists but disagrees with the record is the Router
// section's `router.orphaned-routes` to report, so it is not repeated here.
func (d *sharedDoctor) routeCheck(service config.SharedService, instance shared.Instance) []Check {
	record, ok := d.records[service.Name]
	if !ok {
		return nil
	}
	if !instance.Live() {
		// A container stopped or removed any other way than `shared down`
		// leaves its file, and the hostname answers 502.
		if _, err := os.Lstat(router.SharedFragmentPath(d.opts.RouterState, d.repositoryID, service.Name)); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			// Absent is the only good answer; anything else (permission, I/O)
			// means the file could not be looked for.
			failed := failureCheck("shared.route", "shared service "+service.Name+" route", err)
			failed.Instance = service.Name
			return []Check{failed}
		}
		return []Check{{Status: StatusWarn, ID: "shared.route", Instance: service.Name, Subject: "shared service " + service.Name + " route",
			Detail: fmt.Sprintf("the route file is present but the service is not running, so %s answers 502. '%s shared up' starts it; '%s shared down' removes the route",
				record.Host(), d.opts.CommandName, d.opts.CommandName)}}
	}
	if record.Host() == "" {
		return nil
	}
	subject := "shared service " + service.Name + " route"
	url := "http://" + record.Host()
	if d.opts.RouterErr == nil && d.opts.RouterPort.Port != 0 {
		url = worktree.NewSharedURL(record.Name, record.Domain, d.opts.RouterPort.Port).URL
	}
	path := router.SharedFragmentPath(d.opts.RouterState, d.repositoryID, service.Name)
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []Check{{Status: StatusWarn, ID: "shared.route", Instance: service.Name, Subject: subject,
				Detail: fmt.Sprintf("%s has no route file, so the proxy does not serve it. '%s shared up' writes it",
					url, d.opts.CommandName)}}
		}
		failed := failureCheck("shared.route", subject, err)
		failed.Instance = service.Name
		return []Check{failed}
	}
	return []Check{{Status: StatusOK, ID: "shared.route", Instance: service.Name, Subject: subject,
		Detail: fmt.Sprintf("%s routes to %s", url, loopbackAddr(record.RoutedHostPort()))}}
}

// connectionChecks are the `shared.connection` lines: for each running shared
// service this worktree uses, whether it is attached to the networks of this
// worktree's running consumers. A consumer that is not connected is "up but
// not connected", repaired by `treerig up` or `treerig shared up`.
func (d *sharedDoctor) connectionChecks(ctx context.Context, registration *registry.WorktreeRegistration, instances map[string]shared.Instance) ([]Check, error) {
	ref := worktree.Ref{RepositoryID: registration.RepositoryID, WorktreeID: registration.WorktreeID, Slug: registration.Slug}
	runtime := backend.NewCompose(backend.Options{
		ComposeFile: composeFilePath(d.loaded), ProjectRoot: d.loaded.Root, ConfigFile: d.loaded.File,
		ProjectName: d.loaded.Config.Project.Name, Domain: d.loaded.Config.Project.Domain,
		ControlHome: d.opts.ControlHome, LabelNamespace: d.namespace,
		Runner: d.opts.Runner, Logger: d.opts.Logger, Environ: d.opts.Environ,
	})
	var checks []Check
	for _, service := range sharedNeeded(d.loaded.Config) {
		instance := instances[service.Name]
		if !instance.Live() {
			continue
		}
		consumers, err := runtime.SharedConsumers(ctx, ref, service.Name)
		if err != nil {
			if doctorCancelled(ctx, err) {
				return nil, err
			}
			checks = append(checks, failureCheck("shared.connection", "shared service "+service.Name+" connection", err))
			continue
		}
		var missing []string
		for _, consumer := range consumers {
			for networkName, networkID := range consumer.Networks {
				proof, err := runtime.ProveNetwork(ctx, ref, networkID)
				if err != nil {
					if doctorCancelled(ctx, err) {
						return nil, err
					}
					return append(checks, failureCheck("shared.connection", "shared service "+service.Name+" connection", err)), nil
				}
				if proof.Owned && !instance.Attached(networkID) {
					missing = append(missing, fmt.Sprintf("%s (network %s)", consumer.Service, networkName))
				}
			}
		}
		subject := "shared service " + service.Name + " connection"
		switch {
		case len(consumers) == 0:
			checks = append(checks, Check{Status: StatusSkip, ID: "shared.connection", Instance: service.Name, Subject: subject,
				Detail: "none of this worktree's services that use it is running"})
		case len(missing) > 0:
			slices.Sort(missing)
			checks = append(checks, Check{Status: StatusWarn, ID: "shared.connection", Instance: service.Name, Subject: subject,
				Detail: fmt.Sprintf("up but not connected: %s runs on a network %s is not attached to. '%s up' or '%s shared up' repairs it",
					strings.Join(missing, ", "), service.Name, d.opts.CommandName, d.opts.CommandName)})
		default:
			checks = append(checks, Check{Status: StatusOK, ID: "shared.connection", Instance: service.Name, Subject: subject, Detail: "connected"})
		}
	}
	return checks, nil
}

// doctorCancelled reports whether err is a cancellation, which ends the
// command rather than becoming a line.
func doctorCancelled(ctx context.Context, err error) bool {
	var cancelled *clierr.Cancelled
	return errors.As(err, &cancelled) || ctx.Err() != nil
}

// attachmentChecks are the `shared.attachment` lines: a shared container
// attached to a network that carries no worktree's labels, or that belongs to
// a worktree that is stopped or gone. They are reported and never touched
// here; `down` and `worktree remove` disconnect the second kind.
func (d *sharedDoctor) attachmentChecks(ctx context.Context) ([]Check, error) {
	containers, _, err := d.stack.Verified(ctx)
	if err != nil {
		return []Check{failureCheck("shared.attachment", "shared attachments", err)}, nil
	}
	var checks []Check
	for _, container := range containers {
		names := make([]string, 0, len(container.Networks))
		for name := range container.Networks {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			network, found, err := docker.InspectNetwork(ctx, d.opts.Runner, container.Networks[name].NetworkID, d.opts.dockerTimeout())
			if err != nil {
				if doctorCancelled(ctx, err) {
					return nil, err
				}
				return append(checks, failureCheck("shared.attachment", "shared attachments", err)), nil
			}
			if !found || network.Labels[brand.SharedLabel(d.namespace)] == "true" {
				continue
			}
			service := container.Labels[backend.ComposeServiceLabel]
			subject := fmt.Sprintf("shared service %s on network %s", service, name)
			owner := network.Labels[brand.WorktreeLabel(d.namespace)]
			switch {
			case owner == "":
				checks = append(checks, Check{Status: StatusWarn, ID: "shared.attachment", Instance: service + ":" + name, Subject: subject,
					Detail: "attached to a network that carries no worktree's labels. It is reported and never touched: disconnect it with 'docker network disconnect' if it is not yours"})
			case network.Labels[brand.RepositoryLabel(d.namespace)] != d.repositoryID:
				checks = append(checks, Check{Status: StatusWarn, ID: "shared.attachment", Instance: service + ":" + name, Subject: subject,
					Detail: "attached to a network of another repository's worktree. It is reported and never touched"})
			default:
				running, err := d.worktreeRunning(ctx, owner)
				if err != nil {
					if doctorCancelled(ctx, err) {
						return nil, err
					}
					return append(checks, failureCheck("shared.attachment", "shared attachments", err)), nil
				}
				if !running {
					checks = append(checks, Check{Status: StatusWarn, ID: "shared.attachment", Instance: service + ":" + name, Subject: subject,
						Detail: fmt.Sprintf("attached to the network of %s, which is stopped or gone. '%s down' or '%s worktree remove' there disconnects it",
							d.hostOf(owner), d.opts.CommandName, d.opts.CommandName)})
				}
			}
		}
	}
	return checks, nil
}

// worktreeRunning reports whether any container carrying the worktree's
// label is running.
func (d *sharedDoctor) worktreeRunning(ctx context.Context, worktreeID string) (bool, error) {
	result, err := docker.Run(ctx, d.opts.Runner, []string{"ps", "-q",
		"--filter", "label=" + brand.ManagedLabel(d.namespace) + "=true",
		"--filter", "label=" + brand.RepositoryLabel(d.namespace) + "=" + d.repositoryID,
		"--filter", "label=" + brand.WorktreeLabel(d.namespace) + "=" + worktreeID,
	}, run.Spec{Timeout: d.opts.dockerTimeout()})
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(result.Stdout) != "", nil
}

// orphanChecks are the lines about the stack's containers no declared
// service accounts for, and about a running instance nothing uses: shared
// resources of this repository with no declaration, and a running instance
// with no registered worktree. Both are information, not an error.
func (d *sharedDoctor) orphanChecks(ctx context.Context, declared []string) ([]Check, error) {
	containers, _, err := d.stack.Verified(ctx)
	if err != nil {
		return []Check{failureCheck("shared.orphan", "shared containers", err)}, nil
	}
	var checks []Check
	var undeclared []string
	for _, container := range containers {
		service := container.Labels[backend.ComposeServiceLabel]
		if !slices.Contains(declared, service) && !slices.Contains(undeclared, service) {
			undeclared = append(undeclared, service)
		}
	}
	slices.Sort(undeclared)
	for _, service := range undeclared {
		checks = append(checks, Check{Status: StatusOK, ID: "shared.orphan", Instance: service, Subject: "shared container " + service,
			Detail: fmt.Sprintf("not a service %s declares: a dependency of one, or left from an earlier configuration. '%s shared down' removes it", brand.ConfigFile, d.opts.CommandName)})
	}
	if len(containers) > 0 {
		registered, err := d.store.List()
		if err == nil && len(registered) == 0 {
			checks = append(checks, Check{Status: StatusOK, ID: "shared.unused", Subject: "shared services",
				Detail: fmt.Sprintf("running with no registered worktree of this repository. '%s shared down' stops them", d.opts.CommandName)})
		}
	}
	return checks, nil
}

// sharedLockChecks are the Locks section's lines for repositories' shared
// locks, found by scanning <control home>/locks/shared so a repository that
// is not registered any more is still seen.
func sharedLockChecks(controlHome string, byRepository map[string]string, commandName string) ([]Check, error) {
	dir := layout.ControlLayout(controlHome).LocksDir + "/shared"
	entries, err := readDirIfExists(dir)
	if err != nil {
		return nil, err
	}
	var checks []Check
	for _, entry := range entries {
		id := entry.Name()
		if entry.IsDir() || !registry.IsUUID(id) {
			continue
		}
		scope := lock.SharedScope(controlHome, id)
		subject := "shared services of repository " + id
		handRemoval := fmt.Sprintf("run '%s worktree unlock --shared --holder-unverifiable' once you have made sure its holder, and everything it started, is gone", commandName)
		if name, ok := byRepository[id]; ok {
			subject = "shared services of the repository of " + name
		} else {
			// Nothing of the repository is registered, so there is no worktree
			// to name it by: the command runs from a checkout of it.
			handRemoval = fmt.Sprintf("run '%s worktree unlock --shared --holder-unverifiable' from a checkout of that repository (the lock is %s) once you have made sure its holder, and everything it started, is gone",
				commandName, scope.Target)
		}
		if check, ok := lockCheck("locks.shared", subject, scope, handRemoval); ok {
			check.Instance = id
			checks = append(checks, check)
		}
	}
	return checks, nil
}

// repositoryNames maps each repository with a registered worktree to a name
// for it: the hostname of its first registered worktree.
func repositoryNames(records []registry.RegisteredWorktree) map[string]string {
	names := map[string]string{}
	for _, record := range records {
		if _, ok := names[record.RepositoryID]; !ok {
			names[record.RepositoryID] = layout.WorktreeHost(record.Slug, record.Domain)
		}
	}
	return names
}

// unreadableRecordDetail is a shared-service record problem's line. A record a
// newer treerig wrote is intact, only unreadable here, so its reason already says
// what to do and moving it aside would be wrong; any other is a file nobody
// vouches for, which may hold a port.
func unreadableRecordDetail(problem registry.RegistryProblem) string {
	if problem.Newer {
		return problem.Reason
	}
	return problem.Reason + ". Move the file aside once you know what it is: a record that cannot be read may hold a port."
}
