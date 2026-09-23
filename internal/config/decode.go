package config

import "github.com/sofired/berth/internal/shape"

// decodeConfig converts a [shape.Obj] that rootSchema has already validated
// and defaulted into the Config struct. shape works in terms of any, the way
// zod works in terms of unknown; this is the one place that trusts the
// schema enough to type-assert it back into berth's own types.
func decodeConfig(obj *shape.Obj) Config {
	var cfg Config
	if v, ok := obj.Get("project"); ok {
		cfg.Project = decodeProject(v.(*shape.Obj))
	}
	if v, ok := obj.Get("compose"); ok {
		cfg.Compose = decodeCompose(v.(*shape.Obj))
	}
	// service always comes back present: the schema defaults it to [].
	if v, ok := obj.Get("service"); ok {
		if elements, ok := v.([]any); ok {
			cfg.Service = make([]Service, len(elements))
			for i, element := range elements {
				cfg.Service[i] = decodeService(element.(*shape.Obj))
			}
		}
	}
	if cfg.Service == nil {
		cfg.Service = []Service{}
	}
	if v, ok := obj.Get("hooks"); ok {
		cfg.Hooks = decodeHooks(v.(*shape.Obj))
	}
	return cfg
}

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

func decodeCompose(o *shape.Obj) Compose {
	var c Compose
	if v, ok := o.Get("file"); ok {
		c.File = v.(string)
	}
	return c
}

func decodeService(o *shape.Obj) Service {
	var s Service
	if v, ok := o.Get("name"); ok {
		s.Name = v.(string)
	}
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
	return h
}
