import { describe, expect, it } from 'vitest';
import { MemoryRuntime } from '../../src/runtime/memory.ts';
import type { WorktreeRef } from '../../src/runtime/runtime.ts';

const login: WorktreeRef = { repositoryId: 'repo-a', worktreeId: 'wt-1', slug: 'feature-login' };
const api: WorktreeRef = { repositoryId: 'repo-a', worktreeId: 'wt-2', slug: 'feature-api' };

describe('MemoryRuntime', () => {
  it('brings services up, reports them, and takes them down', async () => {
    const runtime = new MemoryRuntime();
    await runtime.up(login, [{ name: 'web' }, { name: 'db' }]);

    expect(await runtime.status(login)).toEqual([
      { service: 'db', state: 'running' },
      { service: 'web', state: 'running' }
    ]);
    expect(await runtime.endpoints(login)).toHaveLength(2);

    await runtime.down(login);
    expect(await runtime.status(login)).toEqual([
      { service: 'db', state: 'stopped' },
      { service: 'web', state: 'stopped' }
    ]);
  });

  it('scopes inventory to the owning worktree', async () => {
    const runtime = new MemoryRuntime();
    await runtime.up(login, [{ name: 'web' }]);
    await runtime.up(api, [{ name: 'web' }]);

    expect(await runtime.inventory(login)).toHaveLength(1);
    expect(await runtime.inventory(api)).toHaveLength(1);
  });

  it('refuses to reap a resource another worktree owns', async () => {
    const runtime = new MemoryRuntime();
    await runtime.up(login, [{ name: 'web' }]);
    const [resource] = await runtime.inventory(login);

    await expect(runtime.reap(api, resource)).rejects.toThrow('not owned by this worktree');
    expect(await runtime.inventory(login)).toHaveLength(1);

    await runtime.reap(login, resource);
    expect(await runtime.inventory(login)).toHaveLength(0);
  });
});
