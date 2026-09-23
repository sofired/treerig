import { execFile } from 'node:child_process';
import { readdir, readFile, readlink, stat } from 'node:fs/promises';
import { performance } from 'node:perf_hooks';
import { CliError } from './errors.ts';

// What can be proven about a process from outside it, and nothing more. A
// lock holder writes its identity into its record when it takes a lock; a
// later reader checks that identity against the machine, this boot of it,
// and the process table — never against a wall clock, which can step. Linux
// reads /proc directly (no `ps`, which slim images lack and BusyBox answers
// differently); macOS asks `ps`, whose elapsed time is measured on the same
// wall clock the holder recorded its start on.

/** Who a process is, in the terms a later reader can check it by. */
export interface ProcessIdentity {
  /** The machine: `/etc/machine-id` on Linux, the platform UUID on macOS. */
  machineId?: string;
  /** This boot of it. A different one means every process of the earlier boot is gone. */
  bootId?: string;
  /** Linux: the PID namespace a PID means something in. */
  pidNs?: string;
  /** Linux: the process's start, in clock ticks since boot — no wall clock involved. */
  startTicks?: number;
  /** macOS: the process's start, in epoch milliseconds. */
  startedAtMs?: number;
}

export type ProcessState =
  | { state: 'absent' }
  | { state: 'zombie' | 'stopped' | 'running'; startTicks?: number; startedAtMs?: number };

export interface ScannedProcess {
  pid: number;
  args: readonly string[];
  /** `NAME=value` entries; absent when the environment could not be read. */
  env?: readonly string[];
}

export interface ProcessScan {
  processes: ScannedProcess[];
  /** This user's processes whose command line or environment could not be read. */
  unreadable: number;
  /**
   * Other users' processes are hidden from this one (Linux `/proc` mounted
   * with `hidepid`), so a command the holder ran as someone else — through
   * `sudo`, say — could not be looked for.
   */
  othersHidden?: boolean;
}

export interface ProcessTable {
  /** This process, as a lock record describes it. */
  self(): Promise<ProcessIdentity>;
  inspect(pid: number): Promise<ProcessState>;
  /** Every other process in this PID namespace, with what could be read of it. */
  scan(): Promise<ProcessScan>;
}

/* -------------------------------------------------------------- parsing ---- */

/**
 * `/proc/<pid>/stat`: the command name is parenthesised and may itself hold
 * spaces and parentheses, so fields are counted from the last `)`. The state
 * is field 3 and the start time field 22.
 */
export function parseProcStat(text: string): { state: string; startTicks: number } | undefined {
  const close = text.lastIndexOf(')');
  if (close < 0) return undefined;
  const fields = text.slice(close + 2).split(' ');
  const state = fields[0] ?? '';
  const startTicks = Number(fields[19]);
  if (!/^[A-Za-z]$/.test(state) || !Number.isSafeInteger(startTicks)) return undefined;
  return { state, startTicks };
}

/** A process-state letter, as `/proc` and `ps` both spell it. */
export function stateOf(letter: string): 'zombie' | 'stopped' | 'running' {
  if (letter === 'Z' || letter === 'X' || letter === 'x') return 'zombie';
  if (letter === 'T' || letter === 't') return 'stopped';
  return 'running';
}

/** `ps`'s elapsed time, `[[dd-]hh:]mm:ss`, in seconds. */
export function parseEtime(text: string): number | undefined {
  const match = /^(?:(\d+)-)?(?:(\d+):)?(\d+):(\d+)$/.exec(text.trim());
  if (!match) return undefined;
  const [days, hours, minutes, seconds] = [match[1], match[2], match[3], match[4]].map((part) => Number(part ?? 0));
  return (((days ?? 0) * 24 + (hours ?? 0)) * 60 + (minutes ?? 0)) * 60 + (seconds ?? 0);
}

/** One `ps -o pid=,stat=,etime=` line — for the PID asked about, or nothing to go on. */
export function parsePsLine(line: string, pid: number): { stat: string; elapsedSeconds: number } | undefined {
  const [shown, stat, etime] = line.trim().split(/\s+/);
  const elapsedSeconds = parseEtime(etime ?? '');
  if (Number(shown) !== pid || stat === undefined || !/^[A-Za-z]/.test(stat) || elapsedSeconds === undefined) return undefined;
  return { stat, elapsedSeconds };
}

/** `ioreg -rd1 -c IOPlatformExpertDevice` → the platform UUID. */
export function parseIoregUuid(text: string): string | undefined {
  return /"IOPlatformUUID"\s*=\s*"([^"]+)"/.exec(text)?.[1];
}

/**
 * `ps -A -ww -E -o pid=,command=`: arguments and environment in one column,
 * space-separated. They cannot be told apart, so each whole token is offered
 * as both — a marker is a whole token either way.
 */
