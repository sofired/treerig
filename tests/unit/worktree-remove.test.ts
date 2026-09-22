import { chmod, mkdir, rm, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { indexCardPath } from '../../src/checkout-index/card.ts';
import { remove } from '../../src/commands/remove.ts';
import { up } from '../../src/commands/worktree.ts';
import { CliError } from '../../src/errors.ts';
import { pathExists } from '../../src/files.ts';
import { worktreeLayout } from '../../src/layout.ts';
import { silentLogger } from '../../src/logger.ts';
import { MemoryRuntime } from '../../src/runtime/memory.ts';
import type { OwnedResource, Runtime, WorktreeRef } from '../../src/runtime/runtime.ts';
import { WorktreeRegistry } from '../../src/worktree-registry.ts';
import { commandWorld, type CommandWorld } from '../helpers/command-world.ts';

let world: CommandWorld;
afterEach(async () => {
  await world?.dispose();
});

const ref = (r: { repositoryId: string; worktreeId: string; slug: string }): WorktreeRef =>
  ({ repositoryId: r.repositoryId, worktreeId: r.worktreeId, slug: r.slug });

describe('up after a concurrent removal', () => {
  it('creates nothing when the registration vanished while the router was starting', async () => {
    world = await commandWorld();
    const first = await up(world.context(), { services: ['api'] });
    await remove(world.context(), { approved: true });
    // Re-register, then let the router step (which runs outside the worktree lock) remove the record again.
    const context = world.context();
    const registered = await context.registry.ensure();
    const sabotaged = {
      ...context,
      router: {
        ...context.router,
        ensure: () => context.registry.store(registered.repositoryId).remove(registered.worktreeId, registered.slug, context.logger).then(() => undefined)
      }
    };

    await expect(up(sabotaged, { services: ['api'] })).rejects.toThrow("was removed while 'berth up' was waiting for the shared router; run it again to re-register");

    await expect(world.runtime.inventory(ref(registered))).resolves.toEqual([]);
    await expect(world.runtime.inventory(ref(first.registration))).resolves.toEqual([]);
    const layout = worktreeLayout(world.controlHome, registered.repositoryId, registered.worktreeId);
    expect(await pathExists(layout.home)).toBe(false);
    expect(await pathExists(indexCardPath(world.controlHome, registered.repositoryId, registered.worktreeId))).toBe(false);

    // And the re-run the message asks for succeeds: only the record went, so the marker's identity is reused.
    const again = await up(world.context(), { services: ['api'] });
    expect(again.registration.worktreeId).toBe(registered.worktreeId);
    await expect(world.runtime.inventory(ref(again.registration))).resolves.toHaveLength(2);   // the api container and the data volume
  });

  it('creates nothing when the checkout\'s markers changed while the router was starting', async () => {
    world = await commandWorld();
    const context = world.context();
    const registered = await context.registry.ensure();
    const sabotaged = {
      ...context,
      router: {
        ...context.router,
        ensure: async () => {
          // Markers wiped and re-minted: the record survives, the identity does not match it.
          const identity = await context.registry.identities.inspect();
          await context.registry.identities.removeWorktreeMarker(identity!);
          await context.registry.identities.ensure();
        }
      }
    };

    await expect(up(sabotaged, { services: ['api'] })).rejects.toThrow("markers changed or were removed while 'berth up' was waiting for the shared router");

    await expect(world.runtime.inventory(ref(registered))).resolves.toEqual([]);
    expect(await pathExists(worktreeLayout(world.controlHome, registered.repositoryId, registered.worktreeId).home)).toBe(false);
  });
});

describe('up after a change of label namespace', () => {
  it('refuses while resources carry the old labels, and proceeds once they are removed', async () => {
    world = await commandWorld();
    const started = await up(world.context(), { services: ['api'] });
    const adopting = () => world.context(world.root, 'acme.localhost', 'dev.other');

    await expect(up(adopting(), { services: ['api'] })).rejects.toThrow(
      "This worktree's resources are labelled under dev.berth, but berth.toml now says dev.other. Run 'berth worktree remove' first"
    );
    await expect(world.context().registry.current()).resolves.toMatchObject({ labelNamespace: 'dev.berth' });   // record untouched
    await expect(world.runtime.inventory(ref(started.registration))).resolves.toHaveLength(2);

    await remove(world.context(), { approved: true });   // no flag needed: the record knows
    const relabelled = await up(adopting(), { services: ['api'] });
    expect(relabelled.registration.labelNamespace).toBe('dev.other');
  });

  it('does not tell the user to run up from inside up when only the domain changed', async () => {
    world = await commandWorld();
    await up(world.context(), { services: ['api'] });
    const moved = world.context(world.root, 'renamed.localhost');
    await up(moved, { services: ['api'] });
    const warnings = moved.logger.lines.filter((line) => line.startsWith('warn:'));
    expect(warnings).toEqual([expect.stringContaining('This worktree moved from feature-login.acme.localhost to feature-login.renamed.localhost')]);
  });

  it('reads the namespace from the card for a worktree registered before records carried one', async () => {
    world = await commandWorld();
    const started = await up(world.context(), { services: ['api'] });
    // Strip the namespace from the record, as an older tool version would have left it.
    const older = new WorktreeRegistry({ controlHome: world.controlHome, root: world.root, domain: 'acme.localhost', runner: world.runner, logger: silentLogger });
    const store = older.store(started.registration.repositoryId);
    await store.remove(started.registration.worktreeId, 'feature-login', silentLogger);
    const re = await older.ensure();
    expect(re.labelNamespace).toBeUndefined();

    await expect(up(world.context(world.root, 'acme.localhost', 'dev.other'), { services: ['api'] }))
      .rejects.toThrow("labelled under dev.berth, but berth.toml now says dev.other");
  });

  it('refuses under the lock when another up re-labelled the worktree while the router was starting', async () => {
    world = await commandWorld();
    await up(world.context(), { services: ['api'] });
    const context = world.context();
    const sabotaged = {
      ...context,
      router: {
        ...context.router,
        ensure: async () => {
          // A concurrent `up` under a config that says dev.other, after this one's own check passed.
          for (const resource of await world.runtime.inventory(ref((await context.registry.current())!))) {
            await world.runtime.reap(ref((await context.registry.current())!), resource);
          }
          await new WorktreeRegistry({
            controlHome: world.controlHome, root: world.root, domain: 'acme.localhost',
            labelNamespace: 'dev.other', runner: world.runner, logger: silentLogger
          }).ensure();
        }
      }
    };
    await expect(up(sabotaged, { services: ['api'] })).rejects.toThrow("was re-labelled under dev.other while 'berth up' was waiting for the shared router; run it again");
  });
});

describe('worktree remove', () => {
  it('removes the current worktree: resources, home, registration, and identity marker, so the next up starts fresh', async () => {
    world = await commandWorld();
    const started = await up(world.context(), { services: ['api', 'mail'] });
    const context = world.context();
    const layout = worktreeLayout(world.controlHome, started.registration.repositoryId, started.registration.worktreeId);
    expect(await pathExists(layout.home)).toBe(true);

    const result = await remove(context, { approved: true });

    expect(result.resources.map((r) => r.kind)).toEqual(['container', 'container', 'volume']);   // what stopping removed and what reap found, in inventory order
    expect(result).toMatchObject({ home: true, registration: true, marker: true });
    await expect(world.runtime.inventory(ref(started.registration))).resolves.toEqual([]);
    expect(await pathExists(layout.home)).toBe(false);
    expect(await pathExists(layout.quarantinePath)).toBe(false);
    await expect(context.registry.current()).resolves.toBeUndefined();
    await expect(context.registry.identities.inspect()).resolves.toBeUndefined();
    expect(context.logger.lines.at(-1)).toBe(
      'info: Removed feature-login.acme.localhost: 2 containers, 1 volume, home directory, registration, identity marker.'
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

    await expect(world.runtime.inventory(ref(started.registration))).resolves.toHaveLength(2);
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
    const context = { ...world.context(), runtime: suspicious, runtimeFor: () => suspicious };
    const layout = worktreeLayout(world.controlHome, started.registration.repositoryId, started.registration.worktreeId);

    await expect(remove(context, { approved: true })).rejects.toThrow('Refusing to touch volume v9');

    await expect(honest.inventory(ref(started.registration))).resolves.toHaveLength(2);
    expect(await pathExists(layout.home)).toBe(true);
    await expect(context.registry.current()).resolves.toMatchObject({ slug: 'feature-login' });
    // Nothing was destroyed, so nothing is pending: the next `up` must not seed over intact data.
    const store = context.registry.store(started.registration.repositoryId);
    await expect(store.initialization(started.registration.worktreeId, 'feature-login')).resolves.toBe('complete');
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
    const context = { ...world.context(), runtime: flaky, runtimeFor: () => flaky };

    await expect(remove(context, { approved: true })).rejects.toThrow('Refusing to remove volume somebody-elses');

    const registration = await context.registry.current();
    // Pending again — with the namespace the reap ran under still recorded.
    expect(registration).toMatchObject({ slug: 'feature-login', labelNamespace: 'dev.berth' });
    // Data may already be gone, so the record is pending again: an `up` now would seed, not resume.
    const store = context.registry.store(registration!.repositoryId);
    await expect(store.initialization(registration!.worktreeId, 'feature-login')).resolves.toBe('pending');
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

  it('removes an orphaned registration whose Git worktree is gone, stopping its containers, with no marker to remove', async () => {
    world = await commandWorld();
    const other = await world.worktree('gone', 'feature/gone');
    await up(world.context(), { services: ['api'] });
    const orphan = await up(world.context(other), { services: ['api'] });
    world.runner.active = world.runner.active.filter((root) => root !== other);
    await rm(other, { recursive: true, force: true });

    const result = await remove(world.context(), { identifier: orphan.registration.worktreeId, approved: true });

    expect(result).toMatchObject({ registration: true, home: true, marker: false });
    expect(result.resources.map((r) => r.kind)).toEqual(['container', 'volume']);
    await expect(world.context().registry.list()).resolves.toHaveLength(1);
  });

  it('stops a target gracefully when it has containers, and skips the stop when it has none', async () => {
    world = await commandWorld();
    const other = await world.worktree('other', 'feature/other');
    const mine = await up(world.context(), { services: ['api'] });
    const otherUp = await up(world.context(other), { services: ['api'] });
    const honest = world.runtime;
    const stopped: string[] = [];
    const observed: Runtime = {
      kind: 'observed',
      up: (r, s) => honest.up(r, s),
      down: (r) => { stopped.push(r.slug); return honest.down(r); },
      status: (r) => honest.status(r),
      endpoints: (r) => honest.endpoints(r),
      inventory: (r) => honest.inventory(r),
      reap: (r, resource) => honest.reap(r, resource)
    };
    // An orphan (Git no longer lists it) still gets a graceful stop: `down` needs no checkout.
    world.runner.active = world.runner.active.filter((root) => root !== other);
    await rm(other, { recursive: true, force: true });
    const observing = () => ({ ...world.context(), runtime: observed, runtimeFor: () => observed });
    await remove(observing(), { identifier: otherUp.registration.worktreeId, approved: true });
    expect(stopped).toEqual(['feature-other']);

    // Nothing left to stop: containers already reaped by hand.
    for (const resource of await honest.inventory(ref(mine.registration))) await honest.reap(ref(mine.registration), resource);
    await remove(observing(), { approved: true });
    expect(stopped).toEqual(['feature-other']);
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

  it('reaps a target through the runtime for the namespace it registered with, leaving this project\'s runtime alone', async () => {
    world = await commandWorld();
    const started = await up(world.context(), { services: ['api'] });
    const { repositoryId, worktreeId, slug } = started.registration;
    // As if this worktree had been registered by a config that adopted a predecessor's labels.
    const predecessor = new WorktreeRegistry({
      controlHome: world.controlHome, root: world.root, domain: 'acme.localhost',
      labelNamespace: 'dev.predecessor', runner: world.runner, logger: silentLogger
    });
    await predecessor.ensure();
    const theirs = new MemoryRuntime({ services: ['legacy-db'] });
    await theirs.up({ repositoryId, worktreeId, slug }, [{ name: 'legacy-db' }]);
    const asked: string[] = [];
    const context = {
      ...world.context(),
      runtimeFor: (namespace: string) => { asked.push(namespace); return namespace === 'dev.predecessor' ? theirs : world.runtime; }
    };

    const result = await remove(context, { approved: true });

    expect(asked).toEqual(['dev.predecessor']);
    expect(result.resources.map((r) => r.kind)).toEqual(['container', 'volume']);
    await expect(theirs.inventory({ repositoryId, worktreeId, slug })).resolves.toEqual([]);
    await expect(world.runtime.inventory({ repositoryId, worktreeId, slug })).resolves.toHaveLength(2);   // untouched
  });

  it('finds a foreign worktree\'s namespace in its registration even after an interrupted removal took its card', async () => {
    world = await commandWorld();
    const widgets = await world.worktree('widgets', 'main', path.join(world.base, 'widgets-common'));
    await up(world.context(), { services: ['api'] });
    const foreign = await up(world.context(widgets, 'widgets.localhost'), { services: ['api'] });
    await rm(indexCardPath(world.controlHome, foreign.registration.repositoryId, foreign.registration.worktreeId));
    const asked: string[] = [];
    const context = { ...world.context(), runtimeFor: (namespace: string) => { asked.push(namespace); return world.runtime; } };

    const result = await remove(context, { identifier: foreign.registration.worktreeId, approved: true });

    expect(asked).toEqual(['dev.berth']);
    expect(result.registration).toBe(true);
  });

  it('refuses a foreign worktree that nothing on disk describes, and accepts --label-namespace for it', async () => {
    world = await commandWorld();
    const widgets = await world.worktree('widgets', 'main', path.join(world.base, 'widgets-common'));
    await up(world.context(), { services: ['api'] });
    // Registered by a tool version that recorded no namespace, and never brought up: no card either.
    const older = new WorktreeRegistry({
      controlHome: world.controlHome, root: widgets, domain: 'widgets.localhost', runner: world.runner, logger: silentLogger
    });
    const registered = await older.ensure();

    await expect(remove(world.context(), { identifier: registered.worktreeId, approved: true })).rejects.toThrow(
      /Cannot remove main\.widgets\.localhost from this repository: it belongs to a different one[\s\S]*--label-namespace \(dev\.berth unless/
    );
    await expect(remove(world.context(), { identifier: registered.worktreeId, approved: true, labelNamespace: 'not a namespace' }))
      .rejects.toThrow('--label-namespace must look like a reverse domain');

    const asked: string[] = [];
    const context = { ...world.context(), runtimeFor: (namespace: string) => { asked.push(namespace); return world.runtime; } };
    const result = await remove(context, { identifier: registered.worktreeId, approved: true, labelNamespace: 'dev.legacy' });
    expect(asked).toEqual(['dev.legacy']);
    expect(result.registration).toBe(true);
  });

  it('refuses --label-namespace when the registration records a different one, rather than orphaning the data', async () => {
    world = await commandWorld();
    const started = await up(world.context(), { services: ['api'] });
    await expect(remove(world.context(), { approved: true, labelNamespace: 'dev.wrong' })).rejects.toThrow(
      'feature-login.acme.localhost records its resources as labelled under dev.berth, not dev.wrong. Drop --label-namespace, or pass the recorded one.'
    );
    await expect(world.context().registry.current()).resolves.toMatchObject({ slug: 'feature-login' });
    await expect(world.runtime.inventory(ref(started.registration))).resolves.toHaveLength(2);
    // The recorded one, spelled out, is fine.
    const result = await remove(world.context(), { approved: true, labelNamespace: 'dev.berth' });
    expect(result.registration).toBe(true);
  });

  it('does nothing, gracefully, when another removal finished while it waited for the lock', async () => {
    world = await commandWorld();
    await up(world.context(), { services: ['api'] });
    const honest = world.runtime;
    let raced = false;
    // The rival wins after this call resolved its target but before it took the lock: the
    // pre-prompt inventory is the seam.
    const racing: Runtime = {
      kind: 'racing',
      up: (r, s) => honest.up(r, s),
      down: (r) => honest.down(r),
      status: (r) => honest.status(r),
      endpoints: (r) => honest.endpoints(r),
      inventory: async (r) => {
        if (!raced) {
          raced = true;
          await remove(world.context(), { approved: true });
        }
        return honest.inventory(r);
      },
      reap: (r, resource) => honest.reap(r, resource)
    };
    const context = { ...world.context(), runtime: racing, runtimeFor: () => racing };

    const late = await remove(context, { approved: true });

    expect(late).toMatchObject({ alreadyGone: true, resources: [], home: false, registration: false, marker: false });
    expect(context.logger.lines).toContainEqual("info: feature-login.acme.localhost was already removed while 'berth worktree remove' was waiting; nothing left to do.");
    expect(context.logger.lines.some((line) => line.startsWith('info: Removed '))).toBe(false);
  });

  it('runs again when an up re-labelled the worktree while it waited for the lock', async () => {
    world = await commandWorld();
    await up(world.context(), { services: ['api'] });
    const honest = world.runtime;
    let raced = false;
    const racing: Runtime = {
      kind: 'racing',
      up: (r, s) => honest.up(r, s),
      down: (r) => honest.down(r),
      status: (r) => honest.status(r),
      endpoints: (r) => honest.endpoints(r),
      inventory: async (r) => {
        if (!raced) {
          raced = true;
          const relabelled = new WorktreeRegistry({
            controlHome: world.controlHome, root: world.root, domain: 'acme.localhost',
            labelNamespace: 'dev.other', runner: world.runner, logger: silentLogger
          });
          await relabelled.ensure();
        }
        return honest.inventory(r);
      },
      reap: (r, resource) => honest.reap(r, resource)
    };
    const context = { ...world.context(), runtime: racing, runtimeFor: () => racing };

    await expect(remove(context, { approved: true })).rejects.toThrow("was re-labelled under dev.other while 'berth worktree remove' was waiting; run it again");
    await expect(context.registry.current()).resolves.toMatchObject({ labelNamespace: 'dev.other' });
    await expect(honest.inventory(ref((await context.registry.current())!))).resolves.toHaveLength(2);   // nothing destroyed
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
