## What and why

<!-- What this changes and why. Link the issue: "Closes #123". -->

## How it was checked

<!-- The checks you ran (see "Go" in CONTRIBUTING.md) and what they printed.
Say if you ran the Docker integration suites. -->

## Checklist

- [ ] One concern per pull request, with a conventional-commit title (`feat(runtime): …`, `fix(locks): …`, `docs: …`).
- [ ] Anything that removes a resource proves it owns it by its labels, never by its name.
- [ ] No storage name in `internal/brand` changed without a migration.
- [ ] The README, `docs/` and `CONTRIBUTING.md` match any behaviour this changes.
- [ ] A golden file under `internal/cli/testdata` that changed is a reviewed, intended output change.
