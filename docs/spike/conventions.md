# Go spike: working conventions

This is the working agreement for everyone porting berth to Go in the spike.
The spike plan's intended outcome is a decision report. Everything not marked
`// SPIKE:` is written to merge quality.

## Toolchain

- **Go 1.26:** `export PATH=/usr/local/go/bin:$PATH`.
- **Node 24,** to run the TypeScript build as the oracle: `export PATH=$HOME/.local/opt/node-v24.14.1-linux-x64/bin:$PATH`, then `node src/cli/main.ts …`. `node_modules` is installed.
- **Module:** `github.com/sofired/berth`. Do **not** edit `go.mod` or `go.sum`, and do not run `go get` or `go mod tidy`.
- **Allowed dependencies:** the standard library, `github.com/BurntSushi/toml`, `golang.org/x/sys/unix`, and in tests `github.com/google/go-cmp/cmp` and `github.com/rogpeppe/go-internal/testscript`. If you need anything else, stop and say so.
- **Shell guard:** in this environment, Bash commands containing "git" or "github" in a heredoc or compound form are refused. Write files with the Write/Edit tools, and keep Bash commands plain.

## Ownership

- Edit only the packages and files you were assigned.
- **Coordinator-owned:**
  - `internal/brand`, `internal/text`, `internal/clierr`, `internal/logger`, `internal/layout`
  - `internal/run/run.go`, `internal/run/fakerun`
  - `internal/config/config.go` (the types)
  - `internal/commands/env.go`
  - `go.mod`, `cmd/berth` (until the CLI agent takes it)

  If you need a change there, put a helper in your own package, or describe the change in your final report.
- **Tests for a coordinator-owned package** go in a `_test.go` file, only when your assignment says so.
- **No git commits.** The coordinator commits.
- **Testing your own packages:** `go build ./...`, `go vet ./internal/<yours>/...`, `go test ./internal/<yours>/...`, and `gofmt -l` on your files. Other agents are editing other packages at the same time; if the whole module fails to build because of someone else's package, say so rather than touching it.

## Porting rules

- **The TypeScript source under `src/` is the specification.** Port behaviour and every user-facing string exactly: messages, help, tables, JSON. Where Go cannot match, write it down as a candidate "accepted difference" in your final report, with the reason.
- **Test names:** each ported TypeScript `it` becomes `t.Run("<the exact it title>", …)`, so coverage can be compared title by title.
- **Oracle data:**
  - Node scripts in `tools/oracle/<name>.ts` import from `../../src/...`. They write expected data into the Go package's `testdata/`.
  - Commit the generated data; Go tests read it, so running them does not need Node.
  - Each script's header comment says how to regenerate its data.
- **Comments:** match the TypeScript code's comment density and voice. Explain why, never restate the code.
- **Mark spike-only shortcuts** with `// SPIKE: <what and why>`.
- **Real-process and Docker tests** carry `//go:build integration`. Docker tests skip unless `docker info` works, and fail instead when `BERTH_TEST_REQUIRE_DOCKER=1`.
- **Exit codes and streams:** only `Logger.Output` writes stdout. Everything else goes to stderr as `[name] …`, `[name] WARNING: …` or `[name] ERROR: …`.

## Final report (your last message)

Report on:
- files created;
- what is ported;
- the test counts (Go subtests versus the TypeScript `it` titles covered);
- candidate accepted differences;
- anything you needed from a coordinator-owned package;
- anything unverified, such as macOS behaviour.
