// The seam between the control plane and whatever actually runs services.
//
// Everything above this interface — identity, locks, layout, the hook
// sequencer — is backend-agnostic. Everything below it is one backend's
// business. Docker Compose is the backend that ships; the interface exists so
// that the router, the index, and cleanup never have to spell "docker".

export interface WorktreeRef {
  repositoryId: string;
  worktreeId: string;
  /** Human-readable, unique within the project domain; routes as `<slug>.<domain>`. */
  slug: string;
}

export interface ServiceSpec {
  name: string;
}

export type ServiceState = 'running' | 'starting' | 'stopped' | 'missing' | 'unknown';

export interface ServiceStatus {
  service: string;
  state: ServiceState;
  detail?: string;
}

/** Where a service can actually be reached, for the router to point at. */
export interface Endpoint {
  service: string;
  host: string;
  port: number;
}

/**
 * Something the backend created on behalf of a worktree and can prove it owns.
 * `reap` must refuse any resource whose ownership cannot be verified.
 */
export interface OwnedResource {
  kind: string;
  id: string;
  service?: string;
}

export interface UpOptions {
  signal?: AbortSignal;
}

export interface Runtime {
  readonly kind: string;

  /** An empty `services` list means every service the backend's definition declares. */
  up(worktree: WorktreeRef, services: readonly ServiceSpec[], options?: UpOptions): Promise<void>;
  down(worktree: WorktreeRef): Promise<void>;
  status(worktree: WorktreeRef): Promise<ServiceStatus[]>;

  /** Feeds the router: where does each service listen? */
  endpoints(worktree: WorktreeRef): Promise<Endpoint[]>;

  /** Feeds cleanup: everything this worktree owns, provably. */
  inventory(worktree: WorktreeRef): Promise<OwnedResource[]>;
  reap(worktree: WorktreeRef, resource: OwnedResource): Promise<void>;
}
