# Configuration design

How `berth.toml` is shaped, and how its JSON Schema and examples relate to
the code that validates it. The file is half of berth's public contract (the
other half is the hook protocol), so each choice here is a compatibility
promise.

## Named things are tables keyed by name

`service` and `commands` are tables whose keys are the names:

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
- **Patterns are ECMA-262.** Go's regexp is RE2; editors run the pattern as
  ECMA-262. A pattern must mean the same in both, and the generator panics on
  the RE2-only spellings it can recognize.
- **Agreement is tested.** A test compiles the committed schema with an
  independent JSON Schema library (`github.com/santhosh-tekuri/jsonschema/v6`, which is why it is in `go.mod`; nothing outside tests imports it) and checks it reaches the same verdict as
  `config.Parse` on a table of documents, allowing the schema to be looser
  only where the code-only checks are.

## TOML 1.0 in examples

berth's parser (BurntSushi/toml) accepts TOML 1.1, but Taplo and Python's
`tomllib` read 1.0. So `berth.example.toml`, the README and the docs use 1.0
syntax only: no multi-line inline tables, no trailing comma in an inline
table, no `\e` or `\xHH` escapes. A test scans them for those; it is
best-effort and does not catch every 1.1 addition (times without seconds,
say). A README or docs fence written as ```` ```toml invalid ```` is exempt
from the must-parse check, for a block that shows a mistake or elides part of
a file, but not from the syntax scan.
