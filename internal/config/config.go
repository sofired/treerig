// Package config loads and validates the committed berth.toml. This is half
// of the public contract — the other half is the hook protocol — so every
// field here is a compatibility promise.
//
// The struct field order is the JSON key order `config show` prints.
package config

import "github.com/sofired/berth/internal/brand"

// Config is a validated berth.toml with defaults applied.
type Config struct {
	Project Project   `json:"project"`
	Compose Compose   `json:"compose"`
	Service []Service `json:"service"`
	Hooks   Hooks     `json:"hooks"`
}

// Project is the [project] table.
type Project struct {
	Name   string `json:"name"`
	Domain string `json:"domain"`
	// LabelNamespace adopts resources a previous tool created; nil for new projects.
	LabelNamespace *string `json:"label-namespace,omitempty"`
}

// Compose is the [compose] table.
type Compose struct {
	// File is relative to the config file.
	File string `json:"file"`
}

// Service is one [[service]].
type Service struct {
	// Name is the Compose service name.
	Name string `json:"name"`
	// Subdomain under the worktree host; empty routes at the host root.
	Subdomain string `json:"subdomain"`
	// Path routes only this prefix; nil routes the whole host.
	Path *string `json:"path,omitempty"`
	// Port is the container port; nil means the first it exposes or publishes.
	Port *int `json:"port,omitempty"`
	// Index is presence on the shared checkout index; nil keeps the service internal.
	Index *ServiceIndex `json:"index,omitempty"`
}

// ServiceIndex is [service.index].
type ServiceIndex struct {
	Label   string `json:"label"`
	Purpose string `json:"purpose"`
}

// Hooks is the [hooks] table; nil means not configured.
type Hooks struct {
	Prepare *string `json:"prepare,omitempty"`
	Seed    *string `json:"seed,omitempty"`
	Verify  *string `json:"verify,omitempty"`
	Backup  *string `json:"backup,omitempty"`
	Restore *string `json:"restore,omitempty"`
}

// Loaded is a config and where it came from.
type Loaded struct {
	Config Config
	// File is the absolute path of the file that was loaded.
	File string
	// Root is the directory containing it; relative paths resolve from here.
	Root string
}

// LabelNamespace is the project's label namespace, or the product's own.
func (c Config) LabelNamespace() string {
	if c.Project.LabelNamespace != nil {
		return *c.Project.LabelNamespace
	}
	return brand.LabelNamespace
}

// HookStage names a hook, in schema order.
type HookStage string

const (
	Prepare HookStage = "prepare"
	Seed    HookStage = "seed"
	Verify  HookStage = "verify"
	Backup  HookStage = "backup"
	Restore HookStage = "restore"
)

// Configured lists the configured hooks in schema order.
func (h Hooks) Configured() []HookStage {
	var stages []HookStage
	for _, entry := range []struct {
		stage HookStage
		value *string
	}{{Prepare, h.Prepare}, {Seed, h.Seed}, {Verify, h.Verify}, {Backup, h.Backup}, {Restore, h.Restore}} {
		if entry.value != nil {
			stages = append(stages, entry.stage)
		}
	}
	return stages
}

// InvokedStages are the hooks some command runs today; backup and restore are
// accepted but nothing invokes them yet.
var InvokedStages = []HookStage{Prepare, Seed, Verify}
