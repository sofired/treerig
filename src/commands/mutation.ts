import {
  developmentLockTarget,
  OPERATION_LOCK_TIMEOUT_MS,
  ScopedLock,
  worktreeLockTarget,
  type ScopedLockOptions
} from '../operation-lock.ts';
import type { WorktreeRef } from '../runtime/runtime.ts';
import { runWithDeferredSignals } from '../signal-lifecycle.ts';
import type { CommandContext } from './context.ts';

export function lockOptions(context: CommandContext): ScopedLockOptions {
  return {
    acquireTimeoutMs: OPERATION_LOCK_TIMEOUT_MS,
    recoveryCommand: `${context.commandName} worktree unlock`
  };
}

export interface MutationOptions {
  /** Also take the development-session lock: the verb must not run while `dev` is attached. */
  exclusive?: boolean;
}

/**
 * Runs one mutation of one worktree. The worktree lock is held throughout,
 * and SIGINT/SIGTERM are deferred until the action has settled, its
 * subprocesses are reaped, and the lock is released — then re-raised. A
 * half-finished mutation is never left locked, and never left running.
 */
export async function withWorktreeMutation<T>(
  context: CommandContext,
  ref: WorktreeRef,
  action: () => Promise<T>,
  options: MutationOptions = {}
): Promise<T> {
  const { controlHome, logger, runner } = context;
  const locks = [
    new ScopedLock(
      worktreeLockTarget(controlHome, ref.repositoryId, ref.worktreeId),
      `worktree ${ref.slug}`,
      logger,
      lockOptions(context)
    )
  ];
  if (options.exclusive) {
    locks.push(new ScopedLock(
      developmentLockTarget(controlHome, ref.repositoryId, ref.worktreeId),
      `development session for ${ref.slug}`,
      logger,
      // A live `dev` session is not something to wait out; say so quickly.
      { acquireTimeoutMs: 1_000, recoveryCommand: lockOptions(context).recoveryCommand }
    ));
  }

  const held: ScopedLock[] = [];
  const releaseAll = async (): Promise<void> => {
    for (const lock of held.splice(0).reverse()) await lock.release();
  };
  try {
    for (const lock of locks) {
      await lock.acquire();
      held.push(lock);
    }
  } catch (error) {
    await releaseAll();
    throw error;
  }

  return runWithDeferredSignals(async () => {
    const result = await action();
    for (const lock of held) lock.assertHealthy();
    return result;
  }, {
    cancel: (signal) => runner.cancelActive?.(signal) ?? Promise.resolve(),
    // `cancelActive` reaps every child it signalled before returning; calling
    // it again proves nothing is still running.
    postActionQuiesce: () => runner.cancelActive?.('SIGTERM') ?? Promise.resolve(),
    release: releaseAll,
    reenact: (signal) => context.process.reenact(signal),
    forceExit: (error) => context.process.forceExit(error),
    source: context.process.source
  });
}
