package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
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
	// checkout index card, not only the ones with a [service.<name>.index]
	// table, so the config's own [service] table must not outgrow what the
	// card can hold. Hardcoded rather than imported from that package: config is
	// foundational and every command depends on it, while the checkout
	// index is one feature among several that read it — the dependency
	// belongs pointing the other way. A deliberate limit of its own.
	maxServiceEntries = 64
	// maxSharedPorts is how many ports one shared service may declare, its
	// routed port and its raw ports together (docs/design/routing.md,
	// decision 19): each gets a pinned host port, so the count is bounded.
	maxSharedPorts = 16
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

// sharedPortSchema is one container port of a shared service.
func sharedPortSchema() *shape.NumberSchema {
	return shape.Number().Int().Min(1).Max(65_535)
}

// sharedServiceSchema is one [shared.service.<name>] table. The name is the
// table's key, as for [service.<name>].
func sharedServiceSchema() *shape.ObjectSchema {
	return shape.Object(
		shape.F("port", sharedPortSchema().Optional().
			Describe("Container port of the service's web UI, published on a pinned loopback port and routed at <name>.shared.<domain> (BERTH_SHARED_URL_<NAME> is its URL; BERTH_SHARED_ADDR_<NAME>_<PORT> names the pinned address). Omit if the service has no web UI.")),
		shape.F("ports", shape.Array(sharedPortSchema().Describe("A container TCP port, 1 to 65535.")).
			Max(maxSharedPorts, fmt.Sprintf("at most %d ports per shared service, port and ports together", maxSharedPorts)).
			Default([]any{}).
			Describe("Container ports published on pinned loopback ports, for processes on the host (an IDE-run app, `berth run`): BERTH_SHARED_ADDR_<NAME>_<PORT> names each address. Containers reach the service at <name>:<port> instead. At most 16 with port.")),
		shape.F("used-by", shape.Array(nameKey("A service of the worktree's Compose file")).
			Default([]any{}).
			Describe("Services of the worktree's Compose file that use this shared service. A worktree whose Compose file has any of them starts the shared service, and the shared service joins the networks they are on, answering to its name as an alias.")),
	).Strict().
		KeyHint("name", func(path []string) string {
			return fmt.Sprintf("the table's key is the service name: [%s]", shape.PathString(path))
		}).
		Describe("One shared service: a service of the shared Compose file that runs once per repository. The table's key is its Compose service name, and the name containers reach it by.")
}

// sharedSchema is the [shared] table.
func sharedSchema() *shape.ObjectSchema {
	return shape.Object(
		shape.F("file", shape.String().Min(1).Optional().
			Describe("Path to the shared services' Compose file, relative to this file. It is its own Compose project, run once per repository, and may not build images or bind-mount paths inside a worktree. Required when any [shared.service.<name>] table is declared.")),
		shape.F("service", keyedTables(nameKey("The shared Compose service name"), sharedServiceSchema(), "shared services", "shared.service", 0, "").
			Describe("Shared services, one [shared.service.<name>] table each, named by the service in the shared Compose file.")),
	).Strict().Optional().
		Describe("Services that run once per repository and are shared by all of its worktrees, such as an email catcher.")
}

// hookTimeoutSchema is one entry of [hooks.timeout]: a hook's time limit.
func hookTimeoutSchema(stage string) *shape.StringSchema {
	return shape.String().
		SchemaPattern(durationPattern).
		Refine(func(value string) bool { _, ok := parseHookTimeout(value); return ok },
			"must be a duration greater than zero and at most 24h, such as 30s, 10m or 1h30m").
		Optional().
		Describe("How long the " + stage + " hook may run before berth stops it, with its whole process group, and fails the command: a duration such as 30s, 10m or 1h30m, greater than zero and at most 24h. Unset means no limit.")
}

// hookTimeoutsSchema is the [hooks.timeout] table: one optional limit per
// hook, keyed by the hook's name. It lists every hook kind, backup and
// restore included, and hooks.RunHook applies it to whichever hook it runs.
func hookTimeoutsSchema() *shape.ObjectSchema {
	fields := make([]shape.Field, 0, len(hookStages))
	for _, stage := range hookStages {
		fields = append(fields, shape.F(string(stage), hookTimeoutSchema(string(stage))))
	}
	return shape.Object(fields...).Strict().Optional().
		Describe("Time limits for the hooks, keyed by hook name. A hook that runs past its limit is stopped, its whole process group with it, and reported as timed out (exit 124). A hook with no entry has no limit. See docs/hooks.md.")
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
		shape.F("timeout", hookTimeoutsSchema()),
	).Strict().Default(shape.NewObj()).
		Describe("Commands berth runs at points in a worktree's life. Each is resolved relative to this file when it starts with \".\" or contains \"/\", and looked up on PATH otherwise. See docs/hooks.md.")
}

// durationPattern is the spelling of a duration in the config (a [commands]
// kill-after, a [hooks.timeout] entry): one or more whole numbers each with a
// unit (ms, s, m or h), as "30s", "5m" or "1m30s". No sign and no fraction,
// so what time.ParseDuration then reads is exactly what was written.
var durationPattern = regexp.MustCompile(`^([0-9]+(ms|s|m|h))+$`)

// maxKillAfter caps a [commands] kill-after: an hour is far beyond any
// graceful stop, and the cap keeps the runner's wait for a stopped command
// (its kill-after plus a margin) from overflowing a time.Duration.
const maxKillAfter = time.Hour

// maxHookTimeout caps a [hooks.timeout] entry. It is a sanity bound, not
// derived from anything: a day is longer than any hook should run, and the
// cap keeps the runner's timer well inside a time.Duration.
const maxHookTimeout = 24 * time.Hour

