package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/shape"
)

// The committed configuration file. This is half of the public contract —
// the other half is the hook protocol — so every field here is a
// compatibility promise. Keep it small.
//
// Ported from src/config/schema.ts field for field, message for message; see
// that file for the reasoning behind each shape. The struct this decodes
// into, Config, is coordinator-owned (config.go) and already carries the
// field order zod's own output uses.

var (
	slugPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	subdomainPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?)?$`)
	hostnamePattern  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
	// urlPathPattern is a URL path prefix: RFC 3986 path characters, `%` only
	// as a proper escape, and no `$`. It ends up verbatim inside a Traefik
	// rule, where only a backtick could do harm; in a printed URL, where `?`
	// and `#` would change its meaning; and in a Compose label value, where
	// `$` is interpolated.
	urlPathPattern = regexp.MustCompile(`^/(?:[A-Za-z0-9._~!&'()*+,;=:@/-]|%[0-9A-Fa-f]{2})*$`)
)

const (
	dnsLabelMax = 63
	// domainMax: a routed hostname is <subdomain>.<slug>.<domain>; with two
	// 63-character labels ahead of it, the domain gets the rest of 253.
	domainMax = 253 - (dnsLabelMax+1)*2
)

// hostnameSchema validates project.domain.
func hostnameSchema() *shape.StringSchema {
	return shape.String().
		Regex(hostnamePattern, "must be a valid hostname").
		Max(domainMax, fmt.Sprintf("must be at most %d characters, so that <subdomain>.<slug>.<domain> fits in a hostname", domainMax)).
		Refine(func(value string) bool {
			for _, label := range strings.Split(value, ".") {
				if utf16Len(label) > dnsLabelMax {
					return false
				}
			}
			return true
		}, fmt.Sprintf("each label must be at most %d characters", dnsLabelMax))
}

// utf16Len matches shape's own (unexported) length measure — JavaScript's
// String.length — since the per-label check here, like zod's own .max(),
// measures UTF-16 code units.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xffff {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// serviceSchema is one [[service]].
func serviceSchema() *shape.ObjectSchema {
	return shape.Object(
		// Compose service name.
		shape.F("name", shape.String().Regex(slugPattern, "must be lower-case letters, digits, and hyphens")),
		// Subdomain under the worktree host; empty routes at the host root.
		shape.F("subdomain", shape.String().
			Regex(subdomainPattern, "must be lower-case letters, digits, and hyphens, not starting or ending with a hyphen").
			Max(dnsLabelMax, fmt.Sprintf("must be at most %d characters", dnsLabelMax)).
			Default("")),
		// Routes only this path prefix, so several services can share one
		// host; also where the printed URL points. Omit to route the whole host.
		shape.F("path", shape.String().
			Regex(urlPathPattern, "must be a URL path starting with /, without spaces, quotes, backticks, $, ? or #; % must be followed by two hex digits").
			Optional()),
		// Container port to route to; defaults to the first port the service exposes or publishes.
		shape.F("port", shape.Number().Int().Min(1).Max(65_535).Optional()),
		// Presence on the shared checkout index; omit to keep the service internal.
		shape.F("index", shape.Object(
			shape.F("label", shape.String().Min(1).Max(512)),
			shape.F("purpose", shape.String().Max(512).Default("")),
		).Strict().Optional()),
	).Strict()
}

// hooksSchema is the [hooks] table.
func hooksSchema() *shape.ObjectSchema {
	return shape.Object(
		// Services are healthy; nothing has been seeded. Migrations belong here.
		shape.F("prepare", shape.String().Min(1).Optional()),
		// Runs only after `prepare` exits zero.
		shape.F("seed", shape.String().Min(1).Optional()),
		// Run by `doctor` for a registered worktree; exit zero means healthy.
		shape.F("verify", shape.String().Min(1).Optional()),
		// The two below are accepted and validated now, and nothing invokes
		// them yet: they are the contract a future `backup` and `restore`
		// will call, declared early so a repository can commit them once.
		shape.F("backup", shape.String().Min(1).Optional()),
		shape.F("restore", shape.String().Min(1).Optional()),
	).Strict().Default(shape.NewObj())
}

// rootSchema is the whole config file.
func rootSchema() *shape.ObjectSchema {
	project := shape.Object(
		shape.F("name", shape.String().Regex(slugPattern, "must be lower-case letters, digits, and hyphens")),
		// Worktrees are routed as <slug>.<domain>.
		shape.F("domain", hostnameSchema()),
		// The control home is deliberately NOT configurable here. This file
		// is committed and shared; a per-machine path belongs in the
		// environment.
		//
		// Docker label namespace. Only set this to adopt resources created
		// by a previous tool; new projects should leave it unset.
		shape.F("label-namespace", shape.String().
			Regex(brand.LabelNamespacePattern, "must look like a reverse domain, e.g. dev.example").
			Optional()),
	).Strict()

	compose := shape.Object(
		// Relative to the config file.
		shape.F("file", shape.String().Min(1)),
	).Strict()

	return shape.Object(
		shape.F("project", project),
		shape.F("compose", compose),
		shape.F("service", shape.Array(serviceSchema()).Default([]any{})),
		shape.F("hooks", hooksSchema()),
	).Strict().SuperRefine(duplicateServiceCheck)
}

// duplicateServiceCheck is configSchema's superRefine: no two services may
// share a name, and no two may claim the same route — a route being a
// subdomain and path together, so two services may share a host if their
// paths differ.
func duplicateServiceCheck(value *shape.Obj, addIssue func(path []string, message string)) {
	raw, _ := value.Get("service")
	services, _ := raw.([]any)

	names := make(map[string]bool, len(services))
	routes := make(map[string]bool, len(services))
	for i, item := range services {
		service, ok := item.(*shape.Obj)
		if !ok {
			continue
		}
		name, _ := stringField(service, "name")
		if names[name] {
			addIssue([]string{"service", strconv.Itoa(i), "name"}, fmt.Sprintf("duplicate service name %q", name))
		}
		names[name] = true

		subdomain, _ := stringField(service, "subdomain")
		path, hasPath := stringField(service, "path")
		route := subdomain + "\x00" + path
		if routes[route] {
			host := fmt.Sprintf("subdomain %q", subdomain)
			if subdomain == "" {
				host = "the host root"
			}
			where := host
			field := "subdomain"
			if hasPath {
				where = fmt.Sprintf("%s at path %q", host, path)
				field = "path"
			}
			addIssue([]string{"service", strconv.Itoa(i), field}, fmt.Sprintf("%s is already claimed by another service", where))
		}
		routes[route] = true
	}
}

func stringField(o *shape.Obj, key string) (string, bool) {
	raw, ok := o.Get(key)
	if !ok {
		return "", false
	}
	s, _ := raw.(string)
	return s, true
}
