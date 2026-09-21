import { createHash, randomUUID } from 'node:crypto';
import { mkdtemp, mkdir, readFile, realpath, rm, symlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { BRAND } from '../../src/brand.ts';
import { silentLogger as logger } from '../../src/logger.ts';
import { ScopedLock, worktreeLockTarget } from '../../src/operation-lock.ts';
import {
  allocateSlug,
  branchSlug,
  registryDirectory,
  worktreeHome,
  WorktreeIdentityService,
  WorktreeRegistry,
  type RegisteredWorktree,
  type WorktreeIdentity
} from '../../src/worktree-registry.ts';
import { GitRunner } from '../helpers/git-runner.ts';

const temporary: string[] = [];
const ACME = 'acme.localhost';
const WIDGETS = 'widgets.localhost';

afterEach(async () => {
  await Promise.all(
    temporary.splice(0).map((directory) => rm(directory, { recursive: true, force: true }))
  );
});

interface Fixture {
  base: string;
  common: string;
  controlHome: string;
  registry: WorktreeRegistry;
  root: string;
  runner: GitRunner;
}

async function fixture(branch = 'feature/ISSUE-123-routing', domain = ACME): Promise<Fixture> {
  const base = await realpath(await mkdtemp(path.join(tmpdir(), 'berth-registry-')));
  temporary.push(base);
  const controlHome = path.join(base, 'control');
  const root = path.join(base, 'worktree');
  const common = path.join(base, 'common');
  await mkdir(root, { recursive: true });
  await mkdir(path.join(common, 'worktrees', 'worktree'), { recursive: true });
  const runner = new GitRunner();
  runner.add(root, branch, common);
  return {
    base,
    common,
    controlHome,
    root,
    runner,
    registry: new WorktreeRegistry({ controlHome, root, domain, runner, logger })
  };
}

/** A second, unrelated repository sharing the fixture's control home. */
async function secondRepository(context: Fixture, name: string, branch: string, domain: string): Promise<WorktreeRegistry> {
  const root = path.join(context.base, name);
  const common = path.join(context.base, `${name}-common`);
  await mkdir(root, { recursive: true });
  await mkdir(path.join(common, 'worktrees', name), { recursive: true });
  context.runner.add(root, branch, common);
  return new WorktreeRegistry({ controlHome: context.controlHome, root, domain, runner: context.runner, logger });
}

function record(overrides: { slug: string; domain?: string; repositoryId?: string; worktreeId?: string }) {
  return `${JSON.stringify({
    version: 1,
    repositoryId: overrides.repositoryId ?? randomUUID(),
    worktreeId: overrides.worktreeId ?? randomUUID(),
    slug: overrides.slug,
    domain: overrides.domain ?? ACME,
    initialization: 'complete'
  })}\n`;
}

describe('WorktreeRegistry', () => {
  it('uses the complete branch name for new slugs', async () => {
    const nested = await fixture('test/worktrees');
    const deeplyNested = await fixture('users/alice/feature');

    await expect(nested.registry.ensure()).resolves.toMatchObject({ slug: 'test-worktrees' });
    await expect(deeplyNested.registry.ensure()).resolves.toMatchObject({
      slug: 'users-alice-feature'
    });
  });

  it('uses the worktree directory when HEAD is detached', async () => {
    const context = await fixture('');

    await expect(context.registry.ensure()).resolves.toMatchObject({ slug: 'worktree' });
  });

  it('persists UUID identities and derives stable managed paths', async () => {
    const context = await fixture();

    const first = await context.registry.ensure();
    context.runner.setBranch(context.root, 'renamed/ISSUE-123-routing');
    const second = await context.registry.ensure();

    expect(first.slug).toBe('feature-issue-123-routing');
    expect(first.domain).toBe(ACME);
    expect(second).toMatchObject({
      slug: first.slug,
      domain: ACME,
      repositoryId: first.repositoryId,
      worktreeId: first.worktreeId,
      branch: 'renamed/ISSUE-123-routing'
    });
    expect(worktreeHome(context.controlHome, first.repositoryId, first.worktreeId)).toBe(
      path.join(context.controlHome, 'repositories', first.repositoryId, 'instances', first.worktreeId)
    );

    await expect(readFile(path.join(context.common, BRAND.markerDir, 'repository-id'), 'utf8'))
      .resolves.toContain(first.repositoryId);
    await expect(readFile(
      path.join(context.common, 'worktrees', 'worktree', BRAND.markerDir, 'worktree-id'),
      'utf8'
    )).resolves.toContain(first.worktreeId);

    const [stored] = await context.registry.list();
    expect(stored).toMatchObject({
      repositoryId: first.repositoryId,
      worktreeId: first.worktreeId,
      slug: first.slug,
      domain: ACME
    });
    const onDisk = JSON.parse(await readFile(
      path.join(registryDirectory(context.controlHome, first.repositoryId), `${first.worktreeId}.json`),
      'utf8'
    )) as unknown;
    expect(onDisk).toEqual({
      version: 1,
      repositoryId: first.repositoryId,
      worktreeId: first.worktreeId,
      slug: 'feature-issue-123-routing',
      domain: ACME,
      initialization: 'pending'
    });
    expect(registryDirectory(context.controlHome, first.repositoryId)).toContain(
      path.join('repositories', first.repositoryId, 'worktrees')
    );
  });

  it('reads an existing registration without waiting for the operation lock', async () => {
    const context = await fixture();
    const worktree = await context.registry.ensure();
    const operation = new ScopedLock(
      worktreeLockTarget(context.controlHome, worktree.repositoryId, worktree.worktreeId),
      'active build',
      logger
    );
    await operation.acquire();
    try {
      await expect(context.registry.current()).resolves.toMatchObject({
        slug: worktree.slug,
        repositoryId: worktree.repositoryId,
        worktreeId: worktree.worktreeId
      });
    } finally {
      await operation.release();
    }
  });

  it('serializes concurrent first registration', async () => {
    const context = await fixture('feature/concurrent');
    const second = new WorktreeRegistry({
      controlHome: context.controlHome,
      root: context.root,
      domain: ACME,
      runner: context.runner,
      logger
    });

    const [left, right] = await Promise.all([context.registry.ensure(), second.ensure()]);

    expect(left).toEqual(right);
    await expect(context.registry.list()).resolves.toHaveLength(1);
  });

  it('adopts an existing home without a registration as completed', async () => {
    const context = await fixture('feature/recovered');
    const worktree = await context.registry.ensure();
    const file = path.join(
      registryDirectory(context.controlHome, worktree.repositoryId),
      `${worktree.worktreeId}.json`
    );
    await rm(file);
    await mkdir(worktreeHome(context.controlHome, worktree.repositoryId, worktree.worktreeId), {
      recursive: true
    });

    await context.registry.ensure();

    const rewritten = await readFile(file, 'utf8');
    expect(rewritten).toContain('"initialization": "complete"');
    expect(rewritten).toContain(`"domain": "${ACME}"`);
  });

  it('rejects a symlinked identity-marker parent before creating markers', async () => {
    const context = await fixture();
    const markerDirectory = path.join(context.common, 'worktrees', 'worktree', BRAND.markerDir);
    const outside = path.join(path.dirname(context.controlHome), 'outside-markers');
    await mkdir(outside);
    await symlink(outside, markerDirectory, 'dir');

    await expect(context.registry.ensure()).rejects.toThrow('Unsafe identity marker path');
    await expect(readFile(path.join(outside, 'worktree-id'), 'utf8')).rejects.toThrow();
  });

  it('does not rebind data when Git reuses a worktree administrative name', async () => {
    const context = await fixture('feature/old');
    const old = await context.registry.ensure();
    const reusedGitDir = path.join(context.common, 'worktrees', 'worktree');
    await rm(reusedGitDir, { recursive: true, force: true });
    await mkdir(reusedGitDir, { recursive: true });
    context.runner.setBranch(context.root, 'feature/new');

    const fresh = await context.registry.ensure();

    expect(fresh.worktreeId).not.toBe(old.worktreeId);
    expect(worktreeHome(context.controlHome, fresh.repositoryId, fresh.worktreeId)).not.toBe(
      worktreeHome(context.controlHome, old.repositoryId, old.worktreeId)
    );
    const records = await context.registry.list();
    expect(records.map(({ worktreeId }) => worktreeId)).toEqual(
      expect.arrayContaining([old.worktreeId, fresh.worktreeId])
    );
    const freshRecord = await readFile(
      path.join(registryDirectory(context.controlHome, fresh.repositoryId), `${fresh.worktreeId}.json`),
      'utf8'
    );
    expect(freshRecord).toContain('"initialization": "pending"');
    await expect(context.registry.pruneCandidates()).resolves.toEqual(
      expect.arrayContaining([expect.objectContaining({ worktreeId: old.worktreeId })])
    );
  });

  it('preserves identity when a worktree moves', async () => {
    const context = await fixture('feature/move');
    const first = await context.registry.ensure();
    const moved = path.join(path.dirname(context.root), 'moved-worktree');
    await mkdir(moved);
    const current = context.runner.identities.get(context.root);
    context.runner.identities.delete(context.root);
    context.runner.active = [moved];
    if (current) context.runner.identities.set(moved, { ...current, root: moved });
    const movedRegistry = new WorktreeRegistry({
      controlHome: context.controlHome,
      root: moved,
      domain: ACME,
      runner: context.runner,
      logger
    });

    const second = await movedRegistry.ensure();

    expect(second.worktreeId).toBe(first.worktreeId);
    expect(second.root).toBe(moved);
  });

  it('ignores active sibling worktrees that do not have a worktree marker', async () => {
    const context = await fixture('feature/registered');
    await context.registry.ensure();
    const sibling = path.join(path.dirname(context.root), 'unregistered-worktree');
    await mkdir(sibling);
    await mkdir(path.join(context.common, 'worktrees', 'unregistered-worktree'), { recursive: true });
    context.runner.add(sibling, 'feature/unregistered', context.common);

    const identity = await new WorktreeIdentityService(sibling, context.runner).inspect(sibling);

    expect(identity).toBeUndefined();
    await expect(context.registry.pruneCandidates()).resolves.toEqual([]);
  });
});

describe('slug uniqueness is per domain', () => {
  it('suffixes a collision within one repository and domain', async () => {
    const context = await fixture('feature/shared');
    const first = await context.registry.ensure();
    const secondRoot = path.join(path.dirname(context.root), 'second-worktree');
    await mkdir(secondRoot);
    await mkdir(path.join(context.common, 'worktrees', 'second-worktree'), { recursive: true });
    context.runner.add(secondRoot, 'feature_shared', context.common);
    const second = await new WorktreeRegistry({
      controlHome: context.controlHome,
      root: secondRoot,
      domain: ACME,
      runner: context.runner,
      logger
    }).ensure();

    expect(first.slug).toBe('feature-shared');
    expect(second.slug).toMatch(/^feature-shared-[a-f0-9]{12}$/);
  });

  it('suffixes a collision across repositories that share a domain', async () => {
    const context = await fixture('feature/login', ACME);
    const acme = await context.registry.ensure();
    const other = await secondRepository(context, 'acme-fork', 'feature/login', ACME);

    const fork = await other.ensure();

    expect(fork.repositoryId).not.toBe(acme.repositoryId);
    expect(acme.slug).toBe('feature-login');
    expect(fork.slug).toMatch(/^feature-login-[a-f0-9]{12}$/);
  });

  it('lets unrelated projects on different domains share a slug', async () => {
    const context = await fixture('feature/login', ACME);
    const acme = await context.registry.ensure();
    const other = await secondRepository(context, 'widgets-api', 'feature/login', WIDGETS);

    const widgets = await other.ensure();

    expect(widgets.repositoryId).not.toBe(acme.repositoryId);
    expect(acme.slug).toBe('feature-login');
    expect(widgets.slug).toBe('feature-login');
    expect(acme.domain).toBe(ACME);
    expect(widgets.domain).toBe(WIDGETS);
    await expect(context.registry.listAll()).resolves.toHaveLength(2);
  });

  it('reports a duplicate hostname found while scanning without blocking unrelated registration', async () => {
    const context = await fixture('feature/shared');
    await context.registry.ensure();
    const duplicateRepositoryId = randomUUID();
    const duplicateWorktreeId = randomUUID();
    const duplicateDirectory = registryDirectory(context.controlHome, duplicateRepositoryId);
    await mkdir(duplicateDirectory, { recursive: true });
    await writeFile(
      path.join(duplicateDirectory, `${duplicateWorktreeId}.json`),
      record({ slug: 'feature-shared', domain: ACME, repositoryId: duplicateRepositoryId, worktreeId: duplicateWorktreeId })
    );
    const secondRoot = path.join(path.dirname(context.root), 'second-worktree');
    await mkdir(secondRoot);
    await mkdir(path.join(context.common, 'worktrees', 'second-worktree'), { recursive: true });
    context.runner.add(secondRoot, 'feature/unique', context.common);

    await expect(new WorktreeRegistry({
      controlHome: context.controlHome,
      root: secondRoot,
      domain: ACME,
      runner: context.runner,
      logger
    }).ensure()).resolves.toMatchObject({ slug: 'feature-unique' });
    const scan = await context.registry.scanAll();
    expect([...scan.conflicts.keys()]).toEqual([`feature-shared.${ACME}`]);
    expect(scan.conflicts.get(`feature-shared.${ACME}`)).toHaveLength(2);
    await expect(context.registry.findGlobal('feature-shared', ACME)).rejects.toThrow('registered to more than one worktree');
    await expect(context.registry.findGlobal(duplicateWorktreeId)).resolves.toMatchObject({ worktreeId: duplicateWorktreeId });
  });

  it('tolerates the same slug under two domains when scanning', async () => {
    const context = await fixture('feature/shared', ACME);
    await context.registry.ensure();
    const otherRepositoryId = randomUUID();
    const otherWorktreeId = randomUUID();
    const otherDirectory = registryDirectory(context.controlHome, otherRepositoryId);
    await mkdir(otherDirectory, { recursive: true });
    await writeFile(
      path.join(otherDirectory, `${otherWorktreeId}.json`),
      record({ slug: 'feature-shared', domain: WIDGETS, repositoryId: otherRepositoryId, worktreeId: otherWorktreeId })
    );

    await expect(context.registry.listAll()).resolves.toHaveLength(2);
  });
});

describe('slug derivation', () => {
  const identity = (branch: string, root = '/repo'): WorktreeIdentity => ({
    root, branch, gitDir: '/g', gitCommonDir: '/g',
    repositoryId: 'a08b4892-5111-49f1-a47a-5ab988c7f676',
    worktreeId: '2b41d9d4-ed88-49c1-be9c-5439bdf6a431'
  });

  it('never ends in a hyphen even when truncation lands on one', () => {
    expect(branchSlug(identity('a'.repeat(47) + '-b'))).toBe('a'.repeat(47));
    expect(branchSlug(identity('feature/' + 'x'.repeat(39) + '/y'))).toMatch(/[a-z0-9]$/);
    for (const slug of [branchSlug(identity('a'.repeat(47) + '-b')), branchSlug(identity('x'.repeat(60)))]) {
      expect(slug.length).toBeLessThanOrEqual(48);
      expect(slug).toMatch(/^[a-z0-9][a-z0-9-]*[a-z0-9]$|^[a-z0-9]$/);
    }
  });

  it('falls back to the directory name when detached, and to a constant when nothing survives', () => {
    expect(branchSlug(identity('', '/work/my-tree'))).toBe('my-tree');
    expect(branchSlug(identity('---', '/work/---'))).toBe('worktree');
    expect(branchSlug(identity('日本語'))).toBe('worktree');
  });

  it('keeps every suffixed candidate within the DNS label limit, however many collide', () => {
    const id = identity('feature/shared');
    const taken: RegisteredWorktree[] = [];
    const digest = createHash('sha256')
      .update(`${id.repositoryId}:${id.worktreeId}`).digest('hex');
    // Occupy the bare slug and every suffix length the allocator will try.
    taken.push({ repositoryId: 'r', worktreeId: 'w0', slug: 'feature-shared', domain: ACME });
    for (let length = 12; length <= 60; length += 4) {
      const suffix = digest.slice(0, length);
      taken.push({ repositoryId: 'r', worktreeId: `w${length}`, slug: `${'feature-shared'.slice(0, 62 - length)}-${suffix}`, domain: ACME });
    }
    expect(() => allocateSlug(id, ACME, taken)).toThrow('Could not allocate a unique slug');
    // With one fewer collision, the longest candidate is used and is exactly 63 chars.
    const last = taken.pop();
    const slug = allocateSlug(id, ACME, taken);
    expect(slug).toBe(last?.slug);
    expect(slug.length).toBeLessThanOrEqual(63);
  });

  it('prefers the slug a worktree already holds over re-deriving it from the branch', () => {
    const id = identity('feature/renamed-since');
    expect(allocateSlug(id, ACME, [], 'feature-original')).toBe('feature-original');
    const taken = [{ repositoryId: 'r', worktreeId: 'other', slug: 'feature-original', domain: ACME }];
    expect(allocateSlug(id, ACME, taken, 'feature-original')).toBe('feature-renamed-since');
  });

  it('keeps a suffix it already holds even when the bare name is free, so two clones can never swap', () => {
    // Healing the suffix here would look harmless, but "the bare name is free"
    // is the transient state mid-rename; the clone that reconciles second
    // would then be forced onto the suffix — a silent hostname swap.
    const id = identity('feature/login');
    expect(allocateSlug(id, ACME, [], 'feature-login-1d407e139915')).toBe('feature-login-1d407e139915');
  });
});

describe('global lookup', () => {
  it('lists registrations across repository boundaries for the shared index', async () => {
    const context = await fixture('feature/current');
    const current = await context.registry.ensure();
    const otherRepositoryId = randomUUID();
    const otherWorktreeId = randomUUID();
    const otherDirectory = registryDirectory(context.controlHome, otherRepositoryId);
    await mkdir(otherDirectory, { recursive: true });
    await writeFile(
      path.join(otherDirectory, `${otherWorktreeId}.json`),
      record({ slug: 'other-clone', domain: WIDGETS, repositoryId: otherRepositoryId, worktreeId: otherWorktreeId })
    );

    const all = await context.registry.listAll();
    expect(all).toHaveLength(2);
    expect(all).toEqual(expect.arrayContaining([
      expect.objectContaining({
        repositoryId: current.repositoryId,
        worktreeId: current.worktreeId,
        slug: current.slug,
        domain: ACME
      }),
      expect.objectContaining({
        repositoryId: otherRepositoryId,
        worktreeId: otherWorktreeId,
        slug: 'other-clone',
        domain: WIDGETS
      })
    ]));
    await expect(context.registry.findGlobal('other-clone')).resolves.toEqual({
      repositoryId: otherRepositoryId,
      worktreeId: otherWorktreeId,
      slug: 'other-clone',
      domain: WIDGETS
    });
    await expect(context.registry.findGlobal(otherWorktreeId)).resolves.toEqual({
      repositoryId: otherRepositoryId,
      worktreeId: otherWorktreeId,
      slug: 'other-clone',
      domain: WIDGETS
    });
  });

  it('refuses to guess between the same slug under two domains, and narrows by domain', async () => {
    const context = await fixture('feature/login', ACME);
    const acme = await context.registry.ensure();
    const widgets = await (await secondRepository(context, 'widgets-api', 'feature/login', WIDGETS)).ensure();

    await expect(context.registry.findGlobal('feature-login')).rejects.toThrow(
      `Slug feature-login exists under ${ACME}, ${WIDGETS}; specify a domain.`
    );
    await expect(context.registry.findGlobal('feature-login', WIDGETS)).resolves.toMatchObject({
      worktreeId: widgets.worktreeId,
      domain: WIDGETS
    });
    await expect(context.registry.findGlobal('feature-login', ACME)).resolves.toMatchObject({
      worktreeId: acme.worktreeId,
      domain: ACME
    });
    await expect(context.registry.findGlobal(widgets.worktreeId)).resolves.toMatchObject({
      worktreeId: widgets.worktreeId
    });
    await expect(context.registry.findGlobal('feature-login', 'nowhere.localhost')).resolves.toBeUndefined();
  });
});
