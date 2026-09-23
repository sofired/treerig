import { hostname } from 'node:os';
import { CliError } from '../errors.ts';
import { assertNotGoManaged } from '../files.ts';
import { composeProjectName, worktreeHost } from '../layout.ts';
import { unlockGuard, type UnlockProof, type UnlockProofOptions } from '../lock-recovery.ts';
import { clearStaleWorktreeLocks, describeLock, formatDuration, holderLabel, lockedWorktrees, worktreeLocks, type WorktreeLock } from '../operation-lock.ts';
import { processTableFor, type ProcessTable } from '../process-table.ts';
import type { CommandContext } from './context.ts';

// The one way a dead holder's worktree lock is cleared. It never guesses: the
// holder must be shown to be gone, and no command it started on this machine
// may still be running.
// It takes no lock of its own beyond the recovery mutex, so it works exactly
// when every other verb for this worktree cannot.

export interface UnlockOptions {
  /** A slug or worktree UUID; this checkout when absent. */
  identifier?: string;
  domain?: string;
  /** Accept a holder this machine cannot check: another machine or namespace, or no record. */
  allowUnverifiable?: boolean;
}

export interface UnlockResult {
  /** The worktree, as the locks name it: its slug, or its root before it has one. */
  name: string;
  cleared: WorktreeLock[];
  proof?: UnlockProof;
}

/** Seams for tests; production uses the real process table, clock and host. */
export type UnlockDependencies = Partial<Pick<UnlockProofOptions, 'processes' | 'hostname' | 'environment' | 'secondLookMs' | 'sleep' | 'wallClock' | 'monotonic'>>;

interface Target {
  repositoryId: string;
  worktreeId: string;
  name: string;
  host?: string;
}

/**
 * The worktree to unlock: one named by slug or UUID anywhere on this machine —
 * by UUID even if it never registered — or else this checkout, by its markers.
 */
async function resolveTarget(context: CommandContext, options: UnlockOptions): Promise<Target> {
  const { registry } = context;
  if (options.identifier !== undefined) {
    const found = await registry.findGlobal(options.identifier, options.domain);
    if (!found) {
      // A worktree whose first `up` died before it registered, and whose
      // checkout may be gone too, is still named by its UUID in the locks it left.
      const orphan = (await lockedWorktrees(context.controlHome)).find((entry) => entry.worktreeId === options.identifier);
      if (orphan) return { ...orphan, name: orphan.worktreeId };
      const where = options.domain === undefined ? '' : ` under ${options.domain}`;
      const byUuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(options.identifier)
        ? ', and no lock is held for a worktree of that UUID'
        : '';
      throw new CliError(`No worktree is registered as ${options.identifier}${where}${byUuid}.`);
    }
    return { repositoryId: found.repositoryId, worktreeId: found.worktreeId, name: found.slug, host: worktreeHost(found.slug, found.domain) };
  }
  // By its identity markers, not its registration: a first `up` that died
  // before registering still left its lock behind.
  const identity = await registry.identities.inspect();
  if (!identity) {
    throw new CliError(
      `This checkout has never been brought up, so it holds no lock. Name another with '${context.commandName} worktree unlock <slug|uuid>'.`
    );
  }
  const record = await registry.store(identity.repositoryId).find(identity.worktreeId);
  return record
    ? { repositoryId: identity.repositoryId, worktreeId: identity.worktreeId, name: record.slug, host: worktreeHost(record.slug, record.domain) }
    : { repositoryId: identity.repositoryId, worktreeId: identity.worktreeId, name: identity.root };
}

/**
 * `worktree unlock`: refuses while either lock is active, does nothing when
 * neither is held, and otherwise clears both once the guard has checked the
 * holder, and every command it started on this machine, gone. Every refusal
 * is a `CliError` that says why.
 */
export async function unlock(
  context: CommandContext,
  options: UnlockOptions = {},
  dependencies: UnlockDependencies = {}
): Promise<UnlockResult> {
  const { logger } = context;
  await assertNotGoManaged(context.controlHome);
  const target = await resolveTarget(context, options);
  const locks = worktreeLocks(context.controlHome, target.repositoryId, target.worktreeId, target.name);
  const label = target.host ?? target.name;

  const described = await Promise.all(locks.map((lock) => describeLock(lock.target, lock.description)));
  const active = described.findIndex((lock) => lock.state === 'active');
  if (active >= 0) {
    const lock = described[active];
    const age = lock.heartbeatMs === undefined ? '' : `, heartbeat ${formatDuration(Date.now() - lock.heartbeatMs)} ago`;
    throw new CliError(
      `The ${locks[active]?.description ?? label} lock is held by ${holderLabel(lock.record)}${age}: it is alive, so there is nothing to unlock. ` +
      'Wait for it to finish, or stop it.'
    );
  }
  if (described.every((lock) => lock.state === 'missing')) {
    logger.info(`Nothing to unlock: no lock is held for ${label}.`);
    return { name: target.name, cleared: [] };
  }

  const processes: ProcessTable = dependencies.processes ?? processTableFor();
  const guard = unlockGuard({
    processes,
    hostname: dependencies.hostname ?? hostname(),
    markers: { project: composeProjectName(target.repositoryId, target.worktreeId), worktreeId: target.worktreeId },
    allowUnverifiable: options.allowUnverifiable === true,
    environment: dependencies.environment ?? process.env,
    ...(dependencies.secondLookMs === undefined ? {} : { secondLookMs: dependencies.secondLookMs }),
    ...(dependencies.sleep === undefined ? {} : { sleep: dependencies.sleep }),
    ...(dependencies.wallClock === undefined ? {} : { wallClock: dependencies.wallClock }),
    ...(dependencies.monotonic === undefined ? {} : { monotonic: dependencies.monotonic })
  });
  logger.info(`Checking that the holder of ${label}'s lock, and every command it started on this machine, is gone; this takes about 15 seconds.`);
  const cleared = await clearStaleWorktreeLocks(context.controlHome, target.repositoryId, target.worktreeId, target.name, guard);
  if (cleared.length === 0) {
    logger.info(`Nothing to unlock: the lock on ${label} was released while it was being checked.`);
    return { name: target.name, cleared, proof: guard.proof };
  }
  for (const line of guard.proof.verified) logger.info(`Verified: ${line}.`);
  for (const line of guard.proof.unverified) logger.warn(`Not verified, on your word (--holder-unverifiable): ${line}.`);
  logger.info(`Unlocked ${label}: cleared the ${cleared.map((lock) => `${lock.description} lock`).join(' and the ')}.`);
  return { name: target.name, cleared, proof: guard.proof };
}
