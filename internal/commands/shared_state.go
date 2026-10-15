package commands

// Shared services (docs/design/routing.md, decisions 15-22): the pieces every
// shared verb and `up` use — the stack for a repository, what a worktree
// needs of it, the pinned addresses hooks receive, and the lock around it.

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/sofired/treerig/internal/backend"
	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/lifecycle"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/shared"
	"github.com/sofired/treerig/internal/worktree"
)

// sharedFilePath is the shared Compose file, resolved against the config's
// directory like the worktree's own.
func sharedFilePath(loaded *config.Loaded) string {
	file := loaded.Config.Shared.File
	if filepath.IsAbs(file) {
		return filepath.Clean(file)
	}
	return filepath.Join(loaded.Root, file)
}

// sharedStack is repositoryID's shared stack under namespace. It can read the
// shared file only when cctx has a config that declares one, which is all
// that starting a service needs; removal, inspection and connecting work
// from labels alone, and pass no roots.
func (c *CommandContext) sharedStack(repositoryID, namespace string, roots []string) *shared.Stack {
	options := shared.Options{
		RepositoryID:   repositoryID,
		LabelNamespace: namespace,
		ControlHome:    c.ControlHome,
		WorktreeRoots:  roots,
		Environ:        c.Environ,
		Runner:         c.Runner,
		Logger:         c.Logger,
	}
	if c.Loaded != nil {
		options.ProjectName = c.Loaded.Config.Project.Name
		options.Declared = sharedNames(c.Loaded.Config.SharedServices())
		if c.Loaded.Config.Shared != nil && c.Loaded.Config.Shared.File != "" {
			options.ComposeFile = sharedFilePath(c.Loaded)
		}
	}
	return shared.New(options)
}

// stackNamespace is the Docker label namespace the running command's own
// project gives the shared stack: its config's, or the product's own when
// there is no usable config. A command run outside the repository therefore
// finds only a stack under the default namespace.
func (c *CommandContext) stackNamespace() string {
	if c.Loaded != nil {
		return c.Loaded.Config.LabelNamespace()
	}
	return brand.LabelNamespace
}

// sharedScope is repositoryID's shared lock.
func (c *CommandContext) sharedScope(repositoryID string) lock.Scope {
	return lock.SharedScope(c.ControlHome, repositoryID)
}

// holdShared runs action with repositoryID's shared lock held alone. It is for
// a command that holds no worktree lock (decision 18: nothing takes the shared
// lock and then a worktree's).
func (c *CommandContext) holdShared(ctx context.Context, repositoryID string, action func(context.Context) error) error {
	if err := files.SecureSharedDirectories(layout.SharedLayout(c.ControlHome, repositoryID), c.UserHome); err != nil {
		return err
	}
	scope := c.sharedScope(repositoryID)
	return c.hold(ctx, lifecycle.Requests{Shared: &scope}, action)
}

// sharedWorktreeRoots are the checkouts a shared bind mount must stay out of:
// the config's directory and every live worktree of the repository.
func sharedWorktreeRoots(ctx context.Context, cctx *CommandContext, repositoryID string) ([]string, error) {
	var roots []string
	if cctx.Loaded != nil {
		roots = append(roots, cctx.Loaded.Root)
	}
	identities, err := cctx.Registry.Identities.ActiveIdentities(ctx)
	if err != nil {
		return nil, err
	}
	for _, identity := range identities {
		if identity.RepositoryID == repositoryID && !slices.Contains(roots, identity.Root) {
			roots = append(roots, identity.Root)
		}
	}
	return roots, nil
}

// sharedNeeded are the shared services a worktree starts: those with at least
// one used-by service, in file order. A service nothing uses is started only
// by `treerig shared up`.
func sharedNeeded(cfg config.Config) []config.SharedService {
	var needed []config.SharedService
	for _, service := range cfg.SharedServices() {
		if len(service.UsedBy) > 0 {
			needed = append(needed, service)
		}
	}
	return needed
}

// sharedNames are the names of services.
func sharedNames(services []config.SharedService) []string {
	names := make([]string, len(services))
	for i, service := range services {
		names[i] = service.Name
	}
	return names
}

// sharedUseMap inverts used-by: each worktree service to the shared services
// it uses, for the override that stamps the shared-use labels.
func sharedUseMap(cfg config.Config) map[string][]string {
	uses := map[string][]string{}
	for _, service := range cfg.SharedServices() {
		for _, user := range service.UsedBy {
			uses[user] = append(uses[user], service.Name)
		}
	}
	if len(uses) == 0 {
		return nil
	}
	return uses
}

// sharedSpecs are the shared services as the worktree's runtime checks them.
func sharedSpecs(services []config.SharedService) []backend.SharedUseSpec {
	specs := make([]backend.SharedUseSpec, len(services))
	for i, service := range services {
		specs[i] = backend.SharedUseSpec{Name: service.Name, UsedBy: service.UsedBy}
	}
	return specs
}

// loopbackAddr is the address a pinned host port is reachable at.
func loopbackAddr(hostPort int) string { return "127.0.0.1:" + strconv.Itoa(hostPort) }

