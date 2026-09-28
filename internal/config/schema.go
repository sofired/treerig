package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/shape"
)

// The committed configuration file. This is half of the public contract —
// the other half is the hook protocol — so every field here is a
// compatibility promise. Keep it small.
//
// The struct this decodes into, Config (config.go), carries a fixed field
// order for `config show`'s own JSON output.

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
	// maxServiceEntries mirrors internal/checkoutindex/card's own
	// maxServices: #16 made `up` record every declared service on the
	// checkout index card, not only the ones with a [service.index] block,
	// so the config's own [[service]] array must not outgrow what the card
	// can hold. Hardcoded rather than imported from that package: config is
	// foundational and every command depends on it, while the checkout
	// index is one feature among several that read it — the dependency
	// belongs pointing the other way. A deliberate limit of its own.
	maxServiceEntries = 64
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
		// Presence on the shared checkout index; omit to keep the service
		// internal. Neither label nor purpose refuses a control character
		// here: the base schema stays lenient, so every command that only
		// reads an already-running worktree's config can load one a hand
		// edit or a paste left with one, and go ahead. StartIssues
		// (start.go) is the separate, named check `up`, `config validate`
		// and `doctor` run instead (#19).
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

// killAfterPattern is the spelling of a [commands] kill-after: one or more
// whole numbers each with a unit (ms, s, m or h), as "30s", "5m" or "1m30s".
// No sign and no fraction, so what time.ParseDuration then reads is exactly
// what was written.
var killAfterPattern = regexp.MustCompile(`^([0-9]+(ms|s|m|h))+$`)

// maxKillAfter caps a [commands] kill-after: an hour is far beyond any
// graceful stop, and the cap keeps the runner's wait for a stopped command
// (its kill-after plus a margin) from overflowing a time.Duration.
const maxKillAfter = time.Hour

// parseKillAfter reads a kill-after spelling, reporting false unless it is
// well formed, greater than zero and at most maxKillAfter.
func parseKillAfter(value string) (time.Duration, bool) {
	if !killAfterPattern.MatchString(value) {
		return 0, false
	}
	d, err := time.ParseDuration(value)
	return d, err == nil && d > 0 && d <= maxKillAfter
}

// commandsSchema is the [commands] table: any number of entries, each a
// service-style name mapped to either the command string alone or a table
// with the command and its options.
func commandsSchema() *shape.RecordSchema {
	name := shape.String().
		Regex(slugPattern, "must be lower-case letters, digits, and hyphens").
		Max(dnsLabelMax, fmt.Sprintf("must be at most %d characters", dnsLabelMax))
	table := shape.Object(
		shape.F("command", shape.String().Min(1)),
		// Takes the worktree's operation lock while it runs. Trusted, not
		// enforced: see docs/hooks.md.
		shape.F("lock", shape.Bool().Default(true)),
		// How long the command's process group gets to exit once asked to
		// stop, before it is killed.
		shape.F("kill-after", shape.String().
			Refine(func(value string) bool { _, ok := parseKillAfter(value); return ok },
				"must be a duration greater than zero and at most 1h, such as 30s, 5m or 1m30s").
			Default(DefaultCommandKillAfter)),
	).Strict()
	return shape.Record(name, shape.Union(shape.String().Min(1), table)).Default(shape.NewObj())
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
		// maxServiceEntries (#16): every declared service is now recorded on
		// the checkout index's card, whether or not it opted into
		// [service.index] (internal/checkoutindex/card's own maxServices,
		// which bounds the card's services array the same way) — so the
		// config itself must not declare more than a card can hold. Before
		// #16 this could not be reached from config alone: only the services
		// that opted into [service.index] ever reached the card, and berth
		// never validated how many of those there could be either; #16 makes
		// every declared service count, so the config needs its own limit.
		shape.F("service", shape.Array(serviceSchema()).Default([]any{}).Max(maxServiceEntries,
			fmt.Sprintf("at most %d [[service]] entries; the checkout index's card can name at most that many declared services", maxServiceEntries))),
		shape.F("hooks", hooksSchema()),
		shape.F("commands", commandsSchema()),
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

// stringField reads key from o as a string, reporting whether it was
// present at all; a present-but-wrong-typed value reads as "", true, since
// the schema itself already refused that shape before duplicateServiceCheck
// ever runs.
func stringField(o *shape.Obj, key string) (string, bool) {
	raw, ok := o.Get(key)
	if !ok {
		return "", false
	}
	s, _ := raw.(string)
	return s, true
}
