import { mkdir, mkdtemp, rm, stat, utimes, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { silentLogger as logger } from '../../src/logger.ts';
import {
  clearStaleLock,
  clearStaleRegistryLock,
  clearStaleWorktreeLocks,
  developmentLockTarget,
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
  const directory = await mkdtemp(path.join(tmpdir(), `berth-${prefix}-`));
  temporary.push(directory);
  return directory;
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
      'worktree guarded',
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
        'worktree active',
        { beforeOperationLockClear: guard }
      )).rejects.toThrow('operation for worktree active lock is still active');
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
    const first = clearStaleWorktreeLocks(directory, REPOSITORY, WORKTREE, 'worktree serialized', {
      beforeOperationLockClear: firstGuard
    });
    await firstEntered;

    await expect(clearStaleWorktreeLocks(
      directory,
      REPOSITORY,
      WORKTREE,
      'worktree serialized',
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
        'worktree serialized',
        { beforeOperationLockClear: afterActiveGuard }
      )).rejects.toThrow('operation for worktree serialized lock is still active');
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
      'worktree recoverable'
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
      clearStaleWorktreeLocks(directory, REPOSITORY, WORKTREE, 'worktree orphaned-clone')
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
