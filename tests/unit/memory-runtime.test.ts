import { describe, expect, it } from 'vitest';
import { MemoryRuntime } from '../../src/runtime/memory.ts';
import type { WorktreeRef } from '../../src/runtime/runtime.ts';

const login: WorktreeRef = { repositoryId: 'repo-a', worktreeId: 'wt-1', slug: 'feature-login' };
const api: WorktreeRef = { repositoryId: 'repo-a', worktreeId: 'wt-2', slug: 'feature-api' };

describe('MemoryRuntime', () => {
  it('brings services up, reports them, and takes them down like Compose would', async () => {
    const runtime = new MemoryRuntime({ services: ['web', 'db'] });
    await runtime.up(login, [{ name: 'web' }, { name: 'db' }]);

    expect(await runtime.status(login)).toEqual([
      { service: 'db', state: 'running' },
      { service: 'web', state: 'running' }
    ]);
    // Endpoints come from the definition, not from what is running: ports fixed in
    // declaration order, returned sorted by name, the same for every worktree.
    expect(await runtime.endpoints(login)).toEqual([
      { service: 'db', host: '127.0.0.1', port: 30_001 },
      { service: 'web', host: '127.0.0.1', port: 30_000 }
    ]);
    expect(await runtime.endpoints(api)).toEqual(await runtime.endpoints(login));

    await runtime.down(login);
    // The containers are gone, not stopped: that is what `docker compose down` leaves.
    expect(await runtime.status(login)).toEqual([
      { service: 'db', state: 'absent' },
      { service: 'web', state: 'absent' }
    ]);
    expect(await runtime.endpoints(login)).toHaveLength(2);
  });

  it('lists services it was not asked to start as absent, and starts all of them when asked for none in particular', async () => {
    const runtime = new MemoryRuntime({ services: ['web', 'db', 'mail'] });
    await runtime.up(login, [{ name: 'web' }]);
    expect(await runtime.status(login)).toEqual([
      { service: 'db', state: 'absent' },
      { service: 'mail', state: 'absent' },
      { service: 'web', state: 'running' }
    ]);

    await runtime.up(login, []);
    expect((await runtime.status(login)).map((s) => s.state)).toEqual(['running', 'running', 'running']);
  });

  it('refuses a service the definition does not declare', async () => {
    const runtime = new MemoryRuntime({ services: ['web'] });
    await expect(runtime.up(login, [{ name: 'cache' }])).rejects.toThrow('no such service: cache');
    expect(await runtime.inventory(login)).toEqual([]);
  });

  it('keeps the data volume across down, so removal has something left to reap', async () => {
    const runtime = new MemoryRuntime({ services: ['web'] });
    await runtime.up(login, []);
    expect((await runtime.inventory(login)).map((r) => r.kind)).toEqual(['container', 'volume']);

    await runtime.down(login);
    expect((await runtime.inventory(login)).map((r) => r.kind)).toEqual(['volume']);
  });

  it('scopes inventory to the owning worktree', async () => {
    const runtime = new MemoryRuntime({ services: ['web'] });
    await runtime.up(login, [{ name: 'web' }]);
    await runtime.up(api, [{ name: 'web' }]);

    expect(await runtime.inventory(login)).toHaveLength(2);
    expect(await runtime.inventory(api)).toHaveLength(2);
  });

  it('refuses to reap a resource another worktree owns', async () => {
    const runtime = new MemoryRuntime({ services: ['web'] });
    await runtime.up(login, [{ name: 'web' }]);
    const [resource] = await runtime.inventory(login);

    await expect(runtime.reap(api, resource)).rejects.toThrow('not owned by this worktree');
    // Nor a resource named by the wrong kind: Compose inspects by kind, so a mismatch is a refusal too.
    await expect(runtime.reap(login, { ...resource, kind: 'volume' })).rejects.toThrow(`Refusing to remove volume ${resource.id}: it is a container.`);
    expect(await runtime.inventory(login)).toHaveLength(2);

    await runtime.reap(login, resource);
    expect((await runtime.inventory(login)).map((r) => r.kind)).toEqual(['volume']);
  });
});
