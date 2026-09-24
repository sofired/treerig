import { mkdir, readdir, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { list } from '../../src/commands/list.ts';
import { remove } from '../../src/commands/remove.ts';
import { unlock } from '../../src/commands/unlock.ts';
import { down, status, up, urls } from '../../src/commands/worktree.ts';
import { CliError } from '../../src/errors.ts';
import { CONTROL_HOME_MARKER, pathExists } from '../../src/files.ts';
import { worktreeLayout } from '../../src/layout.ts';
import type { Runtime } from '../../src/runtime/runtime.ts';
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

describe('the recheck inside the lock closes the gap between the first check and the first write', () => {
  it('worktree remove refuses when the marker turns up while the confirmation prompt would be open, deleting nothing', async () => {
    world = await commandWorld();
    const { registration } = await up(world.context());
    const ref = { repositoryId: registration.repositoryId, worktreeId: registration.worktreeId, slug: registration.slug };
    const before = await world.runtime.inventory(ref);
    expect(before.length).toBeGreaterThan(0);   // there is something for `remove` to delete, if it is not stopped

    const context = world.context();
    const marker = path.join(world.controlHome, CONTROL_HOME_MARKER);
    // `runtime.inventory` is what `remove` calls right before it shows the
    // plan and waits on the confirmation phrase: writing the marker here
    // stands in for the Go build claiming the control home while a person is
    // still looking at that prompt, well before `remove`'s lock is taken.
    const runtime: Runtime = {
      kind: context.runtime.kind,
      up: context.runtime.up.bind(context.runtime),
      down: context.runtime.down.bind(context.runtime),
      status: context.runtime.status.bind(context.runtime),
      endpoints: context.runtime.endpoints.bind(context.runtime),
      reap: context.runtime.reap.bind(context.runtime),
      inventory: async (target) => {
        await writeFile(marker, 'format 2\n');
        return context.runtime.inventory(target);
      }
    };
    const sabotaged = { ...context, runtimeFor: () => runtime };

    // `approved: true` skips only the terminal read; the plan is still built
    // from `runtime.inventory` first, exactly as when a person is asked.
    await expect(remove(sabotaged, { approved: true })).rejects.toThrow(REFUSAL);

    await expect(world.runtime.inventory(ref)).resolves.toEqual(before);
    expect(await pathExists(worktreeLayout(world.controlHome, ref.repositoryId, ref.worktreeId).home)).toBe(true);
  });

  it('up refuses when the marker turns up while the router would still be starting, creating nothing', async () => {
    world = await commandWorld();
    const context = world.context();
    const marker = path.join(world.controlHome, CONTROL_HOME_MARKER);
    // `router.ensure()` can take minutes in production; writing the marker
    // from inside it stands in for the Go build claiming the control home
    // during that wait, well before `up`'s own lock is taken.
    const sabotaged = {
      ...context,
      router: { ...context.router, ensure: () => writeFile(marker, 'format 2\n') }
    };

    await expect(up(sabotaged)).rejects.toThrow(REFUSAL);

    // `registry.ensure()` ran first (it always does, before the router), so
    // there is a registration; nothing past it — the home, the containers,
    // the hooks — was ever reached.
    const found = await context.registry.current();
    if (!found) throw new Error('expected a registration');
    expect(await pathExists(worktreeLayout(world.controlHome, found.repositoryId, found.worktreeId).home)).toBe(false);
    await expect(world.runtime.inventory({ repositoryId: found.repositoryId, worktreeId: found.worktreeId, slug: found.slug }))
      .resolves.toEqual([]);
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
