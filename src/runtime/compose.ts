import { CliError } from '../errors.ts';
import type { CommandRunner } from '../process-runner.ts';
import type { Endpoint, OwnedResource, Runtime, ServiceStatus } from './runtime.ts';

export interface ComposeRuntimeOptions {
  /** Absolute path to the Compose file. */
  composeFile: string;
  /** Docker label namespace; every created resource is tagged under it. */
  labelNamespace: string;
  runner: CommandRunner;
}

const NOT_YET = 'The Compose runtime is not implemented in this scaffold. ' +
  'Ownership-labelled up/down, label-filtered inventory, and verified reap are the next port.';

/**
 * The shipping backend. Drives Docker Compose through the `docker` CLI — never
 * an SDK — and stamps every resource with `<namespace>.managed`,
 * `<namespace>.repository-uuid`, and `<namespace>.worktree-uuid` so that
 * `inventory` is a label-filtered `docker ps` and `reap` can verify the full
 * ownership tuple before removing anything.
 *
 * Every method currently fails loudly. That is deliberate: a stub that
 * pretends to work is worse than one that says it does not. The methods take
 * no parameters because TypeScript lets a narrower signature satisfy the
 * interface, and unused parameters would be lint noise.
 */
export class ComposeRuntime implements Runtime {
  readonly kind = 'compose';
  readonly options: ComposeRuntimeOptions;

  constructor(options: ComposeRuntimeOptions) {
    this.options = options;
  }

  up(): Promise<void> {
    return Promise.reject(new CliError(NOT_YET, 2));
  }

  down(): Promise<void> {
    return Promise.reject(new CliError(NOT_YET, 2));
  }

  status(): Promise<ServiceStatus[]> {
    return Promise.reject(new CliError(NOT_YET, 2));
  }

  endpoints(): Promise<Endpoint[]> {
    return Promise.reject(new CliError(NOT_YET, 2));
  }

  inventory(): Promise<OwnedResource[]> {
    return Promise.reject(new CliError(NOT_YET, 2));
  }

  reap(): Promise<void> {
    return Promise.reject(new CliError(NOT_YET, 2));
  }
}
