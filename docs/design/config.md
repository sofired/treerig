# Configuration design

How `berth.toml` is shaped, and how its JSON Schema and examples relate to
the code that validates it. The file is half of berth's public contract (the
other half is the hook protocol), so each choice here is a compatibility
promise.

## Named things are tables keyed by name

`service`, `commands` and `shared.service` are tables whose keys are the names:

```toml
[service.api]
subdomain = "api"

[commands.plan]
command = "./scripts/plan.sh"
```

Why not an array of tables with a `name` field:

- **A sub-table attaches silently.** With `[[service]]`, a following
  `[service.index]` belongs to whichever `[[service]]` came last. Nothing in
  the file says so, and moving a block changes what it means. With
  `[service.api.index]` the owner is in the header.
- **TOML enforces uniqueness.** A table defined twice is a syntax error, so
  there is no duplicate-name check to write or to get wrong.
- **It matches what people already know.** Compose's `services:` is a map
  keyed by name, and `[commands]` is keyed the same way; one shape for every
  named thing means one thing to learn.

The parsed table keeps the file's order, so lists built from it (`config
show`, `urls`, the checkout index) follow the file. Name keys share one
definition (`nameKey` in `internal/config/schema.go`: lower-case letters,
digits and hyphens, at most 63 characters), and so do the tables
(`keyedTables`, and `decodeKeyed` to read them back). A new named table uses
both. Checks that span entries, such as two services claiming one route, are
separate `SuperRefine`s on the root.

Writing the array form gets the error `services are tables keyed by name:
write [service.<name>], not [[service]]`; `arrayOfTablesHint` builds that
sentence for any table.

## Hook options are a sibling table

A hook is a command line under `[hooks]`, keyed by its stage, and
`[hooks.timeout]` holds its time limit beside it, keyed the same way:

```toml
[hooks]
prepare = "./scripts/migrate.sh"

[hooks.timeout]
prepare = "30m"
```

`[commands]` takes the other shape, a string or a table with the command and
its options, because a command is a named thing with several options of its
own. A hook is a fixed slot with one option so far. The sibling table keeps
`prepare` a string everywhere it is read (`config show`, the resolution
checks, the runner), so a hook that has no option reads exactly as it did, and
a second option would add a second sibling table, not change a type. Its keys
are every hook kind, and an unset key means no limit. There is deliberately no
default for any hook: berth cannot know how long a project's migration takes,
a default that kills a healthy slow one midway is worse than the hang, and a
default added later would break projects that rely on none, while one never
added breaks nobody (docs/hooks.md, "Timeouts"). The runner applies the limit
in `hooks.RunHook`, so every hook kind gets it from that one place.

## The JSON Schema

`berth config schema` prints a draft-07 JSON Schema of the file, and
`schema/berth.schema.json` is the committed copy.

- **One definition.** The schema is generated from the `internal/shape`
  definitions that validate the file (`shape.JSONSchema`), so keys, types,
  required fields, defaults and the patterns and limits on strings, numbers,
  arrays and tables cannot drift from validation. A test fails when the
  committed file differs from the generated output and says how to
  regenerate it.
- **Descriptions are mandatory.** Every field, table, union alternative and
  key name has a description; editors show them on hover. A test fails on a
  node without one. Write them for someone reading a single key.
- **Identity.** `$id` is the raw GitHub URL of the file on `main`
  (`config.SchemaID`). The published copy therefore tracks `main`;
  `berth config schema > berth.schema.json` pins it to the installed version.
  The README tells users both.
- **Code-only checks.** Some validation is code, which JSON Schema cannot
  state: a `Refine` on a string, a `SuperRefine` on a table (two services
  claiming a route), and `StartIssues` (a control character in an index
  label, checked only by commands that start something). The schema is looser
  than validation exactly there, never stricter, and says nothing about them;
  `berth config validate` makes those checks. `shape.CodeOnlyChecks` lists the
  first two kinds and a test pins the list, so adding one is a visible choice.
  A `Refine` whose rule is a pattern can be restated with `SchemaPattern`.
- **String lengths count differently.** berth counts a string's length in
  UTF-16 code units; JSON Schema's `minLength` and `maxLength` count code
  points. A string with characters outside the Basic Multilingual Plane (an
  emoji, say) can therefore pass the published `maxLength` and still exceed
  berth's limit, so the schema is looser there too. Every `Min` in the config
  is 1, where the two counts agree, so the schema is never stricter.
- **Patterns are ECMA-262.** Go's regexp is RE2; editors run the pattern as
  ECMA-262. A pattern must mean the same in both, and the generator panics on
  the RE2-only spellings it can recognize. A closing `$` is the absolute end
  of the value in both, but a validator built on another engine (Python's
  `re`, say) also lets `$` match before a final newline, so it can accept a
  value such as `"acme\n"` that `berth config validate` rejects. That only
  makes such a validator looser; the patterns are left as they are, since the
  lookahead that would close it is not supported by Taplo's regex engine.
- **Agreement is tested.** A test compiles the committed schema with an
  independent JSON Schema library (`github.com/santhosh-tekuri/jsonschema/v6`,
  which is why it is in `go.mod`; nothing outside tests imports it) and checks
  it reaches the same verdict as `config.Parse` on a table of documents,
  allowing the schema to be looser only where the code-only checks and the
  string-length difference are.

## TOML 1.0 in examples

berth's parser (BurntSushi/toml) accepts TOML 1.1, but Taplo and Python's
`tomllib` read 1.0. So `berth.example.toml`, the README and the docs use 1.0
syntax only: no multi-line inline tables, no trailing comma in an inline
table, no `\e` or `\xHH` escapes. A test scans them for those; it is
best-effort and does not catch every 1.1 addition (times without seconds,
say). A README or docs fence written as ```` ```toml invalid ```` is exempt
from the must-parse check, for a block that shows a mistake or elides part of
a file, but not from the syntax scan.

