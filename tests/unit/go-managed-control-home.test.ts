import { mkdir, readdir, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { list } from '../../src/commands/list.ts';
import { remove } from '../../src/commands/remove.ts';
import { unlock } from '../../src/commands/unlock.ts';
import { down, status, up, urls } from '../../src/commands/worktree.ts';
import { CliError } from '../../src/errors.ts';
import { CONTROL_HOME_MARKER } from '../../src/files.ts';
import { commandWorld, type CommandWorld } from '../helpers/command-world.ts';

// The Go build writes `format 2\n` into the control home's marker; this
// build leaves it empty. The two builds' locks do not exclude each other, so
// every mutating verb must refuse a control home the marker says the Go
// build manages, before it takes a lock or writes anything — and every
// read-only verb must keep working there regardless.

let world: CommandWorld;
afterEach(async () => {
  await world?.dispose();
});

const REFUSAL = /is managed by the Go build of berth; this TypeScript build does not change it\.$/;

/** Marks `world.controlHome` as the Go build's, without creating anything else there. */
async function goManage(target: CommandWorld): Promise<void> {
  await mkdir(target.controlHome, { recursive: true });
  await writeFile(path.join(target.controlHome, CONTROL_HOME_MARKER), 'format 2\n');
}

describe('mutating verbs refuse a Go-managed control home', () => {
  it('up refuses before registering, starting the router, or writing anything', async () => {
    world = await commandWorld();
    await goManage(world);

    await expect(up(world.context())).rejects.toThrow(CliError);
    await expect(up(world.context())).rejects.toThrow(REFUSAL);

    expect(world.routerEnsured).toBe(0);
    expect(world.indexRefreshes).toEqual([]);
    expect(await readdir(world.controlHome)).toEqual([CONTROL_HOME_MARKER]);
  });

  it('down refuses before reading the registration or touching the runtime', async () => {
    world = await commandWorld();
    await goManage(world);

    await expect(down(world.context())).rejects.toThrow(REFUSAL);

    expect(world.indexRefreshes).toEqual([]);
    expect(await readdir(world.controlHome)).toEqual([CONTROL_HOME_MARKER]);
  });

  it('worktree remove refuses before resolving a target', async () => {
    world = await commandWorld();
    await goManage(world);

    await expect(remove(world.context())).rejects.toThrow(REFUSAL);

    expect(world.indexRefreshes).toEqual([]);
    expect(await readdir(world.controlHome)).toEqual([CONTROL_HOME_MARKER]);
  });

  it('worktree unlock refuses before looking for a lock', async () => {
    world = await commandWorld();
    await goManage(world);

    await expect(unlock(world.context())).rejects.toThrow(REFUSAL);

    expect(await readdir(world.controlHome)).toEqual([CONTROL_HOME_MARKER]);
  });
});

describe('read-only verbs still work on a Go-managed control home', () => {
  it('worktree list, status, and urls all keep answering', async () => {
    world = await commandWorld();
    const { registration } = await up(world.context());
    // The Go build only ever takes over a control home this one already used;
    // marking it now stands in for that handoff.
    await writeFile(path.join(world.controlHome, CONTROL_HOME_MARKER), 'format 2\n');

    const context = world.context();
    await expect(list(context)).resolves.toHaveLength(1);
    await expect(status(context)).resolves.toBeDefined();
    await expect(urls(context)).resolves.not.toEqual([]);
    expect(registration.slug).toBe('feature-login');
  });
});

describe('an empty or missing marker leaves today\'s behaviour unchanged', () => {
  it('up succeeds when the control home does not exist yet', async () => {
    world = await commandWorld();

    await expect(up(world.context())).resolves.toBeDefined();
  });

  it('up succeeds when an earlier TypeScript install already left an empty marker', async () => {
    world = await commandWorld();
    await mkdir(world.controlHome, { recursive: true });
    await writeFile(path.join(world.controlHome, CONTROL_HOME_MARKER), '');

    await expect(up(world.context())).resolves.toBeDefined();
  });
});
