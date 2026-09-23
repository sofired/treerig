import { chmod, mkdir, mkdtemp, realpath, rm, symlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import {
  DarwinProcessTable,
  LinuxProcessTable,
  parseEtime,
  parseIoregUuid,
  parseProcStat,
  parsePsLine,
  parsePsScan,
  stateOf,
  type CommandResult
} from '../../src/process-table.ts';

const temporary: string[] = [];
afterEach(async () => {
  await Promise.all(temporary.splice(0).map((d) => rm(d, { recursive: true, force: true })));
});

/** `/proc/<pid>/stat` with the given command name, state and start ticks. */
const statLine = (comm: string, state: string, startTicks: number): string =>
  `42 (${comm}) ${state} 1 42 42 0 -1 4194304 0 0 0 0 0 0 0 0 20 0 1 0 ${startTicks} 5971968 416 18446744073709551615\n`;

describe('parsing', () => {
  it('reads state and start ticks after the last parenthesis, whatever the command name holds', () => {
    expect(parseProcStat(statLine('cat', 'R', 212235381))).toEqual({ state: 'R', startTicks: 212235381 });
    expect(parseProcStat(statLine('evil) Z 1 (x', 'S', 7))).toEqual({ state: 'S', startTicks: 7 });
    expect(parseProcStat('no parenthesis at all')).toBeUndefined();
    expect(parseProcStat('1 (short) R 1 2')).toBeUndefined();
  });

  it('names process states the way both /proc and ps spell them', () => {
    expect(['Z', 'X', 'x'].map(stateOf)).toEqual(['zombie', 'zombie', 'zombie']);
    expect(['T', 't'].map(stateOf)).toEqual(['stopped', 'stopped']);
    expect(['R', 'S', 'D', 'I'].map(stateOf)).toEqual(['running', 'running', 'running', 'running']);
  });

  it('reads ps elapsed times of every length, and nothing else', () => {
    expect(parseEtime('05')).toBeUndefined();
    expect(parseEtime('01:05')).toBe(65);
    expect(parseEtime('02:01:05')).toBe(7_265);
    expect(parseEtime('3-02:01:05')).toBe(266_465);
    expect(parseEtime(' 00:07 ')).toBe(7);
    expect(parseEtime('soon')).toBeUndefined();
  });

  it('trusts a ps line only for the PID it asked about', () => {
    expect(parsePsLine('  4242 Ss   01:02:03', 4242)).toEqual({ stat: 'Ss', elapsedSeconds: 3_723 });
    // BusyBox ignores -p and prints the whole table: the first line is PID 1.
    expect(parsePsLine('    1 S    99-00:00:00', 4242)).toBeUndefined();
    expect(parsePsLine('4242 ?? garbage', 4242)).toBeUndefined();
  });

  it('reads the platform UUID out of ioreg', () => {
    expect(parseIoregUuid('  "IOPlatformUUID" = "564D1D23-ABCD-4E5F-8A9B-0123456789AB"\n')).toBe('564D1D23-ABCD-4E5F-8A9B-0123456789AB');
    expect(parseIoregUuid('nothing here')).toBeUndefined();
  });

  it('offers each whole token of a ps scan as both an argument and an environment entry, skipping the excluded', () => {
    const scanned = parsePsScan('  10 docker compose --project-name berth-abc up BERTH_WORKTREE_ID=w\n  11 ps -A\n junk\n', new Set([11]));
    expect(scanned).toEqual([{
      pid: 10,
      args: ['docker', 'compose', '--project-name', 'berth-abc', 'up', 'BERTH_WORKTREE_ID=w'],
      env: ['docker', 'compose', '--project-name', 'berth-abc', 'up', 'BERTH_WORKTREE_ID=w']
    }]);
  });
});

describe('LinuxProcessTable', () => {
  async function fakeProc(): Promise<string> {
    const root = await realpath(await mkdtemp(path.join(tmpdir(), 'berth-proc-')));
    temporary.push(root);
    await mkdir(path.join(root, 'sys', 'kernel', 'random'), { recursive: true });
    await writeFile(path.join(root, 'sys', 'kernel', 'random', 'boot_id'), 'boot-1\n');
    await mkdir(path.join(root, 'self', 'ns'), { recursive: true });
    await symlink('pid:[1]', path.join(root, 'self', 'ns', 'pid'));
    await writeFile(path.join(root, 'self', 'stat'), statLine('node', 'R', 500));
    return root;
  }

  async function addProcess(root: string, pid: number, fields: { stat?: string; cmdline?: string[]; environ?: string[] | 'unreadable'; ns?: string }): Promise<void> {
    const directory = path.join(root, String(pid));
    await mkdir(path.join(directory, 'ns'), { recursive: true });
    if (fields.stat !== undefined) await writeFile(path.join(directory, 'stat'), fields.stat);
    await writeFile(path.join(directory, 'cmdline'), `${(fields.cmdline ?? []).join('\0')}\0`);
    if (fields.environ !== undefined) {
      const file = path.join(directory, 'environ');
      await writeFile(file, fields.environ === 'unreadable' ? '' : `${fields.environ.join('\0')}\0`);
      if (fields.environ === 'unreadable') await chmod(file, 0o000);
    }
    if (fields.ns !== undefined) await symlink(fields.ns, path.join(directory, 'ns', 'pid'));
  }

  it('describes itself by machine, boot, namespace and start ticks', async () => {
    const root = await fakeProc();
    const machine = path.join(root, 'machine-id');
    await writeFile(machine, 'abc123\n');
    await expect(new LinuxProcessTable(root, [path.join(root, 'missing'), machine]).self()).resolves.toEqual({
      machineId: 'abc123', bootId: 'boot-1', pidNs: 'pid:[1]', startTicks: 500
    });
    await expect(new LinuxProcessTable(root, [path.join(root, 'missing')]).self()).resolves.not.toHaveProperty('machineId');
  });

  it('tells an absent, exited, stopped and running process apart, with its start', async () => {
    const root = await fakeProc();
    await addProcess(root, 7, { stat: statLine('z', 'Z', 11) });
    await addProcess(root, 8, { stat: statLine('t', 'T', 12) });
    await addProcess(root, 9, { stat: statLine('r', 'S', 13) });
    await addProcess(root, 10, { stat: 'garbled' });
    const table = new LinuxProcessTable(root, []);
    await expect(table.inspect(6)).resolves.toEqual({ state: 'absent' });
    await expect(table.inspect(7)).resolves.toEqual({ state: 'zombie', startTicks: 11 });
    await expect(table.inspect(8)).resolves.toEqual({ state: 'stopped', startTicks: 12 });
    await expect(table.inspect(9)).resolves.toEqual({ state: 'running', startTicks: 13 });
    await expect(table.inspect(10)).rejects.toThrow('Could not read the state of process 10');
  });

  it('scans this namespace only, counts what it could not read, and leaves itself out', async () => {
    const root = await fakeProc();
    await addProcess(root, 20, { cmdline: ['docker', 'compose'], environ: ['A=1'], ns: 'pid:[1]' });
    await addProcess(root, 21, { cmdline: ['service'], environ: ['A=1'], ns: 'pid:[2]' });
    await addProcess(root, 22, { cmdline: ['someone-else'], environ: 'unreadable', ns: 'pid:[1]' });
    await addProcess(root, 23, { cmdline: ['no-namespace-link'], environ: ['B=2'] });
    await addProcess(root, process.pid, { cmdline: ['me'], environ: ['C=3'], ns: 'pid:[1]' });
    await mkdir(path.join(root, 'not-a-pid'));
    const { processes, unreadable } = await new LinuxProcessTable(root, []).scan();
    expect(processes.sort((a, b) => a.pid - b.pid)).toEqual([
      { pid: 20, args: ['docker', 'compose'], env: ['A=1'] },
      { pid: 22, args: ['someone-else'] },
      { pid: 23, args: ['no-namespace-link'], env: ['B=2'] }
    ]);
    expect(unreadable).toBe(1);
    // Another user's private environment is no gap in the proof: a worktree's processes run as its user.
    // (No init in this fake /proc, so a non-root reader would also call others hidden; see below.)
    const asSomeoneElse = await new LinuxProcessTable(root, [], (process.getuid?.() ?? 0) + 1).scan();
    expect(asSomeoneElse.unreadable).toBe(0);
    expect(asSomeoneElse.processes.map((entry) => entry.pid)).toContain(22);
  });
});

describe('LinuxProcessTable under hidepid', () => {
  it('notices when other users\' processes are hidden, and counts its own it cannot read rather than failing', async () => {
    const root = await realpath(await mkdtemp(path.join(tmpdir(), 'berth-hidepid-')));
    temporary.push(root);
    await mkdir(path.join(root, 'self', 'ns'), { recursive: true });
    await symlink('pid:[1]', path.join(root, 'self', 'ns', 'pid'));
    await mkdir(path.join(root, '30', 'ns'), { recursive: true });
    await symlink('pid:[1]', path.join(root, '30', 'ns', 'pid'));
    await writeFile(path.join(root, '30', 'cmdline'), 'mine\0');
    await chmod(path.join(root, '30', 'cmdline'), 0o000);
    const uid = process.getuid?.() ?? 1_000;

    // hidepid=2: not even init can be seen by a non-root reader.
    await expect(new LinuxProcessTable(root, [], uid).scan()).resolves.toEqual({ processes: [], unreadable: 1, othersHidden: true });
    await mkdir(path.join(root, '1'));
    await expect(new LinuxProcessTable(root, [], uid).scan()).resolves.toEqual({ processes: [], unreadable: 1, othersHidden: false });
    // hidepid=1: someone else's command line is there but cannot be read.
    await expect(new LinuxProcessTable(root, [], uid + 1).scan()).resolves.toEqual({ processes: [], unreadable: 0, othersHidden: true });
  });

  it('does not take a process whose owner it cannot read for its own', async () => {
    const root = await realpath(await mkdtemp(path.join(tmpdir(), 'berth-owner-')));
    temporary.push(root);
    await mkdir(path.join(root, 'self', 'ns'), { recursive: true });
    await symlink('pid:[1]', path.join(root, 'self', 'ns', 'pid'));
    await mkdir(path.join(root, '1'));
    // Neither its command line nor its owner can be read, and it has not gone: ELOOP, not ENOENT.
    await symlink(path.join(root, '31'), path.join(root, '31'));
    await expect(new LinuxProcessTable(root, [], process.getuid?.() ?? 1_000).scan())
      .resolves.toEqual({ processes: [], unreadable: 0, othersHidden: true });
  });

  it('judges another user\'s process by its command line alone: a private environment is not a blind spot', async () => {
    const root = await realpath(await mkdtemp(path.join(tmpdir(), 'berth-private-')));
    temporary.push(root);
    await mkdir(path.join(root, 'self', 'ns'), { recursive: true });
    await symlink('pid:[1]', path.join(root, 'self', 'ns', 'pid'));
    await mkdir(path.join(root, '1'));
    await mkdir(path.join(root, '40', 'ns'), { recursive: true });
    await symlink('pid:[1]', path.join(root, '40', 'ns', 'pid'));
    await writeFile(path.join(root, '40', 'cmdline'), 'helper\0');
    await writeFile(path.join(root, '40', 'environ'), '');
    await chmod(path.join(root, '40', 'environ'), 0o000);
    await expect(new LinuxProcessTable(root, [], (process.getuid?.() ?? 1_000) + 1).scan())
      .resolves.toEqual({ processes: [{ pid: 40, args: ['helper'] }], unreadable: 0, othersHidden: false });
  });

  it('never calls root\'s view hidden: hidepid does not apply to it', async () => {
    const root = await realpath(await mkdtemp(path.join(tmpdir(), 'berth-rootview-')));
    temporary.push(root);
    await mkdir(path.join(root, 'self', 'ns'), { recursive: true });
    await symlink('pid:[1]', path.join(root, 'self', 'ns', 'pid'));
    await expect(new LinuxProcessTable(root, [], 0).scan()).resolves.toMatchObject({ othersHidden: false });
    await expect(new LinuxProcessTable(root, [], 1_000).scan()).resolves.toMatchObject({ othersHidden: true });
  });
});

describe('DarwinProcessTable', () => {
  function scripted(answers: Record<string, CommandResult | Error>): (command: string, args: readonly string[]) => Promise<CommandResult> {
    return (command, args) => {
      const answer = answers[[command, ...args].join(' ')];
      if (answer === undefined) return Promise.reject(new Error(`unexpected: ${command} ${args.join(' ')}`));
      return answer instanceof Error ? Promise.reject(answer) : Promise.resolve(answer);
    };
  }
  const PS = (pid: number) => `/bin/ps -o pid=,stat=,etime= -p ${pid}`;

  it('describes itself by platform UUID and boot session, with its own start', async () => {
    const table = new DarwinProcessTable(scripted({
      '/usr/sbin/ioreg -rd1 -c IOPlatformExpertDevice': { stdout: '"IOPlatformUUID" = "MAC-1"', exitCode: 0 },
      '/usr/sbin/sysctl -n kern.bootsessionuuid': { stdout: 'BOOT-1\n', exitCode: 0 }
    }));
    const self = await table.self();
    expect(self).toMatchObject({ machineId: 'MAC-1', bootId: 'BOOT-1' });
    expect(self.startedAtMs).toBeGreaterThan(0);
    const blind = new DarwinProcessTable(scripted({}));
    await expect(blind.self()).resolves.not.toHaveProperty('machineId');
  });

  it('reads a missing process from ps\'s exit code, and dates a present one from its elapsed time', async () => {
    const table = new DarwinProcessTable(scripted({
      [PS(5)]: { stdout: '', exitCode: 1 },
      [PS(6)]: { stdout: '    6 Z      00:03\n', exitCode: 0 },
      [PS(7)]: { stdout: '    7 T+     01:00\n', exitCode: 0 },
      [PS(8)]: { stdout: '    1 Ss     00:01\n', exitCode: 0 },
      [PS(9)]: { stdout: 'ps: bad option\n', exitCode: 2 },
      [PS(10)]: { stdout: '', exitCode: 2 }
    }), () => 100_000);
    await expect(table.inspect(5)).resolves.toEqual({ state: 'absent' });
    await expect(table.inspect(6)).resolves.toEqual({ state: 'zombie', startedAtMs: 97_000 });
    await expect(table.inspect(7)).resolves.toEqual({ state: 'stopped', startedAtMs: 40_000 });
    await expect(table.inspect(8)).rejects.toThrow('Could not read the state of process 8');
    await expect(table.inspect(9)).rejects.toThrow('Could not read the state of process 9');
    // Only exit 1 with nothing printed means "no such process"; a failed ps is no evidence.
    await expect(table.inspect(10)).rejects.toThrow('Could not read the state of process 10');
  });

  it('scans every process with its environment, leaving out itself and the ps it ran', async () => {
    const table = new DarwinProcessTable(scripted({
      '/bin/ps -A -ww -E -o pid=,command=': { stdout: `  30 sleep 1 X=1\n  31 ps -A -ww -E\n  ${process.pid} node me\n`, exitCode: 0, pid: 31 }
    }));
    await expect(table.scan()).resolves.toEqual({ processes: [{ pid: 30, args: ['sleep', '1', 'X=1'], env: ['sleep', '1', 'X=1'] }], unreadable: 0 });
    const failing = new DarwinProcessTable(scripted({ '/bin/ps -A -ww -E -o pid=,command=': { stdout: '', exitCode: 1 } }));
    await expect(failing.scan()).rejects.toThrow('Could not list processes');
  });
});
