package config

import (
	"fmt"
	"regexp"
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
	// checkout index card, not only the ones with a [service.<name>.index]
	// table, so the config's own [service] table must not outgrow what the
	// card can hold. Hardcoded rather than imported from that package: config is
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

// SchemaID is the $id of the published JSON Schema: where the copy
// committed as schema/berth.schema.json is served from. It tracks main.
const SchemaID = "https://raw.githubusercontent.com/sofired/berth/main/schema/berth.schema.json"

// nameKey is the key schema of a table of named entries: a name of
// lower-case letters, digits and hyphens, at most dnsLabelMax characters.
// what names the thing being named, for the description editors show.
func nameKey(what string) *shape.StringSchema {
	return shape.String().
		Regex(slugPattern, "must be lower-case letters, digits, and hyphens").
		Max(dnsLabelMax, fmt.Sprintf("must be at most %d characters", dnsLabelMax)).
		Describe(fmt.Sprintf("%s: lower-case letters, digits and hyphens, at most %d characters.", what, dnsLabelMax))
}

// arrayOfTablesHint is the note added to the error for a table of named
// tables written as an array, [[header]]: plural names the entries, header
// the table they belong in.
func arrayOfTablesHint(plural, header string) string {
	return fmt.Sprintf("%s are tables keyed by name: write [%s.<name>], not [[%s]]", plural, header, header)
}

// keyedTables is a table of named entries, [header.<name>] each: names
// validates the keys, entry each value, and an array in its place gets
// arrayOfTablesHint. A positive limit caps the entries, with message. The
// parsed value keeps the file's order; decodeKeyed reads it.
func keyedTables(names *shape.StringSchema, entry shape.Schema, plural, header string, limit int, message string) *shape.RecordSchema {
	r := shape.Record(names, entry).Hint(arrayOfTablesHint(plural, header)).Default(shape.NewObj())
	if limit > 0 {
		r.Max(limit, message)
	}
	return r
}

// serviceSchema is one [service.<name>] table. The name is the table's key,
// so it is not a field here; decodeConfig reads it from the key.
func serviceSchema() *shape.ObjectSchema {
	return shape.Object(
		shape.F("subdomain", shape.String().
			Regex(subdomainPattern, "must be lower-case letters, digits, and hyphens, not starting or ending with a hyphen").
			Max(dnsLabelMax, fmt.Sprintf("must be at most %d characters", dnsLabelMax)).
			Default("").
			Describe("Subdomain under the worktree host, so the service is routed at <subdomain>.<slug>.<domain>. Empty (the default) routes at the worktree host root.")),
		shape.F("path", shape.String().
			Regex(urlPathPattern, "must be a URL path starting with /, without spaces, quotes, backticks, $, ? or #; % must be followed by two hex digits").
			Optional().
			Describe("Routes only this URL path prefix, which must start with /, so several services can share one host; also where the printed URL points. Omit to route the whole host.")),
		shape.F("port", shape.Number().Int().Min(1).Max(65_535).Optional().
			Describe("Container port to route to. Defaults to the first port the service exposes or publishes.")),
		// Neither label nor purpose refuses a control character here: the
		// base schema stays lenient, so every command that only reads an
		// already-running worktree's config can load one a hand edit or a
		// paste left with one, and go ahead. StartIssues (start.go) is the
		// separate, named check `up`, `config validate` and `doctor` run
		// instead (#19).
		shape.F("index", shape.Object(
			shape.F("label", shape.String().Min(1).Max(512).
				Describe("Name shown for the service on the shared checkout index.")),
			shape.F("purpose", shape.String().Max(512).Default("").
				Describe("What the service is for, shown beside its label on the checkout index.")),
		).Strict().Optional().
			Describe("Presence on the shared checkout index. Omit to keep the service off the index.")),
	).Strict().
		KeyHint("name", func(path []string) string {
			return fmt.Sprintf("the table's key is the service name: [%s]", shape.PathString(path))
		}).
		Describe("One routed service. The table's key is its Compose service name.")
}

// hooksSchema is the [hooks] table.
func hooksSchema() *shape.ObjectSchema {
	return shape.Object(
		shape.F("prepare", shape.String().Min(1).Optional().
			Describe("Command run once the services are healthy and before anything is seeded. Migrations belong here.")),
		shape.F("seed", shape.String().Min(1).Optional().
			Describe("Command that seeds data. Runs only the first time the worktree comes up, and only after prepare exits zero.")),
		shape.F("verify", shape.String().Min(1).Optional().
			Describe("Command `berth doctor` runs for a registered worktree; exit zero means healthy. Its output goes to stderr.")),
		// The two below are accepted and validated now, and nothing invokes
		// them yet: they are the contract a future `backup` and `restore`
		// will call, declared early so a repository can commit them once.
		shape.F("backup", shape.String().Min(1).Optional().
			Describe("Command a future `berth backup` will run. Accepted and validated now; nothing invokes it yet.")),
		shape.F("restore", shape.String().Min(1).Optional().
			Describe("Command a future `berth restore` will run. Accepted and validated now; nothing invokes it yet.")),
	).Strict().Default(shape.NewObj()).
		Describe("Commands berth runs at points in a worktree's life. Each is resolved relative to this file when it starts with \".\" or contains \"/\", and looked up on PATH otherwise. See docs/hooks.md.")
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
	table := shape.Object(
		shape.F("command", shape.String().Min(1).
			Describe("The command line, split and resolved exactly as a hook is. See docs/hooks.md.")),
		// Trusted, not enforced: see docs/hooks.md.
		shape.F("lock", shape.Bool().Default(true).
			Describe("Hold the worktree's operation lock while the command runs, so `up`, `down` and a second `run` wait. false skips only the lock; it is trusted, not enforced.")),
		shape.F("kill-after", shape.String().
			SchemaPattern(killAfterPattern).
			Refine(func(value string) bool { _, ok := parseKillAfter(value); return ok },
				"must be a duration greater than zero and at most 1h, such as 30s, 5m or 1m30s").
			Default(DefaultCommandKillAfter).
			Describe("How long the command's process group has to exit once asked to stop, before it is killed: a duration such as 30s, 5m or 1m30s, greater than zero and at most 1h.")),
	).Strict().Describe("A command with options.")
	return keyedTables(nameKey("The command's name"), shape.Union(
		shape.String().Min(1).Describe("The command line on its own, with the default options."),
		table,
	).Describe("A command line, or a table with the command line and its options."), "commands", "commands", 0, "").
		Describe("Named commands for `berth run <name> [-- args...]`: on-demand work that is not part of coming up. Each key is a name of lower-case letters, digits and hyphens.")
}

// rootSchema is the whole config file.
func rootSchema() *shape.ObjectSchema {
	project := shape.Object(
		shape.F("name", shape.String().Regex(slugPattern, "must be lower-case letters, digits, and hyphens").
			Describe("The project's name: lower-case letters, digits and hyphens.")),
		shape.F("domain", hostnameSchema().
			Describe("Parent domain worktrees are routed under, as <slug>.<domain>, where the slug is derived from the branch name.")),
		// The control home is deliberately NOT configurable here. This file
		// is committed and shared; a per-machine path belongs in the
		// environment.
		shape.F("label-namespace", shape.String().
			Regex(brand.LabelNamespacePattern, "must look like a reverse domain, e.g. dev.example").
			Optional().
			Describe("Docker label namespace, as a reverse domain such as dev.example. Only set this to adopt resources created by a previous tool; new projects should leave it unset.")),
	).Strict().Describe("Identity of the project.")

	compose := shape.Object(
		shape.F("file", shape.String().Min(1).
			Describe("Path to the Compose file, relative to this file.")),
	).Strict().Describe("The Compose project each worktree runs.")

	return shape.Object(
		shape.F("project", project),
		shape.F("compose", compose),
		// maxServiceEntries (#16): every declared service is now recorded on
		// the checkout index's card, whether or not it opted into an index
		// table (internal/checkoutindex/card's own maxServices, which
		// bounds the card's services array the same way) — so the config
		// itself must not declare more than a card can hold.
		shape.F("service", keyedTables(nameKey("The Compose service name"), serviceSchema(), "services", "service",
			maxServiceEntries,
			fmt.Sprintf("at most %d [service.<name>] tables; the checkout index's card can name at most that many declared services", maxServiceEntries)).
			Describe("Services to route and index, one [service.<name>] table each, named by the Compose service. Services not listed here still run; they just are not routed or indexed.")),
		shape.F("hooks", hooksSchema()),
		shape.F("commands", commandsSchema()),
	).Strict().SuperRefine(routeCollisionCheck).
		Describe("berth.toml: the committed configuration of a project. Unknown keys are rejected, so a typo cannot silently disable a feature.")
}

// routeCollisionCheck is a cross-table check of rootSchema: no two services
// may claim the same route — a route being a subdomain and path together, so
// two services may share a host if their paths differ. The message names the
// service that got there first. (Two services cannot share a name either,
// but TOML itself refuses a table defined twice.)
func routeCollisionCheck(value *shape.Obj, addIssue func(path []string, message string)) {
	raw, _ := value.Get("service")
	services, _ := raw.(*shape.Obj)
	if services == nil {
		return
	}

	claimedBy := make(map[string]string, services.Len())
	for _, name := range services.Keys() {
		item, _ := services.Get(name)
		service, ok := item.(*shape.Obj)
		if !ok {
			continue
		}
		subdomain, _ := stringField(service, "subdomain")
		path, hasPath := stringField(service, "path")
		route := subdomain + "\x00" + path
		if first, taken := claimedBy[route]; taken {
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
			addIssue([]string{"service", name, field}, fmt.Sprintf("%s is already claimed by service %q", where, first))
			continue
		}
		claimedBy[route] = name
	}
}

// stringField reads key from o as a string, reporting whether it was
// present at all; a present-but-wrong-typed value reads as "", true, since
// the schema itself already refused that shape before routeCollisionCheck
// ever runs.
func stringField(o *shape.Obj, key string) (string, bool) {
	raw, ok := o.Get(key)
	if !ok {
		return "", false
	}
	s, _ := raw.(string)
	return s, true
}

// SchemaJSON is the JSON Schema (draft-07) of the configuration file's
// input shape, generated from the same schema definitions Parse validates
// with. schema/berth.schema.json is a committed copy; a test fails when the
// two differ.
func SchemaJSON() []byte {
	return shape.JSONSchema(rootSchema(), SchemaID, brand.ConfigFile)
}
