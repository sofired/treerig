import { randomUUID } from 'node:crypto';
import { access, lstat, mkdtemp, readFile, realpath, rm, utimes, writeFile } from 'node:fs/promises';
import { hostname, tmpdir } from 'node:os';
import path from 'node:path';
import { execa, type ResultPromise } from 'execa';
import { afterEach, describe, expect, it } from 'vitest';
import { composeProjectName } from '../../src/layout.ts';
import { unlockGuard } from '../../src/lock-recovery.ts';
import { clearStaleWorktreeLocks, worktreeLockTarget } from '../../src/operation-lock.ts';
import { processTableFor } from '../../src/process-table.ts';

// `worktree unlock`'s proof against real processes and the real process
// table of whichever platform runs it (/proc on Linux, ps on macOS): a
// holder killed outright, one merely stopped, one that left a process
// behind, one that exited holding its lock, and a PID taken by another.

const FIXTURE = new URL('./fixtures/hold-worktree-lock.ts', import.meta.url).pathname;
const temporary: string[] = [];
const running: (ResultPromise | number)[] = [];

afterEach(async () => {
  for (const entry of running.splice(0)) {
    if (typeof entry === 'number') {
      try {
        process.kill(entry, 'SIGKILL');
      } catch {
        // Already gone.
      }
    } else {
      entry.kill('SIGCONT');
      entry.kill('SIGKILL');
      await entry;
    }
  }
  await Promise.all(temporary.splice(0).map((d) => rm(d, { recursive: true, force: true })));
});

async function until(condition: () => Promise<boolean>, what: string): Promise<void> {
  for (let attempt = 0; attempt < 200; attempt += 1) {
    if (await condition()) return;
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  throw new Error(`Timed out waiting for ${what}.`);
}

async function world() {
  const controlHome = await realpath(await mkdtemp(path.join(tmpdir(), 'berth-unlock-it-')));
  temporary.push(controlHome);
  const repositoryId = randomUUID();
  const worktreeId = randomUUID();
  const target = worktreeLockTarget(controlHome, repositoryId, worktreeId);

  const hold = async (mode: 'hold' | 'grandchild' | 'exit' = 'hold') => {
    const readyFile = path.join(controlHome, `${randomUUID()}.ready`);
    const holder = execa(process.execPath, [FIXTURE, controlHome, repositoryId, worktreeId, readyFile, mode], { reject: false, cleanup: true });
    running.push(holder);
    await until(() => access(readyFile).then(() => true, () => false), 'the holder');
    const ready = JSON.parse(await readFile(readyFile, 'utf8')) as { pid: number; grandchild?: number };
    if (ready.grandchild !== undefined) running.push(ready.grandchild);
    return { holder, ...ready };
  };

  /** Three missed heartbeats and then some: what 30 s of silence looks like, without the wait. */
  const silence = async (): Promise<void> => {
    const old = new Date(Date.now() - 60_000);
    await utimes(`${target}.lock`, old, old);
  };

  const guard = () => unlockGuard({
    processes: processTableFor(),
    hostname: hostname(),
    markers: { project: composeProjectName(repositoryId, worktreeId), worktreeId },
    allowUnverifiable: false,
    environment: {},
    secondLookMs: 200
  });

  const unlock = (proof = guard()) => clearStaleWorktreeLocks(controlHome, repositoryId, worktreeId, 'main', proof);
  const held = () => lstat(`${target}.lock`).then(() => true, () => false);

  return { controlHome, repositoryId, worktreeId, target, hold, silence, guard, unlock, held };
}

describe('worktree unlock against real processes', () => {
  it('clears the lock of a holder killed outright', async () => {
    const w = await world();
    const { holder } = await w.hold();
    holder.kill('SIGKILL');
    await holder;
    await w.silence();

    const proof = w.guard();
    await expect(w.unlock(proof)).resolves.toEqual([expect.objectContaining({ kind: 'operation' })]);
    expect(await w.held()).toBe(false);
    expect(proof.proof.verified[0]).toMatch(/no longer exists|has exited/);
  });

  it('refuses a holder that is only stopped, and keeps its lock', async () => {
    const w = await world();
    const { holder } = await w.hold();
    holder.kill('SIGSTOP');
    await w.silence();

    await expect(w.unlock()).rejects.toThrow('is stopped, not gone');
    expect(await w.held()).toBe(true);
  });

  it('refuses while a process the holder left behind still runs, and clears once it has gone', async () => {
    const w = await world();
    const { holder, grandchild } = await w.hold('grandchild');
    if (grandchild === undefined) throw new Error('no grandchild');
    holder.kill('SIGKILL');
    await holder;
    await w.silence();

    await expect(w.unlock()).rejects.toThrow(new RegExp(`still running:\\n  PID ${grandchild}: `));
    expect(await w.held()).toBe(true);

    process.kill(grandchild, 'SIGKILL');
    const table = processTableFor();
    await until(async () => {
      const { state } = await table.inspect(grandchild);
      return state === 'absent' || state === 'zombie';
    }, 'the grandchild to go');
    await expect(w.unlock()).resolves.toHaveLength(1);
  });

  it('leaves no record behind when its holder exits while holding it', async () => {
    const w = await world();
    const { holder } = await w.hold('exit');
    await holder;
    expect(await w.held()).toBe(false);
    await expect(readFile(w.target, 'utf8')).resolves.toBe('');
  });

  it('knows a PID now used by a later process is not the holder', async () => {
    const w = await world();
    const { holder } = await w.hold();
    holder.kill('SIGKILL');
    await holder;
    const sleeper = execa('sleep', ['30'], { reject: false });
    running.push(sleeper);
    if (sleeper.pid === undefined) throw new Error('sleep did not start');
    // The dead holder's record, rewritten to name a PID that is alive but started later.
    const record = JSON.parse(await readFile(w.target, 'utf8')) as Record<string, unknown>;
    await writeFile(w.target, `${JSON.stringify({ ...record, pid: sleeper.pid, startTicks: 1, processStartedAtMs: 0 })}\n`);
    await w.silence();

    const proof = w.guard();
    await expect(w.unlock(proof)).resolves.toHaveLength(1);
    expect(proof.proof.verified[0]).toContain('now belongs to a later process');
  });
});
