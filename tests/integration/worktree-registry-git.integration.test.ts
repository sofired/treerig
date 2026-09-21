import { mkdtemp, realpath, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { silentLogger as logger } from '../../src/logger.ts';
import { ExecaRunner } from '../../src/process-runner.ts';
import { worktreeHome, WorktreeRegistry } from '../../src/worktree-registry.ts';

const temporary: string[] = [];

afterEach(async () => {
  await Promise.all(
    temporary.splice(0).map((directory) => rm(directory, { recursive: true, force: true }))
  );
});

async function git(runner: ExecaRunner, cwd: string, args: readonly string[]): Promise<void> {
  await runner.run('git', ['-C', cwd, ...args]);
}

async function repository(): Promise<{
  base: string;
  controlHome: string;
  repository: string;
  runner: ExecaRunner;
}> {
  const base = await realpath(await mkdtemp(path.join(tmpdir(), 'berth-real-git-')));
  temporary.push(base);
  const repository = path.join(base, 'repository');
  const runner = new ExecaRunner();
  await runner.run('git', ['init', '--initial-branch=main', repository]);
  await git(runner, repository, [
    '-c', 'user.name=Registry Tests',
    '-c', 'user.email=tests@example.invalid',
    'commit', '--allow-empty', '-m', 'initial'
  ]);
  return { base, controlHome: path.join(base, 'control'), repository, runner };
}

const DOMAIN = 'acme.localhost';

function registryAt(context: { controlHome: string; runner: ExecaRunner }, root: string): WorktreeRegistry {
  return new WorktreeRegistry({ controlHome: context.controlHome, root, domain: DOMAIN, runner: context.runner, logger });
}

describe('WorktreeRegistry with real Git worktrees', () => {
  it('lists and prunes repository records from an unregistered sibling', async () => {
    const context = await repository();
    const worktree = path.join(context.base, 'registered-worktree');
    await git(context.runner, context.repository, ['worktree', 'add', '-b', 'feature/registered', worktree]);
    const registered = await registryAt(context, worktree).ensure();
    await git(context.runner, context.repository, ['worktree', 'remove', worktree]);
    const sibling = registryAt(context, context.repository);

    await expect(sibling.identities.inspect()).resolves.toBeUndefined();
    await expect(sibling.list()).resolves.toEqual([{
      repositoryId: registered.repositoryId,
      worktreeId: registered.worktreeId,
      slug: registered.slug,
      domain: DOMAIN
    }]);
    await expect(sibling.pruneCandidates()).resolves.toEqual([{
      repositoryId: registered.repositoryId,
      worktreeId: registered.worktreeId,
      slug: registered.slug,
      domain: DOMAIN
    }]);
  });

  it('treats a Git-listed worktree with a missing root as an orphan', async () => {
    const context = await repository();
    const worktree = path.join(context.base, 'missing-worktree');
    await git(context.runner, context.repository, ['worktree', 'add', '-b', 'feature/missing', worktree]);
    const registry = registryAt(context, context.repository);
    await registry.ensure();
    const missing = await registry.ensure(worktree);
    await rm(worktree, { recursive: true, force: true });

    await expect(registry.pruneCandidates()).resolves.toEqual([{
      repositoryId: missing.repositoryId,
      worktreeId: missing.worktreeId,
      slug: missing.slug,
      domain: DOMAIN
    }]);
  });

  it('does not reuse identity when Git reuses a worktree path and administrative slot', async () => {
    const context = await repository();
    const worktree = path.join(context.base, 'reused-worktree');
    await git(context.runner, context.repository, ['worktree', 'add', '-b', 'feature/first', worktree]);
    const first = await registryAt(context, worktree).ensure();

    await git(context.runner, context.repository, ['worktree', 'remove', '--force', worktree]);
    await git(context.runner, context.repository, ['worktree', 'add', '-b', 'feature/second', worktree]);
    const second = await registryAt(context, worktree).ensure();

    expect(second.repositoryId).toBe(first.repositoryId);
    expect(second.worktreeId).not.toBe(first.worktreeId);
    expect(worktreeHome(context.controlHome, second.repositoryId, second.worktreeId)).not.toBe(
      worktreeHome(context.controlHome, first.repositoryId, first.worktreeId)
    );
  });

  it('retains identity across a real Git worktree move', async () => {
    const context = await repository();
    const original = path.join(context.base, 'original-worktree');
    const moved = path.join(context.base, 'moved-worktree');
    await git(context.runner, context.repository, ['worktree', 'add', '-b', 'feature/move', original]);
    const first = await registryAt(context, original).ensure();

    await git(context.runner, context.repository, ['worktree', 'move', original, moved]);
    const second = await registryAt(context, moved).ensure();

    expect(second.repositoryId).toBe(first.repositoryId);
    expect(second.worktreeId).toBe(first.worktreeId);
    expect(second.slug).toBe(first.slug);
  });

  it('allocates distinct slugs for separate clones sharing a branch and a domain', async () => {
    const context = await repository();
    await git(context.runner, context.repository, ['checkout', '-b', 'feature/shared']);
    const clone = path.join(context.base, 'clone');
    await context.runner.run('git', ['clone', '--branch', 'feature/shared', context.repository, clone]);
    const first = await registryAt(context, context.repository).ensure();
    const second = await registryAt(context, clone).ensure();

    expect(first.slug).toBe('feature-shared');
    expect(second.slug).toMatch(/^feature-shared-[a-f0-9]{12}$/);
    expect(second.repositoryId).not.toBe(first.repositoryId);
    expect(second.worktreeId).not.toBe(first.worktreeId);
  });
});
