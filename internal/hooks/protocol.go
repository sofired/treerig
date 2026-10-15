// Package hooks is the other half of the public contract treerig's own
// documented hook protocol describes: hooks are ordinary executables, the
// tool never learns what they do, and everything a hook needs to know
// arrives as environment variables — its exit code is its only answer back.
package hooks

import "github.com/sofired/treerig/internal/config"

// LifecycleOrder is the order stages run in: seed never runs unless prepare
// exited zero. The lifecycle stops at the first failure and reports which
// stage failed. A function, not a package-level slice, so no caller can
// mutate the package's own notion of the order through a shared backing array.
func LifecycleOrder() []config.HookStage {
	return []config.HookStage{config.Prepare, config.Seed, config.Verify}
}

// UpStages are the stages `up` runs; verify is not among them — `doctor`
// runs it, on its own.
func UpStages() []config.HookStage {
	return []config.HookStage{config.Prepare, config.Seed}
}

// BundleStages receive a bundle directory as their first argument.
func BundleStages() []config.HookStage {
	return []config.HookStage{config.Backup, config.Restore}
}

// Configured reports whether hooks configures stage.
func Configured(hooks config.Hooks, stage config.HookStage) bool {
	return commandFor(hooks, stage) != nil
}

// commandFor is the configured command for stage, or nil if none is.
func commandFor(hooks config.Hooks, stage config.HookStage) *string {
	switch stage {
	case config.Prepare:
		return hooks.Prepare
	case config.Seed:
		return hooks.Seed
	case config.Verify:
		return hooks.Verify
	case config.Backup:
		return hooks.Backup
	case config.Restore:
		return hooks.Restore
	default:
		return nil
	}
}
