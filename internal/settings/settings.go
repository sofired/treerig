// Package settings loads and validates settings.toml (#79): a machine-wide,
// per-berth-root file that is not committed to any repository, unlike
// berth.toml (internal/config). It holds defaults that apply before any
// project config is even found — the router's port and the range routed
// services' loopback ports are allocated from.
//
// It is decoded the same strict way berth.toml is: BurntSushi/toml, rebuilt
// into shape's ordered object (config.DecodeOrderedTOML) and validated
// against a schema that rejects unknown keys, so a typo or a stray table
// is reported clearly rather than silently ignored. A missing file is not
// an error — settings.toml is optional — but a malformed one, or one with
// an unrecognized key, is.
package settings

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/shape"
)

// Settings is a validated settings.toml with defaults applied. The zero
// value is what a missing file resolves to.
type Settings struct {
	Router Router `json:"router"`
	Ports  Ports  `json:"ports"`
}

// Ports is the [ports] table (#119).
type Ports struct {
	// Range is where routed services' loopback ports are allocated from;
	// nil means unset, so layout.DefaultPortRange applies.
	Range *layout.PortRange `json:"range,omitempty"`
}

// Router is the [router] table.
type Router struct {
	// Port overrides the router's default port (80); nil means unset, so
	// ResolveRouterPort's own precedence continues past it.
	Port *int `json:"port,omitempty"`
}

// rootSchema is settings.toml's whole shape: a [router] table (its port) and
// a [ports] table (the loopback port range), both strict about unrecognized keys the way berth.toml's
// own rootSchema (internal/config/schema.go) is.
func rootSchema() *shape.ObjectSchema {
	router := shape.Object(
		shape.F("port", shape.Number().Int().Min(1).Max(65_535).Optional()),
	).Strict().Default(shape.NewObj())

	ports := shape.Object(
		shape.F("range", shape.String().Refine(func(value string) bool {
			_, err := layout.ParsePortRange(value)
			return err == nil
		}, "must be <low>-<high>, both between 1024 and 65535 with low no greater than high, for example \"20000-29999\"").Optional()),
	).Strict().Default(shape.NewObj())

	return shape.Object(
		shape.F("router", router),
		shape.F("ports", ports),
	).Strict()
}

// Parse validates contents, reporting problems against file: a syntax error
// as "<file> is not valid TOML: …", an unknown key or a wrong-typed value as
// "<file> is not a valid settings.toml:\n  <path>: <message>" — the same two
// shapes config.Parse reports for berth.toml.
func Parse(contents []byte, file string) (Settings, error) {
	obj, err := config.DecodeOrderedTOML(string(contents))
	if err != nil {
		return Settings{}, clierr.Newf("%s is not valid TOML: %s", file, err.Error())
	}

	value, issues := shape.Parse(rootSchema(), obj)
	if len(issues) > 0 {
		lines := make([]string, len(issues))
		for i, issue := range issues {
			lines[i] = fmt.Sprintf("  %s: %s", shape.PathString(issue.Path), issue.Message)
		}
		return Settings{}, clierr.Newf("%s is not a valid %s:\n%s", file, settingsFileNoun, strings.Join(lines, "\n"))
	}

	return decode(value.(*shape.Obj)), nil
}

// settingsFileNoun is what a schema-violation error names the file's shape
// as; brand.SettingsFile ("settings.toml") without importing brand just for
// this one string would be more indirection than it is worth, so this
// package imports brand transitively through config instead. Kept as its
// own name so a future field with its own file-naming rule (there is none
// today) still reads clearly at the call site.
const settingsFileNoun = "settings.toml"

// decode type-asserts the "router" and "ports" tables out of a validated
// Obj, the same way config's own decodeConfig does for berth.toml.
func decode(obj *shape.Obj) Settings {
	var s Settings
	if v, ok := obj.Get("router"); ok {
		if p, ok := v.(*shape.Obj).Get("port"); ok {
			port := p.(int)
			s.Router.Port = &port
		}
	}
	if v, ok := obj.Get("ports"); ok {
		if r, ok := v.(*shape.Obj).Get("range"); ok {
			// Validated by the schema above, so it parses.
			if parsed, err := layout.ParsePortRange(r.(string)); err == nil {
				s.Ports.Range = &parsed
			}
		}
	}
	return s
}

