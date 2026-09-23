// Oracle for internal/checkoutindex's HTML and plain-text renderers.
// Regenerate with:
//
//   export PATH=$HOME/.local/opt/node-v24.14.1-linux-x64/bin:$PATH
//   node tools/oracle/index-golden.ts > internal/checkoutindex/testdata/index-golden.json
//
// Each fixture is an IndexSnapshot (built directly, the way the unit tests
// build one, not through buildSnapshot — that has its own oracle-free unit
// tests), rendered through the real renderIndexHtml and formatIndexText. Go
// must reproduce every byte, including whitespace: TestIndexGolden reads
// this file back and fails on any diff.
import { renderIndexHtml, formatIndexText } from '../../src/checkout-index/render.ts';
import { placeholderSnapshot, type IndexEntry, type IndexSnapshot } from '../../src/checkout-index/snapshot.ts';

const INDEX_URL = 'http://berth.localhost:8080';

function entry(fields: Omit<IndexEntry, 'services'> & { services?: IndexEntry['services'] }): IndexEntry {
  return { services: [], ...fields };
}

const fixtures: { name: string; snapshot: IndexSnapshot }[] = [
  {
    name: 'empty',
    snapshot: { entries: [], generatedAt: '2026-09-21T12:00:00.000Z', dockerReachable: true }
  },
  {
    name: 'placeholder',
    snapshot: placeholderSnapshot('1970-01-01T00:00:00.000Z')
  },
  {
    name: 'several-domains-and-projects',
    snapshot: {
      generatedAt: '2026-09-21T12:00:00.000Z',
      dockerReachable: true,
      entries: [
        entry({
          repositoryId: 'r1', worktreeId: 'w1', project: 'acme', domain: 'acme.localhost',
          slug: 'feature-a', host: 'feature-a.acme.localhost', branch: 'feature/a',
          root: '/work/acme/feature-a', status: 'running',
          services: [
            { name: 'api', label: 'API', purpose: 'REST surface', url: 'http://feature-a.acme.localhost:8080', available: true },
            { name: 'mail', label: 'Mailpit', purpose: 'Captured email', url: 'http://mail.feature-a.acme.localhost:8080', available: true }
          ]
        }),
        entry({
          repositoryId: 'r1', worktreeId: 'w2', project: 'acme.localhost', domain: 'acme.localhost',
          slug: 'feature-b', host: 'feature-b.acme.localhost', branch: 'feature/b',
          status: 'partial',
          services: [{ name: 'api', label: 'API', purpose: '', url: 'http://feature-b.acme.localhost:8080', available: false }]
        }),
        entry({
          repositoryId: 'r2', worktreeId: 'w3', project: 'widgets', domain: 'widgets.localhost',
          slug: 'main', host: 'main.widgets.localhost', branch: 'main',
          root: '/work/widgets/main', status: 'stopped'
        }),
        entry({
          repositoryId: 'r3', worktreeId: 'w4', project: 'zeta project', domain: 'a-first.localhost',
          slug: 'z', host: 'z.a-first.localhost', branch: 'z', status: 'running',
          services: [{ name: 'web', label: 'Web', purpose: '', url: 'http://z.a-first.localhost:8080', available: true }]
        })
      ]
    }
  },
  {
    name: 'missing-and-unknown',
    snapshot: {
      generatedAt: '2026-09-21T12:00:00.000Z',
      dockerReachable: false,
      dockerError: '`docker ps` did not answer within 30s',
      identityError: { reason: 'Git could not list the worktrees of /work/acme: fatal: not a git repository', rowsUnknown: true },
      entries: [
        entry({
          repositoryId: 'r1', worktreeId: 'w1', project: 'acme', domain: 'acme.localhost',
          slug: 'gone', host: 'gone.acme.localhost', branch: 'branch/gone',
          root: '/work/last-seen/gone', status: 'missing',
          services: [{ name: 'api', label: 'API', purpose: '', url: 'http://gone.acme.localhost:8080', available: false }]
        }),
        entry({
          repositoryId: 'r1', worktreeId: 'w2', project: 'acme', domain: 'acme.localhost',
          slug: 'feature-a', host: 'feature-a.acme.localhost',
          status: 'unknown',
          services: [{ name: 'api', label: 'API', purpose: '', url: 'http://feature-a.acme.localhost:8080', available: false }]
        })
      ]
    }
  },
  {
    name: 'never-brought-up',
    snapshot: {
      generatedAt: '2026-09-21T12:00:00.000Z',
      dockerReachable: true,
      entries: [
        entry({
          repositoryId: 'r1', worktreeId: 'w1', project: 'acme.localhost', domain: 'acme.localhost',
          slug: 'feature-c', host: 'feature-c.acme.localhost', status: 'stopped'
        })
      ]
    }
  },
  {
    name: 'hostile-names',
    snapshot: {
      generatedAt: '2026-09-21T12:00:00.000Z',
      dockerReachable: true,
      identityError: { reason: "fatal: `--porcelain' is not a git command", rowsUnknown: false },
      entries: [
        entry({
          repositoryId: 'r1', worktreeId: 'w1', project: '<script>alert(1)</script>', domain: 'acme.localhost',
          slug: 'feature-a', host: 'feature-a.acme.localhost',
          branch: `feature/${String.fromCodePoint(0x1b)}[2K${String.fromCodePoint(0x202e)}x${String.fromCodePoint(0x2060)}y`,
          root: '/work/acme/feature-<a>', status: 'partial',
          services: [{ name: 'api', label: 'Cool & "Fun" <API>', purpose: "quote's / amp & lt< gt>", url: 'http://feature-a.acme.localhost:8080/o\'brien', available: true }]
        }),
        entry({
          repositoryId: 'r1', worktreeId: 'w2', project: 'acme.localhost', domain: 'acme.localhost',
          slug: 'detached-one', host: 'detached-one.acme.localhost', branch: '',
          status: 'stopped'
        }),
        entry({
          repositoryId: 'r1', worktreeId: 'w3', project: 'acme.localhost', domain: 'acme.localhost',
          slug: 'no-branch-known', host: 'no-branch-known.acme.localhost',
          status: 'stopped'
        })
      ]
    }
  },
  {
    name: 'long-paths-and-wrapping',
    snapshot: {
      generatedAt: '2026-09-21T12:00:00.000Z',
      dockerReachable: false,
      dockerError: 'the `docker` command is not on PATH (ENOENT); is Docker installed? This sentence is padded well past eighty columns so the notice has to fold across more than one line in the text twin.',
      entries: [
        entry({
          repositoryId: 'r1', worktreeId: 'w1', project: 'acme', domain: 'acme.localhost',
          slug: 'feature-a-really-long-branch-name-that-uses-the-whole-slug-budget',
          host: 'feature-a-really-long-branch-name-that-uses-the-whole-slug-budget.acme.localhost',
          branch: 'feature/a-really-long-branch-name-that-uses-the-whole-slug-budget',
          root: '/work/acme/very/deeply/nested/path/that/is/quite/long/indeed/feature-a-really-long-branch-name-that-uses-the-whole-slug-budget',
          status: 'missing',
          services: [{
            name: 'admin', label: 'Postgres admin console', purpose: 'schema browser and query console for the primary database',
            url: 'http://admin.feature-a-really-long-branch-name-that-uses-the-whole-slug-budget.acme.localhost:8080/console',
            available: false
          }]
        })
      ]
    }
  },
  {
    name: 'non-ascii',
    snapshot: {
      generatedAt: '2026-09-21T12:00:00.000Z',
      dockerReachable: true,
      entries: [
        entry({
          repositoryId: 'r1', worktreeId: 'w1', project: '中文项目', domain: 'acme.localhost',
          slug: 'feature-cafe', host: 'feature-cafe.acme.localhost', branch: 'feature/café-menu',
          root: '/work/acme/feature-café', status: 'running',
          services: [{ name: 'api', label: '日本語 API 🚀', purpose: 'Ünïcödé purpose', url: 'http://feature-cafe.acme.localhost:8080', available: true }]
        }),
        entry({
          repositoryId: 'r2', worktreeId: 'w2', project: 'Ångström Systems', domain: 'acme.localhost',
          slug: 'feature-b', host: 'feature-b.acme.localhost', branch: 'feature/b',
          status: 'running',
          services: [{ name: 'api', label: 'API', purpose: '', url: 'http://feature-b.acme.localhost:8080', available: true }]
        })
      ]
    }
  }
];

const results = fixtures.map((fixture) => ({
  name: fixture.name,
  snapshot: fixture.snapshot,
  html: renderIndexHtml(fixture.snapshot),
  text: formatIndexText(fixture.snapshot, INDEX_URL)
}));

process.stdout.write(JSON.stringify({ url: INDEX_URL, fixtures: results }, null, 2) + '\n');
