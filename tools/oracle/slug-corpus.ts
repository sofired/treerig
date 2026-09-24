// Oracle for internal/registry's BranchSlug/AllocateSlug port. Regenerate
// with:
//
//   export PATH=$HOME/.local/opt/node-v24.14.1-linux-x64/bin:$PATH
//   node tools/oracle/slug-corpus.ts > internal/registry/testdata/slug-corpus.json
//
// The corpus exists because slug derivation lowercases with JavaScript's
// String.prototype.toLowerCase, and Go's strings.ToLower disagrees with it
// for a handful of code points — most notably İ (U+0130), which JavaScript's
// unconditional special casing turns into "i" followed by a combining dot
// above (U+0307), while Go collapses it to a plain "i". internal/text.JSLower
// exists to reproduce the JavaScript behaviour; this corpus is what proves it
// does, end to end through BranchSlug.
import { branchSlug } from '../../src/worktree-registry.ts';

interface Case {
  name: string;
  branch: string;
  root: string;
}

const cases: Case[] = [
  { name: 'plain branch', branch: 'feature/login', root: '/repo/worktree' },
  { name: 'uppercase ASCII', branch: 'FEATURE/LOGIN', root: '/repo/worktree' },
  // U+0130 LATIN CAPITAL LETTER I WITH DOT ABOVE: JavaScript's toLowerCase
  // gives "i" + U+0307 COMBINING DOT ABOVE; Go's strings.ToLower alone would
  // give a plain "i", losing the combining mark that then becomes its own
  // hyphen once the slug pattern strips it.
  { name: 'Turkish dotted capital I (U+0130)', branch: 'İstanbul', root: '/repo/worktree' },
  { name: 'U+0130 repeated', branch: 'İİİ', root: '/repo/worktree' },
  { name: 'U+0130 amid ASCII', branch: 'feature/İssue-42', root: '/repo/worktree' },
  // Turkish dotless lowercase ı (U+0131) is already lower-case in JavaScript
  // and in Go; included as a control case.
  { name: 'Turkish dotless lowercase i (U+0131) is unaffected', branch: 'ıstanbul', root: '/repo/worktree' },
  // German sharp s: toLowerCase leaves it as-is (it is already lower-case);
  // included as another control case for a non-ASCII letter that survives
  // lowering unchanged in both languages before the slug pattern strips it.
  { name: 'sharp s (already lower-case)', branch: 'straße', root: '/repo/worktree' },
  { name: 'detached HEAD falls back to directory name', branch: '', root: '/work/my-tree' },
  { name: 'detached HEAD with non-ASCII directory name', branch: '', root: '/work/İstanbul' },
  { name: 'nothing survives', branch: '---', root: '/work/---' },
  { name: 'only non-ASCII survives nothing', branch: '日本語', root: '/repo' },
  { name: 'long branch name truncates before trimming', branch: 'a'.repeat(47) + '-b', root: '/repo' },
];

const results = cases.map((c) => ({
  name: c.name,
  branch: c.branch,
  root: c.root,
  slug: branchSlug({ branch: c.branch, root: c.root })
}));

process.stdout.write(JSON.stringify(results, null, 2) + '\n');
