package commands

import (
	"fmt"
	"path/filepath"

	"github.com/sofired/berth/internal/backend"
	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lifecycle"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/ports"
	"github.com/sofired/berth/internal/procid"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/settings"
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
	Invocation  string
	Logger      logger.Logger
	Runner      run.Runner
	Loaded      *config.Loaded
	ControlHome string
	// UserHome is the checked berth root's own companion value from
	// checkedHome (home.go): the caller's resolved home directory, handed
	// to files.SecureWorktreeDirectories ahead of every write under
	// ControlHome (up.go), the same way registry.Options.UserHome carries
	// it to the registry's own writes (#80).
	UserHome string
	// RouterPort is BERTH_ROUTER_PORT, then settings.toml's [router] port
	// under the checked berth root, then 80 (settings.ResolveRouterPort,
	// #79) — the port `urls` prints routed services on. It is the same
	// value a running RouterService would report as its own `port`,
	// without needing one built.
	// Home is the berth root (BERTH_HOME) this context resolved and checked
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
	// OS is runtime.GOOS, passed in from the entry point (Env.OS): it only
	// picks which tool a message suggests for finding a process.
	OS string
	// RouterState is the shared router's own on-disk layout — where its
	// lock and its index page live (neither is under ControlHome: the
	// router's state is machine-wide, per berth root — see
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

// Runtime is the Compose runtime for resources labelled under namespace; a
// worktree of another repository may carry another project's.
func (c *CommandContext) Runtime(namespace string) *backend.Compose {
	return backend.NewCompose(backend.Options{
		ComposeFile:    c.ComposeFile(),
		ProjectRoot:    c.Loaded.Root,
		ConfigFile:     c.Loaded.File,
		ProjectName:    c.Loaded.Config.Project.Name,
		Domain:         c.Loaded.Config.Project.Domain,
		ControlHome:    c.ControlHome,
		LabelNamespace: namespace,
		Runner:         c.Runner,
		Logger:         c.Logger,
		Environ:        c.Environ,
	})
}

// Context builds a CommandContext: it loads the config (env.ConfigPath, or
// the nearest berth.toml above env.Cwd), resolves the control home, and
// builds this checkout's worktree registry over it.
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
// records alone and must still run where there is no usable berth.toml — a
// checkout of an old branch, or a file with a mistake in it: `shared down`
// has to be able to clean up from anywhere. It loads the config when there is
// a usable one, and warns and goes on without it when the file is there but
// invalid. Loaded is nil without one, so a verb that calls this reads nothing
// from it unchecked.
func ContextWithoutConfig(env *Env) (*CommandContext, error) {
	var loaded *config.Loaded
	var err error
	// file is the config the repository is taken from; fallbackRoot is its
	// directory, used when the file cannot be loaded, so a broken file still
	// names its own repository and never the working directory's.
	file, fallbackRoot := env.ConfigPath, ""
	if file != "" {
		loaded, err = config.Load(file)
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
		env.Logger.Warn(fmt.Sprintf("Going on without %s: %s", brand.ConfigFile, err))
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
	})

	return &CommandContext{
		CommandName:          env.CommandName,
		Invocation:           env.Invocation,
		Logger:               env.Logger,
		Runner:               env.Runner,
		Loaded:               loaded,
		ControlHome:          controlHome,
		UserHome:             userHome,
		Home:                 home,
		RouterPort:           routerPort,
		RouterPortResolution: resolvedPort,
		PortRange:            portRange,
		PortProbe:            env.PortProbe,
		OS:                   env.OS,
		RouterState:          routerState,
		Registry:             reg,
		Environ:              env.Environ,
		Table:                env.Table,
		Lifecycle:            env.Lifecycle,
	}, nil
}
