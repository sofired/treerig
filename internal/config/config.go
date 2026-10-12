// Package config loads and validates the committed berth.toml. This is half
// of the public contract — the other half is the hook protocol — so every
// field here is a compatibility promise.
//
// The struct field order is the JSON key order `config show` prints.
package config

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/sofired/berth/internal/brand"
)

// Config is a validated berth.toml with defaults applied.
type Config struct {
	Project Project   `json:"project"`
	Compose Compose   `json:"compose"`
	Service []Service `json:"service"`
	// Shared is the [shared] table: services that run once per repository.
	// Nil when the file has none, and then left out of `config show`.
	Shared *Shared `json:"shared,omitempty"`
	Hooks  Hooks   `json:"hooks"`
	// Commands are the project's named commands (#94), in file order; empty
	// when the file has no [commands], and then left out of `config show`.
	Commands Commands `json:"commands,omitempty"`
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

// Service is one [service.<name>] table.
type Service struct {
	// Name is the Compose service name: the table's key.
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

// Shared is the [shared] table (docs/design/routing.md, decisions 15-22).
type Shared struct {
	// File is the shared Compose file, relative to the config file; empty
	// only when no service is declared.
	File string `json:"file,omitempty"`
	// Service are the [shared.service.<name>] tables in the file's order.
	Service []SharedService `json:"service"`
}

// SharedService is one [shared.service.<name>] table.
type SharedService struct {
	// Name is the service's name in the shared Compose file, and the
	// network alias containers reach it by: the table's key.
	Name string `json:"name"`
	// Port is the container port of the service's web UI; nil when it has none.
	Port *int `json:"port,omitempty"`
	// Ports are the raw container ports published for processes on the host.
	Ports []int `json:"ports"`
	// UsedBy are the worktree's Compose services that use this service.
	UsedBy []string `json:"used-by"`
}

// ContainerPorts is every container port the service declares, Port first
// and then Ports in the order written: each gets a pinned host port.
func (s SharedService) ContainerPorts() []int {
	var all []int
	if s.Port != nil {
		all = append(all, *s.Port)
	}
	return append(all, s.Ports...)
}

// SharedServices are the declared shared services, in file order; none when
// the file has no [shared].
func (c Config) SharedServices() []SharedService {
	if c.Shared == nil {
		return nil
	}
	return c.Shared.Service
}

// ServiceIndex is [service.<name>.index].
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

// DefaultCommandKillAfter is how long a stopped [commands] entry gets to
// wind down before it is killed, when the file says nothing (#94). It is the
// runner's own 10-second default (internal/run/osrun) written out, so
// `config show` states the effective value.
const DefaultCommandKillAfter = "10s"

// Command is one [commands] entry: what `berth run <name>` runs, and how.
type Command struct {
	// Command is split and resolved exactly as a hook is (docs/hooks.md).
	Command string `json:"command"`
	// Lock takes the worktree's operation lock while the command runs.
	Lock bool `json:"lock"`
	// KillAfter is a positive duration such as "30s" or "5m": how long the
	// command's process group has to exit after it is asked to stop, before
	// it is killed.
	KillAfter string `json:"kill-after"`
}

// KillAfterDuration is KillAfter as a time.Duration. The schema only admits
// a positive duration, so a Command built by hand with anything else falls
// back to the default rather than to zero, which the runner would read as
// its own default anyway.
func (c Command) KillAfterDuration() time.Duration {
	if d, ok := parseKillAfter(c.KillAfter); ok {
		return d
	}
	d, _ := parseKillAfter(DefaultCommandKillAfter)
	return d
}

// NamedCommand is a Command with the name it is run by.
type NamedCommand struct {
	Name string
	Command
}

// Commands is the [commands] table in the order the file wrote it. A slice,
// not a map, because that order is part of the contract: `config show` and
// the list `berth run` prints both follow it.
type Commands []NamedCommand

// Get is the command called name.
func (c Commands) Get(name string) (Command, bool) {
	for _, entry := range c {
		if entry.Name == name {
			return entry.Command, true
		}
	}
	return Command{}, false
}

// Names are the command names, in file order.
func (c Commands) Names() []string {
	names := make([]string, len(c))
	for i, entry := range c {
		names[i] = entry.Name
	}
	return names
}

// MarshalJSON writes the commands as one object keyed by name, in file
// order, each value the full table form with its defaults filled in.
func (c Commands) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, entry := range c {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := marshalJSON(entry.Name)
		if err != nil {
			return nil, err
		}
		value, err := marshalJSON(entry.Command)
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// marshalJSON is json.Marshal without HTML escaping, the encoding every
// other JSON this tool prints uses (text.SafeJSON).
func marshalJSON(value any) ([]byte, error) {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
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
