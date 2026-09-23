import { readFileSync, writeFileSync, type Stats } from 'node:fs';
import { lstat, mkdir, readdir, readFile, realpath, stat, writeFile } from 'node:fs/promises';
import { hostname } from 'node:os';
import path from 'node:path';
import * as lockfile from 'proper-lockfile';
import { z } from 'zod';
import { CliError, invariant } from './errors.ts';
import { atomicWrite } from './files.ts';
import { currentIdentity, type ProcessIdentity } from './process-table.ts';
import { shellQuote } from './shell.ts';
import { oneLine } from './text.ts';
import type { Logger } from './logger.ts';
import { controlLayout, worktreeLayout } from './layout.ts';

// Never steal a lock based only on wall-clock age; laptop sleep can make a live lock look stale.
const LOCK_STALE_MS = Number.MAX_SAFE_INTEGER;
const LOCK_UPDATE_MS = 10_000;
const LOCK_RECOVERY_STALE_MS = 30_000;
const LOCK_RETRY_MAX_MS = 250;
/** With neither a budget nor an interval: 50 ms doubling to 250 ms, twenty tries — about 4.6 s. */
const DEFAULT_LOCK_BUDGET_MS = 4_600;
/** A contended registry lock clears in milliseconds; only a longer wait is worth a line of output. */
const LOCK_NOTICE_DELAY_MS = 2_000;
const LOCK_NOTICE_INTERVAL_MS = 30_000;
/**
 * A holder whose heartbeat looks old may simply be on a machine that just
 * woke up; its next heartbeat comes within one update period. Removal is
 * advised only once a second look, this long after the first, finds the
 * heartbeat unchanged.
 */
const LOCK_STALE_CONFIRMATION_MS = LOCK_UPDATE_MS + 2_000;
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
  /** How long to wait before the first "waiting for the lock" notice. */
  noticeDelayMs?: number;
  /** How often to repeat it. */
  noticeIntervalMs?: number;
  /** How long a silent heartbeat must stay silent before removal is advised. */
  staleConfirmationMs?: number;
  /** How the holding process was invoked (`berth up`, say), for whoever waits on it. */
  holder?: string;
  /** The clock the wait is measured on; tests use it to simulate a suspended machine. */
  clock?: () => number;
  /**
   * The command that proves a silent holder is gone and clears its lock
   * (`berth worktree unlock <uuid>`). Advice names it instead of an
   * `rmdir`, which would skip every check it makes.
   */
  recovery?: string;
  /** Who this process is, for its record; tests supply one. */
  identity?: () => Promise<ProcessIdentity>;
}

interface WaitPlan {
  budgetMs: number;
  firstIntervalMs: number;
  maxIntervalMs: number;
}

/**
 * What the holder wrote about itself into the lock target when it acquired.
 * Read across versions of this tool, so unknown fields are ignored, not refused.
 */
const holderRecordSchema = z.object({
  pid: z.number().int().positive(),
  host: z.string().min(1),
  command: z.string(),
  startedAt: z.string(),
  // What a later reader checks the holder against. Records written before
  // `worktree unlock` existed have none of these.
  /** The `.lock` directory this record describes: a lock taken again is a different directory. */
  lock: z.object({ ino: z.string().regex(/^\d+$/), birthtimeMs: z.number() }).optional(),
  machineId: z.string().min(1).optional(),
  bootId: z.string().min(1).optional(),
  pidNs: z.string().min(1).optional(),
  startTicks: z.number().int().nonnegative().optional(),
  processStartedAtMs: z.number().optional()
});
export type HolderRecord = z.infer<typeof holderRecordSchema>;
/** A record is one short line; anything larger is not ours and is not read. */
const HOLDER_RECORD_MAX_BYTES = 4_096;

/** The record in a lock target, and the exact text it was read from. */
async function readHolderRecord(target: string): Promise<{ record?: HolderRecord; text?: string }> {
  try {
    const { size } = await stat(target);
    if (size === 0 || size > HOLDER_RECORD_MAX_BYTES) return {};
    const text = await readFile(target, 'utf8');
    const parsed = holderRecordSchema.safeParse(JSON.parse(text));
    return parsed.success ? { record: parsed.data, text } : { text };
  } catch {
    return {};
  }
}

