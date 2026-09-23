import { lstat, mkdir, mkdtemp, realpath, rm, utimes, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { judgeHolder, markedProcesses, sameLock, unlockGuard, type HolderJudge, type UnlockProofOptions } from '../../src/lock-recovery.ts';
import { clearStaleWorktreeLocks, worktreeLocks, type HolderRecord, type LockDescription } from '../../src/operation-lock.ts';
import type { ProcessIdentity, ProcessScan, ProcessState, ProcessTable, ScannedProcess } from '../../src/process-table.ts';

const REPOSITORY = 'a08b4892-5111-49f1-a47a-5ab988c7f676';
const WORKTREE = '2b41d9d4-ed88-49c1-be9c-5439bdf6a431';
const PROJECT = 'berth-0123456789abcdef01234567';
const SELF: ProcessIdentity = { machineId: 'machine-1', bootId: 'boot-1', pidNs: 'pid:[1]' };

const temporary: string[] = [];
afterEach(async () => {
  await Promise.all(temporary.splice(0).map((d) => rm(d, { recursive: true, force: true })));
});

class FakeTable implements ProcessTable {
  identity: ProcessIdentity = SELF;
  states = new Map<number, ProcessState>();
  scans: ProcessScan[] = [];
  inspected: number[] = [];
  scanned = 0;

  self(): Promise<ProcessIdentity> {
    return Promise.resolve(this.identity);
  }

  inspect(pid: number): Promise<ProcessState> {
    this.inspected.push(pid);
    return Promise.resolve(this.states.get(pid) ?? { state: 'absent' });
  }

  scan(): Promise<ProcessScan> {
    const next = this.scans[Math.min(this.scanned, this.scans.length - 1)] ?? { processes: [], unreadable: 0 };
    this.scanned += 1;
    return Promise.resolve(next);
  }
}

const record = (overrides: Partial<HolderRecord> = {}): HolderRecord => ({
  pid: 4242, host: 'laptop', command: 'berth up', startedAt: '2026-09-23T00:00:00.000Z',
  machineId: 'machine-1', bootId: 'boot-1', pidNs: 'pid:[1]', startTicks: 900,
  ...overrides
});

const described = (overrides: Partial<LockDescription> = {}): LockDescription => ({
  state: 'stale', lockDirectory: '/x.lock', heartbeatMs: 1, ino: '77', birthtimeMs: 5, record: record(), ...overrides
});

function judge(table = new FakeTable(), self: ProcessIdentity = SELF): HolderJudge {
  return { self, hostname: 'laptop', processes: table };
}

describe('judgeHolder', () => {
  it('finds a holder gone when its PID no longer exists or has exited', async () => {
    await expect(judgeHolder(described(), judge())).resolves.toEqual({ kind: 'gone', reason: 'PID 4242 on "laptop" no longer exists' });
    const table = new FakeTable();
    table.states.set(4242, { state: 'zombie', startTicks: 900 });
    await expect(judgeHolder(described(), judge(table))).resolves.toMatchObject({ kind: 'gone', reason: 'PID 4242 on "laptop" has exited' });
  });

  it('refuses a holder still running, and names a stopped one as stopped', async () => {
    const table = new FakeTable();
    table.states.set(4242, { state: 'running', startTicks: 900 });
    await expect(judgeHolder(described(), judge(table))).resolves.toEqual({ kind: 'alive', reason: 'PID 4242 on "laptop" is still running' });
    table.states.set(4242, { state: 'stopped', startTicks: 900 });
    await expect(judgeHolder(described(), judge(table))).resolves.toMatchObject({ kind: 'alive', reason: expect.stringContaining('`kill -CONT 4242`') as string });
  });

  it('sees through a reused PID by its start ticks, and by its start time on macOS', async () => {
    const table = new FakeTable();
    table.states.set(4242, { state: 'running', startTicks: 901 });
    await expect(judgeHolder(described(), judge(table))).resolves.toMatchObject({ kind: 'gone', reason: expect.stringContaining('later process') as string });

    const mac = described({ record: record({ startTicks: undefined, pidNs: undefined, processStartedAtMs: 10_000 }) });
    const macSelf = { machineId: 'machine-1', bootId: 'boot-1' };
    table.states.set(4242, { state: 'running', startedAtMs: 11_900 });
    await expect(judgeHolder(mac, judge(table, macSelf))).resolves.toMatchObject({ kind: 'alive' });
    table.states.set(4242, { state: 'running', startedAtMs: 12_000 });
    await expect(judgeHolder(mac, judge(table, macSelf))).resolves.toMatchObject({ kind: 'alive' });
    table.states.set(4242, { state: 'running', startedAtMs: 12_100 });
    await expect(judgeHolder(mac, judge(table, macSelf))).resolves.toMatchObject({ kind: 'gone' });
    table.states.set(4242, { state: 'running', startedAtMs: 7_900 });
    await expect(judgeHolder(mac, judge(table, macSelf))).resolves.toMatchObject({ kind: 'gone' });
  });

  it('cannot call a running PID reused without a start to compare, so calls it alive', async () => {
    const table = new FakeTable();
    table.states.set(4242, { state: 'running' });
    await expect(judgeHolder(described(), judge(table))).resolves.toMatchObject({ kind: 'alive' });
  });

  it('finds every holder of an earlier boot gone without asking about its PID', async () => {
    const table = new FakeTable();
    table.states.set(4242, { state: 'running', startTicks: 900 });
    await expect(judgeHolder(described({ record: record({ bootId: 'boot-0' }) }), judge(table)))
      .resolves.toMatchObject({ kind: 'gone', reason: expect.stringContaining('restarted') as string });
    expect(table.inspected).toEqual([]);
  });

  it('cannot check a holder on another machine, in another namespace, or with no record — and says why', async () => {
    const cases: [LockDescription, ProcessIdentity, string][] = [
      [described({ record: record({ machineId: 'machine-2' }) }), SELF, 'another machine'],
      [described({ record: record({ pidNs: 'pid:[9]' }) }), SELF, 'another PID namespace'],
      [described({ record: record({ pidNs: undefined }) }), SELF, 'another PID namespace'],
      [described(), { bootId: 'boot-1', pidNs: 'pid:[1]' }, "this machine's identity cannot be read"],
      [described({ record: undefined }), SELF, 'left no record'],
      [described({ ino: '78', record: record({ lock: { ino: '77', birthtimeMs: 5 } }) }), SELF, 'belongs to an earlier lock'],
      [described({ birthtimeMs: 6, record: record({ lock: { ino: '77', birthtimeMs: 5 } }) }), SELF, 'belongs to an earlier lock']
    ];
    for (const [lock, self, reason] of cases) {
      await expect(judgeHolder(lock, judge(new FakeTable(), self))).resolves.toEqual({ kind: 'unverifiable', reason: expect.stringContaining(reason) as string });
    }
    // A birth time the filesystem does not keep is no evidence either way.
    await expect(judgeHolder(described({ birthtimeMs: 0, record: record({ lock: { ino: '77', birthtimeMs: 5 } }) }), judge()))
      .resolves.toMatchObject({ kind: 'gone' });
    await expect(judgeHolder(described({ record: record({ lock: { ino: '77', birthtimeMs: 5 } }) }), judge()))
      .resolves.toMatchObject({ kind: 'gone' });
  });

  it('goes by hostname alone for a record an earlier version wrote, and never proves that holder gone', async () => {
    const old = (overrides: Partial<HolderRecord> = {}) => described({
      record: { pid: 4242, host: 'laptop', command: '', startedAt: 'x', ...overrides }, ino: undefined
    });
    await expect(judgeHolder(old({ host: 'desktop' }), judge())).resolves.toMatchObject({ kind: 'unverifiable', reason: expect.stringContaining('another host') as string });
    await expect(judgeHolder(old(), judge())).resolves.toMatchObject({ kind: 'unverifiable', reason: expect.stringContaining('PID namespace') as string });
    const table = new FakeTable();
    table.states.set(4242, { state: 'zombie' });
    await expect(judgeHolder(old(), judge(table))).resolves.toMatchObject({ kind: 'unverifiable' });
    table.states.set(4242, { state: 'running' });
    await expect(judgeHolder(old(), judge(table))).resolves.toMatchObject({ kind: 'alive' });
  });
});

describe('sameLock', () => {
  it('holds only while the lock is still stale and nothing about it has moved', () => {
    const before = described({ recordText: 'r' });
    expect(sameLock(before, { ...before })).toBe(true);
    expect(sameLock(before, { ...before, state: 'active' })).toBe(false);
    expect(sameLock(before, { ...before, ino: '78' })).toBe(false);
    expect(sameLock(before, { ...before, heartbeatMs: 2 })).toBe(false);
    expect(sameLock(before, { ...before, recordText: 's' })).toBe(false);
  });
});

describe('markedProcesses', () => {
  const markers = { project: PROJECT, worktreeId: WORKTREE };
  it('finds this worktree\'s processes by whole project token or exact environment marker only', () => {
    const scanned: ScannedProcess[] = [
      { pid: 1, args: ['docker', 'compose', '--project-name', PROJECT, 'up'] },
      { pid: 2, args: ['sh', 'seed.sh'], env: [`BERTH_WORKTREE_ID=${WORKTREE}`] },
      { pid: 3, args: ['docker', 'exec', 'x'], env: [`COMPOSE_PROJECT_NAME=${PROJECT}`] },
      { pid: 4, args: ['docker', 'compose', '--project-name', `${PROJECT}0`] },
      { pid: 5, args: [`--project-name=${PROJECT}`] },
      { pid: 6, args: ['sh'], env: [`BERTH_WORKTREE_ID=${WORKTREE}x`, 'OTHER=1'] },
      { pid: 7, args: ['sh'] }
    ];
    expect(markedProcesses(scanned, markers).map((entry) => entry.pid)).toEqual([1, 2, 3]);
  });
});

describe('unlockGuard', () => {
  async function staleWorktree(overrides: Partial<HolderRecord> = {}, kinds: ('development' | 'operation')[] = ['operation']) {
    const controlHome = await realpath(await mkdtemp(path.join(tmpdir(), 'berth-unlock-')));
    temporary.push(controlHome);
    const locks = worktreeLocks(controlHome, REPOSITORY, WORKTREE, 'main').filter((lock) => kinds.includes(lock.kind));
    for (const lock of locks) {
      await mkdir(path.dirname(lock.target), { recursive: true });
      await mkdir(`${lock.target}.lock`);
      const directory = await lstat(`${lock.target}.lock`, { bigint: true });
      await writeFile(lock.target, `${JSON.stringify(record({
        lock: { ino: directory.ino.toString(), birthtimeMs: Number(directory.birthtimeMs) }, ...overrides
      }))}\n`);
      const old = new Date(Date.now() - 60_000);
      await utimes(`${lock.target}.lock`, old, old);
    }
    return { controlHome, locks };
  }

  function guardOptions(table: FakeTable, overrides: Partial<UnlockProofOptions> = {}): UnlockProofOptions {
    return {
      processes: table,
      hostname: 'laptop',
      markers: { project: PROJECT, worktreeId: WORKTREE },
      allowUnverifiable: false,
      environment: {},
      sleep: () => Promise.resolve(),
      ...overrides
    };
  }

  const clear = (controlHome: string, options: UnlockProofOptions) =>
    clearStaleWorktreeLocks(controlHome, REPOSITORY, WORKTREE, 'main', unlockGuard(options));

  it('clears both locks of a holder proven gone, and says what it proved', async () => {
    const { controlHome } = await staleWorktree({}, ['development', 'operation']);
    const table = new FakeTable();
    const guard = unlockGuard(guardOptions(table));
    const cleared = await clearStaleWorktreeLocks(controlHome, REPOSITORY, WORKTREE, 'main', guard);
    expect(cleared.map((lock) => lock.kind)).toEqual(['development', 'operation']);
    expect(guard.proof.verified).toEqual([
      expect.stringMatching(/^main development session: PID 4242 on "laptop" no longer exists; its heartbeat has been silent for 1m \ds$/),
      expect.stringMatching(/^worktree main: PID 4242 on "laptop" no longer exists/),
      'no command started for this worktree is still running on this machine'
    ]);
    expect(guard.proof.unverified).toEqual([]);
  });

  it('clears nothing while the holder runs, and keeps the lock for another look', async () => {
    const { controlHome, locks } = await staleWorktree();
    const table = new FakeTable();
    table.states.set(4242, { state: 'running', startTicks: 900 });
    await expect(clear(controlHome, guardOptions(table))).rejects.toThrow('Not clearing the worktree main lock: PID 4242 on "laptop" is still running.');
    await expect(lstat(`${locks[0]?.target}.lock`)).resolves.toBeDefined();
  });

  it('refuses a holder it cannot check unless told to take the person\'s word, and then says so', async () => {
    const { controlHome } = await staleWorktree({ machineId: 'machine-2' });
    await expect(clear(controlHome, guardOptions(new FakeTable()))).rejects.toThrow('--holder-unverifiable');
    const guard = unlockGuard(guardOptions(new FakeTable(), { allowUnverifiable: true }));
    await expect(clearStaleWorktreeLocks(controlHome, REPOSITORY, WORKTREE, 'main', guard)).resolves.toHaveLength(1);
    expect(guard.proof.unverified).toEqual([expect.stringContaining('ran on another machine')]);
  });

  it('never takes anyone\'s word over a running holder', async () => {
    const { controlHome } = await staleWorktree();
    const table = new FakeTable();
    table.states.set(4242, { state: 'running', startTicks: 900 });
    await expect(clear(controlHome, guardOptions(table, { allowUnverifiable: true }))).rejects.toThrow('is still running');
  });

  it('refuses when the lock moves during the second look', async () => {
    const { controlHome, locks } = await staleWorktree();
    const lock = locks[0];
    if (!lock) throw new Error('no lock');
    const touch = async (): Promise<void> => {
      const later = new Date(Date.now() - 45_000);
      await utimes(`${lock.target}.lock`, later, later);
    };
    await expect(clear(controlHome, guardOptions(new FakeTable(), { sleep: touch }))).rejects.toThrow('it changed while it was being checked');
  });

  it('looks again when the clocks disagree over the second look, and gives up if they keep disagreeing', async () => {
    const { controlHome } = await staleWorktree();
    let wall = 0;
    const jumpy = (jumps: number) => {
      let left = jumps;
      return {
        sleep: () => {
          wall += left-- > 0 ? 60_000 : 0;
          return Promise.resolve();
        },
        wallClock: () => wall,
        monotonic: () => 0
      };
    };
    await expect(clear(controlHome, guardOptions(new FakeTable(), jumpy(3)))).rejects.toThrow('The clock kept jumping');
    await expect(clear(controlHome, guardOptions(new FakeTable(), jumpy(2)))).resolves.toHaveLength(1);
  });

  it('refuses while anything started for the worktree is still running, listing it', async () => {
    const { controlHome } = await staleWorktree();
    const table = new FakeTable();
    const orphan = { pid: 77, args: ['docker', 'compose', '--project-name', PROJECT, 'up'] };
    table.scans = [{ processes: [orphan], unreadable: 0 }, { processes: [orphan], unreadable: 2 }];
    await expect(clear(controlHome, guardOptions(table))).rejects.toThrow(
      `Not clearing any lock: a process started for this worktree is still running:\n  PID 77: docker compose --project-name ${PROJECT} up\n`
    );
  });

  it('counts a process that started between the two scans: it is running', async () => {
    const { controlHome } = await staleWorktree();
    const table = new FakeTable();
    table.scans = [
      { processes: [{ pid: 77, args: [PROJECT] }], unreadable: 0 },
      { processes: [{ pid: 78, args: ['docker', 'compose', '--project-name', PROJECT, 'down'] }], unreadable: 0 }
    ];
    await expect(clear(controlHome, guardOptions(table))).rejects.toThrow('PID 78: docker compose');
  });

  it('refuses a lock that came back to life before the proof began', async () => {
    const { controlHome, locks } = await staleWorktree();
    const lock = locks[0];
    if (!lock) throw new Error('no lock');
    const guard = unlockGuard(guardOptions(new FakeTable()));
    await expect(clearStaleWorktreeLocks(controlHome, REPOSITORY, WORKTREE, 'main', {
      verify: async (stale) => {
        const now = new Date();
        await utimes(`${lock.target}.lock`, now, now);
        await guard.verify(stale);
      },
      beforeClear: guard.beforeClear
    })).rejects.toThrow('Not clearing the worktree main lock: it is active again.');
  });

  it('lets a process that was only passing through go, and says how many it could not read', async () => {
    const { controlHome } = await staleWorktree();
    const table = new FakeTable();
    table.scans = [{ processes: [{ pid: 77, args: [PROJECT] }], unreadable: 0 }, { processes: [], unreadable: 2 }];
    const guard = unlockGuard(guardOptions(table));
    await expect(clearStaleWorktreeLocks(controlHome, REPOSITORY, WORKTREE, 'main', guard)).resolves.toHaveLength(1);
    expect(guard.proof.verified.at(-1)).toBe('no command started for this worktree is still running on this machine (2 of your processes could not be read)');
  });

  it('refuses when other users\' processes are hidden, unless told to take the person\'s word, and then says so', async () => {
    const { controlHome } = await staleWorktree();
    const hidden = () => {
      const table = new FakeTable();
      table.scans = [{ processes: [], unreadable: 0, othersHidden: true }];
      return table;
    };
    await expect(clear(controlHome, guardOptions(hidden()))).rejects.toThrow(
      "Not clearing any lock: other users' processes are hidden from you (/proc mounted with hidepid, say)"
    );
    const guard = unlockGuard(guardOptions(hidden(), { allowUnverifiable: true }));
    await expect(clearStaleWorktreeLocks(controlHome, REPOSITORY, WORKTREE, 'main', guard)).resolves.toHaveLength(1);
    expect(guard.proof.unverified).toEqual([expect.stringContaining('could not be looked for')]);
    // And it claims only what it saw.
    expect(guard.proof.verified.at(-1)).toBe('none of your own processes started for this worktree is still running');
  });

  it('judges hidden processes by the rescan when there is one', async () => {
    const { controlHome } = await staleWorktree();
    const table = new FakeTable();
    table.scans = [{ processes: [{ pid: 77, args: [PROJECT] }], unreadable: 0, othersHidden: false }, { processes: [], unreadable: 0, othersHidden: true }];
    await expect(clear(controlHome, guardOptions(table))).rejects.toThrow('hidden from you');
  });

  it('will not run from inside the worktree\'s own environment', async () => {
    const { controlHome } = await staleWorktree();
    await expect(clear(controlHome, guardOptions(new FakeTable(), { environment: { BERTH_WORKTREE_ID: WORKTREE } })))
      .rejects.toThrow('inside the worktree\'s own environment');
  });

  it('refuses a lock that changed between being checked and being cleared', async () => {
    const { controlHome, locks } = await staleWorktree();
    const lock = locks[0];
    if (!lock) throw new Error('no lock');
    const guard = unlockGuard(guardOptions(new FakeTable()));
    await expect(clearStaleWorktreeLocks(controlHome, REPOSITORY, WORKTREE, 'main', {
      verify: async (stale) => {
        await guard.verify(stale);
        await writeFile(lock.target, 'rewritten');
      },
      beforeClear: guard.beforeClear
    })).rejects.toThrow('it changed after it was checked');
  });
});