// parseDuration reads a duration spelling, reporting false unless it is well
// formed, greater than zero and at most limit.
func parseDuration(value string, limit time.Duration) (time.Duration, bool) {
	if !durationPattern.MatchString(value) {
		return 0, false
	}
	d, err := time.ParseDuration(value)
	return d, err == nil && d > 0 && d <= limit
}

// parseKillAfter reads a kill-after spelling, reporting false unless it is
// well formed, greater than zero and at most maxKillAfter.
func parseKillAfter(value string) (time.Duration, bool) {
	return parseDuration(value, maxKillAfter)
}

// parseHookTimeout reads a [hooks.timeout] spelling, reporting false unless
// it is well formed, greater than zero and at most maxHookTimeout.
func parseHookTimeout(value string) (time.Duration, bool) {
	return parseDuration(value, maxHookTimeout)
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
			SchemaPattern(durationPattern).
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

// rootSchema is the whole config file. root is the directory holding the file
// being checked, which the relative paths in it resolve from; the published
// JSON Schema has no file and passes "".
func rootSchema(root string) *shape.ObjectSchema {
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
		shape.F("shared", sharedSchema()),
		shape.F("hooks", hooksSchema()),
		shape.F("commands", commandsSchema()),
	).Strict().SuperRefine(routeCollisionCheck).SuperRefine(func(value *shape.Obj, addIssue func(path []string, message string)) {
		sharedChecks(value, root, addIssue)
	}).
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

// sharedChecks is a cross-table check of rootSchema for [shared]: the file
// the services live in, and what each service claims against its siblings
// and against the worktree's own services. Everything here needs more than
// one value at once, so none of it is in the published JSON Schema.
func sharedChecks(value *shape.Obj, root string, addIssue func(path []string, message string)) {
	raw, _ := value.Get("shared")
	shared, _ := raw.(*shape.Obj)
	if shared == nil {
		return
	}
	rawServices, _ := shared.Get("service")
	services, _ := rawServices.(*shape.Obj)
	if services == nil || services.Len() == 0 {
		return
	}

	file, hasFile := stringField(shared, "file")
	if !hasFile {
		addIssue([]string{"shared", "file"}, "required when any [shared.service.<name>] table is declared")
	} else if compose, ok := composeFile(value); ok && resolveFrom(root, file) == resolveFrom(root, compose) {
		addIssue([]string{"shared", "file"}, "must not be the [compose] file: shared services run in their own Compose project, from their own file")
	}

	own := map[string]bool{}
	if rawOwn, ok := value.Get("service"); ok {
		if table, ok := rawOwn.(*shape.Obj); ok {
			for _, name := range table.Keys() {
				own[name] = true
			}
		}
	}

	for _, name := range services.Keys() {
		item, _ := services.Get(name)
		service, ok := item.(*shape.Obj)
		if !ok {
			continue
		}
		at := []string{"shared", "service", name}
		if own[name] {
			addIssue(at, fmt.Sprintf("%q is also a [service.%s] table: a shared service's name is the alias containers reach it by, so it cannot name one of the worktree's own services", name, name))
		}

		declared := map[int]bool{}
		count := 0
		if port, ok := intField(service, "port"); ok {
			declared[port] = true
			count++
		}
		for i, port := range intsField(service, "ports") {
			if declared[port] {
				addIssue(append(slices.Clone(at), "ports", strconv.Itoa(i)), fmt.Sprintf("port %d is listed more than once for this service", port))
				continue
			}
			declared[port] = true
			count++
		}
		if count > maxSharedPorts {
			addIssue(append(slices.Clone(at), "ports"), fmt.Sprintf("at most %d ports per shared service, port and ports together; this declares %d", maxSharedPorts, count))
		}

		seen := map[string]bool{}
		for i, use := range stringsField(service, "used-by") {
			where := append(slices.Clone(at), "used-by", strconv.Itoa(i))
			switch {
			case seen[use]:
				addIssue(where, fmt.Sprintf("service %q is listed twice", use))
			case hasKey(services, use):
				addIssue(where, fmt.Sprintf("%q is a shared service: used-by lists the worktree's own Compose services, and a shared service cannot use another", use))
			}
			seen[use] = true
		}
	}
}

// hasKey reports whether o has key.
func hasKey(o *shape.Obj, key string) bool {
	_, ok := o.Get(key)
	return ok
}

// resolveFrom is path as the file in root names it: absolute as it is,
// relative from root, either way cleaned. Two spellings of one file therefore
// compare equal, whichever way each was written.
func resolveFrom(root, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(root, path)
}

// composeFile is [compose] file as written, when the table and key are there.
func composeFile(root *shape.Obj) (string, bool) {
	raw, _ := root.Get("compose")
	table, _ := raw.(*shape.Obj)
	if table == nil {
		return "", false
	}
	return stringField(table, "file")
}

// intField reads key from o as a whole number the schema has already vetted.
func intField(o *shape.Obj, key string) (int, bool) {
	raw, ok := o.Get(key)
	if !ok {
		return 0, false
	}
	n, ok := raw.(int)
	return n, ok
}

// intsField reads key from o as a list of whole numbers the schema has
// already vetted; absent reads as none.
func intsField(o *shape.Obj, key string) []int {
	raw, _ := o.Get(key)
	list, _ := raw.([]any)
	out := make([]int, 0, len(list))
	for _, item := range list {
		if n, ok := item.(int); ok {
			out = append(out, n)
		}
	}
	return out
}

// stringsField reads key from o as a list of strings the schema has already
// vetted; absent reads as none.
func stringsField(o *shape.Obj, key string) []string {
	raw, _ := o.Get(key)
	list, _ := raw.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
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
	return shape.JSONSchema(rootSchema(""), SchemaID, brand.ConfigFile)
}