/**
 * Targets whose record this process wrote and still holds. A process that
 * exits holding one empties it on the way out, as proper-lockfile removes the
 * `.lock` directory. A fatal signal can still leave a record beside a lock
 * that is gone; the next holder writes its own over it, and until then the
 * record's `lock` binding shows it describes an earlier directory.
 */
const heldRecords = new Set<string>();
let exitHandlerInstalled = false;

/** Exported for tests: what the exit handler does. */
export function forgetRecordsOnExit(): void {
  for (const target of heldRecords) {
    try {
      const parsed = holderRecordSchema.safeParse(JSON.parse(readFileSync(target, 'utf8')));
      // Only our own: someone may already have taken the lock and written theirs.
      if (parsed.success && parsed.data.pid === process.pid) writeFileSync(target, '');
    } catch {
      // Nothing more can be done on the way out.
    }
  }
}

/** Exported for tests: what acquiring does once its record is written. */
export function rememberHeldRecord(target: string): void {
  heldRecords.add(target);
  if (!exitHandlerInstalled) {
    exitHandlerInstalled = true;
    process.on('exit', forgetRecordsOnExit);
  }
}

interface LockHolder {
  state: LockRecoveryState;
  lockDirectory: string;
  /** Milliseconds since the holder last touched the lock. */
  heartbeatAgeMs?: number;
  /** The heartbeat's timestamp, to tell "still silent" from "silent again". */
  heartbeatMs?: number;
  heldForMs?: number;
  record?: HolderRecord;
}

/**
 * A lock target is a managed file of ours; a symlink there would put the
 * holder record — and the `.lock` directory beside it — somewhere else.
 */
async function ensurePlainTarget(target: string, description: string): Promise<void> {
  await mkdir(path.dirname(target), { recursive: true, mode: 0o700 });
  for (;;) {
    let metadata: Stats | undefined;
    try {
      metadata = await lstat(target);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error;
    }
    if (metadata !== undefined) {
      invariant(
        metadata.isFile() && !metadata.isSymbolicLink(),
        `Refusing to use ${target} for the ${description} lock: it is not a plain file ` +
        '(a directory or symbolic link stands in its place). Inspect it before removing anything.'
      );
      return;
    }
    try {
      await writeFile(target, '', { flag: 'wx', mode: 0o600 });
      return;
    } catch (error) {
      // Another waiter created it in between; look at what it made before trusting it.
      if ((error as NodeJS.ErrnoException).code !== 'EEXIST') throw error;
    }
  }
}

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

