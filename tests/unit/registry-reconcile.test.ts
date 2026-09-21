import { randomUUID } from 'node:crypto';
import { mkdtemp, mkdir, readFile, realpath, rm, symlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import type { Logger } from '../../src/logger.ts';
import type { CommandRunner, RunResult } from '../../src/process-runner.ts';
import { registryDirectory, WorktreeRegistry, type WorktreeIdentity } from '../../src/worktree-registry.ts';

const temporary: string[] = [];
afterEach(async () => { await Promise.all(temporary.splice(0).map((d) => rm(d, { recursive: true, force: true }))); });

class GitRunner implements CommandRunner {
  readonly identities = new Map<string, Omit<WorktreeIdentity, 'repositoryId' | 'worktreeId'>>();
  active: string[] = [];
  add(root: string, branch: string, common: string): void {
    this.identities.set(root, { root, branch, gitCommonDir: common, gitDir: path.join(common, 'worktrees', path.basename(root)) });
    this.active.push(root);
  }
  run(command: string, args: readonly string[] = []): Promise<RunResult> {
    if (args.includes('worktree') && args.includes('list')) {
      return Promise.resolve({ stdout: this.active.map((root) => `worktree ${root}\0HEAD x\0branch refs/heads/${this.identities.get(root)?.branch ?? ''}\0\0`).join(''), stderr: '', exitCode: 0 });
    }
    const item = this.identities.get(args[args.indexOf('-C') + 1] ?? '');
    if (!item) return Promise.reject(new Error('unknown worktree'));
    let stdout = `${item.branch}\n`;
    if (args.includes('--show-toplevel')) stdout = `${item.root}\n`;
    else if (args.includes('--git-common-dir')) stdout = `${item.gitCommonDir}\n`;
    else if (args.includes('--git-dir')) stdout = `${item.gitDir}\n`;
    return Promise.resolve({ stdout, stderr: '', exitCode: 0 });
  }
  which(): Promise<boolean> { return Promise.resolve(true); }
}

const OLD = 'acme.localhost';
const NEW = 'acme.dev';
const SUFFIXED = /^feature-login-[a-f0-9]{12}$/;

async function world() {
  const base = await realpath(await mkdtemp(path.join(tmpdir(), 'berth-reconcile-')));
  temporary.push(base);
  const controlHome = path.join(base, 'control');
  const runner = new GitRunner();
  const warnings: string[] = [];
  const logger: Logger = { info: () => {}, output: () => {}, warn: (m) => { warnings.push(m); }, error: () => {} };
  const common = path.join(base, 'common');
  const wt = async (name: string, branch: string, sharedCommon = common) => {
    const root = path.join(base, name);
    await mkdir(root, { recursive: true });
    await mkdir(path.join(sharedCommon, 'worktrees', name), { recursive: true });
    runner.add(root, branch, sharedCommon);
    return root;
  };
  const reg = (root: string, domain: string) => new WorktreeRegistry({ controlHome, root, domain, runner, logger, commandName: 'berth' });
  const plant = async (repositoryId: string, record: object, name = randomUUID()) => {
    const dir = registryDirectory(controlHome, repositoryId);
    await mkdir(dir, { recursive: true });
    await writeFile(path.join(dir, `${name}.json`), JSON.stringify(record));
  };
  return { base, controlHome, runner, warnings, wt, reg, plant };
}

describe('domain reconciliation in ensure()', () => {
  it('re-registers under the new domain, keeps the slug when it is free, preserves initialization, reports the move', async () => {
    const w = await world();
    const a = await w.wt('a', 'feature/login');
    const before = await w.reg(a, OLD).ensure();
    const after = await w.reg(a, NEW).ensure();
    expect(after).toMatchObject({ worktreeId: before.worktreeId, slug: 'feature-login', domain: NEW, previousHost: `feature-login.${OLD}` });
    const onDisk = JSON.parse(await readFile(path.join(registryDirectory(w.controlHome, before.repositoryId), `${before.worktreeId}.json`), 'utf8')) as { domain: string; initialization: string };
    expect(onDisk).toMatchObject({ domain: NEW, initialization: 'pending' });
    expect(w.warnings).toEqual([expect.stringContaining(`moved from feature-login.${OLD} to feature-login.${NEW}`)]);
    expect(await w.reg(a, NEW).ensure()).not.toHaveProperty('previousHost');
    await expect(w.reg(a, NEW).listAll()).resolves.toHaveLength(1);
  });

  it('takes a suffix when a newer worktree already owns the bare slug in the new domain', async () => {
    const w = await world();
    const a = await w.wt('a', 'feature/login');
    await w.reg(a, OLD).ensure();
    const b = await w.wt('b', 'feature/login');
    expect((await w.reg(b, NEW).ensure()).slug).toBe('feature-login');
    const reconciled = await w.reg(a, NEW).ensure();
    expect(reconciled.slug).toMatch(SUFFIXED);
    expect(reconciled.domain).toBe(NEW);
    const scan = await w.reg(a, NEW).scanAll();
    expect(scan.conflicts.size).toBe(0);
    expect(scan.records.map((r) => `${r.slug}.${r.domain}`).sort()).toEqual([`feature-login.${NEW}`, `${reconciled.slug}.${NEW}`].sort());
  });

  it('serializes a rename against a concurrent first registration; hostnames stay unique', async () => {
    const w = await world();
    const a = await w.wt('a', 'feature/login');
    await w.reg(a, OLD).ensure();
    const c = await w.wt('c', 'feature/login');
    const [ra, rc] = await Promise.all([w.reg(a, NEW).ensure(), w.reg(c, NEW).ensure()]);
    expect([ra.slug, rc.slug].sort()).toEqual(expect.arrayContaining(['feature-login']));
    expect(ra.slug).not.toBe(rc.slug);
    expect([ra.slug, rc.slug].filter((s) => SUFFIXED.test(s))).toHaveLength(1);
    expect((await w.reg(a, NEW).scanAll()).conflicts.size).toBe(0);
  });

  it('flips back deterministically when the config reverts', async () => {
    const w = await world();
    const a = await w.wt('a', 'feature/login');
    const first = await w.reg(a, OLD).ensure();
    await w.reg(a, NEW).ensure();
    const back = await w.reg(a, OLD).ensure();
    expect(back).toMatchObject({ slug: first.slug, domain: OLD, previousHost: `feature-login.${NEW}` });
  });

  it('current() warns on a mismatch and does not mutate', async () => {
    const w = await world();
    const a = await w.wt('a', 'feature/login');
    const before = await w.reg(a, OLD).ensure();
    const seen = await w.reg(a, NEW).current();
    expect(seen).toMatchObject({ slug: before.slug, domain: OLD });
    expect(w.warnings).toEqual([expect.stringContaining(`now says ${NEW}`)]);
    expect((await w.reg(a, OLD).current())?.domain).toBe(OLD);
    expect(w.warnings).toHaveLength(1);
  });
});

describe('scan failure modes', () => {
  it('an unreadable record does not brick reads or registered worktrees, but blocks new allocation', async () => {
    const w = await world();
    const a = await w.wt('a', 'main');
    const registered = await w.reg(a, OLD).ensure();
    const futureRepo = randomUUID();
    await w.plant(futureRepo, { version: 2, repositoryId: futureRepo, worktreeId: randomUUID(), slug: 'x', domain: 'y.localhost', initialization: 'complete', extra: true });
    await expect(w.reg(a, OLD).ensure()).resolves.toMatchObject({ slug: registered.slug });
    await expect(w.reg(a, OLD).current()).resolves.toMatchObject({ slug: registered.slug });
    await expect(w.reg(a, OLD).listAll()).resolves.toHaveLength(1);
    await expect(w.reg(a, OLD).findGlobal(registered.worktreeId)).resolves.toMatchObject({ slug: 'main' });
    const scan = await w.reg(a, OLD).scanAll();
    expect(scan.problems).toHaveLength(1);
    expect(scan.problems[0]?.file).toContain(futureRepo);
    const fresh = await w.wt('fresh', 'feature/new');
    await expect(w.reg(fresh, OLD).ensure()).rejects.toThrow(/Refusing to allocate a hostname while the registry has unreadable records/);
  });

  it('a symlinked record is reported as a problem, not silently skipped, and blocks new allocation', async () => {
    const w = await world();
    const a = await w.wt('a', 'main');
    await w.reg(a, OLD).ensure();
    const repo = randomUUID();
    const id = randomUUID();
    const dir = registryDirectory(w.controlHome, repo);
    await mkdir(dir, { recursive: true });
    const elsewhere = path.join(w.base, 'elsewhere.json');
    await writeFile(elsewhere, JSON.stringify({ version: 1, repositoryId: repo, worktreeId: id, slug: 'linked', domain: OLD, initialization: 'complete' }));
    await symlink(elsewhere, path.join(dir, `${id}.json`));

    const scan = await w.reg(a, OLD).scanAll();
    expect(scan.problems).toHaveLength(1);
    expect(scan.problems[0]?.reason).toMatch(/symbolic link/);
    expect(scan.records.map((r) => r.slug)).not.toContain('linked');
    const fresh = await w.wt('fresh', 'feature/new');
    await expect(w.reg(fresh, OLD).ensure()).rejects.toThrow(/unreadable records/);
  });

  it('a duplicate hostname is reported, still occupies its host, and only fails lookups that land on it', async () => {
    const w = await world();
    const a = await w.wt('a', 'main');
    const registry = w.reg(a, OLD);
    await registry.ensure();
    const repo = randomUUID();
    const x = randomUUID(); const y = randomUUID();
    await w.plant(repo, { version: 1, repositoryId: repo, worktreeId: x, slug: 'feature-login', domain: OLD, initialization: 'complete' }, x);
    await w.plant(repo, { version: 1, repositoryId: repo, worktreeId: y, slug: 'feature-login', domain: OLD, initialization: 'complete' }, y);
    await expect(registry.listAll()).resolves.toHaveLength(3);
    expect([...(await registry.scanAll()).conflicts.keys()]).toEqual([`feature-login.${OLD}`]);
    await expect(registry.findGlobal(x)).resolves.toMatchObject({ worktreeId: x });
    await expect(registry.findGlobal('feature-login', OLD)).rejects.toThrow(/registered to more than one worktree/);
    await expect(registry.findGlobal('feature-login')).rejects.toThrow(/registered to more than one worktree/);
    await expect(registry.findGlobal('main', OLD)).resolves.toMatchObject({ slug: 'main' });
    const other = await w.wt('other', 'feature/login');
    expect((await w.reg(other, OLD).ensure()).slug).toMatch(SUFFIXED);
    const unrelated = await w.wt('unrelated', 'feature/other');
    expect((await w.reg(unrelated, OLD).ensure()).slug).toBe('feature-other');
  });
});
