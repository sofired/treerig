import { CliError } from '../errors.ts';
import type {
  Endpoint,
  OwnedResource,
  Runtime,
  ServiceSpec,
  ServiceStatus,
  WorktreeRef
} from './runtime.ts';

interface MemoryResource extends OwnedResource {
  owner: string;
  state: 'running' | 'stopped';
}

const ownerKey = (worktree: WorktreeRef): string =>
  `${worktree.repositoryId}:${worktree.worktreeId}`;

/**
 * A backend that tracks fake resources in memory.
 *
 * It exists so the `Runtime` interface is exercised by more than one
 * implementation — an interface with a single implementation is an interface
 * nobody has tested. It also models the one property every real backend must
 * have: `reap` refuses a resource it cannot prove belongs to the caller.
 */
export class MemoryRuntime implements Runtime {
  readonly kind = 'memory';
  private readonly resources = new Map<string, MemoryResource>();
  private nextPort = 30_000;

  async up(worktree: WorktreeRef, services: readonly ServiceSpec[]): Promise<void> {
    const owner = ownerKey(worktree);
    for (const service of services) {
      const id = `${owner}:${service.name}`;
      const existing = this.resources.get(id);
      if (existing) {
        existing.state = 'running';
        continue;
      }
      this.resources.set(id, {
        kind: 'container',
        id,
        service: service.name,
        owner,
        state: 'running'
      });
    }
    await Promise.resolve();
  }

  async down(worktree: WorktreeRef): Promise<void> {
    const owner = ownerKey(worktree);
    for (const resource of this.resources.values()) {
      if (resource.owner === owner) resource.state = 'stopped';
    }
    await Promise.resolve();
  }

  async status(worktree: WorktreeRef): Promise<ServiceStatus[]> {
    const owner = ownerKey(worktree);
    const statuses: ServiceStatus[] = [];
    for (const resource of this.resources.values()) {
      if (resource.owner !== owner || !resource.service) continue;
      statuses.push({ service: resource.service, state: resource.state });
    }
    await Promise.resolve();
    return statuses.sort((a, b) => a.service.localeCompare(b.service));
  }

  async endpoints(worktree: WorktreeRef): Promise<Endpoint[]> {
    const owner = ownerKey(worktree);
    const endpoints: Endpoint[] = [];
    for (const resource of this.resources.values()) {
      if (resource.owner !== owner || resource.state !== 'running' || !resource.service) continue;
      endpoints.push({ service: resource.service, host: '127.0.0.1', port: this.nextPort++ });
    }
    await Promise.resolve();
    return endpoints;
  }

  async inventory(worktree: WorktreeRef): Promise<OwnedResource[]> {
    const owner = ownerKey(worktree);
    await Promise.resolve();
    return [...this.resources.values()]
      .filter((resource) => resource.owner === owner)
      .map(({ kind, id, service }) => ({ kind, id, service }));
  }

  async reap(worktree: WorktreeRef, resource: OwnedResource): Promise<void> {
    const existing = this.resources.get(resource.id);
    if (!existing || existing.owner !== ownerKey(worktree)) {
      throw new CliError(
        `Refusing to remove ${resource.kind} ${resource.id}: it is not owned by this worktree.`
      );
    }
    this.resources.delete(resource.id);
    await Promise.resolve();
  }
}
