package config

import (
	"strconv"
	"strings"
)

// starterMaxName bounds a derived project name well inside the 63-character
// DNS label that <name>.localhost, the starter's domain, has to fit.
const starterMaxName = 40

// starterFallbackName is the project name for a directory whose own name has
// no letter or digit to derive one from.
const starterFallbackName = "my-project"

// StarterName derives a valid [project] name from a directory's base name:
// lower-case ASCII letters and digits, with every other run of characters
// becoming one hyphen, trimmed of hyphens at both ends and cut to
// starterMaxName. A directory that leaves nothing gets "my-project".
func StarterName(directory string) string {
	var b strings.Builder
	pendingHyphen := false
	for _, r := range strings.ToLower(directory) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if pendingHyphen && b.Len() > 0 {
				b.WriteByte('-')
			}
			pendingHyphen = false
			b.WriteRune(r)
			continue
		}
		pendingHyphen = true
	}
	name := b.String()
	if len(name) > starterMaxName {
		name = strings.TrimRight(name[:starterMaxName], "-")
	}
	if name == "" {
		return starterFallbackName
	}
	return name
}

// Starter is the berth.toml `init` writes: a minimal working file for the
// project called name, whose Compose file is composeFile (relative to the
// file), with the two things most projects add next, a routed service and
// hooks, left as commented-out examples.
//
// It is valid TOML 1.0 and parses as written when name is valid (use
// StarterName); its first line is the same Taplo `#:schema` directive
// berth.example.toml starts with, built from SchemaID. Its comments are kept
// to lines that are also in berth.example.toml, plus a few starter-only
// lines, so what it says cannot drift from the annotated reference: the JSON
// Schema the directive points at carries each field's rules as hover text.
// starter_test.go holds every comment line to that, and the commented-out
// examples to the schema.
func Starter(name, composeFile string) []byte {
	return []byte(strings.NewReplacer(
		"{{schema}}", SchemaID,
		"{{example-url}}", exampleFileURL,
		"{{name}}", strconv.Quote(name),
		"{{domain}}", strconv.Quote(name+".localhost"),
		"{{compose}}", strconv.Quote(composeFile),
	).Replace(starterTemplate))
}

// starterTemplate is Starter's text. Every comment line is either copied
// verbatim from berth.example.toml (a prose comment, or a line of its TOML
// with "# " in front) or listed in starter_test.go's starterOnly.
const starterTemplate = `#:schema {{schema}}
# berth.toml — commit this at the root of your repository.
#
# The line above points TOML editors (Even Better TOML/Taplo, or any editor
# that takes a JSON Schema for TOML) at berth's JSON Schema, for completion,
# hover text and unknown-key warnings.
# Unknown keys are rejected so a typo cannot silently disable a feature.
#
# The annotated reference, with every option, is berth.example.toml:
#   {{example-url}}

[project]
# Lower-case letters, digits, and hyphens.
name = {{name}}

# Every worktree is routed as <slug>.<domain>, where the slug is derived from
# the branch name: feature/login becomes feature-login.acme.localhost.
# A shared parent domain lets password managers reuse a local credential
# across every environment.
domain = {{domain}}

[compose]
file = {{compose}}

# Services. Each [service.<name>] table names a Compose service and says how
# it is reached. The name is lower-case letters, digits and hyphens.
# Services not listed here still run; they just are not routed or indexed.
#
# Placeholder: rename api to a service from your Compose file.
# [service.api]
# subdomain = ""        # empty (the default) routes at the worktree host root
#
# [service.api.index]    # presence on the shared checkout index; omit to hide
# label   = "API"
# purpose = "REST surface"

# [hooks]
# Services are up and healthy; nothing has been seeded. Migrations go here.
# prepare = "./scripts/migrate.sh"
#
# Runs only after ` + "`prepare`" + ` exits zero.
# seed = "./scripts/seed.py"
`
