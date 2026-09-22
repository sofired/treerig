import { mkdir, mkdtemp, readFile, realpath, rm, stat, symlink, utimes, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { silentLogger as logger } from '../../src/logger.ts';
import { RecordingLogger } from '../helpers/command-doubles.ts';
import {
  clearStaleLock,
  clearStaleRegistryLock,
  clearStaleWorktreeLocks,
  developmentLockTarget,
  formatDuration,
  inspectLockForRecovery,
  OPERATION_LOCK_TIMEOUT_MS,
  REGISTRY_LOCK_RETRY_MS,
  REGISTRY_LOCK_TIMEOUT_MS,
  registryLockTarget,
  ScopedLock,
  worktreeLockTarget,
  worktreeRecoveryLockTarget
} from '../../src/operation-lock.ts';

const temporary: string[] = [];
const REPOSITORY = 'a08b4892-5111-49f1-a47a-5ab988c7f676';
const WORKTREE = '2b41d9d4-ed88-49c1-be9c-5439bdf6a431';

async function home(prefix: string): Promise<string> {
  // The real path: lock advice names what proper-lockfile locked, and macOS puts tmp behind a symlink.
  const directory = await realpath(await mkdtemp(path.join(tmpdir(), `berth-${prefix}-`)));
  temporary.push(directory);
  return directory;
}

/** Resolves once the recorder has a line the predicate accepts; fails the test rather than hang. */
async function untilLine(recorder: RecordingLogger, accept: (line: string) => boolean): Promise<void> {
  const deadline = Date.now() + 5_000;
  while (!recorder.lines.some(accept)) {
    if (Date.now() > deadline) throw new Error(`No matching line within 5s; saw: ${JSON.stringify(recorder.lines)}`);
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
}

async function makeStale(target: string): Promise<void> {
  await mkdir(path.dirname(target), { recursive: true });
  await writeFile(target, '');
  await mkdir(`${target}.lock`);
  const stale = new Date(Date.now() - 60_000);
  await utimes(`${target}.lock`, stale, stale);
}

afterEach(async () => {
  await Promise.all(
    temporary.splice(0).map((directory) => rm(directory, { recursive: true, force: true }))
  );
});

describe('ScopedLock', () => {
  it('allows operations to wait up to twenty-five minutes for a busy worktree', () => {
    expect(OPERATION_LOCK_TIMEOUT_MS).toBe(25 * 60_000);
  });

  it('serializes the same scope and releases it for the next process', async () => {
    const target = worktreeLockTarget(await home('lock'), REPOSITORY, WORKTREE);
    const events: string[] = [];
    const first = new ScopedLock(target, 'first worktree', logger);
    const second = new ScopedLock(target, 'second worktree', logger);

    await first.acquire();
    const waiting = second.run(() => {
      events.push('second');
      return Promise.resolve();
    });
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(events).toEqual([]);
    await first.release();
    await waiting;

    expect(events).toEqual(['second']);
  });

  it('allows independent scopes concurrently', async () => {
    const directory = await home('locks');
    const left = new ScopedLock(path.join(directory, 'left'), 'left worktree', logger);
    const right = new ScopedLock(path.join(directory, 'right'), 'right worktree', logger);

    await Promise.all([
      left.run(async () => new Promise((resolve) => setTimeout(resolve, 20))),
      right.run(async () => new Promise((resolve) => setTimeout(resolve, 20)))
    ]);

    await expect(left.run(() => Promise.resolve('released'))).resolves.toBe('released');
  });

  it('allows a caller to extend acquisition for a longer transaction', async () => {
    const target = path.join(await home('lock-timeout'), 'worktree');
    const first = new ScopedLock(target, 'active worktree', logger);
    const waiting = new ScopedLock(target, 'waiting worktree', logger, {
      acquireTimeoutMs: 150
    });

    await first.acquire();
    const acquired = waiting.run(() => Promise.resolve('acquired'));
    await new Promise((resolve) => setTimeout(resolve, 75));
    await first.release();

    await expect(acquired).resolves.toBe('acquired');
  });

  it('serializes development sessions without holding the operation lock', async () => {
    const directory = await home('development-lock');
    const developmentTarget = developmentLockTarget(directory, REPOSITORY, WORKTREE);
    const first = new ScopedLock(developmentTarget, 'first development session', logger);
    const second = new ScopedLock(developmentTarget, 'second development session', logger);
    const operation = new ScopedLock(
      worktreeLockTarget(directory, REPOSITORY, WORKTREE),
      'worktree operation',
      logger
    );
    const events: string[] = [];

    await first.acquire();
    await expect(operation.run(() => Promise.resolve('available'))).resolves.toBe('available');
    const waiting = second.run(() => {
      events.push('second');
      return Promise.resolve();
    });
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(events).toEqual([]);
    await first.release();
    await waiting;

    expect(events).toEqual(['second']);
  });

  it('clears an abandoned lock without exposing its path', async () => {
    const target = path.join(await home('stale-lock'), 'worktree');
    await makeStale(target);

    await expect(clearStaleLock(target, 'test worktree')).resolves.toBe(true);
    await expect(stat(`${target}.lock`)).rejects.toMatchObject({ code: 'ENOENT' });
  });

  it('checks stale state without removing the lock before recovery is guarded', async () => {
    const target = path.join(await home('readonly-stale-lock'), 'worktree');
    await makeStale(target);

    await expect(inspectLockForRecovery(target, 'test worktree')).resolves.toBe('stale');
    await expect(stat(`${target}.lock`)).resolves.toBeDefined();
  });

  it('preserves a stale operation lock when its recovery guard fails', async () => {
    const directory = await home('guarded-stale-lock');
    const target = worktreeLockTarget(directory, REPOSITORY, WORKTREE);
    const development = developmentLockTarget(directory, REPOSITORY, WORKTREE);
    await makeStale(target);
    await makeStale(development);

    await expect(clearStaleWorktreeLocks(
      directory,
      REPOSITORY,
      WORKTREE,
      'guarded',
      { beforeOperationLockClear: () => Promise.reject(new Error('ambiguous runtime')) }
    )).rejects.toThrow('ambiguous runtime');
    await expect(stat(`${target}.lock`)).resolves.toBeDefined();
    await expect(stat(`${development}.lock`)).resolves.toBeDefined();
  });

  it('does not clear a stale development lock after detecting an active operation', async () => {
    const directory = await home('two-phase-lock-check');
    const development = developmentLockTarget(directory, REPOSITORY, WORKTREE);
    await makeStale(development);
    const operation = new ScopedLock(
      worktreeLockTarget(directory, REPOSITORY, WORKTREE),
      'active operation',
      logger
    );
    await operation.acquire();
    const guard = vi.fn(() => Promise.resolve());
    try {
      await expect(clearStaleWorktreeLocks(
        directory,
        REPOSITORY,
        WORKTREE,
        'active',
        { beforeOperationLockClear: guard }
      )).rejects.toThrow('The worktree active lock is still active');
      expect(guard).not.toHaveBeenCalled();
      await expect(stat(`${development}.lock`)).resolves.toBeDefined();
    } finally {
      await operation.release();
    }
  });

  it('serializes recovery before runtime guards and refuses a new active owner', async () => {
    const directory = await home('serialized-recovery');
    const target = worktreeLockTarget(directory, REPOSITORY, WORKTREE);
    await makeStale(target);

    let enterFirst: (() => void) | undefined;
    const firstEntered = new Promise<void>((resolve) => {
      enterFirst = resolve;
    });
    let finishFirst: (() => void) | undefined;
    const firstBlocked = new Promise<void>((resolve) => {
      finishFirst = resolve;
    });
    const firstGuard = vi.fn(async () => {
      enterFirst?.();
      await firstBlocked;
    });
    const secondGuard = vi.fn(() => Promise.resolve());
    const first = clearStaleWorktreeLocks(directory, REPOSITORY, WORKTREE, 'serialized', {
      beforeOperationLockClear: firstGuard
    });
    await firstEntered;

    await expect(clearStaleWorktreeLocks(
      directory,
      REPOSITORY,
      WORKTREE,
      'serialized',
      { beforeOperationLockClear: secondGuard }
    )).rejects.toThrow('Another recovery is active');
    expect(secondGuard).not.toHaveBeenCalled();

    finishFirst?.();
    await expect(first).resolves.toBe(1);
    const active = new ScopedLock(target, 'new worktree owner', logger);
    await active.acquire();
    const afterActiveGuard = vi.fn(() => Promise.resolve());
    try {
      await expect(clearStaleWorktreeLocks(
        directory,
        REPOSITORY,
        WORKTREE,
        'serialized',
        { beforeOperationLockClear: afterActiveGuard }
      )).rejects.toThrow('The worktree serialized lock is still active');
      expect(afterActiveGuard).not.toHaveBeenCalled();
    } finally {
      await active.release();
    }
  });

  it('recovers an orphaned recovery mutex after thirty seconds', async () => {
    const directory = await home('orphaned-recovery');
    const operation = worktreeLockTarget(directory, REPOSITORY, WORKTREE);
    const recovery = worktreeRecoveryLockTarget(directory, REPOSITORY, WORKTREE);
    await makeStale(operation);
    await makeStale(recovery);

    await expect(clearStaleWorktreeLocks(
      directory,
      REPOSITORY,
      WORKTREE,
      'recoverable'
    )).resolves.toBe(1);
    await expect(stat(`${recovery}.lock`)).rejects.toMatchObject({ code: 'ENOENT' });
  });

  it('clears both abandoned locks for an explicitly targeted worktree', async () => {
    const directory = await home('stale-worktree-locks');
    const targets = [
      developmentLockTarget(directory, REPOSITORY, WORKTREE),
      worktreeLockTarget(directory, REPOSITORY, WORKTREE)
    ];
    for (const target of targets) await makeStale(target);

    await expect(
      clearStaleWorktreeLocks(directory, REPOSITORY, WORKTREE, 'orphaned-clone')
    ).resolves.toBe(2);
    for (const target of targets) {
      await expect(stat(`${target}.lock`)).rejects.toMatchObject({ code: 'ENOENT' });
    }
  });

  it('polls at the requested interval so many contenders all get through', async () => {
    const target = path.join(await home('contenders'), 'registry');
    const order: number[] = [];
    const contenders = Array.from({ length: 30 }, (_, index) =>
      new ScopedLock(target, `contender ${index}`, logger, {
        acquireTimeoutMs: REGISTRY_LOCK_TIMEOUT_MS,
        retryIntervalMs: REGISTRY_LOCK_RETRY_MS
      }).run(async () => {
        order.push(index);
        await new Promise((resolve) => setTimeout(resolve, 5));
      })
    );

    await expect(Promise.all(contenders)).resolves.toHaveLength(30);
    expect(new Set(order).size).toBe(30);
  });

  it('honors an explicit retry interval as a fixed schedule even without an acquire budget', async () => {
    const target = path.join(await home('interval-only'), 'lock');
    const holder = new ScopedLock(target, 'holder', logger);
    await holder.acquire();
    // 20 tries at 10 ms is roughly 200 ms of patience. The holder lets go at
    // one second — inside the old hardcoded ~4.6 s schedule, outside this one.
    const release = setTimeout(() => { void holder.release(); }, 1_000);
    try {
      const waiter = new ScopedLock(target, 'waiter', logger, { retryIntervalMs: 10 });
      await expect(waiter.acquire()).rejects.toThrow('Another process holds the waiter lock.');
    } finally {
      clearTimeout(release);
      await holder.release();
    }
  });

  it('advises removal only after a heartbeat stays silent across two looks, names the directory, and touches nothing', async () => {
    const target = path.join(await home('stale-holder'), 'worktree');
    await makeStale(target);
    const before = await stat(`${target}.lock`);
    const recorder = new RecordingLogger();
    const waiter = new ScopedLock(target, 'stale worktree', recorder, {
      acquireTimeoutMs: 400, retryIntervalMs: 25, noticeDelayMs: 0, noticeIntervalMs: 50, staleConfirmationMs: 120
    });

    await expect(waiter.acquire()).rejects.toThrow(
      new RegExp(String.raw`^Another process holds the stale worktree lock\. An unidentified holder has (it|held it for [^;]+); its heartbeat has stayed silent for [^.]+\. Locks are never stolen automatically: if that process is gone, and nothing it started \(such as a \x60docker compose\x60 command\) is still running, remove the directory with \x60rmdir '${target.replaceAll('.', String.raw`\.`)}\.lock'\x60 and retry\.$`)
    );
    const [first, ...rest] = recorder.lines.filter((line) => line.includes('Waiting for the stale worktree lock'));
    // The first look is provisional — a machine that just woke up looks exactly like this.
    expect(first).toMatch(/^info: .*so this may be nothing — checking again in [^.]+\.$/);
    expect(first).not.toContain('rmdir');
    expect(rest.some((line) => line.startsWith('warn:') && line.includes(`rmdir '${target}.lock'\`; this command will then carry on by itself.`))).toBe(true);
    const after = await stat(`${target}.lock`);
    expect(after.mtimeMs).toBe(before.mtimeMs);   // repeated attempts neither steal nor touch it
  });

  it('starts the two looks over when the heartbeat moves', async () => {
    const target = path.join(await home('revived-holder'), 'worktree');
    await makeStale(target);
    const recorder = new RecordingLogger();
    const waiter = new ScopedLock(target, 'flickering worktree', recorder, {
      acquireTimeoutMs: 1_000, retryIntervalMs: 25, noticeDelayMs: 0, noticeIntervalMs: 50, staleConfirmationMs: 120
    });
    const attempt = waiter.acquire().catch((error: unknown) => error);
    await untilLine(recorder, (line) => line.startsWith('warn:'));
    // A heartbeat that is newer but still stale — the holder stirred, then went quiet again.
    const stirred = new Date(Date.now() - 45_000);
    await utimes(`${target}.lock`, stirred, stirred);
    const seen = recorder.lines.length;

    await expect(attempt).resolves.toBeInstanceOf(Error);
    const afterwards = recorder.lines.slice(seen).filter((line) => line.includes('Waiting for the flickering worktree lock'));
    // A notice already in flight when the heartbeat moved may still land first; the fresh look follows it.
    const fresh = afterwards.findIndex((line) => line.startsWith('info:'));
    expect(fresh).toBeGreaterThanOrEqual(0);                                          // a new silence gets a fresh first look ...
    expect(afterwards[fresh]).not.toContain('rmdir');
    expect(afterwards.slice(fresh + 1).some((line) => line.startsWith('warn:'))).toBe(true);   // ... and is confirmed only after the window
  });

  it('treats a wall-clock jump as a suspended machine: the deadline moves and the holder gets a fresh look', async () => {
    const target = path.join(await home('resumed-waiter'), 'worktree');
    await makeStale(target);
    const recorder = new RecordingLogger();
    let offset = 0;
    const waiter = new ScopedLock(target, 'sleepy worktree', recorder, {
      acquireTimeoutMs: 600, retryIntervalMs: 25, noticeDelayMs: 0, noticeIntervalMs: 50, staleConfirmationMs: 120,
      clock: () => Date.now() + offset
    });
    const started = Date.now();
    const attempt = waiter.acquire().catch((error: unknown) => error);
    await untilLine(recorder, (line) => line.startsWith('warn:'));
    offset = 20_000;   // the laptop lid was closed for twenty seconds
    const seen = recorder.lines.length;

    await expect(attempt).resolves.toBeInstanceOf(Error);
    expect(Date.now() - started).toBeGreaterThanOrEqual(550);   // the jump did not eat the budget
    const afterwards = recorder.lines.slice(seen).filter((line) => line.includes('Waiting for the sleepy worktree lock'));
    const fresh = afterwards.findIndex((line) => line.startsWith('info:'));
    expect(fresh).toBeGreaterThanOrEqual(0);
    expect(afterwards[fresh]).not.toContain('rmdir');
  });

  it('gives up on an unconfirmed silence without promising a look it cannot take', async () => {
    const target = path.join(await home('short-budget'), 'worktree');
    await makeStale(target);
    const waiter = new ScopedLock(target, 'brief worktree', new RecordingLogger(), { acquireTimeoutMs: 100, retryIntervalMs: 25 });

    await expect(waiter.acquire()).rejects.toThrow(
      new RegExp(String.raw`^Another process holds the brief worktree lock\. An unidentified holder has (it|held it for [^;]+); its last heartbeat was [^.]+\. If its machine only just woke up, the next heartbeat arrives within 10s, so this is too soon to call it dead\. Locks are never stolen automatically: run this command again in a moment; if the heartbeat still has not moved and that process is gone, and nothing it started \(such as a \x60docker compose\x60 command\) is still running, remove the directory with \x60rmdir '[^']+\.lock'\x60 and retry\.$`)
    );
  });

  it('tells a waiter who the live holder is, without inviting anyone to remove anything', async () => {
    const target = path.join(await home('live-holder'), 'worktree');
    const holder = new ScopedLock(target, 'holder', logger, { holder: 'berth up api' });
    await holder.acquire();
    const recorder = new RecordingLogger();
    const waiter = new ScopedLock(target, 'busy worktree', recorder, {
      acquireTimeoutMs: 300, retryIntervalMs: 25, noticeDelayMs: 0, noticeIntervalMs: 50
    });
    try {
      await expect(waiter.acquire()).rejects.toThrow(
        new RegExp(String.raw`^Another process holds the busy worktree lock\. The holder, 'berth up api' \(PID ${process.pid} on .+\), has held it for \d+s; its last heartbeat was (just now|\d+s ago), so it is alive\. Wait for it to finish, or stop it, then retry\.$`)
      );
      const notices = recorder.lines.filter((line) => line.includes('Waiting for the busy worktree lock'));
      expect(notices.length).toBeGreaterThanOrEqual(2);   // budget 300 ms, notice every 50 ms
      expect(notices.every((line) => line.startsWith('info:') && line.includes(`PID ${process.pid}`) && !line.includes('rmdir'))).toBe(true);
    } finally {
      await holder.release();
    }
  });

  it('describes itself only while it holds the lock', async () => {
    const target = path.join(await home('described-holder'), 'worktree');
    const holder = new ScopedLock(target, 'holder', logger, { holder: 'berth worktree remove' });
    await holder.acquire();
    expect(JSON.parse(await readFile(target, 'utf8'))).toMatchObject({ pid: process.pid, command: 'berth worktree remove' });
    await holder.release();
    expect(await readFile(target, 'utf8')).toBe('');
  });

  it('says nothing at all when the lock frees up before the notice delay', async () => {
    const target = path.join(await home('quick-holder'), 'worktree');
    const holder = new ScopedLock(target, 'holder', logger);
    await holder.acquire();
    setTimeout(() => { void holder.release(); }, 50);
    const recorder = new RecordingLogger();
    const waiter = new ScopedLock(target, 'patient worktree', recorder, { acquireTimeoutMs: 5_000, retryIntervalMs: 25, noticeDelayMs: 1_000 });

    await expect(waiter.run(() => Promise.resolve('acquired'))).resolves.toBe('acquired');
    expect(recorder.lines).toEqual([]);
  });

  it('refuses, naming the path, when something other than a lock directory sits at the lock path', async () => {
    const target = path.join(await home('odd-lock'), 'worktree');
    await writeFile(target, '');
    await writeFile(`${target}.lock`, 'not a directory');
    const waiter = new ScopedLock(target, 'odd worktree', new RecordingLogger(), { acquireTimeoutMs: 200, retryIntervalMs: 25, noticeDelayMs: 0 });
    await expect(waiter.acquire()).rejects.toThrow(`Refusing to touch the odd worktree lock: ${target}.lock is not a plain directory`);
  });

  it('refuses a lock target that is a symbolic link', async () => {
    const directory = await home('linked-target');
    const elsewhere = path.join(directory, 'elsewhere');
    await writeFile(elsewhere, '');
    const target = path.join(directory, 'worktree');
    await symlink(elsewhere, target);
    await expect(new ScopedLock(target, 'linked worktree', logger).acquire()).rejects.toThrow(
      `Refusing to use ${target} for the linked worktree lock: it is not a plain file (a directory or symbolic link stands in its place). Inspect it before removing anything.`
    );
  });

  it('formats durations the way a person reads them', () => {
    expect([4_000, 750_000, 7_500_000, 200_000_000].map(formatDuration)).toEqual(['4s', '12m 30s', '2h 5m', '2d 7h']);
  });

  it('rejects a non-positive retry interval', () => {
    const target = path.join(tmpdir(), 'unused');
    expect(() => new ScopedLock(target, 'bad interval', logger, { retryIntervalMs: 0 })).toThrow(
      'Lock retry interval must be greater than zero.'
    );
  });

  it('clears an orphaned registry lock and refuses a live one', async () => {
    const directory = await home('registry-recovery');
    const target = registryLockTarget(directory);
    await makeStale(target);

    await expect(clearStaleRegistryLock(directory)).resolves.toBe(true);
    await expect(stat(`${target}.lock`)).rejects.toMatchObject({ code: 'ENOENT' });
    await expect(clearStaleRegistryLock(directory)).resolves.toBe(false);

    const active = new ScopedLock(target, 'global worktree registry', logger);
    await active.acquire();
    try {
      await expect(clearStaleRegistryLock(directory)).rejects.toThrow(
        'The global worktree registry lock is still active.'
      );
    } finally {
      await active.release();
    }
  });

  it('refuses to clear a lock with a live heartbeat', async () => {
    const target = path.join(await home('active-lock'), 'worktree');
    const active = new ScopedLock(target, 'active worktree', logger);
    await active.acquire();
    try {
      await expect(clearStaleLock(target, 'test worktree')).rejects.toThrow(
        'The test worktree lock is still active. Stop the process holding it, wait up to 30 seconds, and retry.'
      );
    } finally {
      await active.release();
    }
  });
});
