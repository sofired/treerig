import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { readIndexCard, writeIndexCard, type IndexCard } from '../../src/checkout-index/card.ts';
import { compactPath, formatIndexText, renderIndexHtml } from '../../src/checkout-index/render.ts';
import { CheckoutIndexService } from '../../src/checkout-index/service.ts';
import { buildSnapshot, entryKey, parseContainerInventory, type IndexSnapshot } from '../../src/checkout-index/snapshot.ts';
import { controlLayout } from '../../src/layout.ts';
import type { CommandRunner, RunResult } from '../../src/process-runner.ts';
import type { RegisteredWorktree, WorktreeIdentity } from '../../src/worktree-registry.ts';
import { RecordingLogger } from '../helpers/command-doubles.ts';
import { fileMode } from '../helpers/fs.ts';

const REPO = '11111111-1111-4111-8111-111111111111';
const OTHER_REPO = '33333333-3333-4333-8333-333333333333';
const WT = ['22222222-2222-4222-8222-222222222221', '22222222-2222-4222-8222-222222222222', '22222222-2222-4222-8222-222222222223', '22222222-2222-4222-8222-222222222224'] as const;

const registrations: RegisteredWorktree[] = [
  { repositoryId: REPO, worktreeId: WT[0], slug: 'feature-a', domain: 'acme.localhost' },
  { repositoryId: REPO, worktreeId: WT[1], slug: 'feature-b', domain: 'acme.localhost' },
  { repositoryId: REPO, worktreeId: WT[2], slug: 'gone', domain: 'acme.localhost' },
  { repositoryId: OTHER_REPO, worktreeId: WT[3], slug: 'main', domain: 'widgets.localhost' }
];

function identity(record: RegisteredWorktree, branch: string): WorktreeIdentity {
  return {
    branch,
    gitCommonDir: '/repo/.git',
    gitDir: `/repo/.git/worktrees/${record.slug}`,
    repositoryId: record.repositoryId,
    root: `/Users/dev/worktrees/${record.slug}`,
    worktreeId: record.worktreeId
  };
}

function card(record: RegisteredWorktree, project: string, services: IndexCard['services'] = []): IndexCard {
  return {
    version: 1,
    project,
    domain: record.domain,
    slug: record.slug,
    host: `${record.slug}.${record.domain}`,
    root: `/Users/dev/last-seen/${record.slug}`,
    branch: `branch/${record.slug}`,
    labelNamespace: 'dev.berth',
    services,
    updatedAt: '2026-09-21T10:00:00.000Z'
  };
}

const API = { name: 'api', url: 'http://feature-a.acme.localhost', label: 'API', purpose: 'REST surface' };
const MAIL = { name: 'mail', url: 'http://mail.feature-a.acme.localhost', label: 'Mailpit', purpose: 'Captured email' };

function line(record: RegisteredWorktree, service: string, state = 'running', status = 'Up 20 seconds (healthy)'): string {
  return [state, status, record.repositoryId, record.worktreeId, service].join('\t');
}

describe('parseContainerInventory', () => {
  it('marks a container available only when running and not unhealthy or still starting', () => {
    const [r0, r1] = registrations;
    if (!r0 || !r1) throw new Error('fixture');
    const parsed = parseContainerInventory([
      line(r0, 'api'),
      line(r0, 'mail', 'running', 'Up 3 seconds (health: starting)'),
      line(r1, 'api', 'exited', 'Exited (0) 2 minutes ago'),
      line(r1, 'mail', 'running', 'Up 5 seconds (unhealthy)'),
      line(r1, 'db', 'running', 'Up 5 seconds')
    ].join('\n'));
    expect(parsed.map((c) => [c.service, c.available])).toEqual([['api', true], ['mail', false], ['api', false], ['mail', false], ['db', true]]);
  });

  it('rejects a line that does not have exactly five fields', () => {
    expect(() => parseContainerInventory('running\tUp\tonly-three')).toThrow('unrecognized container inventory line');
  });
});

