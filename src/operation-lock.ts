import { lstat, mkdir, writeFile } from 'node:fs/promises';
import path from 'node:path';
import * as lockfile from 'proper-lockfile';
import { CliError, invariant } from './errors.ts';
import type { Logger } from './logger.ts';
import { controlLayout, worktreeLayout } from './layout.ts';

// Never steal a lock based only on wall-clock age; laptop sleep can make a live lock look stale.
const LOCK_STALE_MS = Number.MAX_SAFE_INTEGER;
const LOCK_UPDATE_MS = 10_000;
const LOCK_RECOVERY_STALE_MS = 30_000;
const LOCK_RETRY_MAX_MS = 250;
/** How long an operation will wait for a busy worktree before giving up. */
export const OPERATION_LOCK_TIMEOUT_MS = 25 * 60_000;
/**
 * The registry lock guards a few milliseconds of JSON writes, so contenders
 * poll quickly and wait patiently rather than failing after a handful of
 * slow retries.
 */
export const REGISTRY_LOCK_TIMEOUT_MS = 60_000;
export const REGISTRY_LOCK_RETRY_MS = 50;

export type LockRecoveryState = 'active' | 'missing' | 'stale';

export interface ScopedLockOptions {
  acquireTimeoutMs?: number;
  /** Poll interval while waiting; defaults to a 250 ms ceiling. */
  retryIntervalMs?: number;
  /** Shown to the user when the lock is held, e.g. `berth worktree unlock`. */
  recoveryCommand?: string;
}

/* ---- the four lock scopes ---- */

export function registryLockTarget(controlHome: string): string {
  return controlLayout(controlHome).registryLockPath;
}

export function worktreeLockTarget(
  controlHome: string,
  repositoryId: string,
  worktreeId: string
): string {
  return worktreeLayout(controlHome, repositoryId, worktreeId).lockPath;
}

export function developmentLockTarget(
  controlHome: string,
  repositoryId: string,
  worktreeId: string
): string {
  return worktreeLayout(controlHome, repositoryId, worktreeId).developmentLockPath;
}

export function worktreeRecoveryLockTarget(
  controlHome: string,
  repositoryId: string,
  worktreeId: string
): string {
  return `${worktreeLockTarget(controlHome, repositoryId, worktreeId)}.recovery`;
}

export function routerLockTarget(controlHome: string): string {
  return controlLayout(controlHome).routerLockPath;
}

/**
 * An advisory lock with an effectively infinite stale window and a heartbeat,
 * so a long-running holder is never silently stolen. Recovery from a genuinely
 * dead holder is explicit — see `clearStaleLock` — never automatic.
 */
export class ScopedLock {
  private readonly target: string;
  private readonly description: string;
  private readonly logger: Logger;
  private readonly acquireTimeoutMs?: number;
  private readonly retryIntervalMs?: number;
  private readonly recoveryCommand?: string;
  private releaseLock?: () => Promise<void>;
  private compromised?: Error;

  constructor(
    target: string,
    description: string,
    logger: Logger,
    options: ScopedLockOptions = {}
  ) {
    invariant(
      options.acquireTimeoutMs === undefined || options.acquireTimeoutMs > 0,
      'Lock acquisition timeout must be greater than zero.'
    );
    invariant(
      options.retryIntervalMs === undefined || options.retryIntervalMs > 0,
      'Lock retry interval must be greater than zero.'
    );
    this.target = path.resolve(target);
    this.description = description;
    this.logger = logger;
    this.acquireTimeoutMs = options.acquireTimeoutMs;
    this.retryIntervalMs = options.retryIntervalMs;
    this.recoveryCommand = options.recoveryCommand;
  }

  /**
   * Three schedules, chosen by which options were given:
   *  - a budget: poll at a fixed interval until the budget is spent;
   *  - an interval alone: poll at that fixed interval for the default number of tries;
   *  - neither: the default short exponential backoff, unchanged.
   */
  private retrySchedule() {
    if (this.acquireTimeoutMs !== undefined) {
      const interval = Math.min(this.retryIntervalMs ?? LOCK_RETRY_MAX_MS, this.acquireTimeoutMs);
      return {
        retries: Math.ceil(this.acquireTimeoutMs / interval),
        minTimeout: interval,
        maxTimeout: interval,
        factor: 1,
        randomize: false
      };
    }
    if (this.retryIntervalMs !== undefined) {
      return {
        retries: 20,
        minTimeout: this.retryIntervalMs,
        maxTimeout: this.retryIntervalMs,
        factor: 1,
        randomize: false
      };
    }
    return { retries: 20, minTimeout: 50, maxTimeout: LOCK_RETRY_MAX_MS };
  }