export function parsePsScan(text: string, exclude: ReadonlySet<number>): ScannedProcess[] {
  const processes: ScannedProcess[] = [];
  for (const line of text.split('\n')) {
    const match = /^\s*(\d+)\s+(.*)$/.exec(line);
    if (!match) continue;
    const pid = Number(match[1]);
    if (exclude.has(pid)) continue;
    const tokens = (match[2] ?? '').split(/\s+/).filter(Boolean);
    processes.push({ pid, args: tokens, env: tokens });
  }
  return processes;
}

/* ---------------------------------------------------------------- Linux ---- */

const nulSplit = (text: string): string[] => text.split('\0').filter(Boolean);

function errno(error: unknown): string | undefined {
  return (error as NodeJS.ErrnoException).code;
}

/** Reads `/proc` itself; `root` is for tests. */
export class LinuxProcessTable implements ProcessTable {
  private readonly root: string;
  private readonly machineIdFiles: readonly string[];
  private readonly uid: number | undefined;

  constructor(
    root = '/proc',
    machineIdFiles: readonly string[] = ['/etc/machine-id', '/var/lib/dbus/machine-id'],
    uid: number | undefined = process.getuid?.()
  ) {
    this.root = root;
    this.machineIdFiles = machineIdFiles;
    this.uid = uid;
  }

  private async firstLine(file: string): Promise<string | undefined> {
    try {
      return (await readFile(file, 'utf8')).trim() || undefined;
    } catch {
      return undefined;
    }
  }

  async self(): Promise<ProcessIdentity> {
    let machineId: string | undefined;
    for (const file of this.machineIdFiles) {
      machineId = await this.firstLine(file);
      if (machineId !== undefined) break;
    }
    const bootId = await this.firstLine(`${this.root}/sys/kernel/random/boot_id`);
    const pidNs = await readlink(`${this.root}/self/ns/pid`).catch(() => undefined);
    const stat = parseProcStat(await readFile(`${this.root}/self/stat`, 'utf8').catch(() => ''));
    return {
      ...(machineId === undefined ? {} : { machineId }),
      ...(bootId === undefined ? {} : { bootId }),
      ...(pidNs === undefined ? {} : { pidNs }),
      ...(stat === undefined ? {} : { startTicks: stat.startTicks })
    };
  }

  async inspect(pid: number): Promise<ProcessState> {
    let text: string;
    try {
      text = await readFile(`${this.root}/${pid}/stat`, 'utf8');
    } catch (error) {
      if (errno(error) === 'ENOENT' || errno(error) === 'ESRCH') return { state: 'absent' };
      throw error;
    }
    const stat = parseProcStat(text);
    if (stat === undefined) throw new CliError(`Could not read the state of process ${pid} from ${this.root}.`);
    return { state: stateOf(stat.state), startTicks: stat.startTicks };
  }

  /** Whose a process is — or that it has gone. An owner that cannot be read is not assumed to be us. */
  private async ownerOf(pid: number): Promise<'ours' | 'theirs' | 'gone'> {
    try {
      return (await stat(`${this.root}/${pid}`)).uid === this.uid ? 'ours' : 'theirs';
    } catch (error) {
      return errno(error) === 'ENOENT' || errno(error) === 'ESRCH' ? 'gone' : 'theirs';
    }
  }

  async scan(): Promise<ProcessScan> {
    const own = await readlink(`${this.root}/self/ns/pid`).catch(() => undefined);
    const processes: ScannedProcess[] = [];
    let unreadable = 0;
    // `hidepid=2` hides every other user's process from a non-root one, init included.
    let othersHidden = this.uid !== undefined && this.uid !== 0 &&
      !await stat(`${this.root}/1`).then(() => true, () => false);
    for (const entry of await readdir(this.root)) {
      if (!/^\d+$/.test(entry)) continue;
      const pid = Number(entry);
      if (pid === process.pid) continue;
      // Another namespace's processes are not this worktree's children, and
      // their PIDs mean nothing here: a container's own services, say.
      const namespace = await readlink(`${this.root}/${pid}/ns/pid`).catch(() => undefined);
      if (own !== undefined && namespace !== undefined && namespace !== own) continue;
      let args: string[];
      try {
        args = nulSplit(await readFile(`${this.root}/${pid}/cmdline`, 'utf8'));
      } catch (error) {
        if (errno(error) === 'ENOENT' || errno(error) === 'ESRCH') continue;   // gone mid-scan
        // `hidepid=1`: visible, but not readable. Ours would be a gap; someone else's, a blind spot.
        const owner = await this.ownerOf(pid);
        if (owner === 'ours') unreadable += 1;
        else if (owner === 'theirs') othersHidden = true;
        continue;
      }
      let env: string[] | undefined;
      try {
        env = nulSplit(await readFile(`${this.root}/${pid}/environ`, 'utf8'));
      } catch (error) {
        if (errno(error) === 'ENOENT' || errno(error) === 'ESRCH') continue;
        // Another user's environment is private on every Linux host, so such
        // a process is judged by its command line alone (the README says so);
        // counting them would make every scan look blind. Only an unreadable
        // one of ours is a gap worth reporting.
        if (await this.ownerOf(pid) === 'ours') unreadable += 1;
      }
      processes.push(env === undefined ? { pid, args } : { pid, args, env });
    }
    return { processes, unreadable, othersHidden };
  }
}