describe('buildSnapshot', () => {
  const [a, b, gone, foreign] = registrations as [RegisteredWorktree, RegisteredWorktree, RegisteredWorktree, RegisteredWorktree];

  function snapshot(overrides: Partial<Parameters<typeof buildSnapshot>[0]> = {}): IndexSnapshot {
    return buildSnapshot({
      registrations,
      cards: new Map([
        [entryKey(a), card(a, 'acme', [API, MAIL])],
        [entryKey(b), card(b, 'acme', [API])],
        [entryKey(gone), card(gone, 'acme', [API])],
        [entryKey(foreign), card(foreign, 'widgets')]
      ]),
      identities: [identity(a, 'feature/a'), identity(b, 'feature/b')],
      currentRepositoryId: REPO,
      containers: parseContainerInventory([
        line(a, 'api'), line(a, 'mail'), line(a, 'db'),
        line(b, 'api'), line(b, 'db', 'exited', 'Exited (1) 1 minute ago'),
        line(gone, 'api'),
        line(foreign, 'web', 'exited', 'Exited (0) 1 hour ago')
      ].join('\n')),
      generatedAt: '2026-09-21T12:00:00.000Z',
      ...overrides
    });
  }

  it('derives running, partial, stopped, and missing, sorted by project with missing last', () => {
    expect(snapshot().entries.map((e) => [e.project, e.slug, e.status])).toEqual([
      ['acme', 'feature-a', 'running'],
      ['acme', 'feature-b', 'partial'],
      ['acme', 'gone', 'missing'],
      ['widgets', 'main', 'stopped']
    ]);
  });

  it('prefers live Git identity over the card, and falls back to the card for other repositories', () => {
    const entries = snapshot().entries;
    expect(entries[0]).toMatchObject({ branch: 'feature/a', root: '/Users/dev/worktrees/feature-a' });
    expect(entries[3]).toMatchObject({ branch: 'branch/main', root: '/Users/dev/last-seen/main' });
    // A missing checkout still shows where it was last seen.
    expect(entries[2]).toMatchObject({ branch: 'branch/gone', root: '/Users/dev/last-seen/gone' });
  });

  it('links only services whose container is available, and none for a missing checkout', () => {
    const entries = snapshot().entries;
    expect(entries[0]?.services.map((s) => [s.label, s.available])).toEqual([['API', true], ['Mailpit', true]]);
    expect(entries[1]?.services.map((s) => [s.label, s.available])).toEqual([['API', true]]);
    expect(entries[2]?.services.map((s) => [s.label, s.available])).toEqual([['API', false]]);
  });

  it('reports unknown when Docker could not be asked, and stopped when a worktree has no containers', () => {
    const unknown = snapshot({ containers: undefined }).entries.map((e) => e.status);
    expect(unknown).toEqual(['unknown', 'unknown', 'missing', 'unknown']);
    const empty = snapshot({ containers: [] }).entries.map((e) => e.status);
    expect(empty).toEqual(['stopped', 'stopped', 'missing', 'stopped']);
  });

  it('shows a registration that never produced a card under its domain, with no links', () => {
    const entries = snapshot({ cards: new Map() }).entries;
    expect(entries.find((e) => e.slug === 'main')).toMatchObject({ project: 'widgets.localhost', services: [] });
    expect(entries.find((e) => e.slug === 'main')?.branch).toBeUndefined();
  });
});

describe('rendering', () => {
  const entry = {
    repositoryId: REPO, worktreeId: WT[0], project: 'acme', domain: 'acme.localhost', slug: 'feature-a',
    host: 'feature-a.acme.localhost', branch: 'feature/<a>', root: '/Users/dev/worktrees/feature-a',
    status: 'partial' as const,
    services: [{ ...API, available: true }, { ...MAIL, purpose: 'Captured "email"', available: false }]
  };
  const snapshot: IndexSnapshot = { entries: [entry], generatedAt: '2026-09-21T12:00:00.000Z' };

  it('renders a link for available services, a disabled span otherwise, and escapes everything', () => {
    const html = renderIndexHtml(snapshot);
    expect(html).toContain('<h2>acme · acme.localhost</h2>');
    expect(html).toContain('<strong>feature/&lt;a&gt;</strong>');
    expect(html).toContain('<a class="service-link" href="http://feature-a.acme.localhost"');
    expect(html).toContain('<span class="service-link service-disabled" title="http://mail.feature-a.acme.localhost"');
    expect(html).toContain('Captured &quot;email&quot;');
    expect(html).toContain('status-partial');
    expect(html).toContain('http-equiv="refresh"');
  });

  it('says so when nothing is indexed, and points at the missing-checkout remedy', () => {
    expect(renderIndexHtml({ entries: [], generatedAt: 'now' })).toContain('No checkouts have been indexed yet');
    expect(renderIndexHtml({ entries: [{ ...entry, status: 'missing' }], generatedAt: 'now' })).toContain('berth worktree remove feature-a');
  });

  it('formats the text twin with the index URL and unavailable markers', () => {
    const text = formatIndexText(snapshot, 'http://berth.localhost');
    expect(text.split('\n')).toEqual([
      'Checkout index  http://berth.localhost',
      'Updated         2026-09-21T12:00:00.000Z',
      '',
      'acme · acme.localhost',
      '  feature/<a>  [Partial]',
      '    host      feature-a.acme.localhost',
      `    checkout  ${compactPath('/Users/dev/worktrees/feature-a')}`,
      '    API                http://feature-a.acme.localhost',
      '    Mailpit            http://mail.feature-a.acme.localhost  (unavailable)'
    ]);
  });

  it('compacts paths under the home directory only', () => {
    expect(compactPath('/home/me/dev/x', '/home/me')).toBe('~/dev/x');
    expect(compactPath('/opt/x', '/home/me')).toBe('/opt/x');
  });
});

