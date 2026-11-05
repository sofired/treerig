package commands

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/sofired/treerig/internal/backend"
	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/lifecycle"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/ports"
	"github.com/sofired/treerig/internal/procid"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/settings"
)

// CommandContext is everything a verb needs, assembled once per invocation
// from an Env. Commands operate on a CommandContext, not an Env directly, so
// they can be exercised in tests without a config file on disk or a real
// process environment — see registry/registrytest and (for command-level
// fixtures) commandstest.
//
// It carries what every verb needs, including the router and the checkout
// index.
type CommandContext struct {
	CommandName string
	// Invocation is the command line as invoked, arguments space-joined;
	// names this process to whoever waits on a lock it holds.
	Invocation string
	Logger     logger.Logger
	Runner     run.Runner
	Loaded     *config.Loaded
	// Cwd is the physical working directory the command was run from; it
	// anchors a relative bundle directory (backup, restore), since a hook
	// runs in the project root instead.
	Cwd         string
	ControlHome string
	// UserHome is the checked treerig root's own companion value from
	// checkedHome (home.go): the caller's resolved home directory, handed
	// to files.SecureWorktreeDirectories ahead of every write under
	// ControlHome (up.go), the same way registry.Options.UserHome carries
	// it to the registry's own writes (#80).
	UserHome string
	// RouterPort is TREERIG_ROUTER_PORT, then settings.toml's [router] port
	// under the checked treerig root, then 80 (settings.ResolveRouterPort,
	// #79) — the port `urls` prints routed services on. It is the same
	// value a running RouterService would report as its own `port`,
	// without needing one built.
	// Home is the treerig root (TREERIG_HOME) this context resolved and checked
	// once; `up` hands it to the router it starts, as the root that owns it
	// (#82).
	Home       string
	RouterPort int
	// RouterPortResolution is RouterPort with where it came from and the
	// settings file consulted (#79), the one resolution this context made:
	// `up` builds its router from it rather than resolving again.
	RouterPortResolution settings.PortResolution
	// PortRange is where routed services' loopback ports are allocated from
	// (settings.toml's [ports] range, else layout.DefaultPortRange, #119);
	// the zero value means the default.
	PortRange layout.PortRange
	// PortProbe decides whether a candidate or pinned port is free; nil
	// asks the operating system (ports.LoopbackProbe). Tests set a fake
	// so they bind nothing.
	PortProbe ports.Probe
	// Listeners says which loopback addresses a host route's port answers
	// on; nil dials (ports.DialListener).
	Listeners ports.ListenerProbe
	// OS is runtime.GOOS, passed in from the entry point (Env.OS): it only
	// picks which tool a message suggests for finding a process.
	OS string
	// RouterState is the shared router's own on-disk layout — where its
	// lock and its index page live (neither is under ControlHome: the
	// router's state is machine-wide, per treerig root — see
	// internal/layout's RouterStateDir comment). Resolved once here so
	// newCheckoutIndex needs no Getenv of its own.
	RouterState layout.RouterLayout
	Registry    *registry.Registry
	// Environ is what Compose inherits.
	Environ []string
	// Table identifies and inspects processes for lock recovery's survivor
	// scan (UnlockWithContext); nil defaults to procid.Platform(), carried
	// over from Env.Table unchanged.
	Table procid.Table
	// Lifecycle holds mutations' locks and defers signals; nil builds one that never listens.
	Lifecycle *lifecycle.Lifecycle
}

// ComposeFile is the consumer's Compose file, resolved against the config's directory.
func (c *CommandContext) ComposeFile() string { return composeFilePath(c.Loaded) }

// composeFilePath is loaded's Compose file, resolved against the config's directory.
func composeFilePath(loaded *config.Loaded) string {
	if filepath.IsAbs(loaded.Config.Compose.File) {
		return filepath.Clean(loaded.Config.Compose.File)
	}
	return filepath.Join(loaded.Root, loaded.Config.Compose.File)
}

// LabelRuntime is the Compose runtime for resources labelled under namespace; a
// worktree of another repository may carry another project's. It carries no
// routed URLs (TREERIG_URL and friends), so it is only for calls that look
// resources up by label and never read the Compose file; anything that can
// uses RuntimeFor. The name is the reminder.
func (c *CommandContext) LabelRuntime(namespace string) *backend.Compose {
	return c.RuntimeFor(namespace, nil, 0)
}

// RuntimeFor is LabelRuntime for a worktree whose registration is at hand: its
// Compose invocations also get TREERIG_URL and TREERIG_SERVICE_URL_<NAME>
// (#177), built from registered and routerPort the way `treerig urls` builds
// them. A nil registered or an unresolved (not positive) routerPort leaves
// them unset. Callers pass the router port their command works with:
// CommandContext.RouterPort, or the running Router's Port() in `up` and `dev`.
func (c *CommandContext) RuntimeFor(namespace string, registered *registry.RegisteredWorktree, routerPort int) *backend.Compose {
	options := backend.Options{
		ControlHome:    c.ControlHome,
		LabelNamespace: namespace,
		Runner:         c.Runner,
		Logger:         c.Logger,
		Environ:        c.Environ,
	}
	// Without a treerig.toml (ContextWithoutConfig) there is no Compose file,
	// project or domain to hand over: the label-only calls LabelRuntime is for
	// never read them.
	if c.Loaded != nil {
		options.URLVariables = composeURLVariables(c.Loaded.Config, registered, routerPort)
		options.ComposeFile = c.ComposeFile()
		options.ProjectRoot = c.Loaded.Root
		options.ConfigFile = c.Loaded.File
		options.ProjectName = c.Loaded.Config.Project.Name
		options.Domain = c.Loaded.Config.Project.Domain
	}
	return backend.NewCompose(options)
}

