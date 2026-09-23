import { performance } from 'node:perf_hooks';
import { CliError } from './errors.ts';
import { describeLock, formatDuration, type LockDescription, type WorktreeLock, type WorktreeRecoveryOptions } from './operation-lock.ts';
import type { ProcessIdentity, ProcessTable, ScannedProcess } from './process-table.ts';
import { oneLine } from './text.ts';

// The proof `worktree unlock` owes before it clears a lock: that the holder
// is gone and that nothing it started is still running. A silent heartbeat is
// never enough on its own — a stopped or sleeping holder is silent too — so
// every lock needs positive evidence, and every doubt is a refusal.

/** How far apart a macOS holder's recorded start and `ps`'s may be: `etime` has one-second steps. */
const START_TOLERANCE_MS = 2_000;
/** The second look: longer than a heartbeat period, so a holder that only just woke has touched its lock. */
export const SECOND_LOOK_MS = 12_000;
/** Wall-clock and monotonic time may disagree by this much over the second look before it is taken again. */
const DRIFT_TOLERANCE_MS = 1_000;
const RESCAN_DELAY_MS = 1_000;

export type Verdict =
  /** Proven: the holder cannot still be running. */
  | { kind: 'gone'; reason: string }
  /** It may still be running; nothing overrides this. */
  | { kind: 'alive'; reason: string }
  /** This machine cannot check it; only the person running the command can. */
  | { kind: 'unverifiable'; reason: string };

export interface HolderJudge {
  self: ProcessIdentity;
  hostname: string;
  processes: ProcessTable;
}

/** What the lock's record, this machine, and the process table say about the holder. */
export async function judgeHolder(lock: LockDescription, judge: HolderJudge): Promise<Verdict> {
  const { record } = lock;
  if (record === undefined) return { kind: 'unverifiable', reason: 'its holder left no record of who it is' };
  const who = `PID ${record.pid} on ${JSON.stringify(oneLine(record.host))}`;
  if (record.lock !== undefined && lock.ino !== undefined) {
    // A directory born after the one the record names is another one, whatever
    // its inode. Not an earlier birth: macOS moves a directory's birth time back
    // to its mtime whenever the mtime is set before it, as a heartbeat taken
    // across a backward clock step does. Linux never changes a birth time.
    const [recorded, current] = [record.lock.birthtimeMs, lock.birthtimeMs ?? 0];
    const reborn = recorded > 0 && current > recorded;
    if (record.lock.ino !== lock.ino || reborn) {
      return { kind: 'unverifiable', reason: `its record, naming ${who}, belongs to an earlier lock, so its holder never identified itself` };
    }
  }

  if (record.machineId === undefined && record.bootId === undefined) {
    // Written before holders said which machine they were on: only the hostname to go on.
    if (record.host !== judge.hostname) return { kind: 'unverifiable', reason: `its holder, ${who}, ran on another host` };
    const state = await judge.processes.inspect(record.pid);
    if (state.state === 'absent' || state.state === 'zombie') {
      return {
        kind: 'unverifiable',
        reason: `${who} is gone, but its record, from an earlier version, cannot say whether that PID was in this PID namespace`
      };
    }
    return { kind: 'alive', reason: `${who} is running, and its record, from an earlier version, cannot tell whether that is the holder` };
  }
  // This machine, by its machine ID where both sides have one. Where either
  // has none (a container image without /etc/machine-id, say), by the boot:
  // only the same running kernel has the same boot ID, and the PID namespace
  // below tells its containers apart.
  if (record.machineId !== undefined && judge.self.machineId !== undefined) {
    if (record.machineId !== judge.self.machineId) {
      return { kind: 'unverifiable', reason: `its holder, ${who}, ran on another machine` };
    }
    if (record.bootId !== undefined && judge.self.bootId !== undefined && record.bootId !== judge.self.bootId) {
      return { kind: 'gone', reason: `this machine has restarted since ${who} took it` };
    }
  } else if (record.bootId === undefined || judge.self.bootId === undefined) {
    return { kind: 'unverifiable', reason: `this machine's identity cannot be read, so ${who} cannot be checked` };
  } else if (record.bootId !== judge.self.bootId) {
    return {
      kind: 'unverifiable',
      reason: `its holder, ${who}, ran on another machine or before this one restarted, and with no machine ID to go on, this command cannot tell which`
    };
  }
  if ((record.pidNs ?? judge.self.pidNs) !== undefined && record.pidNs !== judge.self.pidNs) {
    return { kind: 'unverifiable', reason: `its holder, ${who}, ran in another PID namespace (a container, say), which this command cannot see into` };
  }

  const state = await judge.processes.inspect(record.pid);
  if (state.state === 'absent') return { kind: 'gone', reason: `${who} no longer exists` };
  if (state.state === 'zombie') return { kind: 'gone', reason: `${who} has exited` };
  // A PID is reused. The process there now is the holder only if it started when the holder did.
  const reused = record.startTicks !== undefined && state.startTicks !== undefined
    ? state.startTicks !== record.startTicks
    : record.processStartedAtMs !== undefined && state.startedAtMs !== undefined
      ? Math.abs(state.startedAtMs - record.processStartedAtMs) > START_TOLERANCE_MS
      : false;
  if (reused) return { kind: 'gone', reason: `${who} has exited; that PID now belongs to a later process` };
  if (state.state === 'stopped') {
    return { kind: 'alive', reason: `${who} is stopped, not gone: resume it with \`kill -CONT ${record.pid}\` and let it finish, or end it` };
  }
  return { kind: 'alive', reason: `${who} is still running` };
}