describe('CheckoutIndexService', () => {
  const temporary: string[] = [];
  afterEach(async () => {
    await Promise.all(temporary.splice(0).map((d) => rm(d, { recursive: true, force: true })));
  });

  class DockerFake implements CommandRunner {
    readonly calls: readonly string[][] = [];
    stdout = '';
    exitCode = 0;
    run(_command: string, args: readonly string[] = []): Promise<RunResult> {
      (this.calls as string[][]).push([...args]);
      return Promise.resolve({ stdout: this.stdout, stderr: '', exitCode: this.exitCode });
    }
    which(): Promise<boolean> {
      return Promise.resolve(true);
    }
  }

  async function service(docker = new DockerFake(), identities: WorktreeIdentity[] = []) {
    const controlHome = await mkdtemp(path.join(tmpdir(), 'berth-index-'));
    temporary.push(controlHome);
    const [a, b] = registrations as [RegisteredWorktree, RegisteredWorktree];
    await writeIndexCard(controlHome, a.repositoryId, a.worktreeId, card(a, 'acme', [API]));
    const logger = new RecordingLogger();
    const index = new CheckoutIndexService({
      controlHome,
      routerPort: 8080,
      registry: {
        listAll: () => Promise.resolve([a, b]),
        identities: { activeIdentities: () => Promise.resolve(identities), repositoryId: () => Promise.resolve(REPO) }
      },
      runner: docker,
      logger,
      now: () => new Date('2026-09-21T12:00:00.000Z')
    });
    return { controlHome, docker, logger, index, a, b };
  }

  it('writes the page from registrations, cards, identities, and one docker ps per namespace', async () => {
    const docker = new DockerFake();
    const [a] = registrations as [RegisteredWorktree];
    docker.stdout = line(a, 'api');
    const s = await service(docker, [identity(a, 'feature/a')]);

    const snapshot = await s.index.refresh();

    expect(s.index.url).toBe('http://berth.localhost:8080');
    expect(snapshot.entries.map((e) => [e.slug, e.status, e.branch ?? '-'])).toEqual([['feature-a', 'running', 'feature/a'], ['feature-b', 'missing', '-']]);
    const html = await readFile(controlLayout(s.controlHome).indexFile, 'utf8');
    expect(html).toContain('feature-a.acme.localhost');
    expect(html).toContain('href="http://feature-a.acme.localhost"');
    expect(s.docker.calls).toEqual([['ps', '--all', '--filter', 'label=dev.berth.managed=true', '--format', expect.stringContaining('{{.State}}') as string]]);
  });

  it('leaves the served directory and page readable by the index container on a fresh control home', async () => {
    const s = await service();
    await s.index.refresh();
    const layout = controlLayout(s.controlHome);
    expect(await fileMode(layout.indexDir)).toBe(0o755);
    expect(await fileMode(layout.indexFile)).toBe(0o644);
    expect(await fileMode(layout.routerDir)).toBe(0o700);
  });

  it('still writes the page when Docker is unavailable, with unknown statuses', async () => {
    const docker = new DockerFake();
    docker.exitCode = 1;
    const s = await service(docker);
    const snapshot = await s.index.refresh();
    expect(snapshot.entries.map((e) => e.status)).toEqual(['missing', 'missing']);   // both local, neither listed by Git
    expect(await readFile(controlLayout(s.controlHome).indexFile, 'utf8')).toContain('status-missing');
  });

  it('refreshBestEffort warns instead of failing the command', async () => {
    const s = await service();
    const broken = new CheckoutIndexService({
      controlHome: s.controlHome,
      routerPort: 80,
      registry: { listAll: () => Promise.reject(new Error('registry exploded')), identities: { activeIdentities: () => Promise.resolve([]), repositoryId: () => Promise.resolve(undefined) } },
      runner: s.docker,
      logger: s.logger
    });
    await expect(broken.refreshBestEffort('up')).resolves.toBeUndefined();
    expect(s.logger.lines).toContainEqual('warn: Could not refresh the checkout index after up: registry exploded');
  });

  it('reads back what it wrote and treats anything else as no card', async () => {
    const s = await service();
    await expect(readIndexCard(s.controlHome, s.a.repositoryId, s.a.worktreeId)).resolves.toMatchObject({ slug: 'feature-a' });
    await expect(readIndexCard(s.controlHome, s.b.repositoryId, s.b.worktreeId)).resolves.toBeUndefined();
  });
});