/** `4s`, `12m 30s`, `2h 5m`, `3d 4h` — what a person at a terminal wants. */
export function formatDuration(ms: number): string {
  const seconds = Math.max(0, Math.round(ms / 1_000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${seconds % 60}s`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ${minutes % 60}m`;
  return `${Math.floor(hours / 24)}d ${hours % 24}h`;
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

/**
 * How to clear a worktree's locks once their holder is dead, as every notice
 * and `doctor` names it: by UUID, since a slug is unique only within its
 * domain, and unlock finds a worktree by UUID whether it registered or not.
 */
export function worktreeUnlockCommand(commandName: string, worktreeId: string): string {
  return `${commandName} worktree unlock ${worktreeId}`;
}

export function routerLockTarget(controlHome: string): string {
  return controlLayout(controlHome).routerLockPath;
}

/**
 * An advisory lock with an effectively infinite stale window and a heartbeat,
 * so a long-running holder is never silently stolen. Recovery from a genuinely
 * dead holder is never automatic. A waiter is told what it is waiting for:
 * which process has held the lock for how long, whether it is still
 * heartbeating, and — only once the heartbeat has stayed silent across two
 * looks — exactly what to remove after confirming the process is gone.
 */
export class ScopedLock {
  private readonly target: string;
  private readonly description: string;
  private readonly logger: Logger;
  private readonly acquireTimeoutMs?: number;
  private readonly retryIntervalMs?: number;
  private readonly noticeDelayMs: number;
  private readonly noticeIntervalMs: number;
  private readonly staleConfirmationMs: number;
  private readonly holderCommand?: string;
  private readonly clock: () => number;
  private readonly recovery?: string;
  private readonly identity: () => Promise<ProcessIdentity>;
  private lockDirectory?: string;
  private releaseLock?: () => Promise<void>;
  private compromised?: Error;
  /** The heartbeat first seen silent, and when; cleared as soon as it moves. */
  private silentSince?: { at: number; heartbeatMs: number };

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
    invariant(
      options.noticeDelayMs === undefined || options.noticeDelayMs >= 0,
      'Lock notice delay must not be negative.'
    );
    invariant(
      options.noticeIntervalMs === undefined || options.noticeIntervalMs > 0,
      'Lock notice interval must be greater than zero.'
    );
    invariant(
      options.staleConfirmationMs === undefined || options.staleConfirmationMs > 0,
      'Lock stale confirmation window must be greater than zero.'
    );
    this.target = path.resolve(target);
    this.description = description;
    this.logger = logger;
    this.acquireTimeoutMs = options.acquireTimeoutMs;
    this.retryIntervalMs = options.retryIntervalMs;
    this.noticeDelayMs = options.noticeDelayMs ?? LOCK_NOTICE_DELAY_MS;
    this.noticeIntervalMs = options.noticeIntervalMs ?? LOCK_NOTICE_INTERVAL_MS;
    this.staleConfirmationMs = options.staleConfirmationMs ?? LOCK_STALE_CONFIRMATION_MS;
    this.holderCommand = options.holder;
    this.clock = options.clock ?? Date.now;
    this.recovery = options.recovery;
    this.identity = options.identity ?? currentIdentity;
  }

  /**
   * Three schedules, chosen by which options were given:
   *  - a budget: poll at a fixed interval until the budget is spent;
   *  - an interval alone: poll at that fixed interval for twenty tries;
   *  - neither: a short exponential backoff, about 4.6 seconds in all.
   */
  private waitPlan(): WaitPlan {
    if (this.acquireTimeoutMs !== undefined) {
      const interval = Math.min(this.retryIntervalMs ?? LOCK_RETRY_MAX_MS, this.acquireTimeoutMs);
      return { budgetMs: this.acquireTimeoutMs, firstIntervalMs: interval, maxIntervalMs: interval };
    }
    if (this.retryIntervalMs !== undefined) {
      return { budgetMs: 20 * this.retryIntervalMs, firstIntervalMs: this.retryIntervalMs, maxIntervalMs: this.retryIntervalMs };
    }
    return { budgetMs: DEFAULT_LOCK_BUDGET_MS, firstIntervalMs: 50, maxIntervalMs: LOCK_RETRY_MAX_MS };
  }

  /** One attempt; `ELOCKED` means somebody else has it, anything else is a real error. */
  private async attempt(): Promise<boolean> {
    try {
      this.releaseLock = await lockfile.lock(this.target, {
        realpath: true,
        stale: LOCK_STALE_MS,
        update: LOCK_UPDATE_MS,
        retries: 0,
        onCompromised: (error) => {
          this.compromised = error;
          // The lock, and whatever record is in it now, belongs to someone else.
          heldRecords.delete(this.target);
          this.logger.error(`${this.description} lock was compromised: ${error.message}`);
        }
      });
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === 'ELOCKED') return false;
      throw error;
    }
    await this.recordHolder();
    return true;
  }

  /**
   * The `.lock` directory must stay empty (release is an rmdir), so the record
   * goes into the target file, which is ours for as long as the lock is. It
   * says which directory it describes and who this process is in terms a
   * later reader can check, so `worktree unlock` can prove a silent holder
   * gone. A holder that cannot say who it is could never be shown to be gone, so
   * it gives the lock back rather than hold it unidentified.
   */
  private async recordHolder(): Promise<void> {
    try {
      invariant(this.lockDirectory !== undefined, `${this.description} lock recorded before acquisition began.`);
      const [identity, directory] = await Promise.all([this.identity(), lstat(this.lockDirectory, { bigint: true })]);
      const record: HolderRecord = {
        pid: process.pid,
        host: hostname(),
        command: oneLine(this.holderCommand ?? ''),
        startedAt: new Date().toISOString(),
        lock: { ino: directory.ino.toString(), birthtimeMs: Number(directory.birthtimeMs) },
        ...(identity.machineId === undefined ? {} : { machineId: identity.machineId }),
        ...(identity.bootId === undefined ? {} : { bootId: identity.bootId }),
        ...(identity.pidNs === undefined ? {} : { pidNs: identity.pidNs }),
        ...(identity.startTicks === undefined ? {} : { startTicks: identity.startTicks }),
        ...(identity.startedAtMs === undefined ? {} : { processStartedAtMs: identity.startedAtMs })
      };
      await atomicWrite(this.target, `${JSON.stringify(record)}\n`);
      rememberHeldRecord(this.target);
    } catch (error) {
      const release = this.releaseLock;
      this.releaseLock = undefined;
      await release?.().catch(() => undefined);
      throw new CliError(
        `Could not record this process as the holder of the ${this.description} lock, so it was given back: ` +
        `${error instanceof Error ? error.message : String(error)}`
      );
    }
  }

  private async holderRecord(): Promise<HolderRecord | undefined> {
    return (await readHolderRecord(this.target)).record;
  }

  /** Who holds the lock, as far as the filesystem can tell. */
  private async holder(): Promise<LockHolder> {
    invariant(this.lockDirectory !== undefined, `${this.description} lock was inspected before acquisition began.`);
    const lockDirectory = this.lockDirectory;
    const state = await inspectLockForRecovery(this.target, this.description);
    if (state === 'missing') return { state, lockDirectory };
    let metadata: Stats;
    try {
      metadata = await lstat(lockDirectory);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === 'ENOENT') return { state: 'missing', lockDirectory };
      throw error;
    }
    const record = await this.holderRecord();
    const now = this.clock();
    const startedMs = record === undefined ? Number.NaN : Date.parse(record.startedAt);
    // Filesystems without a birth time report the epoch or the change time; neither is a start.
    const birthMs = metadata.birthtimeMs > 0 && metadata.birthtimeMs <= metadata.mtimeMs ? metadata.birthtimeMs : Number.NaN;
    const sinceMs = Number.isFinite(startedMs) ? startedMs : birthMs;
    return {
      state,
      lockDirectory,
      heartbeatMs: metadata.mtimeMs,
      heartbeatAgeMs: Math.max(0, now - metadata.mtimeMs),
      ...(Number.isFinite(sinceMs) ? { heldForMs: Math.max(0, now - sinceMs) } : {}),
      ...(record === undefined ? {} : { record })
    };
  }

  /**
   * Two silent looks at the same heartbeat, far enough apart that a resumed
   * machine would have heartbeated in between. Any movement starts over.
   */
  private confirmStale(holder: LockHolder, now: number): boolean {
    if (holder.state !== 'stale' || holder.heartbeatMs === undefined) {
      this.silentSince = undefined;
      return false;
    }
    if (this.silentSince === undefined || this.silentSince.heartbeatMs !== holder.heartbeatMs) {
      this.silentSince = { at: now, heartbeatMs: holder.heartbeatMs };
      return false;
    }
    return now - this.silentSince.at >= this.staleConfirmationMs;
  }

  private who(holder: LockHolder): string {
    const held = holder.heldForMs === undefined ? undefined : formatDuration(holder.heldForMs);
    if (holder.record === undefined) {
      return held === undefined ? 'An unidentified holder has it' : `An unidentified holder has held it for ${held}`;
    }
    const { record } = holder;
    // Quoted like the command beside it: plain prose in a hostname would
    // otherwise close the clause and append a sentence of its own.
    const where = `PID ${record.pid} on ${JSON.stringify(oneLine(record.host))}`;
    const identity = record.command === '' ? where : `${JSON.stringify(oneLine(record.command))} (${where})`;
    return held === undefined ? `The holder is ${identity}` : `The holder, ${identity}, has held it for ${held}`;
  }

  /**
   * What a waiter should know and, once a holder is confirmed silent, what to
   * do. `final` is the giving-up message: it cannot promise another look.
   */
  private advice(holder: LockHolder, confirmed: boolean, final: boolean): string {
    const ageMs = holder.heartbeatAgeMs ?? 0;
    const age = ageMs < 1_000 ? 'just now' : `${formatDuration(ageMs)} ago`;
    const who = this.who(holder);
    if (holder.state === 'active') {
      return `${who}; its last heartbeat was ${age}, so it is alive. Wait for it to finish, or stop it${final ? ', then retry' : ''}.`;
    }
    if (holder.state === 'stale') {
      if (this.recovery !== undefined) {
        const recover = `run \`${this.recovery}\`, which clears it only once it has checked that the holder, ` +
          'and every command it started on this machine, is gone';
        if (confirmed) {
          return `${who}; its heartbeat has stayed silent for ${formatDuration(ageMs)}. Locks are never stolen automatically: ` +
            `${recover}${final ? ', then retry.' : '; this command will then carry on by itself.'}`;
        }
        if (final) {
          return `${who}; its last heartbeat was ${age}. If its machine only just woke up, the next heartbeat arrives within ` +
            `${formatDuration(LOCK_UPDATE_MS)}, so this is too soon to call it dead. Locks are never stolen automatically: ` +
            `${recover}, then retry.`;
        }
      }
      const holderProcess = holder.record === undefined
        ? 'that process'
        : `that process (\`ps -p ${holder.record.pid}\` on ${JSON.stringify(oneLine(holder.record.host))})`;
      const gone = `${holderProcess} is gone, and nothing it started (such as a \`docker compose\` command) is still running`;
      const removal = `remove the directory with \`rmdir ${shellQuote(holder.lockDirectory)}\``;
      if (confirmed) {
        return `${who}; its heartbeat has stayed silent for ${formatDuration(ageMs)}. Locks are never stolen automatically: ` +
          `if ${gone}, ${removal}${final ? ' and retry.' : '; this command will then carry on by itself.'}`;
      }
      const wake = `If its machine only just woke up, the next heartbeat arrives within ${formatDuration(LOCK_UPDATE_MS)}`;
      if (final) {
        return `${who}; its last heartbeat was ${age}. ${wake}, so this is too soon to call it dead. ` +
          'Locks are never stolen automatically: run this command again in a moment; if the heartbeat still has not moved and ' +
          `${gone}, ${removal} and retry.`;
      }
      return `${who}; its last heartbeat was ${age}. ${wake}, so this may be nothing — ` +
        `checking again in ${formatDuration(this.noticeIntervalMs)}.`;
    }
    invariant(holder.state !== 'missing', `${this.description} lock: no advice to give about nobody.`);
    return holder.state;
  }

  private async notice(now: number): Promise<void> {
    const holder = await this.holder();
    if (holder.state === 'missing') return;
    const confirmed = this.confirmStale(holder, now);
    const line = `Waiting for the ${this.description} lock. ${this.advice(holder, confirmed, false)}`;
    if (confirmed) this.logger.warn(line);
    else this.logger.info(line);
  }

  async acquire(): Promise<void> {
    invariant(!this.releaseLock, `${this.description} lock is already held by this process.`);
    await ensurePlainTarget(this.target, this.description);
    // proper-lockfile locks the real path; report the same one.
    this.lockDirectory = `${await realpath(this.target)}.lock`;
    this.silentSince = undefined;
    this.compromised = undefined;
    const plan = this.waitPlan();
    const started = this.clock();
    let deadline = started + plan.budgetMs;
    let interval = plan.firstIntervalMs;
    let nextNotice = started + this.noticeDelayMs;
    let lastTick = started;
    let slept = 0;
    let handovers = 0;
    for (;;) {
      if (await this.attempt()) return;
      const now = this.clock();
      // Far more wall-clock time than the sleep means the machine was suspended
      // (or the filesystem stalled): the wait did not really elapse, and the
      // holder deserves a heartbeat's grace before anything is concluded about
      // it. The deadline grows by exactly the gap, so a stall can only ever
      // defer the deadline, never remove it.
      const jump = now - lastTick - slept;
      if (jump > LOCK_UPDATE_MS) {
        deadline += jump;
        nextNotice = Math.max(nextNotice, now + this.staleConfirmationMs);
        this.silentSince = undefined;
      }
      lastTick = now;
      if (now >= deadline) {
        const holder = await this.holder();
        if (holder.state === 'missing') {
          // Released between the attempt and the look: one more go, not a contradiction.
          if (handovers++ < 3) {
            slept = 0;
            continue;
          }
          throw new CliError(`The ${this.description} lock changed hands while this command was giving up; run it again.`);
        }
        const confirmed = this.confirmStale(holder, now);
        throw new CliError(`Another process holds the ${this.description} lock. ${this.advice(holder, confirmed, true)}`);
      }
      if (now >= nextNotice) {
        await this.notice(now);
        nextNotice = this.clock() + this.noticeIntervalMs;
      }
      slept = Math.min(interval, deadline - now);
      await sleep(slept);
      interval = Math.min(interval * 2, plan.maxIntervalMs);
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
    if (!release) return;
    // A compromised lock already belongs to someone else, record included; and
    // proper-lockfile has dropped it, so there is nothing left to release. The
    // compromise can also land during the truncation, hence the second look.
    if (this.compromised) return;
    // Only a holder should be described; the record is wiped while the lock is still ours.
    heldRecords.delete(this.target);
    await writeFile(this.target, '', { mode: 0o600 }).catch(() => undefined);
    if (this.compromised) return;
    try {
      await release();
    } catch (error) {
      if (!this.compromised) throw error;
    }
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
  const unexpected = `Refusing to touch the ${description} lock: ${lockDirectory} is not a plain directory ` +
    '(a file or symbolic link stands in its place). Inspect it before removing anything.';
  try {
    const metadata = await lstat(lockDirectory);
    invariant(metadata.isDirectory() && !metadata.isSymbolicLink(), unexpected);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return 'missing';
    throw error;
  }

  let active: boolean;
  try {
    active = await lockfile.check(resolvedTarget, {
      realpath: true,
      stale: LOCK_RECOVERY_STALE_MS
    });
  } catch (error) {
    // A lock directory whose target file is gone: proper-lockfile cannot
    // resolve the target, but the heartbeat is still the directory's mtime.
    if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error;
    try {
      active = Date.now() - (await lstat(lockDirectory)).mtimeMs <= LOCK_RECOVERY_STALE_MS;
    } catch (inner) {
      if ((inner as NodeJS.ErrnoException).code === 'ENOENT') return 'missing';
      throw inner;
    }
  }
  if (active) return 'active';

  // The owner may have released the lock between the initial lstat and check.
  try {
    const metadata = await lstat(lockDirectory);
    invariant(metadata.isDirectory() && !metadata.isSymbolicLink(), unexpected);
    return 'stale';
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return 'missing';
    throw error;
  }
}

/** A holder as every report names it: `"berth up" (PID 42 on "laptop")`. */
export function holderLabel(record: HolderRecord | undefined): string {
  if (record === undefined) return 'an unidentified holder';
  const where = `PID ${record.pid} on ${JSON.stringify(oneLine(record.host))}`;
  return record.command === '' ? where : `${JSON.stringify(oneLine(record.command))} (${where})`;
}

/** Everything a reader can learn about a lock without taking or changing anything. */
export interface LockDescription {
  state: LockRecoveryState;
  lockDirectory: string;
  /** The heartbeat: the directory's modification time. */
  heartbeatMs?: number;
  /** The directory's identity; a lock taken again is a different directory. */
  ino?: string;
  birthtimeMs?: number;
  record?: HolderRecord;
  /** The record file's exact content, to tell "unchanged" from "rewritten". */
  recordText?: string;
}

/** Read-only: never creates the target, never touches the `.lock` directory. */
export async function describeLock(target: string, description: string): Promise<LockDescription> {
  const resolvedTarget = path.resolve(target);
  const lockDirectory = `${resolvedTarget}.lock`;
  const state = await inspectLockForRecovery(resolvedTarget, description);
  if (state === 'missing') return { state, lockDirectory };
  let metadata;
  try {
    metadata = await lstat(lockDirectory, { bigint: true });
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return { state: 'missing', lockDirectory };
    throw error;
  }
  const { record, text } = await readHolderRecord(resolvedTarget);
  return {
    state,
    lockDirectory,
    heartbeatMs: Number(metadata.mtimeMs),
    ino: metadata.ino.toString(),
    birthtimeMs: Number(metadata.birthtimeMs),
    ...(record === undefined ? {} : { record }),
    ...(text === undefined ? {} : { recordText: text })
  };
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

  // proper-lockfile claims through the target's real path, so it must exist.
  await ensurePlainTarget(resolvedTarget, description);
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
  // The dead holder's record goes with its lock, while the claim is ours:
  // left behind, the next holder's lock would carry it until that holder wrote its own.
  await writeFile(resolvedTarget, '', { mode: 0o600 }).catch(() => undefined);
  await release();
  return true;
}

export type WorktreeLockKind = 'development' | 'operation';

export interface WorktreeLock {
  kind: WorktreeLockKind;
  target: string;
  /** As a waiter names it: `worktree main`, `main development session`. */
  description: string;
}

/**
 * A worktree's two locks, in the order they are cleared. Every operation takes
 * the operation lock first, so while it stays in place nothing can start; and
 * an exclusive operation waiting on it will not then trip over a development
 * lock left behind by the same dead holder.
 */
export function worktreeLocks(controlHome: string, repositoryId: string, worktreeId: string, description: string): WorktreeLock[] {
  return [
    { kind: 'development', target: developmentLockTarget(controlHome, repositoryId, worktreeId), description: `${description} development session` },
    { kind: 'operation', target: worktreeLockTarget(controlHome, repositoryId, worktreeId), description: `worktree ${description}` }
  ];
}

export interface WorktreeRecoveryOptions {
  /**
   * Runs once, under the recovery mutex, with every stale lock, before any is
   * claimed: the check that the holder, and every command it started on this machine, is gone.
   * Throwing clears nothing.
   */
  verify?: (stale: readonly WorktreeLock[]) => Promise<void>;
  /** Runs immediately before each claim; throwing stops there. */
  beforeClear?: (lock: WorktreeLock) => Promise<void>;
}

/**
 * Clears a worktree's stale development and operation locks under a recovery
 * mutex, so two concurrent unlock attempts cannot interleave. Nothing is
 * cleared while either lock is active, and nothing until `verify` has proven
 * the holder gone; each claim is then guarded again. `description` is the
 * worktree's slug. Returns the locks it cleared.
 */
export async function clearStaleWorktreeLocks(
  controlHome: string,
  repositoryId: string,
  worktreeId: string,
  description: string,
  options: WorktreeRecoveryOptions = {}
): Promise<WorktreeLock[]> {
  const recoveryTarget = worktreeRecoveryLockTarget(controlHome, repositoryId, worktreeId);
  await ensurePlainTarget(recoveryTarget, `${description} recovery`);

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

  const recover = async (): Promise<WorktreeLock[]> => {
    const locks = worktreeLocks(controlHome, repositoryId, worktreeId, description);

    // The recovery mutex excludes another unlock before anything is proven.
    // Re-read both locks while holding it so a queued recovery does not act
    // on state observed before the prior recovery completed.
    const states = await Promise.all(locks.map((lock) => inspectLockForRecovery(lock.target, lock.description)));
    assertRecoveryHealthy();
    const active = locks.find((_, index) => states[index] === 'active');
    if (active) throw new CliError(STILL_ACTIVE(active.description));
    const stale = locks.filter((_, index) => states[index] === 'stale');
    if (stale.length > 0) await options.verify?.(stale);

    const cleared: WorktreeLock[] = [];
    for (const lock of stale) {
      assertRecoveryHealthy();
      const beforeClear = async (): Promise<void> => {
        assertRecoveryHealthy();
        await options.beforeClear?.(lock);
        assertRecoveryHealthy();
      };
      if (await clearStaleLock(lock.target, lock.description, { beforeClear })) cleared.push(lock);
    }
    assertRecoveryHealthy();
    return cleared;
  };

  let cleared: WorktreeLock[];
  try {
    cleared = await recover();
  } catch (error) {
    // A compromised mutex cannot be released cleanly; the failure already
    // being reported must not be replaced by that one.
    await releaseRecovery().catch(() => undefined);
    throw error;
  }
  try {
    await releaseRecovery();
  } catch (error) {
    // The locks are already cleared; only the recovery mutex went wrong.
    throw new CliError(
      `Cleared the stale locks of ${description}, but its recovery lock could not be released: ` +
      `${error instanceof Error ? error.message : String(error)}`
    );
  }
  return cleared;
}

/**
 * Every worktree with a `.lock` directory for its operation or development
 * lock — registered or not — read without touching anything.
 */
export async function lockedWorktrees(controlHome: string): Promise<{ repositoryId: string; worktreeId: string }[]> {
  const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
  const worktrees = path.join(controlLayout(controlHome).locksDir, 'worktrees');
  const found: { repositoryId: string; worktreeId: string }[] = [];
  for (const repositoryId of await entriesOf(worktrees)) {
    if (!uuid.test(repositoryId)) continue;
    const ids = new Set<string>();
    for (const entry of await entriesOf(path.join(worktrees, repositoryId))) {
      const worktreeId = /^(.+?)(?:\.dev)?\.lock$/.exec(entry)?.[1];
      if (worktreeId !== undefined && uuid.test(worktreeId)) ids.add(worktreeId);
    }
    for (const worktreeId of [...ids].sort()) found.push({ repositoryId, worktreeId });
  }
  return found;
}

async function entriesOf(directory: string): Promise<string[]> {
  try {
    return await readdir(directory);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return [];
    throw error;
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