// Context builds a CommandContext: it loads the config (env.ConfigPath, or
// the nearest treerig.toml above env.Cwd), resolves the control home, and
// builds this worktree's registry over it.
func Context(env *Env) (*CommandContext, error) {
	var loaded *config.Loaded
	var err error
	if env.ConfigPath != "" {
		loaded, err = config.Load(env.ConfigPath)
	} else {
		loaded, err = config.LoadNearest(env.Cwd)
	}
	if err != nil {
		return nil, err
	}
	return buildContext(env, loaded, "")
}

// ContextWithoutConfig is Context for a verb that works from labels and
// records alone and must still run where there is no usable treerig.toml — in
// a worktree of an old branch, beside a file with a mistake in it, or outside
// every repository: `shared down` has to be able to clean up from anywhere, as
// can `worktree list`, `worktree unlock` and `worktree remove <worktree>`. It
// loads the config when there is a usable one, and warns and goes on without
// it when the file is there but invalid. No treerig.toml anywhere above the
// working directory is no error either, but a file named with --config that is
// not there is a mistake in the command line, and is one. Loaded is nil
// without a usable config, so a verb that calls this reads nothing from it
// unchecked.
func ContextWithoutConfig(env *Env) (*CommandContext, error) {
	var loaded *config.Loaded
	var err error
	// file is the config the repository is taken from; fallbackRoot is its
	// directory, used when the file cannot be loaded, so a broken file still
	// names its own repository and never the working directory's.
	file, fallbackRoot := env.ConfigPath, ""
	if file != "" {
		loaded, err = config.Load(file)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	} else {
		found := false
		var findErr error
		file, found, findErr = config.FindConfigFile(env.Cwd)
		if findErr != nil {
			return nil, findErr
		}
		if found {
			loaded, err = config.Load(file)
		}
	}
	if err != nil {
		env.Logger.Warn(fmt.Sprintf("Ignoring %s for this command: %s", brand.ConfigFile, err))
		loaded = nil
		resolved, resolveErr := layout.Resolve(file)
		if resolveErr != nil {
			return nil, resolveErr
		}
		fallbackRoot = filepath.Dir(resolved)
	}
	return buildContext(env, loaded, fallbackRoot)
}

// buildContext assembles the CommandContext around loaded, which may be nil
// (ContextWithoutConfig): the registry then answers for the working
// directory, or for fallbackRoot when that is set, with no domain.
func buildContext(env *Env, loaded *config.Loaded, fallbackRoot string) (*CommandContext, error) {
	root, domain, namespace := env.Cwd, "", ""
	if fallbackRoot != "" {
		root = fallbackRoot
	}
	if loaded != nil {
		root, domain, namespace = loaded.Root, loaded.Config.Project.Domain, loaded.Config.LabelNamespace()
	}
	home, userHome, err := checkedHome(env.Getenv)
	if err != nil {
		return nil, err
	}
	controlHome, err := layout.ControlHomeFrom(home, env.Getenv, "")
	if err != nil {
		return nil, err
	}
	resolvedPort, err := settings.ResolveRouterPort(env.Getenv, home)
	if err != nil {
		return nil, err
	}
	routerPort := resolvedPort.Port
	portRange, err := settings.ResolvePortRange(home)
	if err != nil {
		return nil, err
	}
	routerState := layout.RouterStateFrom(home)

	reg := registry.New(registry.Options{
		ControlHome:    controlHome,
		Root:           root,
		Domain:         domain,
		LabelNamespace: namespace,
		Runner:         env.Runner,
		Logger:         env.Logger,
		CommandName:    env.CommandName,
		Invocation:     env.Invocation,
		UserHome:       userHome,
		Environ:        env.Environ,
	})

	return &CommandContext{
		CommandName:          env.CommandName,
		Invocation:           env.Invocation,
		Logger:               env.Logger,
		Runner:               env.Runner,
		Loaded:               loaded,
		Cwd:                  env.Cwd,
		ControlHome:          controlHome,
		UserHome:             userHome,
		Home:                 home,
		RouterPort:           routerPort,
		RouterPortResolution: resolvedPort,
		PortRange:            portRange,
		PortProbe:            env.PortProbe,
		Listeners:            env.Listeners,
		OS:                   env.OS,
		RouterState:          routerState,
		Registry:             reg,
		Environ:              env.Environ,
		Table:                env.Table,
		Lifecycle:            env.Lifecycle,
	}, nil
}

// nameOptions are the registry.NameOptions every command that takes a worktree
// reads its argument with: the working directory a relative path starts from,
// and the home directory a "~" stands for. repositoryID is the repository the
// command runs in when the caller already asked Git (nil asks only when it
// matters). withinDirectory is for a read-only command, which may name a
// worktree by a path inside its directory.
func (c *CommandContext) nameOptions(domain string, repositoryID *string, withinDirectory bool) registry.NameOptions {
	return registry.NameOptions{
		Domain: domain, Cwd: c.Cwd, Home: c.UserHome, RepositoryID: repositoryID, WithinDirectory: withinDirectory,
	}
}
