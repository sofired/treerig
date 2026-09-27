# Porting process

This document is deleted when TypeScript is retired (#46). Until then, it
governs how the Go port of berth proceeds.

## How a work package runs

Each work package (#38–#46) is done on its own branch, named
`port/wpN-*`. It is opened as a draft pull request against `main` early, and
goes through a four-role review loop — platform, DevOps, Go/backend, and
frontend — before the owner merges it. A round of review either finds
something substantive, which gets fixed and re-reviewed, or finds nothing
further, which ends the loop.

## Parity rules

- **The TypeScript source under `src/` is the specification**, for as long
  as it exists. Go ports behaviour and every user-facing string exactly:
  messages, help, tables, JSON. Where Go cannot match it, the difference
  goes in the divergence ledger (below), not left silently.
- **Test names carry over.** Each TypeScript `it` becomes a Go
  `t.Run("<the exact it title>", …)`, so coverage can be compared title by
  title against the TypeScript suite.
- **Oracles.** Node scripts in `tools/oracle/*.ts` import from `../../src/...`
  and write expected data into the matching Go package's `testdata/`. The
  generated data is committed, so Go tests read it without needing Node; each
  script's header comment says how to regenerate it.
- **Live parity.** `internal/cli/parity_integration_test.go`
  (`TestParityWithTheTypeScriptBuild`) compares the Go build's stdout,
  stderr, exit code and every `docker` call against goldens committed under
  `internal/cli/testdata/parity/` by default, needing no Node to run. Only
  `-update` runs the TypeScript build live, to re-record those goldens; CI's
  oracle-drift job does this on every pull request and fails on any diff, so
  a change to the TypeScript build's own behaviour cannot silently drift
  from what is committed.
- **The streams contract.** `tests/cli/streams.test.ts` runs against either
  build: `BERTH_CLI` points it at an executable. Every verb is ported, so
  no case skips for the Go build. `internal/cli/streams_integration_test.go`
  (`TestStreamsContract`) is the same contract as a `-tags integration` Go
  test, against a binary it builds itself, kept in step with the TypeScript
  file by hand until #46 deletes it.

## Divergence ledger

Any accepted difference from TypeScript's behaviour is recorded in
[`divergences.md`](divergences.md): what changed, why, and which test pins
it. New entries need the owner's approval, given per pull request.

## The TypeScript freeze

Until TypeScript is retired (#46), `src/` takes fixes only — no new
features. Every fix is mirrored in the Go build, in the same pull request or
a follow-up shortly after, so the two builds never drift apart on behaviour
that both still claim to have.

## One machine, one build

The TypeScript and Go builds' lock mechanisms — a heartbeat-and-directory
design versus `flock(2)` — do not exclude each other. Running both against
the same worktree on the same machine at the same time is unsafe and
untested; do not do it.
