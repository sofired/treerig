import { rm } from 'node:fs/promises';
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

  it('reports a registration whose worktree Git no longer lists as orphaned, and one whose first up never finished as pending', async () => {
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
      ['feature-gone', 'orphaned', '-'],
      ['feature-half', 'pending', 'feature/half'],
      ['feature-login', 'ready', 'feature/login']
    ]);
  });

  it('--all spans repositories and domains, without Git detail for the others', async () => {
    world = await commandWorld();
    const widgets = await world.worktree('widgets', 'main', path.join(world.base, 'widgets-common'));
    await up(world.context());
    await up(world.context(widgets, 'widgets.localhost'));

    const local = await list(world.context());
    expect(local.map((row) => row.host)).toEqual(['feature-login.acme.localhost']);

    const everything = await list(world.context(), { all: true });
    expect(everything.map((row) => [row.host, row.state, row.root ?? '-', row.current])).toEqual([
      ['feature-login.acme.localhost', 'ready', world.root, true],
      ['main.widgets.localhost', 'ready', '-', false]
    ]);
  });

  it('--json prints the rows', async () => {
    world = await commandWorld();
    await up(world.context());
    const context = world.context();
    const rows = await list(context, { json: true });
    expect(JSON.parse(context.logger.printed.join('\n'))).toEqual(rows);
  });

  it('says so when nothing is registered', async () => {
    world = await commandWorld();
    const context = world.context();
    await expect(list(context)).resolves.toEqual([]);
    expect(context.logger.printed).toEqual(['No worktrees are registered for this repository.']);
    expect(formatTable([], true)).toEqual(['No worktrees are registered on this machine.']);
  });
});
