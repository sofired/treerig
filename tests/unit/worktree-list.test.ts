import { rm } from 'node:fs/promises';
import { indexCardPath } from '../../src/checkout-index/card.ts';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { formatTable, list } from '../../src/commands/list.ts';
import { up } from '../../src/commands/worktree.ts';
import { commandWorld, type CommandWorld } from '../helpers/command-world.ts';

let world: CommandWorld;
afterEach(async () => {
  await world?.dispose();
});

describe('worktree list', () => {
  it('lists this repository with branch, root, and state, marking the current worktree', async () => {
    world = await commandWorld();
    const other = await world.worktree('other', 'feature/other');
    await up(world.context(other));
    await up(world.context());
    const context = world.context();

    const rows = await list(context);

    expect(rows).toEqual([
      expect.objectContaining({ host: 'feature-login.acme.localhost', state: 'ready', branch: 'feature/login', root: world.root, current: true }),
      expect.objectContaining({ host: 'feature-other.acme.localhost', state: 'ready', branch: 'feature/other', root: other, current: false })
    ]);
    expect(context.logger.printed).toEqual([
      '  HOST                          STATE  BRANCH         ROOT',
      `* feature-login.acme.localhost  ready  feature/login  ${world.root}`,
      `  feature-other.acme.localhost  ready  feature/other  ${other}`
    ]);
  });

  it('reports a registration whose worktree Git no longer lists as missing (with its last-seen branch), and one whose first up never finished as pending', async () => {
    world = await commandWorld();
    const gone = await world.worktree('gone', 'feature/gone');
    const half = await world.worktree('half', 'feature/half');
    await up(world.context());
    await up(world.context(gone));
    await world.context(half).registry.ensure();   // registered, never brought up
    world.runner.active = world.runner.active.filter((root) => root !== gone);
    await rm(gone, { recursive: true, force: true });

    const rows = await list(world.context());

    expect(rows.map((row) => [row.slug, row.state, row.branch ?? '-'])).toEqual([
      ['feature-gone', 'missing', 'feature/gone'],
      ['feature-half', 'pending', 'feature/half'],
      ['feature-login', 'ready', 'feature/login']
    ]);
  });

  it('says a detached checkout is detached, in the index\'s words', async () => {
    world = await commandWorld();
    const detached = await world.worktree('detached', '');
    await up(world.context(detached));
    const context = world.context(detached);

    const rows = await list(context);

    expect(rows.map((row) => [row.slug, row.branch])).toEqual([['detached', '']]);
    expect(context.logger.printed[1]).toContain('(detached)');
  });

  it('--all spans repositories and domains, with last-seen detail for the others', async () => {
    world = await commandWorld();
    const widgets = await world.worktree('widgets', 'main', path.join(world.base, 'widgets-common'));
    await up(world.context());
    await up(world.context(widgets, 'widgets.localhost'));

    const local = await list(world.context());
    expect(local.map((row) => row.host)).toEqual(['feature-login.acme.localhost']);

    const everything = await list(world.context(), { all: true });
    expect(everything.map((row) => [row.host, row.state, row.root ?? '-', row.current])).toEqual([
      ['feature-login.acme.localhost', 'ready', world.root, true],
      ['main.widgets.localhost', 'ready', widgets, false]
    ]);

    // With the other repository's card gone, nothing records its branch.
    const [, other] = everything;
    if (!other) throw new Error('fixture');
    await rm(indexCardPath(world.controlHome, other.repositoryId, other.worktreeId), { force: true });
    const forgotten = await list(world.context(), { all: true });
    expect(forgotten.map((row) => row.branch)).toEqual(['feature/login', undefined]);
    expect(world.context().logger.printed.join('\n')).not.toContain('(no branch)');
  });

  it('--json prints the rows', async () => {
    world = await commandWorld();
    await up(world.context());
    const context = world.context();
    const rows = await list(context, { json: true });
    expect(JSON.parse(context.logger.printed.join('\n'))).toEqual(rows);
  });

  it('keeps a branch name from driving the terminal, in the table and in the JSON', async () => {
    world = await commandWorld();
    // A clone chooses its own branch names: an escape sequence here would move
    // a real cursor, and a right-to-left override would reverse the row.
    const hostile = await world.worktree('hostile', 'feature/\u001b[2K\u202ex');
    await up(world.context(hostile));
    const context = world.context(hostile);

    await list(context);
    const table = context.logger.printed.join('\n');
    expect([...table].some((character) => character === '\u001b' || character === '\u202e')).toBe(false);
    expect(table).toContain('\uFFFD');
    // Still a table: the widths are measured after the replacement, not before.
    const lines = context.logger.printed.filter(Boolean);
    expect(new Set(lines.map((line) => line.trimEnd().length)).size).toBeLessThanOrEqual(lines.length);

    const json = world.context(hostile);
    await list(json, { json: true });
    const printed = json.logger.printed.join('\n');
    expect(printed.includes('\u202e')).toBe(false);
    expect(printed).toContain('\\u202e');
    const parsed = JSON.parse(printed) as { branch: string }[];
    expect(parsed[0]?.branch).toBe('feature/\u001b[2K\u202ex');
  });

  it('says so when nothing is registered', async () => {
    world = await commandWorld();
    const context = world.context();
    await expect(list(context)).resolves.toEqual([]);
    // Nothing to pipe, so nothing on stdout: the sentence is a notice.
    expect(context.logger.printed).toEqual([]);
    expect(context.logger.lines).toContainEqual('info: No worktrees are registered for this repository.');
    expect(formatTable([], true)).toEqual(['No worktrees are registered on this machine.']);
  });
});
