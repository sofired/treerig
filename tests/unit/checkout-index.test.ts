import { createHash } from 'node:crypto';
import { runInNewContext, Script } from 'node:vm';
import { mkdir, mkdtemp, readFile, rename, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { indexCardPath, readIndexCard, writeIndexCard, type IndexCard } from '../../src/checkout-index/card.ts';
import { compactPath, formatIndexText, liveUpdateScript, planIndexUpdate, printable, renderIndexHtml, wrapText } from '../../src/checkout-index/render.ts';
import { CheckoutIndexService } from '../../src/checkout-index/service.ts';
import { buildSnapshot, describeBranch, entryKey, parseContainerInventory, placeholderSnapshot, type IndexEntry, type IndexSnapshot } from '../../src/checkout-index/snapshot.ts';
import { secureControlDirectories } from '../../src/files.ts';
import { controlLayout, worktreeLayout } from '../../src/layout.ts';
import { OperationCancelledError } from '../../src/errors.ts';
import { CommandStartError, type CommandRunner, type RunResult } from '../../src/process-runner.ts';
import type { RegisteredWorktree, WorktreeIdentity } from '../../src/worktree-registry.ts';
import { RecordingLogger } from '../helpers/command-doubles.ts';
import { fileMode } from '../helpers/fs.ts';

const DOCKER_NOTICE_HTML = 'Docker could not be asked about containers, so no service below has been checked. ' +
  'Rows show Unknown unless Git reports the checkout gone. Run <code>berth index</code> again once it answers.';

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

/** The same services, on the host the registration says this checkout has. */
function hosted(record: RegisteredWorktree, ...services: { name: string; url: string; label: string; purpose: string }[]) {
  const host = `${record.slug}.${record.domain}`;
  return services.map((service) => ({
    ...service,
    url: service.name === 'api' ? `http://${host}` : `http://${service.name}.${host}`
  }));
}

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
    // A tab inside a status would shift every field right, and the row would
    // read Stopped rather than being refused.
    expect(() => parseContainerInventory('running\tUp 2 seconds\t(healthy)\tr\tw\tapi'))
      .toThrow('unrecognized container inventory line');
  });
});

