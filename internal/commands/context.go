package commands

import (
	"path/filepath"

	"github.com/sofired/berth/internal/backend"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lifecycle"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/run"
)

// CommandContext is everything a verb needs, assembled once per invocation
// from an Env. Commands operate on a CommandContext, not an Env directly, so
// they can be exercised in tests without a config file on disk or a real
// process environment — see registry/registrytest and (for command-level
// fixtures) commandstest.
//
// This is the Go counterpart of src/commands/context.ts's CommandContext,
// carrying what the verbs ported so far need; the router and the checkout
// index join it as they are ported.
type CommandContext struct {
	CommandName string
	// Invocation is the command line as invoked, arguments space-joined;
	// names this process to whoever waits on a lock it holds.
	Invocation  string
	Logger      logger.Logger
	Runner      run.Runner
	Loaded      *config.Loaded
	ControlHome string
	// RouterPort is BERTH_ROUTER_PORT, or 80 — the port `urls` prints
	// routed services on. It is the same value a running RouterService would
	// report as its own `port`, without needing one built.
	RouterPort int
	Registry   *registry.Registry
	// Environ is what Compose inherits.
	Environ []string
	// Lifecycle holds mutations' locks and defers signals; nil builds one that never listens.
	Lifecycle *lifecycle.Lifecycle
}

// ComposeFile is the consumer's Compose file, resolved against the config's directory.
func (c *CommandContext) ComposeFile() string {
	if filepath.IsAbs(c.Loaded.Config.Compose.File) {
		return filepath.Clean(c.Loaded.Config.Compose.File)
	}
	return filepath.Join(c.Loaded.Root, c.Loaded.Config.Compose.File)
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

	controlHome, err := layout.ResolveControlHome(env.Getenv, "")
	if err != nil {
		return nil, err
	}
	routerPort, err := layout.ResolveRouterPort(env.Getenv)
	if err != nil {
		return nil, err
	}

	reg := registry.New(registry.Options{
		ControlHome:    controlHome,
		Root:           loaded.Root,
		Domain:         loaded.Config.Project.Domain,
		LabelNamespace: loaded.Config.LabelNamespace(),
		Runner:         env.Runner,
		Logger:         env.Logger,
		CommandName:    env.CommandName,
		Invocation:     env.Invocation,
	})

	return &CommandContext{
		CommandName: env.CommandName,
		Invocation:  env.Invocation,
		Logger:      env.Logger,
		Runner:      env.Runner,
		Loaded:      loaded,
		ControlHome: controlHome,
		RouterPort:  routerPort,
		Registry:    reg,
		Environ:     env.Environ,
		Lifecycle:   env.Lifecycle,
	}, nil
}