// Load reads and validates the settings file at path. A missing file
// resolves to the zero value (every default applies) rather than an error:
// unlike berth.toml, settings.toml is optional.
func Load(path string) (Settings, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Settings{}, nil
		}
		// Present but unreadable is a problem with the file, named as such,
		// not a bare path error: the file is optional, but not one to skip
		// silently once it exists.
		return Settings{}, clierr.Newf("Could not read %s: %s", path, err.Error())
	}
	return Parse(contents, path)
}

// PortSource says where a resolved router port came from.
type PortSource int

const (
	// PortDefault means neither BERTH_ROUTER_PORT nor the settings file set
	// a port: the port is 80.
	PortDefault PortSource = iota
	// PortFile means the settings file's [router] port set it.
	PortFile
	// PortEnv means BERTH_ROUTER_PORT set it, ahead of the settings file.
	PortEnv
)

// String is what `router status` and `doctor` print for a resolved port's
// source.
func (s PortSource) String() string {
	switch s {
	case PortEnv:
		return layout.RouterPortEnv
	case PortFile:
		return "the settings file"
	default:
		return "the default"
	}
}

// PortResolution is the router's port together with where it came from, and
// the settings file that was — or would have been — consulted, for `router
// status` and `doctor` to print (#79).
type PortResolution struct {
	Port         int
	Source       PortSource
	SettingsPath string
}

// Describe is the one line `router status` and `doctor` print about the
// resolved port: the port, where it came from, and the settings file that
// was — or, when the environment overrode it, would have been — read.
func (p PortResolution) Describe() string {
	switch p.Source {
	case PortEnv:
		return fmt.Sprintf("port %d from %s (overriding %s)", p.Port, layout.RouterPortEnv, p.SettingsPath)
	case PortFile:
		return fmt.Sprintf("port %d from %s", p.Port, p.SettingsPath)
	default:
		return fmt.Sprintf("port %d by default (%s sets none)", p.Port, p.SettingsPath)
	}
}

// ResolveRouterPort resolves the router's port: BERTH_ROUTER_PORT
// (layout.RouterPortEnv), then settings.toml's [router] port under home,
// then 80. home is the already-checked berth root (commands.checkedHome);
// this package touches the filesystem (Load), which is why the precedence
// is combined here rather than in internal/layout, which stays pure path
// derivation.
func ResolveRouterPort(getenv layout.Getenv, home string) (PortResolution, error) {
	path := layout.SettingsPathFrom(home)

	if raw, ok := getenv(layout.RouterPortEnv); ok && raw != "" {
		port, err := layout.EnvRouterPort(getenv)
		if err != nil {
			return PortResolution{}, err
		}
		return PortResolution{Port: port, Source: PortEnv, SettingsPath: path}, nil
	}

	loaded, err := Load(path)
	if err != nil {
		return PortResolution{}, err
	}
	if loaded.Router.Port != nil {
		return PortResolution{Port: *loaded.Router.Port, Source: PortFile, SettingsPath: path}, nil
	}

	return PortResolution{Port: layout.DefaultRouterPort, Source: PortDefault, SettingsPath: path}, nil
}

// ResolvePortRange is settings.toml's [ports] range under home, or
// layout.DefaultPortRange when the file or the key is absent. home is the
// already-checked berth root (commands.checkedHome). Scratch roots and tests
// set their own range here so they never collide with a developer's real
// allocations.
func ResolvePortRange(home string) (layout.PortRange, error) {
	loaded, err := Load(layout.SettingsPathFrom(home))
	if err != nil {
		return layout.PortRange{}, err
	}
	if loaded.Ports.Range != nil {
		return *loaded.Ports.Range, nil
	}
	return layout.DefaultPortRange, nil
}