describe('buildSnapshot', () => {
  const [a, b, gone, foreign] = registrations as [RegisteredWorktree, RegisteredWorktree, RegisteredWorktree, RegisteredWorktree];

  function snapshot(overrides: Partial<Parameters<typeof buildSnapshot>[0]> = {}): IndexSnapshot {
    return buildSnapshot({
      registrations,
      cards: new Map([
        [entryKey(a), card(a, 'acme', hosted(a, API, MAIL))],
        [entryKey(b), card(b, 'acme', hosted(b, API))],
        [entryKey(gone), card(gone, 'acme', hosted(gone, API))],
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
      routerPort: 80,
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

  it('keeps a detached HEAD detached instead of showing the card\'s last branch', () => {
    const detached = snapshot({ identities: [identity(a, '')] }).entries.find((e) => e.slug === 'feature-a');
    expect(detached?.branch).toBe('');
  });

  it('sorts a detached checkout by its slug, having no branch name to sort by', () => {
    // `||` in the comparator, not `??`: an empty branch is not a sort key.
    expect(snapshot({ identities: [identity(a, ''), identity(b, 'aaa')] }).entries.map((e) => e.slug))
      .toEqual(['feature-b', 'feature-a', 'gone', 'main']);
  });

  it('is not Running while one replica of a scaled service is down', () => {
    // Every replica carries the same Compose service label, so a service is
    // only answering when all of its containers are.
    const scaled = snapshot({
      containers: parseContainerInventory([
        line(a, 'api'), line(a, 'api', 'exited', 'Exited (1) 1 minute ago'), line(a, 'mail')
      ].join('\n'))
    }).entries.find((e) => e.slug === 'feature-a');
    expect(scaled).toMatchObject({ status: 'partial' });
  });

  it('is not Running while a declared service has no container at all', () => {
    // `up --services api` on a worktree that also indexes mail: one container,
    // two services. A green pill there would say every service answers.
    const partial = snapshot({ containers: parseContainerInventory(line(a, 'api')) })
      .entries.find((e) => e.slug === 'feature-a');
    expect(partial).toMatchObject({ status: 'partial' });
    expect(partial?.services.map((service) => [service.label, service.available]))
      .toEqual([['API', true], ['Mailpit', false]]);
  });

  it('records whether Docker could be asked at all, so the page can say so', () => {
    expect(snapshot().dockerReachable).toBe(true);
    expect(snapshot({ containers: [] }).dockerReachable).toBe(true);
    expect(snapshot({ containers: undefined }).dockerReachable).toBe(false);
  });

  it('is not Stopped while a scaled service is partly up, and says the same in the link', () => {
    // One declared service, three replicas, one down: the legend says Stopped
    // means none answer, and two of them do.
    const partly = snapshot({
      cards: new Map([[entryKey(a), card(a, 'acme', hosted(a, API))]]),
      containers: parseContainerInventory([
        line(a, 'api'), line(a, 'api'), line(a, 'api', 'exited', 'Exited (1) 1 minute ago')
      ].join('\n'))
    }).entries.find((e) => e.slug === 'feature-a');
    expect(partly).toMatchObject({ status: 'partial' });
    // The pill and the link cannot contradict each other.
    expect(partly?.services.map((service) => service.available)).toEqual([true]);
  });

  it('drops a service the card points somewhere other than this checkout', () => {
    // Every field in a card is self-attested, and the card lives in a home
    // every lifecycle hook is handed. Only the registration says where a
    // checkout is, so a link anywhere else is not published at all.
    const elsewhere = card(a, 'acme', [
      { ...API, url: 'http://attacker.example.com/login' },
      { ...API, name: 'real', url: 'http://feature-a.acme.localhost/real' }
    ]);
    const entries = snapshot({ cards: new Map([[entryKey(a), { ...elsewhere, host: 'attacker.example.com' }]]) }).entries;
    const row = entries.find((e) => e.slug === 'feature-a');
    expect(row?.services.map((service) => service.url)).toEqual(['http://feature-a.acme.localhost/real']);
    // Every worktree host is loopback, so another port is another listener on
    // this machine — and another scheme is not a routed service at all.
    const offPort = snapshot({
      routerPort: 80,
      cards: new Map([[entryKey(a), card(a, 'acme', [
        { ...API, url: 'http://feature-a.acme.localhost:11434/api' },
        { ...API, name: 'tls', url: 'https://feature-a.acme.localhost/' }
      ])]])
    }).entries.find((e) => e.slug === 'feature-a');
    expect(offPort?.services).toEqual([]);
    expect(renderIndexHtml({ entries, generatedAt: 'now', dockerReachable: true })).not.toContain('attacker.example.com');
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
  const snapshot: IndexSnapshot = { entries: [entry], generatedAt: '2026-09-21T12:00:00.000Z', dockerReachable: true };
  const url = 'http://berth.localhost';
  const count = (html: string, pattern: RegExp): number => (html.match(pattern) ?? []).length;

  it('renders a link for available services, a disabled span otherwise, and escapes everything', () => {
    const html = renderIndexHtml(snapshot);
    expect(html).toContain('>acme · acme.localhost</h2>');
    expect(html).toContain('<strong>feature/&lt;a&gt;</strong>');
    expect(html).toContain('<a class="service-link" href="http://feature-a.acme.localhost"');
    // WebKit drops a bulletless list's role unless it is asked for.
    expect(html).toContain('<ul role="list">');
    expect(html).toContain('<span class="service-link service-disabled" title="http://mail.feature-a.acme.localhost"');
    expect(html).toContain('Captured &quot;email&quot;');
    expect(html).toContain('status-partial');
  });

  it('says so when nothing is indexed, and points at the missing-checkout remedy', () => {
    const empty = { entries: [], generatedAt: 'now', dockerReachable: true };
    expect(renderIndexHtml(empty)).toContain('No checkouts have been indexed yet. Run <code>berth up</code> in a worktree.');
    expect(formatIndexText(empty, url)).toContain('No checkouts have been indexed yet. Run `berth up` in a worktree.');
    const gone = { entries: [{ ...entry, status: 'missing' as const }], generatedAt: 'now', dockerReachable: true };
    expect(renderIndexHtml(gone)).toContain('berth worktree remove feature-a');
    expect(formatIndexText(gone, url)).toContain('    gone      berth worktree remove feature-a');
  });

  it('formats the text twin with the index URL, each purpose, the legend, and state markers', () => {
    expect(formatIndexText(snapshot, url).split('\n')).toEqual([
      'Checkout index  http://berth.localhost',
      'Updated         2026-09-21T12:00:00.000Z',
      '',
      'Running: every service answers. Partial: some do. Stopped: none do. Missing:',
      'the checkout is gone. Unknown: the state could not be read.',
      'Services: ! not answering, ? not checked, - checkout gone.',
      '',
      'acme · acme.localhost',
      '  [Partial]  feature/<a>',
      '    host      feature-a.acme.localhost',
      `    checkout  ${compactPath('/Users/dev/worktrees/feature-a')}`,
      '      API (REST surface)           http://feature-a.acme.localhost',
      '    ! Mailpit (Captured "email")   http://mail.feature-a.acme.localhost'
    ]);
  });

  it('keeps every text line inside eighty columns, however long the notice', () => {
    const wordy = formatIndexText(
      { ...snapshot, dockerReachable: false, identityError: { reason: 'fatal: not a git repository (or any of the parent directories): .git', rowsUnknown: true } },
      url
    );
    expect(wordy.split('\n').filter((line) => line.length > 80)).toEqual([]);
    expect(wordy).toContain('not a git repository');
  });

  it('names a detached head and a branch nothing recorded, and cannot be mistaken for a real one', () => {
    expect(renderIndexHtml({ ...snapshot, entries: [{ ...entry, branch: '' }] }))
      .toContain('<th scope="row"><span class="muted">(detached)</span>');
    const nothingKnows: IndexEntry = { ...entry };
    delete nothingKnows.branch;
    const blank = { ...snapshot, entries: [nothingKnows] };
    expect(renderIndexHtml(blank)).toContain('<th scope="row"><span class="muted">(no branch)</span>');
    expect(formatIndexText(blank, url)).toContain('  [Partial]  (no branch)');
    // `detached` is a legal branch name: the parentheses are what tell them apart.
    expect(formatIndexText({ ...snapshot, entries: [{ ...entry, branch: 'detached' }] }, url))
      .toContain('  [Partial]  detached');
    expect(describeBranch('detached')).toEqual({ text: 'detached', muted: false });
  });

  it('heads a project with its domain alone when no card has named the project', () => {
    const unnamed = renderIndexHtml({ ...snapshot, entries: [{ ...entry, project: 'acme.localhost' }] });
    expect(unnamed).toContain('>acme.localhost</h2>');
    expect(unnamed).not.toContain('>acme.localhost · ');
  });

  it('keeps one table per domain, whatever each row knows about its project', () => {
    // The second checkout is registered but was never brought up, so no card
    // names its project and it falls back to the domain. Same table all the same.
    const second = { ...entry, slug: 'feature-b', branch: 'feature/b', project: 'acme.localhost' };
    const other = { ...entry, slug: 'main', domain: 'widgets.localhost', project: 'widgets', branch: 'main' };
    const html = renderIndexHtml({ ...snapshot, entries: [entry, second, other] });
    expect(count(html, /<h2/g)).toBe(2);
    expect(count(html, /<th scope="row">/g)).toBe(3);
    expect(html).toContain('>acme · acme.localhost</h2>');
    const [, acme = '', widgets = ''] = html.split('<section>');
    expect(count(acme, /<th scope="row">/g)).toBe(2);
    expect(count(widgets, /<th scope="row">/g)).toBe(1);
    const text = formatIndexText({ ...snapshot, entries: [entry, second, other] }, url);
    expect(count(text, /^acme · acme\.localhost$/gm)).toBe(1);
    expect(text).toContain('feature/b');
  });

  it('keeps service links live when Docker was never asked, and says which state is unread', () => {
    const blind = { ...snapshot, dockerReachable: false };
    const html = renderIndexHtml(blind);
    expect(html).toContain(DOCKER_NOTICE_HTML);
    // Both services stay clickable: nothing checked them, so nothing may grey them out.
    expect(count(html, /<a class="service-link"/g)).toBe(2);
    expect(html).toContain('<span class="sr-only"> (state unknown)</span>');
    expect(html).not.toContain('(unavailable)');
    const text = formatIndexText(blind, url);
    expect(text).toContain('    ? API (REST surface)');
    expect(text).not.toContain('! Mailpit');
    // A checkout Git reports gone is not in doubt, and nothing was asked of its
    // services: they are not "not answering", they are not there.
    const gone = renderIndexHtml({ ...blind, entries: [{ ...entry, status: 'missing' as const }] });
    expect(gone).toContain('<span class="sr-only"> (checkout gone)</span>');
    expect(gone).not.toContain('(state unknown)');
    expect(gone).toContain('status-missing');
    const goneText = formatIndexText({ ...blind, entries: [{ ...entry, status: 'missing' as const }] }, url);
    expect(goneText).not.toContain('! API');
    // Its own marker: blank would read as a service that answers.
    expect(goneText).toContain('    - API (REST surface)');
  });

  it('says nothing about Docker or Git when there are no rows for it to be about', () => {
    const nothing = { entries: [], generatedAt: 'now', dockerReachable: false, identityError: { reason: 'git exploded', rowsUnknown: true } };
    expect(renderIndexHtml(nothing)).not.toContain('Docker could not be asked');
    expect(renderIndexHtml(nothing)).not.toContain('git exploded');
    expect(formatIndexText(nothing, url)).not.toContain('Docker could not be asked');
    expect(renderIndexHtml(placeholderSnapshot('now'))).toContain('No checkouts have been indexed yet');
    expect(renderIndexHtml(placeholderSnapshot('now'))).not.toContain('class="notice"');
  });

  it('tells a screen reader what the grey tells everyone else about a disabled service', () => {
    const html = renderIndexHtml(snapshot);
    expect(html).toContain('<span class="sr-only"> (unavailable)</span>');
    // Visually hidden, not merely small: the text must not land in the layout.
    expect(html).toMatch(/\.sr-only \{[^}]*position: absolute[^}]*clip-path: inset\(50%\)/);
  });

  it('explains the status words once for the page and counts the rows in the title', () => {
    const html = renderIndexHtml(snapshot);
    expect(html).toContain('<title>Checkouts (1)</title>');
    expect(html).toContain('<p class="legend">Running: every service answers.');
    expect(html).toContain('Missing: the checkout is gone.');
    const twoProjects = renderIndexHtml({ ...snapshot, entries: [entry, { ...entry, slug: 'main', domain: 'widgets.localhost', project: 'widgets' }] });
    expect(count(twoProjects, /class="legend"/g)).toBe(1);
    expect(twoProjects).toContain('<title>Checkouts (2)</title>');
    expect(renderIndexHtml({ ...snapshot, entries: [] })).not.toContain('class="legend"');
  });

  it('heads every column and row for assistive technology, and names each table', () => {
    const html = renderIndexHtml(snapshot);
    expect(html).toContain('<th scope="col">Branch</th>');
    expect(count(html, /scope="col"/g)).toBe(5);
    expect(html).toContain('<th scope="row"><strong>feature/&lt;a&gt;</strong>');
    // Named once, by the heading it already shows, through an id that counts
    // sections: a registry record's domain is not validated as a hostname.
    expect(html).toContain('<div class="table-wrap" role="region" aria-labelledby="checkouts-0">');
    // Focusable only while it has something to scroll to, which the script decides.
    expect(html).not.toContain('tabindex="0"');
    expect(liveUpdateScript()).toContain("setAttribute('tabindex', '0')");
    expect(html).not.toContain('<caption');
    expect(html).toContain('<h2 id="checkouts-0">');
  });

  it('keeps a service in the tab it already opened rather than one tab per click', () => {
    const html = renderIndexHtml(snapshot);
    expect(html).toContain('target="feature-a.acme.localhost-api"');
    // `noopener` would make every named target behave like `_blank`.
    expect(html).not.toContain('rel="noopener"');
  });

  it('re-colours the muted text and every status pill for a dark scheme', () => {
    const dark = (renderIndexHtml(snapshot).split('@media (prefers-color-scheme: dark)')[1] ?? '').split('</style>')[0] ?? '';
    for (const status of ['running', 'partial', 'stopped', 'missing', 'unknown']) {
      expect(dark).toMatch(new RegExp(`\\.status-${status}\\s*\\{`));
    }
    for (const selector of ['muted', 'guidance', 'empty', 'notice', 'path-compact']) {
      expect(dark).toMatch(new RegExp(`\\.${selector}[^{]*\\{`));
    }
  });

  it('moves the checkout path under the branch on a narrow screen instead of dropping it', () => {
    const html = renderIndexHtml(snapshot);
    const narrow = (html.split('@media (max-width: 900px)')[1] ?? '').split('@media')[0] ?? '';
    expect(narrow).toMatch(/\.checkout \{ display: none; \}/);
    expect(narrow).toMatch(/\.path-compact \{ display: block;/);
    // Rendered either way; the stylesheet decides which one a reader sees.
    expect(html).toContain('<td class="checkout">');
    // The host follows it at the narrowest width, for the same reason.
    const phone = (html.split('@media (max-width: 400px)')[1] ?? '').split('@media')[0] ?? '';
    expect(phone).toMatch(/\.host \{ display: none; \}/);
    expect(phone).toMatch(/\.host-compact \{ display: block;/);
    expect(html).toContain('<span class="host-compact"><code>feature-a.acme.localhost</code></span>');
    expect(html).toContain(`<span class="path-compact">${compactPath('/Users/dev/worktrees/feature-a')}</span>`);
  });

  it('declares exactly the one script it carries, and no network destination but itself', () => {
    const html = renderIndexHtml(snapshot);
    // indexOf, like a browser: the element ends at the first closing tag,
    // and a hash over more than that would be a policy nothing satisfies.
    const script = html.slice(html.indexOf('<script>') + '<script>'.length, html.indexOf('</script>'));
    expect(script).not.toMatch(/<\/script|<!--/i);
    const hash = createHash('sha256').update(script, 'utf8').digest('base64');
    expect(html).toContain(`script-src 'sha256-${hash}'`);
    expect(html).toContain("default-src 'none'");
    expect(html).toContain("connect-src 'self'");
    // Without this the page's own stylesheet is blocked and it renders bare.
    expect(html).toContain("style-src 'unsafe-inline'");
  });

  it('refreshes the whole page when there is no scripting to swap with', () => {
    expect(renderIndexHtml(snapshot)).toContain('<noscript><meta http-equiv="refresh" content="60"></noscript>');
  });

  it('renders a registration that has never been up, with nothing to show in either twin', () => {
    const bare: IndexEntry = {
      repositoryId: REPO, worktreeId: WT[1], project: 'acme.localhost', domain: 'acme.localhost',
      slug: 'feature-b', host: 'feature-b.acme.localhost', status: 'stopped', services: []
    };
    const html = renderIndexHtml({ ...snapshot, entries: [bare] });
    expect(count(html, /<span class="muted">—<\/span>/g)).toBe(2);
    expect(html).not.toContain('<span class="path-compact">');
    // The text twin has to say it too, or an empty line reads as an omission.
    const text = formatIndexText({ ...snapshot, entries: [bare] }, url);
    expect(text).toContain('    checkout  -');
    expect(text).toContain('    (no services)');
  });

  it('gives a service its own line for the URL rather than one a terminal would fold', () => {
    const wordy = {
      ...snapshot,
      entries: [{
        ...entry,
        services: [{
          name: 'admin', label: 'Postgres admin', purpose: 'schema browser and query console',
          url: 'http://admin.feature-a.acme.localhost/console', available: true
        }]
      }]
    };
    const lines = formatIndexText(wordy, url).split('\n');
    expect(lines.filter((line) => line.length > 80)).toEqual([]);
    expect(lines).toContain('      Postgres admin (schema browser and query console)');
    expect(lines).toContain('        http://admin.feature-a.acme.localhost/console');
  });

  it('says what a failed Git lookup cost the page, which is not always the same thing', () => {
    const reason = 'Git could not list the worktrees of /work/acme: fatal: not a git repository';
    const scoped = renderIndexHtml({ ...snapshot, identityError: { reason, rowsUnknown: true } });
    expect(scoped).toContain('Its rows show Unknown rather than Missing.');
    // The repository itself could not be identified, so no row was ever going
    // to be called missing and none of them is Unknown for this reason.
    const wholesale = renderIndexHtml({ ...snapshot, identityError: { reason, rowsUnknown: false } });
    expect(wholesale).toContain('No checkout below could be compared against it, so none is reported missing.');
    expect(wholesale).not.toContain('rather than Missing');
    expect(formatIndexText({ ...snapshot, identityError: { reason, rowsUnknown: false } }, url))
      .toContain('none is reported missing');
  });

  it('keeps a backtick in whatever Git said from marking up the rest of the sentence', () => {
    const html = renderIndexHtml({
      ...snapshot,
      identityError: { reason: "fatal: `--porcelain' is not a git command", rowsUnknown: true }
    });
    const notice = html.slice(html.indexOf('<p class="notice">'), html.indexOf('</p>', html.indexOf('<p class="notice">')));
    expect(notice).toContain('<code>berth index</code>');
    expect(count(notice, /<code>/g)).toBe(1);
  });

  it('leaves a word longer than the terminal whole rather than breaking a path', () => {
    expect(wrapText('x'.repeat(95), 78)).toBe('x'.repeat(95));
    expect(wrapText('one two three', 7)).toBe('one two\nthree');
  });

  it('carries every hook its own script looks up', () => {
    // The script finds these by name. Rename one in the markup and the page
    // still renders, still passes every other test, and never updates again.
    const html = renderIndexHtml(snapshot);
    const script = liveUpdateScript();
    for (const id of ['index-announcer', 'index-stale']) {
      expect(script).toContain(`getElementById('${id}')`);
      expect(html).toContain(`id="${id}"`);
    }
    expect(script).toContain("querySelector('header time')");
    expect(script).toContain("querySelector('header')");
    expect(script).toContain("querySelector('main')");
    expect(html).toMatch(/<header>[\s\S]*<time datetime=/);
    expect(html).toContain('<main data-body=');
  });

  it('replaces what a terminal or a reader would be fooled by, and keeps what they need', () => {
    const hostile = `feature/${String.fromCodePoint(0x1b)}[2K${String.fromCodePoint(0x202e)}x${String.fromCodePoint(0x2060)}y`;
    expect(printable(hostile)).toBe('feature/\uFFFD[2K\uFFFDx\uFFFDy');
    expect(printable('fea\u00ADture')).toBe('fea\uFFFDture');
    // Whitespace is a place to fold, not something to hide.
    expect(printable('one\ntwo\tthree')).toBe('one two three');
    // The joiners carry meaning: an emoji sequence and Persian orthography.
    expect(printable('\u{1F468}\u200D\u{1F4BB} m\u200Ci')).toBe('\u{1F468}\u200D\u{1F4BB} m\u200Ci');
    // The tag block is invisible in both a terminal and a browser.
    expect(printable('a\u{E0041}b')).toBe('a\uFFFDb');
    const one = { ...snapshot, entries: [{ ...entry, branch: hostile }] };
    // Whitespace is the page's own layout. What must not survive is anything
    // that moves a cursor, reverses a line, or takes up no room at all.
    const hidden = (text: string): string[] => [...text].filter((character) => {
      const code = character.codePointAt(0) ?? 0;
      const whitespace = code === 0x09 || code === 0x0a || code === 0x0d;
      const control = (code < 0x20 && !whitespace) || code === 0x7f || (code >= 0x80 && code <= 0x9f);
      return control || code === 0x00ad || code === 0x061c || code === 0xfeff ||
        (code >= 0x200b && code <= 0x200f) || (code >= 0x202a && code <= 0x202e) ||
        (code >= 0x2060 && code <= 0x2069);
    });
    expect(hidden(renderIndexHtml(one))).toEqual([]);
    expect(hidden(formatIndexText(one, url))).toEqual([]);
  });

  it('folds a multi-line reason from Git into a sentence that still fits a terminal', () => {
    const reason = 'fatal: not a git repository\nhint: use the checkout this repository was cloned into, or run it again from there';
    const wordy = { ...snapshot, identityError: { reason, rowsUnknown: true } };
    const text = formatIndexText(wordy, url);
    // A line break is a place to fold, not a replacement character.
    expect(text).not.toContain('\uFFFD');
    expect(text.split('\n').filter((line) => line.length > 80)).toEqual([]);
    expect(renderIndexHtml(wordy)).toContain('fatal: not a git repository hint: use the checkout');
  });

  it('leaves the remove command on one line however long the slug', () => {
    const long = 'feature-a-really-long-branch-name-that-uses-the-whole-slug-budget';
    const gone = { ...snapshot, entries: [{ ...entry, slug: long, status: 'missing' as const }] };
    // Folded, it would be copied off a terminal truncated.
    expect(formatIndexText(gone, url)).toContain(`    gone      berth worktree remove ${long}`);
  });

  it('orders the tables by the heading a reader sees, and names every project on a shared domain', () => {
    // beta's row comes first, so the heading has to sort the names rather than
    // list them in whatever order the rows arrived in.
    const rows = [
      { ...entry, project: 'zeta', domain: 'a.localhost', slug: 'z', branch: 'z' },
      { ...entry, project: 'beta', domain: 'b.localhost', slug: 'b', branch: 'b' },
      { ...entry, project: 'alpha', domain: 'b.localhost', slug: 'a', branch: 'a' }
    ];
    const headings = [...renderIndexHtml({ ...snapshot, entries: rows }).matchAll(/<h2 id="[^"]*">([^<]*)<\/h2>/g)].map((m) => m[1]);
    expect(headings).toEqual(['alpha, beta · b.localhost', 'zeta · a.localhost']);
    expect(formatIndexText({ ...snapshot, entries: rows }, url)).toContain('alpha, beta · b.localhost');
  });

  it('compacts paths under the home directory only', () => {
    expect(compactPath('/home/me/dev/x', '/home/me')).toBe('~/dev/x');
    expect(compactPath('/opt/x', '/home/me')).toBe('/opt/x');
    // The home itself is not `~`: it is where the path already is.
    expect(compactPath('/home/me', '/home/me')).toBe('/home/me');
  });
});

const entryFixture = {
  repositoryId: REPO, worktreeId: WT[0], project: 'acme', domain: 'acme.localhost', slug: 'feature-a',
  host: 'feature-a.acme.localhost', branch: 'feature/a', root: '/Users/dev/worktrees/feature-a',
  status: 'running' as const, services: []
};

describe('planIndexUpdate', () => {
  const page = (stamp: string, main: string, title = 'Checkouts (1)', shell = 'shell-1', body = 'body-1'): string =>
    `<!doctype html><html lang="en" data-shell="${shell}"><head><title>${title}</title></head><body>` +
    `<header><p>Updated <time datetime="${stamp}">x</time></p></header>` +
    `<main data-body="${body}">${main}</main></body></html>`;

  it('reports the stamp, title, fingerprints and table of a page whose stamp has moved', () => {
    expect(planIndexUpdate('2026-09-21T12:00:00.000Z', page('2026-09-21T12:30:00.000Z', '<p>rows</p>', 'Checkouts (2)')))
      .toEqual({
        kind: 'update',
        stamp: '2026-09-21T12:30:00.000Z',
        title: 'Checkouts (2)',
        main: '<p>rows</p>',
        shell: 'shell-1',
        body: 'body-1'
      });
  });

  it('says a page whose stamp has not moved is unchanged, which is not the same as unreadable', () => {
    expect(planIndexUpdate('2026-09-21T12:00:00.000Z', page('2026-09-21T12:00:00.000Z', '<p>rows</p>')))
      .toEqual({ kind: 'unchanged' });
  });

  it('calls anything it cannot read foreign, so a tab can say so instead of sitting on stale rows', () => {
    const full = page('2026-09-21T12:30:00.000Z', '<p>rows</p>');
    // Cut between the header and the table: a stamp that moved, no table to show.
    expect(planIndexUpdate('2026-09-21T12:00:00.000Z', full.slice(0, full.indexOf('<main')))).toEqual({ kind: 'foreign' });
    expect(planIndexUpdate('2026-09-21T12:00:00.000Z', full.slice(0, full.indexOf('</main>')))).toEqual({ kind: 'foreign' });
    // Someone else's 200 answers none of it.
    expect(planIndexUpdate('2026-09-21T12:00:00.000Z', '<html><body>404 Not Found</body></html>')).toEqual({ kind: 'foreign' });
    expect(planIndexUpdate(null, page('', '<p>rows</p>'))).toEqual({ kind: 'foreign' });
  });

  it('refuses an attribute the tag ends before, rather than reading on to the next quote', () => {
    // Cut inside the timestamp: the next quote in the document is a whole tag away.
    const truncated = '<html data-shell="s"><head><title>T</title></head><body><header>' +
      '<time datetime="2026><main data-body="b"><p>rows</p></main></body></html>';
    expect(planIndexUpdate(null, truncated)).toEqual({ kind: 'foreign' });
    // And one with no closing quote anywhere after it.
    expect(planIndexUpdate(null, '<html data-shell="s"><title>T</title><main data-body="b">x</main><time datetime="2026'))
      .toEqual({ kind: 'foreign' });
  });

  it('reads back exactly what this module renders, and none of the script that parses it', () => {
    const first = renderIndexHtml({ entries: [], generatedAt: '2026-09-21T12:00:00.000Z', dockerReachable: true });
    const second = renderIndexHtml({ entries: [], generatedAt: '2026-09-21T12:30:00.000Z', dockerReachable: true });
    const plan = planIndexUpdate('2026-09-21T12:00:00.000Z', second);
    if (plan.kind !== 'update') throw new Error('expected an update');
    expect(plan.stamp).toBe('2026-09-21T12:30:00.000Z');
    expect(plan.title).toBe('Checkouts (0)');
    // Exactly the body, to the byte. The page embeds this function's own
    // source, so a sentinel matched against that copy would splice the script
    // into the table and still look right in a browser.
    expect(plan.main).toBe(second.slice(second.indexOf('>', second.indexOf('<main')) + 1, second.indexOf('</main>')));
    expect(plan.main).not.toContain('<script');
    expect(plan.main).not.toContain('planIndexUpdate');
    // The clock, the announcer and the stale marker live outside the swapped
    // region: inside it, each would be detached on the first update.
    expect(plan.main).not.toContain('index-announcer');
    expect(plan.main).not.toContain('index-stale');
    expect(plan.main).not.toContain('<time');
    expect(planIndexUpdate('2026-09-21T12:00:00.000Z', first)).toEqual({ kind: 'unchanged' });
  });

  it('fingerprints the shell and the rows separately, from what they are made of', () => {
    const rendered = renderIndexHtml({ entries: [], generatedAt: '2026-09-21T12:30:00.000Z', dockerReachable: true });
    const plan = planIndexUpdate('2026-09-21T12:00:00.000Z', rendered);
    if (plan.kind !== 'update') throw new Error('expected an update');
    // The body is a digest of exactly what lands between the tags: the tab
    // compares its own `data-body` against it to decide whether to swap.
    expect(plan.body).toBe(createHash('sha256').update(plan.main, 'utf8').digest('base64url').slice(0, 16));
    // Different rows, same build: the body moves and the shell does not.
    const other = planIndexUpdate(null, renderIndexHtml({
      entries: [{ ...entryFixture, slug: 'feature-z', branch: 'feature/z' }],
      generatedAt: '2026-09-21T12:30:00.000Z',
      dockerReachable: true
    }));
    expect(other).toMatchObject({ shell: plan.shell });
    expect((other as { body: string }).body).not.toBe(plan.body);
    // Same build, same fingerprints; the timestamp is part of neither.
    const later = planIndexUpdate(null, renderIndexHtml({ entries: [], generatedAt: 'later', dockerReachable: true }));
    expect(later).toMatchObject({ shell: plan.shell, body: plan.body });
  });

  it('calls a page with no fingerprints foreign, rather than reloading for a shell it never read', () => {
    const rendered = renderIndexHtml({ entries: [], generatedAt: '2026-09-21T12:30:00.000Z', dockerReachable: true });
    // An older berth wrote this: without both fingerprints there is nothing to
    // compare, and a plan would drive a reload keyed on nothing.
    expect(planIndexUpdate('2026-09-21T12:00:00.000Z', rendered.replace(/ data-shell="[^"]+"/, ''))).toEqual({ kind: 'foreign' });
    expect(planIndexUpdate('2026-09-21T12:00:00.000Z', rendered.replace(/ data-body="[^"]+"/, ''))).toEqual({ kind: 'foreign' });
  });

  it('survives the round trip through the page as plain JavaScript', () => {
    // The page carries this function as text. Anything it closed over, or any
    // syntax the emitted source loses, would show up only as a page that
    // quietly stops updating.
    const isolated = runInNewContext(`(${planIndexUpdate.toString()})`) as typeof planIndexUpdate;
    const rendered = renderIndexHtml({ entries: [], generatedAt: '2026-09-21T12:30:00.000Z', dockerReachable: true });
    expect(isolated('2026-09-21T12:00:00.000Z', rendered)).toEqual(planIndexUpdate('2026-09-21T12:00:00.000Z', rendered));
    expect(() => new Script(liveUpdateScript())).not.toThrow();
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
    /** Pretend `docker` is not installed at all. */
    missing = false;
    /** Something other than Docker went wrong — a cancellation, say. */
    failure?: Error;
    /** Set when `docker` was asked to give up, so the wait can be asserted. */
    readonly options: Record<string, unknown>[] = [];
    timedOut = false;
    run(command: string, args: readonly string[] = [], options: Record<string, unknown> = {}): Promise<RunResult> {
      (this.calls as string[][]).push([...args]);
      this.options.push(options);
      if (this.failure) return Promise.reject(this.failure);
      if (this.missing) return Promise.reject(new CommandStartError(command, 'ENOENT', `spawn ${command} ENOENT`));
      return Promise.resolve({ stdout: this.stdout, stderr: '', exitCode: this.exitCode, timedOut: this.timedOut });
    }
    which(): Promise<boolean> {
      return Promise.resolve(true);
    }
  }

  async function service(docker = new DockerFake(), identities: WorktreeIdentity[] = []) {
    const controlHome = await mkdtemp(path.join(tmpdir(), 'berth-index-'));
    temporary.push(controlHome);
    // As in production: the control home is established (and marked) before any card is written.
    await secureControlDirectories(controlLayout(controlHome));
    const [a, b] = registrations as [RegisteredWorktree, RegisteredWorktree];
    // The router in this suite publishes 8080, so that is where its services are.
    await writeIndexCard(controlHome, a.repositoryId, a.worktreeId,
      card(a, 'acme', [{ ...API, url: 'http://feature-a.acme.localhost:8080' }]));
    const logger = new RecordingLogger();
    const index = new CheckoutIndexService({
      controlHome,
      routerPort: 8080,
      registry: {
        listAll: () => Promise.resolve([a, b]),
        identities: { root: '/work/acme', activeIdentities: () => Promise.resolve(identities), repositoryId: () => Promise.resolve(REPO) }
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
    expect(html).toContain('href="http://feature-a.acme.localhost:8080"');
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

  it('still writes the page when docker ps fails, with unknown statuses and a reason', async () => {
    const docker = new DockerFake();
    docker.exitCode = 1;
    const [a] = registrations as [RegisteredWorktree];
    const s = await service(docker, [identity(a, 'feature/a')]);
    const snapshot = await s.index.refresh();
    expect(snapshot.entries.map((e) => [e.slug, e.status])).toEqual([['feature-a', 'unknown'], ['feature-b', 'missing']]);
    expect(s.logger.lines).toContainEqual(
      'warn: The index shows service state as Unknown because containers could not be listed: `docker ps` exited 1 without saying why'
    );
    expect(await readFile(controlLayout(s.controlHome).indexFile, 'utf8')).toContain('status-unknown');
  });

  it('still writes the page when docker itself cannot be started, saying why the states are unknown', async () => {
    const docker = new DockerFake();
    docker.missing = true;
    const [a] = registrations as [RegisteredWorktree];
    const s = await service(docker, [identity(a, 'feature/a')]);
    const snapshot = await s.index.refresh();
    expect(snapshot.entries.map((e) => [e.slug, e.status])).toEqual([['feature-a', 'unknown'], ['feature-b', 'missing']]);
    expect(s.logger.lines).toContainEqual(
      'warn: The index shows service state as Unknown because containers could not be listed: Docker is not available: the `docker` command is not on PATH (ENOENT); is Docker installed?'
    );
    expect(await readFile(controlLayout(s.controlHome).indexFile, 'utf8')).toContain('status-unknown');
  });

  it('lets a cancelled run through rather than rewriting the page as Unknown', async () => {
    const docker = new DockerFake();
    docker.failure = new OperationCancelledError('SIGINT');
    const s = await service(docker);
    const before = await readFile(controlLayout(s.controlHome).indexFile, 'utf8').catch(() => '');

    await expect(s.index.refresh()).rejects.toThrow(OperationCancelledError);

    // The page it would have written says every service is Unknown, and blames
    // Docker for something the operator did.
    expect(await readFile(controlLayout(s.controlHome).indexFile, 'utf8').catch(() => '')).toBe(before);
    expect(s.logger.lines.join('\n')).not.toContain('could not be listed');
  });

  it('gives Docker a deadline, and says so on the page when it misses it', async () => {
    const docker = new DockerFake();
    docker.timedOut = true;
    // A daemon that answers 0 with nothing after timing out would otherwise read
    // as a machine with no containers at all.
    docker.exitCode = 0;
    const [a] = registrations as [RegisteredWorktree];
    const s = await service(docker, [identity(a, 'feature/a')]);

    const snapshot = await s.index.refresh();

    expect(s.docker.options.every((call) => call['timeoutMs'] === 30_000 && call['reject'] === false)).toBe(true);
    expect(snapshot.dockerReachable).toBe(false);
    expect(snapshot.entries.map((e) => e.status)).toEqual(['unknown', 'missing']);
    expect(s.logger.lines.join('\n')).toContain('did not answer within 30s');
    // The page is read by people who did not run the command: it says why.
    const html = await readFile(controlLayout(s.controlHome).indexFile, 'utf8');
    expect(html).toContain('did not answer within 30s');
  });

  it('says once why the states are unknown, however many namespaces it had to ask', async () => {
    const docker = new DockerFake();
    docker.exitCode = 1;
    const [a, b] = registrations as [RegisteredWorktree, RegisteredWorktree];
    const controlHome = await mkdtemp(path.join(tmpdir(), 'berth-index-'));
    temporary.push(controlHome);
    await secureControlDirectories(controlLayout(controlHome));
    const logger = new RecordingLogger();
    const index = new CheckoutIndexService({
      controlHome,
      routerPort: 8080,
      registry: {
        listAll: () => Promise.resolve([{ ...a, labelNamespace: 'dev.one' }, { ...b, labelNamespace: 'dev.two' }]),
        identities: { root: '/work/acme', activeIdentities: () => Promise.resolve([]), repositoryId: () => Promise.resolve(undefined) }
      },
      runner: docker,
      logger,
      now: () => new Date('2026-09-21T12:00:00.000Z')
    });

    await index.refresh();

    expect(logger.lines.filter((line) => line.includes('could not be listed'))).toHaveLength(1);
  });

  it('never calls a checkout missing when Git could not be asked which worktrees exist', async () => {
    const docker = new DockerFake();
    const s = await service(docker);
    const flaky = new CheckoutIndexService({
      controlHome: s.controlHome,
      routerPort: 80,
      registry: {
        listAll: () => Promise.resolve([s.a, s.b]),
        identities: {
          root: '/work/acme',
          activeIdentities: () => Promise.reject(new Error('Invalid identity marker for a sibling')),
          repositoryId: () => Promise.resolve(REPO)
        }
      },
      runner: docker,
      logger: s.logger,
      now: () => new Date('2026-09-21T12:00:00.000Z')
    });
    const snapshot = await flaky.refresh();
    expect(snapshot.entries.map((e) => e.status)).toEqual(['unknown', 'unknown']);
    expect(snapshot.identityError).toEqual({ reason: 'Git could not list the worktrees of /work/acme: Invalid identity marker for a sibling', rowsUnknown: true });
    const html = await readFile(controlLayout(s.controlHome).indexFile, 'utf8');
    expect(html).not.toContain('worktree remove');
    expect(html).toContain('Checkout state unavailable for the repository that last regenerated this page — Git could not list the worktrees of /work/acme');
    expect(html).toContain('Run <code>berth index</code> from one of its checkouts');
    // Wrapped for a terminal, so the sentence is matched in the pieces it survives as.
    const text = formatIndexText(snapshot, 'http://berth.localhost');
    expect(text).toContain('Checkout state unavailable for the repository that last regenerated');
    expect(text).toContain('Invalid identity marker for');
    expect(text.split('\n').filter((line) => line.length > 80)).toEqual([]);
    expect(s.logger.lines).toContainEqual('warn: Git could not list the worktrees of /work/acme: Invalid identity marker for a sibling. The index shows its checkouts as unknown.');
  });

  it('refreshBestEffort warns instead of failing the command', async () => {
    const s = await service();
    const broken = new CheckoutIndexService({
      controlHome: s.controlHome,
      routerPort: 80,
      registry: { listAll: () => Promise.reject(new Error('registry exploded')), identities: { root: '/work/acme', activeIdentities: () => Promise.resolve([]), repositoryId: () => Promise.resolve(undefined) } },
      runner: s.docker,
      logger: s.logger
    });
    await expect(broken.refreshBestEffort('up')).resolves.toBeUndefined();
    expect(s.logger.lines).toContainEqual('warn: Could not refresh the checkout index after up: registry exploded');
  });

  it('asks Docker about a namespace the registration records even when the card is gone', async () => {
    const docker = new DockerFake();
    const [a, b] = registrations as [RegisteredWorktree, RegisteredWorktree];
    const controlHome = await mkdtemp(path.join(tmpdir(), 'berth-index-'));
    temporary.push(controlHome);
    await secureControlDirectories(controlLayout(controlHome));
    // No card for either: the home is gone, and the registration is all that is left.
    const index = new CheckoutIndexService({
      controlHome,
      routerPort: 8080,
      registry: {
        listAll: () => Promise.resolve([a, { ...b, labelNamespace: 'dev.custom' }]),
        identities: { root: '/work/acme', activeIdentities: () => Promise.resolve([]), repositoryId: () => Promise.resolve(undefined) }
      },
      runner: docker,
      logger: new RecordingLogger(),
      now: () => new Date('2026-09-21T12:00:00.000Z')
    });

    await index.refresh();

    expect(docker.calls.map((call) => call[3])).toEqual(['label=dev.berth.managed=true', 'label=dev.custom.managed=true']);
    expect(docker.calls.at(-1)?.at(-1)).toContain('{{.Label "dev.custom.repository-uuid"}}');
  });

  it('asks Docker about a namespace only the card records, wherever that card is', async () => {
    const [a, b] = registrations as [RegisteredWorktree, RegisteredWorktree];
    for (const quarantined of [false, true]) {
      const docker = new DockerFake();
      const controlHome = await mkdtemp(path.join(tmpdir(), 'berth-index-'));
      temporary.push(controlHome);
      await secureControlDirectories(controlLayout(controlHome));
      // The registration predates namespace tracking, so only the card knows.
      await writeIndexCard(controlHome, b.repositoryId, b.worktreeId, { ...card(b, 'acme'), labelNamespace: 'dev.custom' });
      if (quarantined) {
        const layout = worktreeLayout(controlHome, b.repositoryId, b.worktreeId);
        await rename(layout.home, layout.quarantinePath);
      }
      const index = new CheckoutIndexService({
        controlHome,
        routerPort: 8080,
        registry: {
          listAll: () => Promise.resolve([a, b]),
          identities: { root: '/work/acme', activeIdentities: () => Promise.resolve([]), repositoryId: () => Promise.resolve(undefined) }
        },
        runner: docker,
        logger: new RecordingLogger(),
        now: () => new Date('2026-09-21T12:00:00.000Z')
      });

      await index.refresh();

      expect(docker.calls.map((call) => call[3]).sort()).toEqual(['label=dev.berth.managed=true', 'label=dev.custom.managed=true']);
    }
  });

  it('shows the page rather than failing when Docker answers with a line it cannot read', async () => {
    const docker = new DockerFake();
    docker.stdout = 'running\tUp 2 seconds\t(healthy)\tr\tw\tapi';
    const s = await service(docker);

    const snapshot = await s.index.refresh();

    expect(snapshot.dockerReachable).toBe(false);
    expect(s.logger.lines.join('\n')).toContain('unrecognized container inventory line');
    expect(await readFile(controlLayout(s.controlHome).indexFile, 'utf8')).toContain('status-unknown');
  });

  it('still finds the namespace in a card the rest of the schema refuses', async () => {
    const docker = new DockerFake();
    const [a, b] = registrations as [RegisteredWorktree, RegisteredWorktree];
    const controlHome = await mkdtemp(path.join(tmpdir(), 'berth-index-'));
    temporary.push(controlHome);
    await secureControlDirectories(controlLayout(controlHome));
    // A key this version does not know: no card at all, as far as the page is
    // concerned — but its containers still have to be found.
    await mkdir(path.dirname(indexCardPath(controlHome, b.repositoryId, b.worktreeId)), { recursive: true });
    await writeFile(indexCardPath(controlHome, b.repositoryId, b.worktreeId),
      JSON.stringify({ ...card(b, 'acme'), labelNamespace: 'dev.custom', somethingNewer: true }));
    const index = new CheckoutIndexService({
      controlHome,
      routerPort: 8080,
      registry: {
        listAll: () => Promise.resolve([a, b]),
        identities: { root: '/work/acme', activeIdentities: () => Promise.resolve([]), repositoryId: () => Promise.resolve(undefined) }
      },
      runner: docker,
      logger: new RecordingLogger(),
      now: () => new Date('2026-09-21T12:00:00.000Z')
    });

    await index.refresh();

    await expect(readIndexCard(controlHome, b.repositoryId, b.worktreeId)).resolves.toBeUndefined();
    expect(docker.calls.map((call) => call[3]).sort()).toEqual(['label=dev.berth.managed=true', 'label=dev.custom.managed=true']);
  });

  it('does not read a file too large to be a card, whatever it would have parsed as', async () => {
    const docker = new DockerFake();
    const s = await service(docker);
    const [a] = registrations as [RegisteredWorktree];
    // A card that would parse, padded past the limit with insignificant space.
    const padded = `${JSON.stringify({ ...card(a, 'acme'), labelNamespace: 'dev.custom' })}${' '.repeat(128 * 1024)}`;
    await writeFile(indexCardPath(s.controlHome, a.repositoryId, a.worktreeId), padded);

    await expect(readIndexCard(s.controlHome, a.repositoryId, a.worktreeId)).resolves.toBeUndefined();
    await s.index.refresh();
    expect(docker.calls.map((call) => call[3])).toEqual(['label=dev.berth.managed=true']);
  });

  it('refuses a card whose service URL is not a plain web address', async () => {
    const s = await service();
    const [a] = registrations as [RegisteredWorktree];
    const refused = [
      'javascript:alert(1)',
      'http://feature-a.acme.localhost\n',
      'http://feature-a.acme.localhost/\u0007',
      'http://feature-a.acme.localhost/\u202e',
      // Reads as the trusted host, resolves to the other one.
      'http://feature-a.acme.localhost@evil.localhost/',
      `http://feature-a.acme.localhost/${'a'.repeat(2048)}`
    ];
    for (const url of refused) {
      await expect(writeIndexCard(s.controlHome, a.repositoryId, a.worktreeId, card(a, 'acme', [{ ...API, url }])))
        .rejects.toThrow();
    }
    // An over-long field, and too many services, are not a card either.
    await expect(writeIndexCard(s.controlHome, a.repositoryId, a.worktreeId,
      card(a, 'acme', [{ ...API, purpose: 'x'.repeat(513) }]))).rejects.toThrow();
    await expect(writeIndexCard(s.controlHome, a.repositoryId, a.worktreeId,
      card(a, 'acme', Array.from({ length: 65 }, (_, index) => ({ ...API, name: `api-${String(index)}` }))))).rejects.toThrow();
    // A path the config's own rules allow, and a service subdomain, stay allowed.
    const quoted = card(a, 'acme', [
      { ...API, url: "http://feature-a.acme.localhost/o'brien" },
      { ...MAIL, url: 'http://mail.feature-a.acme.localhost' }
    ]);
    await expect(writeIndexCard(s.controlHome, a.repositoryId, a.worktreeId, quoted)).resolves.toBeUndefined();
    // And one that reached the disk some other way is simply not a card.
    const hostile = card(a, 'acme', [{ ...API, url: 'javascript:alert(1)' }]);
    await writeFile(indexCardPath(s.controlHome, a.repositoryId, a.worktreeId), JSON.stringify(hostile));
    await expect(readIndexCard(s.controlHome, a.repositoryId, a.worktreeId)).resolves.toBeUndefined();
  });

  it('says the repository could not be identified, rather than blaming the worktree list', async () => {
    const docker = new DockerFake();
    const s = await service(docker);
    const blind = new CheckoutIndexService({
      controlHome: s.controlHome,
      routerPort: 80,
      registry: {
        listAll: () => Promise.resolve([s.a, s.b]),
        identities: {
          root: '/work/acme',
          activeIdentities: () => Promise.resolve([]),
          repositoryId: () => Promise.reject(new Error('fatal: not a git repository'))
        }
      },
      runner: docker,
      logger: s.logger,
      now: () => new Date('2026-09-21T12:00:00.000Z')
    });

    const snapshot = await blind.refresh();

    // Nothing on the page belongs to a repository nobody could name, so no row
    // is Unknown for this reason and none was going to be called missing.
    expect(snapshot.identityError).toEqual({
      reason: 'Git could not identify the repository at /work/acme: fatal: not a git repository',
      rowsUnknown: false
    });
    expect(snapshot.entries.map((entry) => entry.status)).toEqual(['stopped', 'stopped']);
    const html = await readFile(controlLayout(s.controlHome).indexFile, 'utf8');
    expect(html).toContain('Git could not identify the repository at /work/acme');
    expect(html).toContain('No checkout below could be compared against it');
    expect(html).not.toContain('rather than Missing');
    expect(s.logger.lines).toContainEqual(
      'warn: Git could not identify the repository at /work/acme: fatal: not a git repository. The index cannot tell which of its checkouts are missing.'
    );
  });

  it('reads back what it wrote and treats anything else as no card', async () => {
    const s = await service();
    await expect(readIndexCard(s.controlHome, s.a.repositoryId, s.a.worktreeId)).resolves.toMatchObject({ slug: 'feature-a' });
    await expect(readIndexCard(s.controlHome, s.b.repositoryId, s.b.worktreeId)).resolves.toBeUndefined();
  });
});