## The starter file

`berth init` writes a starter `berth.toml` at the Git toplevel of the
checkout it runs in. The text is a template in `internal/config/starter.go`,
filled with a project name taken from the directory (`config.StarterName`) and
the Compose file found at the root, `compose.yaml` if none.

- **A template, not a copy of the example.** It is a few lines of live
  configuration and two commented-out blocks (a routed service, hooks). Each
  of its comment lines is a line of `berth.example.toml`, verbatim (or a line
  of the example's TOML commented out), apart from a short list of
  starter-only lines in `starter_test.go`; `TestStarter` fails on any other, so
  what it says cannot drift from the example. The `#:schema` directive on its
  first line, the same one the example opens with and built from the same
  `config.SchemaID`, gives editors each field's rules as hover text, and a
  comment points at the annotated example for the rest. The test also parses
  it as written, parses it again with the commented-out blocks uncommented,
  and scans it for TOML 1.1 syntax, so it cannot drift into keys the schema no
  longer has.
- **Never replaces.** `files.WriteNew` creates the file with `O_EXCL`, so a
  file or symlink that is already there, or one a racing `init` creates
  meanwhile, makes it fail with `fs.ErrExist` and nothing else changes. The
  file is written in place, not renamed into position, so there is a short
  window in which a reader can see it incomplete, and a failed write removes
  it. A kill or power loss inside the window leaves a truncated `berth.toml`,
  which `init` then refuses to replace as it would any other: delete it and
  run `init` again. Writing a temp file instead would trade that for a stray
  `berth.toml.tmp.*` a kill leaves in the repository root, where `git add -A`
  would commit it. In a submodule it writes into the submodule, its own
  checkout.
- **No YAML.** The Compose file is only named, never read: berth has no YAML
  parser, and `config validate` after `init` should not depend on one. The
  starter's service is a placeholder to rename.
- **`--config` is refused.** `init` reads no configuration; accepting a path
  it then ignored would hide a mistake.