// sharedAddrs are the pinned addresses of the repository's declared shared
// services, one per declared container port that has a record entry, in the
// file's order. They come from the records and not from Docker, so a hook
// gets the same address whether or not the service is running. An unreadable
// record is skipped here; `doctor` reports it.
func sharedAddrs(cctx *CommandContext, repositoryID string) []worktree.SharedAddr {
	if cctx.Loaded == nil {
		return nil
	}
	return sharedAddrsFor(cctx.ControlHome, repositoryID, cctx.Loaded.Config)
}

// sharedAddrsFor is sharedAddrs from a control home and a config alone.
func sharedAddrsFor(controlHome, repositoryID string, cfg config.Config) []worktree.SharedAddr {
	if len(cfg.SharedServices()) == 0 {
		return nil
	}
	records, _, err := registry.NewStore(controlHome, repositoryID).SharedRecords()
	if err != nil {
		return nil
	}
	byName := make(map[string]registry.SharedRecord, len(records))
	for _, record := range records {
		byName[record.Name] = record
	}
	var addrs []worktree.SharedAddr
	for _, service := range cfg.SharedServices() {
		record, ok := byName[service.Name]
		if !ok {
			continue
		}
		for _, container := range service.ContainerPorts() {
			if host := record.HostPort(container); host != 0 {
				addrs = append(addrs, worktree.SharedAddr{Name: service.Name, Port: container, Addr: loopbackAddr(host)})
			}
		}
	}
	return addrs
}

// definedByHost names the worktree that started a shared container, from the
// UUID on its label: its hostname, or "a removed worktree" when the UUID is
// empty or its registration is missing or unreadable.
func definedByHost(cctx *CommandContext, repositoryID, worktreeID string) string {
	const removed = "a removed worktree"
	if worktreeID == "" {
		return removed
	}
	record, err := cctx.Registry.Store(repositoryID).Find(worktreeID)
	if err != nil || record == nil {
		return removed
	}
	return layout.WorktreeHost(record.Slug, record.Domain)
}

// pinSharedPorts commits the pinned loopback ports of service under the
// registry lock: each declared container port gets a host port from the
// allocator, kept across restarts. prune drops a port no longer declared,
// which only a caller about to create the container afresh may ask for.
func pinSharedPorts(ctx context.Context, cctx *CommandContext, repositoryID string, service config.SharedService, prune bool, reallocate []int) (registry.SharedPortResult, error) {
	reserved, err := routerReservedPorts(cctx)
	if err != nil {
		return registry.SharedPortResult{}, err
	}
	routed := sharedRoutedPort(service)
	return cctx.Registry.Store(repositoryID).SyncSharedPorts(ctx, cctx.writer(), registry.SharedPortPlan{
		Routed:     &routed,
		Name:       service.Name,
		Domain:     cctx.Loaded.Config.Project.Domain,
		Ports:      service.ContainerPorts(),
		Prune:      prune,
		Reallocate: reallocate,
		Range:      cctx.portRange(),
		Probe:      cctx.portProbe(),
		Reserved:   reserved,
	})
}

// parseReallocation reads --reallocate-port <name>:<port> values against the
// declared shared services: a map of service to the container ports to move.
func parseReallocation(cfg config.Config, requested []string) (map[string][]int, error) {
	moves := map[string][]int{}
	for _, value := range requested {
		name, rawPort, ok := strings.Cut(value, ":")
		port, err := strconv.Atoi(rawPort)
		if !ok || err != nil || name == "" {
			return nil, clierr.Newf("--reallocate-port %s: write it as <name>:<container port>, such as mailpit:1025.", value)
		}
		var service *config.SharedService
		for i := range cfg.SharedServices() {
			if cfg.Shared.Service[i].Name == name {
				service = &cfg.Shared.Service[i]
			}
		}
		if service == nil {
			return nil, clierr.Newf("--reallocate-port %s: no such shared service. %s", value, declaredSharedHint(cfg))
		}
		if !slices.Contains(service.ContainerPorts(), port) {
			ports := make([]string, 0, len(service.ContainerPorts()))
			for _, p := range service.ContainerPorts() {
				ports = append(ports, strconv.Itoa(p))
			}
			known := fmt.Sprintf("%s declares no ports.", name)
			if len(ports) > 0 {
				known = fmt.Sprintf("%s declares %s.", name, strings.Join(ports, ", "))
			}
			return nil, clierr.Newf("--reallocate-port %s: %d is not one of its container ports. %s", value, port, known)
		}
		if !slices.Contains(moves[name], port) {
			moves[name] = append(moves[name], port)
		}
	}
	return moves, nil
}

// declaredSharedHint lists the declared shared services for a refusal.
func declaredSharedHint(cfg config.Config) string {
	names := sharedNames(cfg.SharedServices())
	if len(names) == 0 {
		return brand.ConfigFile + " declares no shared services."
	}
	return brand.ConfigFile + " declares " + strings.Join(names, ", ") + "."
}