/* ---------------------------------------------------------------- macOS ---- */

export interface CommandResult {
  stdout: string;
  exitCode: number;
  pid?: number;
}

/** A clean environment: no `PS_FORMAT`, `COLUMNS` or locale to reshape `ps` output. */
const QUIET_ENV = { PATH: '/bin:/usr/bin:/usr/sbin:/sbin', LC_ALL: 'C' };

export function runQuietly(command: string, args: readonly string[]): Promise<CommandResult> {
  return new Promise((resolve, reject) => {
    const child = execFile(command, [...args], { env: QUIET_ENV, timeout: 30_000, maxBuffer: 256 * 1024 * 1024 }, (error, stdout) => {
      const code = (error as { code?: unknown } | null)?.code;
      if (error && typeof code !== 'number') {
        reject(new CliError(`\`${command}\` could not be run: ${error.message}`));
        return;
      }
      resolve({ stdout, exitCode: typeof code === 'number' ? code : 0, ...(child.pid === undefined ? {} : { pid: child.pid }) });
    });
  });
}

export class DarwinProcessTable implements ProcessTable {
  private readonly run: (command: string, args: readonly string[]) => Promise<CommandResult>;
  private readonly now: () => number;

  constructor(run = runQuietly, now: () => number = Date.now) {
    this.run = run;
    this.now = now;
  }

  async self(): Promise<ProcessIdentity> {
    const ioreg = await this.run('/usr/sbin/ioreg', ['-rd1', '-c', 'IOPlatformExpertDevice']).catch(() => undefined);
    const boot = await this.run('/usr/sbin/sysctl', ['-n', 'kern.bootsessionuuid']).catch(() => undefined);
    const machineId = ioreg?.exitCode === 0 ? parseIoregUuid(ioreg.stdout) : undefined;
    const bootId = boot?.exitCode === 0 ? boot.stdout.trim() || undefined : undefined;
    return {
      ...(machineId === undefined ? {} : { machineId }),
      ...(bootId === undefined ? {} : { bootId }),
      startedAtMs: Math.round(performance.timeOrigin)
    };
  }

  async inspect(pid: number): Promise<ProcessState> {
    const result = await this.run('/bin/ps', ['-o', 'pid=,stat=,etime=', '-p', String(pid)]);
    const line = result.stdout.trim();
    if (result.exitCode === 1 && line === '') return { state: 'absent' };
    const parsed = result.exitCode === 0 ? parsePsLine(line, pid) : undefined;
    if (parsed === undefined) throw new CliError(`Could not read the state of process ${pid} from \`ps\`.`);
    return {
      state: stateOf(parsed.stat.charAt(0)),
      startedAtMs: this.now() - parsed.elapsedSeconds * 1_000
    };
  }

  async scan(): Promise<ProcessScan> {
    const result = await this.run('/bin/ps', ['-A', '-ww', '-E', '-o', 'pid=,command=']);
    if (result.exitCode !== 0) throw new CliError('Could not list processes with `ps`.');
    const exclude = new Set([process.pid, ...(result.pid === undefined ? [] : [result.pid])]);
    return { processes: parsePsScan(result.stdout, exclude), unreadable: 0 };
  }
}

/* ----------------------------------------------------------- the choice ---- */

class UnsupportedProcessTable implements ProcessTable {
  self(): Promise<ProcessIdentity> {
    return Promise.resolve({});
  }

  inspect(): Promise<ProcessState> {
    return Promise.reject(new CliError(`Processes cannot be checked on ${process.platform}.`));
  }

  scan(): Promise<ProcessScan> {
    return Promise.reject(new CliError(`Processes cannot be listed on ${process.platform}.`));
  }
}

export function processTableFor(platform: NodeJS.Platform = process.platform): ProcessTable {
  if (platform === 'linux') return new LinuxProcessTable();
  if (platform === 'darwin') return new DarwinProcessTable();
  return new UnsupportedProcessTable();
}

let ownIdentity: Promise<ProcessIdentity> | undefined;

/** This process's identity, read once: every lock it takes records the same one. */
export function currentIdentity(): Promise<ProcessIdentity> {
  ownIdentity ??= processTableFor().self().catch(() => ({}));
  return ownIdentity;
}
