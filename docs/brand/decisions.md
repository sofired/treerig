# Brand decisions

How the name and the mark were chosen, so later changes can build on what was
tried. Recorded in October 2026.

## The name

**Why the previous name was dropped.** By late 2026 at least two other
open-source tools used the same name for the same job: isolated environments
for Git worktrees, one built on Docker Compose. Each installs a command with
that name, so a user could end up with two on `PATH`. The name was also taken
on crates.io and as an npm scope by an unrelated tool, and every short domain
for it was registered.

**What was screened.** Well over 150 candidates were checked on npm, PyPI,
crates.io and by DNS for .dev, .sh, .com, .tools and .build, then searched for
existing tools. Most short English harbour words were taken on every registry.
Foreign-language words, coined words and tree-prefixed compounds fared better.
The main finalists and why they lost:

- **treehold** (`th`): a good meaning (a holding, as in freehold), but `th` was
  the old Torch7 interpreter.
- **treenv** (`tnv`): free everywhere and descriptive, but its creator typed
  it as "treeenv" twice in a row on first try. The doubled letter at the seam
  is a permanent typo trap.
- **moord**, **noust**, **moorhold**: free, but obscure or awkward to type.
- **treekeeper** (TreeKeeper® is an established tree-inventory software
  product), **treewright** and **treehouse** (existing worktree tools),
  **treewarden** (an existing app), **treeland** (a Linux compositor): ruled
  out.

**treerig** won because:

- it was free on every channel checked, .com included;
- it avoids the doubled-letter typo;
- it carries two meanings: a dev rig (a full working setup) and a ship's
  rigging, since masts were once single trees;
- `trig` was clear as a short command.

Distribution is a GitHub release, an install script and a Homebrew tap, so the
names that matter are the command on `PATH`, the Homebrew formula and the
domain. Package-registry names don't.

## Competitive landscape

Worktree environment tools appeared quickly in 2026, among them worktree-compose,
workz, git-treeline, a herdr plugin, and a Vercel tool for routed local URLs
(portless). As of October 2026 none combined a Compose project per worktree,
a shared hostname router, ownership-verified teardown and durable identity.
That combination, and above all "never removes what it cannot prove it owns",
is what sets treerig apart.

## The mark

More than a dozen rounds of marks were drawn. In order:

1. A harbour plan, from the previous name.
2. Abstract holdings and git forks.
3. Living trees with a git graph inside.
4. Rigged trees and git-graph sailboats.
5. Sailboats rigged with branch diagrams.
6. Solid-sail boats.
7. Split marks.

What the rounds taught:

- **Draw at 16 px first.** Thin lines and rings that look fine large blur at
  16 px. Every candidate was checked at true pixel sizes, and the final mark
  has a pixel-built 16 px file.
- **Test with fresh eyes.** The sailboat marks passed every self-review but
  failed a five-second test with someone seeing them for the first time: the
  boat wasn't obvious. Run that test before adopting any change to the mark.
- **One meaning for red.** Red marks "yours" and nothing else, across every
  round.

**Where the final idea came from.** The Zed editor draws a worktree as one path
splitting into separate headings. treerig's mark takes that idea, not Zed's
drawing:

- `main` starts at a commit and runs on;
- your branch breaks away at the split, in red, with a clear gap, because a
  worktree is your branch in a space of its own.

Of the variations, the one with your branch as the detached one was chosen.
Its geometry was then left exactly as approved; the polish went into
centring and the 16 px file.
