// Oracle for the claim that `list` and the checkout index can sort with Go's
// plain byte-order string comparison instead of `localeCompare(…, 'en')`,
// which is what the TypeScript build actually sorts with (worktree-registry.ts's
// WorktreeStore.list, commands/list.ts, checkout-index/snapshot.ts). The claim
// only needs to hold for the alphabet these fields are restricted to: slugs
// match `SLUG` and domain labels match `HOSTNAME`, both `[a-z0-9.-]`.
//
// Regenerate with:
//
//   export PATH=$HOME/.local/opt/node-v24.14.1-linux-x64/bin:$PATH
//   node tools/oracle/locale-sort.ts > internal/registry/testdata/locale-sort.json
const corpus = [
  'a', 'a-1', 'a1', 'acme', 'widgets', 'z9', '9z', '9-a',
  'feature-login', 'feature-login-1d407e139915',
  'feature.example', 'feature-example',
  '0', '00', '0-0', 'a.b', 'a-b', 'a.a', '--', '-a', 'a-',
  'main', 'dev', 'staging', 'x', 'xx', 'x-x', 'x.x',
  'localhost', 'acme.localhost', 'widgets.localhost', 'a.localhost', 'ab.localhost'
];

const byLocale = [...corpus].sort((a, b) => a.localeCompare(b, 'en'));

process.stdout.write(JSON.stringify({ input: corpus, sorted: byLocale }, null, 2) + '\n');