  async acquire(): Promise<void> {
    invariant(!this.releaseLock, `${this.description} lock is already held by this process.`);
    await mkdir(path.dirname(this.target), { recursive: true, mode: 0o700 });
    await writeFile(this.target, '', { flag: 'a', mode: 0o600 });
    try {
      this.releaseLock = await lockfile.lock(this.target, {
        realpath: true,
        stale: LOCK_STALE_MS,
        update: LOCK_UPDATE_MS,
        retries: this.retrySchedule(),
        onCompromised: (error) => {
          this.compromised = error;
          this.logger.error(`${this.description} lock was compromised: ${error.message}`);
        }
      });
    } catch (error) {
      const detail = error as NodeJS.ErrnoException;
      if (detail.code === 'ELOCKED') {
        const recovery = this.recoveryCommand
          ? ` If the prior process terminated unexpectedly, run \`${this.recoveryCommand}\`.`
          : '';
        throw new CliError(
          `Another process holds the ${this.description} lock.${recovery}`
        );
      }
      throw error;
    }
  }

  assertHealthy(): void {
    if (this.compromised) {
      throw new CliError(
        `${this.description} lock was compromised; refusing to continue: ${this.compromised.message}`
      );
    }
    invariant(this.releaseLock, `${this.description} lock is not held.`);
  }

  async release(): Promise<void> {
    const release = this.releaseLock;
    this.releaseLock = undefined;
    if (release) await release();
  }

  async run<T>(action: () => Promise<T>): Promise<T> {
    await this.acquire();
    try {
      const result = await action();
      this.assertHealthy();
      return result;
    } finally {
      await this.release();
    }
  }
}

/** Non-mutating probe used before deciding whether a lock can be recovered. */
export async function inspectLockForRecovery(
  target: string,
  description: string
): Promise<LockRecoveryState> {
  const resolvedTarget = path.resolve(target);
  const lockDirectory = `${resolvedTarget}.lock`;
  try {
    const metadata = await lstat(lockDirectory);
    invariant(
      metadata.isDirectory() && !metadata.isSymbolicLink(),
      `Refusing unexpected ${description} lock state.`
    );
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return 'missing';
    throw error;
  }

  const active = await lockfile.check(resolvedTarget, {
    realpath: true,
    stale: LOCK_RECOVERY_STALE_MS
  });
  if (active) return 'active';

  // The owner may have released the lock between the initial lstat and check.
  try {
    const metadata = await lstat(lockDirectory);
    invariant(
      metadata.isDirectory() && !metadata.isSymbolicLink(),
      `Refusing unexpected ${description} lock state.`
    );
    return 'stale';
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return 'missing';
    throw error;
  }
}

const STILL_ACTIVE = (description: string): string =>
  `The ${description} lock is still active. Stop the process holding it, wait up to 30 seconds, and retry.`;

export interface ClearStaleLockOptions {
  /** Runs between confirming the lock is stale and claiming it; may throw to abort. */
  beforeClear?: () => Promise<void>;
}

export async function clearStaleLock(
  target: string,
  description: string,
  options: ClearStaleLockOptions = {}
): Promise<boolean> {
  const resolvedTarget = path.resolve(target);
  const initialState = await inspectLockForRecovery(resolvedTarget, description);
  if (initialState === 'missing') return false;
  if (initialState === 'active') throw new CliError(STILL_ACTIVE(description));

  await options.beforeClear?.();

  // Recovery can take long enough for an owner to resume after laptop sleep.
  // Re-check without mutating before proper-lockfile atomically claims it.
  const guardedState = await inspectLockForRecovery(resolvedTarget, description);
  if (guardedState === 'missing') return false;
  if (guardedState === 'active') {
    throw new CliError(
      `The ${description} lock became active during recovery; it was not cleared.`
    );
  }

  let release: (() => Promise<void>) | undefined;
  try {
    release = await lockfile.lock(resolvedTarget, {
      realpath: true,
      stale: LOCK_RECOVERY_STALE_MS,
      update: LOCK_UPDATE_MS,
      retries: 0
    });
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ELOCKED') {
      throw new CliError(STILL_ACTIVE(description));
    }
    throw error;
  }
  await release();
  return true;
}

