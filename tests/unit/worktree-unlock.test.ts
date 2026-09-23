import { lstat, mkdir, utimes, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { unlock, type UnlockDependencies } from '../../src/commands/unlock.ts';
import { up } from '../../src/commands/worktree.ts';
import { ScopedLock, worktreeLocks, type HolderRecord } from '../../src/operation-lock.ts';
import type { ProcessIdentity, ProcessScan, ProcessState, ProcessTable } from '../../src/process-table.ts';
import { commandWorld, type CommandWorld } from '../helpers/command-world.ts';

const SELF: ProcessIdentity = { machineId: 'machine-1', bootId: 'boot-1', pidNs: 'pid:[1]' };

class FakeTable implements ProcessTable {
  states = new Map<number, ProcessState>();
  self(): Promise<ProcessIdentity> {
    return Promise.resolve(SELF);
  }
  inspect(pid: number): Promise<ProcessState> {
    return Promise.resolve(this.states.get(pid) ?? { state: 'absent' });
  }
  scan(): Promise<ProcessScan> {
    return Promise.resolve({ processes: [], unreadable: 0 });
  }
}

const worlds: CommandWorld[] = [];
afterEach(async () => {
  await Promise.all(worlds.splice(0).map((w) => w.dispose()));
});

async function world(): Promise<CommandWorld> {
  const created = await commandWorld();
  worlds.push(created);
  return created;
}

const fast = (table = new FakeTable()): UnlockDependencies => ({
  processes: table, hostname: 'laptop', environment: {}, sleep: () => Promise.resolve()
});

/** Leaves the worktree's locks as a holder killed outright would: held, silent, and naming it. */
async function killedHolder(controlHome: string, repositoryId: string, worktreeId: string, name: string, overrides: Partial<HolderRecord> = {}): Promise<void> {
  for (const lock of worktreeLocks(controlHome, repositoryId, worktreeId, name)) {
    await mkdir(path.dirname(lock.target), { recursive: true });
    await mkdir(`${lock.target}.lock`);
    const directory = await lstat(`${lock.target}.lock`, { bigint: true });
    const record: HolderRecord = {
      pid: 4242, host: 'laptop', command: 'berth up', startedAt: new Date().toISOString(),
      lock: { ino: directory.ino.toString(), birthtimeMs: Number(directory.birthtimeMs) },
      ...SELF, startTicks: 900, ...overrides
    };
    await writeFile(lock.target, `${JSON.stringify(record)}\n`);
    const silent = new Date(Date.now() - 90_000);
    await utimes(`${lock.target}.lock`, silent, silent);
  }
}

const gone = async (target: string) => expect(lstat(`${target}.lock`)).rejects.toMatchObject({ code: 'ENOENT' });

describe('worktree unlock', () => {
  it('says there is nothing to unlock when nothing holds the worktree', async () => {
    const w = await world();
    await up(w.context());
    const context = w.context();
    const result = await unlock(context, {}, fast());
    expect(result.cleared).toEqual([]);
    expect(context.logger.lines).toContain('info: Nothing to unlock: no lock is held for feature-login.acme.localhost.');
    expect(context.logger.printed).toEqual([]);
  });

  it('refuses a live holder, naming it, and clears nothing', async () => {
    const w = await world();
    const context = w.context();
    const { registration } = await up(context);
    const [, operation] = worktreeLocks(w.controlHome, registration.repositoryId, registration.worktreeId, registration.slug);
    if (!operation) throw new Error('no operation lock');
    const holder = new ScopedLock(operation.target, operation.description, context.logger, { holder: 'berth up' });
    await holder.acquire();
    try {
      await expect(unlock(context, {}, fast())).rejects.toThrow(
        new RegExp(`^The worktree feature-login lock is held by "berth up" \\(PID ${process.pid} on "[^"]+"\\), heartbeat \\d+s ago: it is alive, so there is nothing to unlock\\. Wait for it to finish, or stop it\\.$`)
      );
    } finally {
      await holder.release();
    }
  });

  it('clears the locks of a holder proven gone, and says what it checked', async () => {
    const w = await world();
    const { registration } = await up(w.context());
    await killedHolder(w.controlHome, registration.repositoryId, registration.worktreeId, registration.slug);
    const context = w.context();

    const result = await unlock(context, {}, fast());

    expect(result.cleared.map((lock) => lock.kind)).toEqual(['development', 'operation']);
    for (const lock of result.cleared) await gone(lock.target);
    expect(context.logger.lines).toEqual([
      'info: Checking that the holder of feature-login.acme.localhost\'s lock, and every command it started on this machine, is gone; this takes about 15 seconds.',
      expect.stringMatching(/^info: Verified: feature-login development session: PID 4242 on "laptop" no longer exists; its heartbeat has been silent for 1m 3\ds\.$/),
      expect.stringMatching(/^info: Verified: worktree feature-login: PID 4242 on "laptop" no longer exists/),
      'info: Verified: no command started for this worktree is still running on this machine.',
      'info: Unlocked feature-login.acme.localhost: cleared the feature-login development session lock and the worktree feature-login lock.'
    ]);
    // And the worktree works again.
    await expect(up(w.context())).resolves.toBeDefined();
  });

  it('names another worktree by its slug', async () => {
    const w = await world();
    const other = await w.worktree('other', 'feature/other');
    const { registration } = await up(w.context(other));
    await killedHolder(w.controlHome, registration.repositoryId, registration.worktreeId, registration.slug, { pidNs: undefined, bootId: 'boot-0' });

    const result = await unlock(w.context(), { identifier: 'feature-other' }, fast());
    expect(result.cleared).toHaveLength(2);
  });

  it('refuses a name nobody registered, and a checkout that was never brought up', async () => {
    const w = await world();
    await expect(unlock(w.context(), { identifier: 'nobody' }, fast())).rejects.toThrow('No worktree is registered as nobody.');
    await expect(unlock(w.context(), {}, fast())).rejects.toThrow("This checkout has never been brought up, so it holds no lock. Name another with 'berth worktree unlock <slug|uuid>'.");
  });

  it('finds a lock a first up left behind before it registered, by the checkout\'s own markers', async () => {
    const w = await world();
    const context = w.context();
    const identity = await context.registry.identities.ensure();
    await killedHolder(w.controlHome, identity.repositoryId, identity.worktreeId, identity.root);

    const result = await unlock(context, {}, fast());
    expect(result.name).toBe(identity.root);
    expect(result.cleared).toHaveLength(2);
    expect(context.logger.lines.at(-1)).toBe(`info: Unlocked ${identity.root}: cleared the ${identity.root} development session lock and the worktree ${identity.root} lock.`);
  });

  it('reaches a worktree that never registered, and whose checkout is gone, by its UUID', async () => {
    const w = await world();
    const repositoryId = 'a08b4892-5111-49f1-a47a-5ab988c7f676';
    const worktreeId = '2b41d9d4-ed88-49c1-be9c-5439bdf6a431';
    await killedHolder(w.controlHome, repositoryId, worktreeId, worktreeId);

    const context = w.context();
    const result = await unlock(context, { identifier: worktreeId }, fast());
    expect(result).toMatchObject({ name: worktreeId });
    expect(result.cleared).toHaveLength(2);
    await expect(unlock(w.context(), { identifier: 'ffffffff-ffff-4fff-8fff-ffffffffffff' }, fast())).rejects.toThrow(
      'No worktree is registered as ffffffff-ffff-4fff-8fff-ffffffffffff, and no lock is held for a worktree of that UUID.'
    );
  });

  it('takes the person\'s word for a holder it cannot check only when told to, and says so', async () => {
    const w = await world();
    const context = w.context();
    const { registration } = await up(context);
    await killedHolder(w.controlHome, registration.repositoryId, registration.worktreeId, registration.slug, { machineId: 'machine-2' });

    await expect(unlock(context, {}, fast())).rejects.toThrow('ran on another machine');
    const result = await unlock(context, { allowUnverifiable: true }, fast());
    expect(result.cleared).toHaveLength(2);
    expect(context.logger.lines.filter((line) => line.startsWith('warn:'))).toEqual([
      expect.stringMatching(/^warn: Not verified, on your word \(--holder-unverifiable\): feature-login development session: its holder, PID 4242 on "laptop", ran on another machine/),
      expect.stringMatching(/^warn: Not verified, on your word \(--holder-unverifiable\): worktree feature-login: its holder/)
    ]);
  });
});
