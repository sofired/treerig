package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/text"
)

// load reads --config if given, else the nearest berth.toml above env.Cwd.
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
	configured := loaded.Config.Hooks.Configured()
	// Counted separately, because a report about what a file does must not
	// count the hooks nothing invokes yet among the ones that run.
	invoked := make(map[config.HookStage]bool, len(config.InvokedStages))
	for _, stage := range config.InvokedStages {
		invoked[stage] = true
	}
	var inert []string
	for _, stage := range configured {
		if !invoked[stage] {
			inert = append(inert, string(stage))
		}
	}
	live := len(configured) - len(inert)

	// A validator's report is what was asked for, so it goes to stdout the
	// way every other reporting tool's does.
	env.Logger.Output(text.Printable(fmt.Sprintf("%s is valid.", loaded.File)))
	env.Logger.Output(text.Printable(fmt.Sprintf(
		"project %s · domain %s · %d service(s) · %d hook(s)",
		loaded.Config.Project.Name, loaded.Config.Project.Domain, services, live,
	)))
	if len(inert) > 0 {
		env.Logger.Output("accepted but not invoked yet: " + strings.Join(inert, ", "))
	}
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
