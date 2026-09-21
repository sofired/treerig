import { chmod, mkdir, rm, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { remove } from '../../src/commands/remove.ts';
import { up } from '../../src/commands/worktree.ts';
import { CliError } from '../../src/errors.ts';
import { pathExists } from '../../src/files.ts';
import { worktreeLayout } from '../../src/layout.ts';
import type { OwnedResource, Runtime, WorktreeRef } from '../../src/runtime/runtime.ts';
import { commandWorld, type CommandWorld } from '../helpers/command-world.ts';

let world: CommandWorld;
afterEach(async () => {
  await world?.dispose();
});

const ref = (r: { repositoryId: string; worktreeId: string; slug: string }): WorktreeRef =>
  ({ repositoryId: r.repositoryId, worktreeId: r.worktreeId, slug: r.slug });

describe('worktree remove', () => {
  it('removes the current worktree: resources, home, registration, and identity marker, so the next up starts fresh', async () => {
    world = await commandWorld();
    const started = await up(world.context(), { services: ['api', 'mail'] });
    const context = world.context();
    const layout = worktreeLayout(world.controlHome, started.registration.repositoryId, started.registration.worktreeId);
    expect(await pathExists(layout.home)).toBe(true);

    const result = await remove(context, { approved: true });

    expect(result.resources.map((r) => r.service).sort()).toEqual(['api', 'mail']);
    expect(result).toMatchObject({ home: true, registration: true, marker: true });
    await expect(world.runtime.inventory(ref(started.registration))).resolves.toEqual([]);
    expect(await pathExists(layout.home)).toBe(false);
    expect(await pathExists(layout.quarantinePath)).toBe(false);
    await expect(context.registry.current()).resolves.toBeUndefined();
    await expect(context.registry.identities.inspect()).resolves.toBeUndefined();
    expect(context.logger.lines.at(-1)).toBe(
      'info: Removed feature-login.acme.localhost: 2 containers, home directory, registration, identity marker.'
    );

    const again = await up(world.context(), { services: ['api'] });
    expect(again.registration.worktreeId).not.toBe(started.registration.worktreeId);
    expect(again.registration.slug).toBe('feature-login');
  });

  it('requires the hostname as a confirmation phrase and touches nothing without it', async () => {
    world = await commandWorld();
    const started = await up(world.context(), { services: ['api'] });
    const context = world.context();

    await expect(remove(context)).rejects.toThrow('Interactive confirmation required: "feature-login.acme.localhost".');

    await expect(world.runtime.inventory(ref(started.registration))).resolves.toHaveLength(1);
    await expect(context.registry.current()).resolves.toMatchObject({ slug: 'feature-login' });
  });

  it('stops before the prompt when a resource cannot be proven ours, and keeps the registration', async () => {
    world = await commandWorld();
    const started = await up(world.context(), { services: ['api'] });
    const honest = world.runtime;
    // A backend whose inventory contains something it cannot verify.
    const suspicious: Runtime = {
      kind: 'suspicious',
      up: (r, s) => honest.up(r, s),
      down: (r) => honest.down(r),
      status: (r) => honest.status(r),
      endpoints: (r) => honest.endpoints(r),
      inventory: () => Promise.reject(new CliError('Refusing to touch volume v9: its ownership labels do not match this worktree.')),
      reap: (r, resource) => honest.reap(r, resource)
    };
    const context = { ...world.context(), runtime: suspicious };
    const layout = worktreeLayout(world.controlHome, started.registration.repositoryId, started.registration.worktreeId);

    await expect(remove(context, { approved: true })).rejects.toThrow('Refusing to touch volume v9');

    await expect(honest.inventory(ref(started.registration))).resolves.toHaveLength(1);
    expect(await pathExists(layout.home)).toBe(true);
    await expect(context.registry.current()).resolves.toMatchObject({ slug: 'feature-login' });
  });

  it('keeps the registration when a reap fails midway, so a retry can finish', async () => {
    world = await commandWorld();
    await up(world.context(), { services: ['api'] });
    const honest = world.runtime;
    const foreign: OwnedResource = { kind: 'volume', id: 'somebody-elses' };
    // Inventory looks fine; the last resource turns out not to be ours at reap time.
    const flaky: Runtime = {
      kind: 'flaky',
      up: (r, s) => honest.up(r, s),
      down: (r) => honest.down(r),
      status: (r) => honest.status(r),
      endpoints: (r) => honest.endpoints(r),
      inventory: async (r) => [...await honest.inventory(r), foreign],
      reap: (r, resource) => honest.reap(r, resource)
    };
    const context = { ...world.context(), runtime: flaky };

    await expect(remove(context, { approved: true })).rejects.toThrow('Refusing to remove volume somebody-elses');

    await expect(context.registry.current()).resolves.toMatchObject({ slug: 'feature-login' });
    const retry = await remove(world.context(), { approved: true });
    expect(retry).toMatchObject({ registration: true, marker: true });
  });

  it('removes another worktree of this repository by slug, including its identity marker', async () => {
    world = await commandWorld();
    const other = await world.worktree('other', 'feature/other');
    await up(world.context(), { services: ['api'] });
    const otherUp = await up(world.context(other), { services: ['api'] });

    const result = await remove(world.context(), { identifier: 'feature-other', approved: true });

    expect(result.target.worktreeId).toBe(otherUp.registration.worktreeId);
    expect(result.marker).toBe(true);
    await expect(world.context(other).registry.current()).resolves.toBeUndefined();
    await expect(world.context(other).registry.identities.inspect()).resolves.toBeUndefined();
    // This worktree is untouched.
    await expect(world.context().registry.current()).resolves.toMatchObject({ slug: 'feature-login' });
    await expect(world.context().registry.list()).resolves.toHaveLength(1);
  });

  it('removes an orphaned registration whose Git worktree is gone, with nothing to stop and no marker to remove', async () => {
    world = await commandWorld();
    const other = await world.worktree('gone', 'feature/gone');
    await up(world.context(), { services: ['api'] });
    const orphan = await up(world.context(other), { services: ['api'] });
    world.runner.active = world.runner.active.filter((root) => root !== other);
    await rm(other, { recursive: true, force: true });

    const result = await remove(world.context(), { identifier: orphan.registration.worktreeId, approved: true });

    expect(result).toMatchObject({ registration: true, home: true, marker: false });
    expect(result.resources).toHaveLength(1);
    await expect(world.context().registry.list()).resolves.toHaveLength(1);
  });

  it('finishes an interrupted removal that left a quarantine directory behind', async () => {
    world = await commandWorld();
    const started = await up(world.context(), { services: ['api'] });
    const layout = worktreeLayout(world.controlHome, started.registration.repositoryId, started.registration.worktreeId);
    await rm(layout.home, { recursive: true, force: true });
    await mkdir(layout.quarantinePath, { recursive: true });

    const result = await remove(world.context(), { approved: true });

    expect(result.home).toBe(true);
    expect(await pathExists(layout.quarantinePath)).toBe(false);
  });

  // Root ignores mode bits, so the provocation below cannot work as uid 0.
  it.skipIf(process.getuid?.() === 0)('explains a home it cannot delete, and finishes once the files are reclaimed', async () => {
    world = await commandWorld();
    const started = await up(world.context(), { services: ['api'] });
    const layout = worktreeLayout(world.controlHome, started.registration.repositoryId, started.registration.worktreeId);
    // A directory the owner cannot unlink from stands in for files another uid left behind.
    const stubborn = path.join(layout.home, 'data');
    await mkdir(stubborn);
    await writeFile(path.join(stubborn, 'base'), '');
    await chmod(stubborn, 0o000);
    const reclaim = async (): Promise<void> => {
      for (const candidate of [path.join(layout.quarantinePath, 'data'), stubborn]) {
        await chmod(candidate, 0o700).catch(() => undefined);
      }
    };
    try {
      // The chown must name this user's uid literally: the command runs as root inside the container.
      await expect(remove(world.context(), { approved: true })).rejects.toThrow(
        new RegExp(String.raw`owned by another user[\s\S]*chown -R ${process.getuid?.() ?? 0}:${process.getgid?.() ?? 0} /d[\s\S]*'berth worktree remove' again`)
      );
      // The registration survives, so the removal can be run again once the files are reclaimed.
      await expect(world.context().registry.current()).resolves.toMatchObject({ slug: 'feature-login' });
    } finally {
      await reclaim();
    }

    const retry = await remove(world.context(), { approved: true });

    expect(retry).toMatchObject({ home: true, registration: true });
    expect(await pathExists(layout.home)).toBe(false);
    expect(await pathExists(layout.quarantinePath)).toBe(false);
    await expect(world.context().registry.current()).resolves.toBeUndefined();
  });

  it('names what it could not find', async () => {
    world = await commandWorld();
    await up(world.context());
    await expect(remove(world.context(), { identifier: 'nope', approved: true }))
      .rejects.toThrow('No worktree is registered as nope.');
    await expect(remove(world.context(), { identifier: 'nope', domain: 'x.localhost', approved: true }))
      .rejects.toThrow('No worktree is registered as nope under x.localhost.');
  });

  it('refuses when this worktree was never registered', async () => {
    world = await commandWorld();
    await expect(remove(world.context(), { approved: true }))
      .rejects.toThrow("This worktree is not registered; nothing to remove. Name another with 'berth worktree remove <slug|uuid>'.");
  });
});