export interface WorktreeMarkers {
  /** The Compose project name, `berth-<hash>`. */
  project: string;
  worktreeId: string;
}

/** Processes started for this worktree: its Compose project on the command line, or its markers in the environment. */
export function markedProcesses(processes: readonly ScannedProcess[], markers: WorktreeMarkers): ScannedProcess[] {
  const environment = new Set([`BERTH_WORKTREE_ID=${markers.worktreeId}`, `COMPOSE_PROJECT_NAME=${markers.project}`]);
  return processes.filter((candidate) =>
    candidate.args.includes(markers.project) || (candidate.env?.some((entry) => environment.has(entry)) ?? false)
  );
}

export interface UnlockProofOptions {
  processes: ProcessTable;
  hostname: string;
  markers: WorktreeMarkers;
  /** Accept holders this machine cannot check: another machine or namespace, or no record. */
  allowUnverifiable: boolean;
  /** This process's own environment. */
  environment: NodeJS.ProcessEnv;
  secondLookMs?: number;
  sleep?: (ms: number) => Promise<void>;
  wallClock?: () => number;
  monotonic?: () => number;
}

export interface UnlockProof {
  /** What was checked and found gone. */
  verified: string[];
  /** What was taken on the word of whoever ran the command. */
  unverified: string[];
  /** Processes whose environment could not be read in the scan. */
  unreadable: number;
}

export interface UnlockGuard extends Required<WorktreeRecoveryOptions> {
  /** What the proof established; complete once `verify` has returned. */
  readonly proof: UnlockProof;
}

const realSleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

/**
 * The same silent lock as before: still stale, the same directory (a lock
 * taken again is a new one), the same heartbeat, the same record.
 */
export function sameLock(before: LockDescription, after: LockDescription): boolean {
  return after.state === 'stale' && after.ino === before.ino && after.heartbeatMs === before.heartbeatMs &&
    after.recordText === before.recordText;
}

function listed(processes: readonly ScannedProcess[]): string {
  const shown = processes.slice(0, 10).map((entry) => `  PID ${entry.pid}: ${oneLine(entry.args.join(' '), 160)}`);
  const more = processes.length > shown.length ? [`  … and ${processes.length - shown.length} more`] : [];
  return [...shown, ...more].join('\n');
}

/**
 * The guard `clearStaleWorktreeLocks` runs under its recovery mutex:
 * `verify` proves every stale lock's holder gone, takes a second look, and
 * scans for anything the holder started; `beforeClear` then refuses a lock
 * that changed in any way since.
 */
