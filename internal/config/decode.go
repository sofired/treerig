package config

import "github.com/sofired/treerig/internal/shape"

// decodeConfig converts a [shape.Obj] that rootSchema has already validated
// and defaulted into the Config struct. shape works in terms of any, the way
// zod works in terms of unknown; this is the one place that trusts the
// schema enough to type-assert it back into treerig's own types.
func decodeConfig(obj *shape.Obj) Config {
	var cfg Config
	if v, ok := obj.Get("project"); ok {
		cfg.Project = decodeProject(v.(*shape.Obj))
	}
	if v, ok := obj.Get("compose"); ok {
		cfg.Compose = decodeCompose(v.(*shape.Obj))
	}
	// service always comes back present: the schema defaults it to an empty
	// table. The list follows the file's order of the tables, and each
	// table's key is the service's name.
	cfg.Service = []Service{}
	if v, ok := obj.Get("service"); ok {
		cfg.Service = decodeKeyed(v.(*shape.Obj), func(name string, table any) Service {
			service := decodeService(table.(*shape.Obj))
			service.Name = name
			return service
		})
	}
	if v, ok := obj.Get("shared"); ok {
		shared := decodeShared(v.(*shape.Obj))
		cfg.Shared = &shared
	}
	if v, ok := obj.Get("hooks"); ok {
		cfg.Hooks = decodeHooks(v.(*shape.Obj))
	}
	if v, ok := obj.Get("commands"); ok {
		cfg.Commands = decodeCommands(v.(*shape.Obj))
	}
	return cfg
}

// decodeProject type-asserts the "project" table out of a validated Obj.
func decodeProject(o *shape.Obj) Project {
	var p Project
	if v, ok := o.Get("name"); ok {
		p.Name = v.(string)
	}
	if v, ok := o.Get("domain"); ok {
		p.Domain = v.(string)
	}
	if v, ok := o.Get("label-namespace"); ok {
		s := v.(string)
		p.LabelNamespace = &s
	}
	return p
}

// decodeCompose type-asserts the "compose" table out of a validated Obj.
func decodeCompose(o *shape.Obj) Compose {
	var c Compose
	if v, ok := o.Get("file"); ok {
		c.File = v.(string)
	}
	return c
}

// decodeService type-asserts one [service.<name>] table out of a validated
// Obj. The caller sets Name from the table's key.
func decodeService(o *shape.Obj) Service {
	var s Service
	if v, ok := o.Get("subdomain"); ok {
		s.Subdomain = v.(string)
	}
	if v, ok := o.Get("path"); ok {
		path := v.(string)
		s.Path = &path
	}
	if v, ok := o.Get("port"); ok {
		port := v.(int)
		s.Port = &port
	}
	if v, ok := o.Get("index"); ok {
		s.Index = decodeServiceIndex(v.(*shape.Obj))
	}
	return s
}

// decodeShared type-asserts the "shared" table out of a validated Obj. The
// service list follows the file's order, each table's key its name.
func decodeShared(o *shape.Obj) Shared {
	shared := Shared{Service: []SharedService{}}
	if v, ok := o.Get("file"); ok {
		shared.File = v.(string)
	}
	if v, ok := o.Get("service"); ok {
		shared.Service = decodeKeyed(v.(*shape.Obj), func(name string, table any) SharedService {
			service := SharedService{Name: name, Ports: []int{}, UsedBy: []string{}}
			t := table.(*shape.Obj)
			if port, ok := intField(t, "port"); ok {
				service.Port = &port
			}
			service.Ports = append(service.Ports, intsField(t, "ports")...)
			service.UsedBy = append(service.UsedBy, stringsField(t, "used-by")...)
			return service
		})
	}
	return shared
}

// decodeServiceIndex type-asserts a service's "index" table out of a
// validated Obj.
func decodeServiceIndex(o *shape.Obj) *ServiceIndex {
	var idx ServiceIndex
	if v, ok := o.Get("label"); ok {
		idx.Label = v.(string)
	}
	if v, ok := o.Get("purpose"); ok {
		idx.Purpose = v.(string)
	}
	return &idx
}

// decodeHooks type-asserts the "hooks" table out of a validated Obj, field
// by field so a hook left unset stays nil rather than an empty string.
func decodeHooks(o *shape.Obj) Hooks {
	var h Hooks
	for _, field := range []struct {
		key string
		dst **string
	}{
		{"prepare", &h.Prepare},
		{"seed", &h.Seed},
		{"verify", &h.Verify},
		{"backup", &h.Backup},
		{"restore", &h.Restore},
	} {
		if v, ok := o.Get(field.key); ok {
			s := v.(string)
			*field.dst = &s
		}
	}
	if v, ok := o.Get("timeout"); ok {
		h.Timeout = decodeHookTimeouts(v.(*shape.Obj))
	}
	return h
}

// decodeHookTimeouts type-asserts the "timeout" table out of a validated
// Obj, field by field so a hook with no entry stays nil.
func decodeHookTimeouts(o *shape.Obj) *HookTimeouts {
	var t HookTimeouts
	for _, field := range []struct {
		key string
		dst **string
	}{
		{"prepare", &t.Prepare},
		{"seed", &t.Seed},
		{"verify", &t.Verify},
		{"backup", &t.Backup},
		{"restore", &t.Restore},
	} {
		if v, ok := o.Get(field.key); ok {
			s := v.(string)
			*field.dst = &s
		}
	}
	return &t
}

// decodeKeyed reads a table of named entries in the order the file wrote
// them, converting each with decode; the result is never nil.
func decodeKeyed[T any](o *shape.Obj, decode func(name string, entry any) T) []T {
	out := make([]T, 0, o.Len())
	for _, name := range o.Keys() {
		entry, _ := o.Get(name)
		out = append(out, decode(name, entry))
	}
	return out
}

// decodeCommands type-asserts the "commands" table out of a validated Obj,
// keeping its key order. An entry written as a bare string gets the table
// form's defaults; a table has had them applied by the schema already.
func decodeCommands(o *shape.Obj) Commands {
	commands := decodeKeyed(o, func(name string, raw any) NamedCommand {
		entry := NamedCommand{Name: name, Command: Command{Lock: true, KillAfter: DefaultCommandKillAfter}}
		switch v := raw.(type) {
		case string:
			entry.Command.Command = v
		case *shape.Obj:
			if c, ok := v.Get("command"); ok {
				entry.Command.Command = c.(string)
			}
			if l, ok := v.Get("lock"); ok {
				entry.Lock = l.(bool)
			}
			if k, ok := v.Get("kill-after"); ok {
				entry.KillAfter = k.(string)
			}
		}
		return entry
	})
	if len(commands) == 0 {
		return nil
	}
	return commands
}
