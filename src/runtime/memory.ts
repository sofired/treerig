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
}

export interface MemoryRuntimeOptions {
  /** What the "Compose file" declares: `up([])` starts all of them, and an unknown name is refused. */
  services: readonly string[];
}

const FIRST_PORT = 30_000;

const ownerKey = (worktree: WorktreeRef): string =>
  `${worktree.repositoryId}:${worktree.worktreeId}`;

/**
 * A backend that tracks fake resources in memory, shaped like the Compose
 * one where commands can tell the difference: it has a declared service
 * list, `status` reports every declared service (`absent` when no container
 * exists), `down` removes containers but keeps the worktree's data volume,
 * and `reap` refuses a resource it cannot prove belongs to the caller.
 *
 * It exists so the `Runtime` interface is exercised by more than one
 * implementation — an interface with a single implementation is an interface
 * nobody has tested.
 */
export class MemoryRuntime implements Runtime {
  readonly kind = 'memory';
  private readonly services: readonly string[];
  private readonly resources = new Map<string, MemoryResource>();

  constructor(options: MemoryRuntimeOptions) {
    this.services = [...options.services];
  }

  async up(worktree: WorktreeRef, services: readonly ServiceSpec[]): Promise<void> {
    const owner = ownerKey(worktree);
    const names = services.length === 0 ? this.services : services.map((service) => service.name);
    for (const name of names) {
      // Compose's own wording, lower case included.
      if (!this.services.includes(name)) throw new CliError(`no such service: ${name}`);
    }
    // Compose creates the declared volume on the first `up`; it outlives `down`.
    const volume = `${owner}:volume:data`;
    if (!this.resources.has(volume)) this.resources.set(volume, { kind: 'volume', id: volume, owner });
    for (const name of names) {
      const id = `${owner}:container:${name}`;
      if (!this.resources.has(id)) this.resources.set(id, { kind: 'container', id, service: name, owner });
    }
    await Promise.resolve();
  }

  /** Removes the containers, as `docker compose down` does; the volume stays. */
  async down(worktree: WorktreeRef): Promise<void> {
    const owner = ownerKey(worktree);
    for (const [id, resource] of this.resources) {
      if (resource.owner === owner && resource.kind === 'container') this.resources.delete(id);
    }
    await Promise.resolve();
  }

  async status(worktree: WorktreeRef): Promise<ServiceStatus[]> {
    const owner = ownerKey(worktree);
    await Promise.resolve();
    // Compose sorts service names plainly here and by locale in `endpoints`; so does this.
    return [...this.services]
      .sort()
      .map((service) => ({ service, state: this.resources.has(`${owner}:container:${service}`) ? 'running' : 'absent' }));
  }

  /**
   * Read from the "model", as Compose does: every declared service, whatever
   * its state, at a fixed port assigned in declaration order, returned sorted
   * by name — the order `ComposeRuntime.endpoints` returns. The answer does
   * not depend on the worktree. Two things it cannot model, neither visible to
   * any caller because nothing reads endpoints yet: the host is loopback
   * rather than the service's in-network name, and every declared service
   * gets a port, because a name list has nowhere to say "this one exposes
   * nothing".
   */
  async endpoints(worktree: WorktreeRef): Promise<Endpoint[]> {
    void worktree;
    await Promise.resolve();
    return this.services
      .map((service, index) => ({ service, host: '127.0.0.1', port: FIRST_PORT + index }))
      .sort((left, right) => left.service.localeCompare(right.service));
  }

  /** In removal order: containers first, then the volume. */
  async inventory(worktree: WorktreeRef): Promise<OwnedResource[]> {
    const owner = ownerKey(worktree);
    await Promise.resolve();
    const owned = [...this.resources.values()].filter((resource) => resource.owner === owner);
    return [...owned.filter((r) => r.kind === 'container'), ...owned.filter((r) => r.kind !== 'container')]
      .map(({ kind, id, service }) => (service === undefined ? { kind, id } : { kind, id, service }));
  }

  async reap(worktree: WorktreeRef, resource: OwnedResource): Promise<void> {
    const existing = this.resources.get(resource.id);
    if (!existing || existing.owner !== ownerKey(worktree)) {
      throw new CliError(
        `Refusing to remove ${resource.kind} ${resource.id}: it is not owned by this worktree.`
      );
    }
    // Compose inspects by kind, so a resource named as the wrong one is refused too.
    if (existing.kind !== resource.kind) {
      throw new CliError(`Refusing to remove ${resource.kind} ${resource.id}: it is a ${existing.kind}.`);
    }
    this.resources.delete(resource.id);
    await Promise.resolve();
  }
}