export function unlockGuard(options: UnlockProofOptions): UnlockGuard {
  const sleep = options.sleep ?? realSleep;
  const wallClock = options.wallClock ?? Date.now;
  const monotonic = options.monotonic ?? (() => performance.now());
  const secondLookMs = options.secondLookMs ?? SECOND_LOOK_MS;
  const proof: UnlockProof = { verified: [], unverified: [], unreadable: 0 };
  const seen = new Map<string, LockDescription>();

  const refuse = (lock: WorktreeLock, reason: string): never => {
    throw new CliError(`Not clearing the ${lock.description} lock: ${reason}.`);
  };

  /** Judges each lock's holder, refusing a live one — or one it cannot check, unless allowed — and remembers what it saw. */
  async function judgeAll(stale: readonly WorktreeLock[], judge: HolderJudge): Promise<Map<WorktreeLock, Verdict>> {
    const verdicts = new Map<WorktreeLock, Verdict>();
    for (const lock of stale) {
      const description = await describeLock(lock.target, lock.description);
      if (description.state !== 'stale') refuse(lock, `it is ${description.state === 'active' ? 'active again' : 'gone already'}`);
      const verdict = await judgeHolder(description, judge);
      if (verdict.kind === 'alive') refuse(lock, verdict.reason);
      if (verdict.kind === 'unverifiable' && !options.allowUnverifiable) {
        refuse(lock, `${verdict.reason}. If you have made sure it, and every command it started on this machine, is gone, run this again with --holder-unverifiable`);
      }
      verdicts.set(lock, verdict);
      seen.set(lock.target, description);
    }
    return verdicts;
  }

  /** Waits out the second look, and waits again if the wall clock and the monotonic clock disagree over it. */
  async function secondLook(): Promise<void> {
    for (let attempt = 1; ; attempt++) {
      const [wall, mono] = [wallClock(), monotonic()];
      await sleep(secondLookMs);
      const drift = Math.abs((wallClock() - wall) - (monotonic() - mono));
      if (drift <= DRIFT_TOLERANCE_MS) return;
      // A suspended machine, or a clock step: the look did not really last as long as it seemed.
      if (attempt >= 3) throw new CliError('The clock kept jumping during the second look at the lock, so nothing was cleared; run this again.');
    }
  }

  const unchanged = sameLock;

  async function verify(stale: readonly WorktreeLock[]): Promise<void> {
    if (options.environment['BERTH_WORKTREE_ID'] === options.markers.worktreeId) {
      throw new CliError(
        'This command is running inside the worktree\'s own environment (BERTH_WORKTREE_ID is set, as in its hooks), ' +
        'so it cannot tell its own processes from the holder\'s. Run it from an ordinary shell.'
      );
    }
    const judge: HolderJudge = { self: await options.processes.self(), hostname: options.hostname, processes: options.processes };
    const first = await judgeAll(stale, judge);
    const before = new Map(seen);

    await secondLook();
    const second = await judgeAll(stale, judge);
    for (const lock of stale) {
      const earlier = before.get(lock.target);
      const later = seen.get(lock.target);
      if (earlier === undefined || later === undefined || !unchanged(earlier, later)) {
        refuse(lock, 'it changed while it was being checked, so its holder may be alive');
      }
    }

    const markers = options.markers;
    const scanned = await options.processes.scan();
    let survivors = markedProcesses(scanned.processes, markers);
    let unreadable = scanned.unreadable;
    let othersHidden = scanned.othersHidden === true;
    if (survivors.length > 0) {
      // Something just finishing is not a survivor, so look again a moment
      // later. Whatever is running then counts, even if it only started in
      // between: it is running.
      await sleep(RESCAN_DELAY_MS);
      const rescanned = await options.processes.scan();
      survivors = markedProcesses(rescanned.processes, markers);
      unreadable = rescanned.unreadable;
      othersHidden = rescanned.othersHidden === true;
    }
    if (survivors.length > 0) {
      throw new CliError(
        `Not clearing any lock: ${survivors.length === 1 ? 'a process' : `${survivors.length} processes`} started for this ` +
        `worktree ${survivors.length === 1 ? 'is' : 'are'} still running:\n${listed(survivors)}\n` +
        'Let them finish or stop them, then run this again.'
      );
    }

    if (othersHidden) {
      const reason = 'other users\' processes are hidden from you (/proc mounted with hidepid, say), so a command the holder ran ' +
        'as another user — through sudo, say — could not be looked for';
      if (!options.allowUnverifiable) {
        throw new CliError(`Not clearing any lock: ${reason}. If you have made sure none is running, run this again with --holder-unverifiable.`);
      }
      proof.unverified.push(reason);
    }

    for (const lock of stale) {
      const verdict = second.get(lock) ?? first.get(lock);
      if (verdict === undefined) continue;
      const silence = seen.get(lock.target)?.heartbeatMs;
      const heartbeat = silence === undefined ? '' : `; its heartbeat has been silent for ${formatDuration(wallClock() - silence)}`;
      (verdict.kind === 'gone' ? proof.verified : proof.unverified).push(`${lock.description}: ${verdict.reason}${heartbeat}`);
    }
    const unread = unreadable > 0 ? ` (${unreadable} of your processes could not be read)` : '';
    proof.verified.push(othersHidden
      ? `none of your own processes started for this worktree is still running${unread}`
      : `no command started for this worktree is still running on this machine${unread}`);
    proof.unreadable = unreadable;
  }

  async function beforeClear(lock: WorktreeLock): Promise<void> {
    const checked = seen.get(lock.target);
    const now = await describeLock(lock.target, lock.description);
    if (checked === undefined || !unchanged(checked, now)) refuse(lock, 'it changed after it was checked, so its holder may be alive');
  }

  return { verify, beforeClear, proof };
}