/**
 * Clears a worktree's stale operation and development locks under a recovery
 * mutex, so two concurrent unlock attempts cannot interleave. The runtime
 * backend supplies `beforeOperationLockClear` to prove nothing owned by the
 * dead holder is still mutating state before the operation lock is released.
 */
export async function clearStaleWorktreeLocks(
  controlHome: string,
  repositoryId: string,
  worktreeId: string,
  description: string,
  options: { beforeOperationLockClear?: () => Promise<void> } = {}
): Promise<number> {
  const recoveryTarget = worktreeRecoveryLockTarget(controlHome, repositoryId, worktreeId);
  await mkdir(path.dirname(recoveryTarget), { recursive: true, mode: 0o700 });
  await writeFile(recoveryTarget, '', { flag: 'a', mode: 0o600 });

  let recoveryCompromised: Error | undefined;
  let releaseRecovery: (() => Promise<void>) | undefined;
  try {
    releaseRecovery = await lockfile.lock(recoveryTarget, {
      realpath: true,
      stale: LOCK_RECOVERY_STALE_MS,
      update: LOCK_UPDATE_MS,
      retries: 0,
      onCompromised: (error) => {
        recoveryCompromised = error;
      }
    });
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ELOCKED') {
      throw new CliError(
        `Another recovery is active for ${description}. Wait up to 30 seconds after it exits unexpectedly, then retry.`
      );
    }
    throw error;
  }

  const assertRecoveryHealthy = (): void => {
    if (recoveryCompromised) {
      throw new CliError(
        `Recovery ownership for ${description} was compromised; no further locks were cleared.`
      );
    }
  };

  try {
    const targets = [
      {
        path: developmentLockTarget(controlHome, repositoryId, worktreeId),
        description: `development session for ${description}`
      },
      {
        path: worktreeLockTarget(controlHome, repositoryId, worktreeId),
        description: `operation for ${description}`
      }
    ];

    // The recovery mutex excludes another manual unlock before any runtime
    // mutation. Re-read both locks while holding it so a queued recovery does
    // not act on state observed before the prior recovery completed.
    const states = await Promise.all(
      targets.map((target) => inspectLockForRecovery(target.path, target.description))
    );
    assertRecoveryHealthy();
    const activeIndex = states.findIndex((state) => state === 'active');
    if (activeIndex >= 0) {
      throw new CliError(STILL_ACTIVE(targets[activeIndex]?.description ?? description));
    }

    let cleared = 0;
    // Recover the guarded operation lock first so a runtime ambiguity leaves
    // the complete stale lock set intact for diagnosis and retry.
    for (const index of [1, 0]) {
      const target = targets[index];
      if (!target) continue;
      if (states[index] !== 'stale') continue;
      assertRecoveryHealthy();
      const beforeClear = index === 1
        ? async (): Promise<void> => {
            assertRecoveryHealthy();
            await options.beforeOperationLockClear?.();
            assertRecoveryHealthy();
          }
        : undefined;
      if (await clearStaleLock(target.path, target.description, { beforeClear })) cleared += 1;
    }
    assertRecoveryHealthy();
    return cleared;
  } finally {
    await releaseRecovery();
  }
}

/**
 * Clears an orphaned registry lock — left behind by `kill -9`, an OOM kill, or
 * power loss during the milliseconds a registration holds it. The registry
 * lock guards only JSON files, so no runtime needs to be quiesced first, but
 * recovery still requires the heartbeat to have been silent for 30 seconds.
 */
export async function clearStaleRegistryLock(controlHome: string): Promise<boolean> {
  return clearStaleLock(registryLockTarget(controlHome), 'global worktree registry');
}

export async function withScopedLock<T>(
  target: string,
  description: string,
  logger: Logger,
  action: () => Promise<T>,
  options: ScopedLockOptions = {}
): Promise<T> {
  return new ScopedLock(target, description, logger, options).run(action);
}
