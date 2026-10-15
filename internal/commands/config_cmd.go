package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/text"
)

// load reads --config if given, else the nearest treerig.toml above env.Cwd.
func load(env *Env) (*config.Loaded, error) {
	if env.ConfigPath != "" {
		return config.Load(env.ConfigPath)
	}
	return config.LoadNearest(env.Cwd)
}

// ConfigValidate is `config validate`. It answers "would up work?", so it
// runs config.CheckStart the same as `up` itself: the base load above stays
// lenient about a control character in [service.<name>.index] label or purpose
// (#19), and only this check refuses.
func ConfigValidate(ctx context.Context, env *Env) error {
	loaded, err := load(env)
	if err != nil {
		return err
	}
	if err := config.CheckStart(loaded); err != nil {
		return err
	}

	services := len(loaded.Config.Service)
	hookCount := len(loaded.Config.Hooks.Configured())

	// A validator's report is what was asked for, so it goes to stdout the
	// way every other reporting tool's does.
	env.Logger.Output(text.Printable(fmt.Sprintf("%s is valid.", loaded.File)))
	summary := fmt.Sprintf(
		"project %s · domain %s · %d service(s) · %d hook(s)",
		loaded.Config.Project.Name, loaded.Config.Project.Domain, services, hookCount,
	)
	if shared := len(loaded.Config.SharedServices()); shared > 0 {
		summary += fmt.Sprintf(" · %d shared service(s)", shared)
	}
	env.Logger.Output(text.Printable(summary))
	controlHome, err := resolveControlHome(env.Getenv)
	if err != nil {
		return err
	}
	env.Logger.Output(text.Printable(fmt.Sprintf("control home: %s", controlHome)))
	return nil
}

// ConfigShow is `config show`.
func ConfigShow(ctx context.Context, env *Env) error {
	loaded, err := load(env)
	if err != nil {
		return err
	}
	out, err := text.SafeJSON(loaded.Config)
	if err != nil {
		return err
	}
	env.Logger.Output(out)
	return nil
}

// ConfigSchema is `config schema`: the JSON Schema of the config file's
// input shape. It reads no configuration, so it works anywhere.
func ConfigSchema(ctx context.Context, env *Env) error {
	env.Logger.Output(strings.TrimSuffix(string(config.SchemaJSON()), "\n"))
	return nil
}
