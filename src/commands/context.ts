import path from 'node:path';
import { BRAND } from '../brand.ts';
import { CheckoutIndexService, type CheckoutIndex } from '../checkout-index/service.ts';
import type { LoadedConfig } from '../config/load.ts';
import type { ProjectConfig } from '../config/schema.ts';
import type { Logger } from '../logger.ts';
import type { CommandRunner } from '../process-runner.ts';
import { RouterService, type Router } from '../router/router.ts';
import { ComposeRuntime } from '../runtime/compose.ts';
import type { Runtime } from '../runtime/runtime.ts';
import type { SignalSource } from '../signal-lifecycle.ts';
import { WorktreeRegistry } from '../worktree-registry.ts';

// Everything a command needs, assembled once per invocation. Commands are
// plain functions over this context so they can be exercised end to end with
// an in-memory runtime and a fake process — no Docker, no real signals.

/** The process-level effects a mutating command may need; injectable for tests. */
export interface ProcessControl {
  source: SignalSource;
  /** Re-raise a deferred signal now that the lock is released. */
  reenact(signal: NodeJS.Signals): void;
  /** Escape hatch when quiescence cannot be proven; must not return. */
  forceExit(error: unknown): never;
}

export interface CommandContext {
  commandName: string;
  /** The command line as invoked, arguments space-joined (`berth up api`); names this process to whoever waits on its locks. */
  invocation: string;
  logger: Logger;
  runner: CommandRunner;
  loaded: LoadedConfig;
  controlHome: string;
  registry: WorktreeRegistry;
  /** The runtime for this project's own label namespace. */
  runtime: Runtime;
  /** A runtime for another namespace — a worktree that adopted a predecessor's labels, say. */
  runtimeFor(labelNamespace: string): Runtime;
  router: Router;
  index: CheckoutIndex;
  process: ProcessControl;
}

export interface CommandContextOptions {
  loaded: LoadedConfig;
  controlHome: string;
  routerPort: number;
  commandName: string;
  /** Defaults to `commandName` alone. */
  invocation?: string;
  logger: Logger;
  runner: CommandRunner;
  /** Defaults to the Compose runtime over `runner`; a test double serves every namespace. */
  runtime?: Runtime;
  /** Overrides how a runtime for a foreign namespace is built; defaults to `runtime` when that is given. */
  runtimeFor?(labelNamespace: string): Runtime;
  /** Defaults to the shared Traefik router. */
  router?: Router;
  /** Defaults to the checkout index served from the router stack. */
  index?: CheckoutIndex;
  /** Defaults to the real process. */
  process?: ProcessControl;
}

export function labelNamespace(config: ProjectConfig): string {
  return config.project['label-namespace'] ?? BRAND.labelNamespace;
}

export function composeFilePath(loaded: LoadedConfig): string {
  return path.resolve(loaded.root, loaded.config.compose.file);
}

/** The registry every command uses for the checkout a config belongs to. */
export function checkoutRegistry(
  options: Pick<CommandContextOptions, 'loaded' | 'controlHome' | 'commandName' | 'logger' | 'runner'> & { invocation: string }
): WorktreeRegistry {
  const { loaded } = options;
  return new WorktreeRegistry({
    controlHome: options.controlHome,
    root: loaded.root,
    domain: loaded.config.project.domain,
    labelNamespace: labelNamespace(loaded.config),
    runner: options.runner,
    logger: options.logger,
    commandName: options.commandName,
    invocation: options.invocation
  });
}

export function createCommandContext(options: CommandContextOptions): CommandContext {
  const { loaded, controlHome, commandName, logger, runner } = options;
  const invocation = options.invocation ?? commandName;
  const registry = checkoutRegistry({ loaded, controlHome, commandName, logger, runner, invocation });
  // The router is machine-wide, so it keeps the tool's own label namespace
  // even when a project adopts a predecessor's for its worktree resources.
  const router = options.router ?? new RouterService({ controlHome, port: options.routerPort, runner, logger, invocation, commandName });
  const compose = (namespace: string): Runtime => new ComposeRuntime({
    composeFile: composeFilePath(loaded),
    projectRoot: loaded.root,
    configFile: loaded.file,
    projectName: loaded.config.project.name,
    domain: loaded.config.project.domain,
    controlHome,
    labelNamespace: namespace,
    routes: loaded.config.service.map((service) => ({
      service: service.name,
      subdomain: service.subdomain,
      ...(service.path === undefined ? {} : { path: service.path }),
      ...(service.port === undefined ? {} : { port: service.port })
    })),
    routerNetwork: router.network,
    runner,
    logger
  });
  const own = labelNamespace(loaded.config);
  const runtime = options.runtime ?? compose(own);
  const runtimeFor = (namespace: string): Runtime => {
    if (namespace === own) return runtime;
    if (options.runtimeFor) return options.runtimeFor(namespace);
    return options.runtime ?? compose(namespace);
  };
  const index = options.index ?? new CheckoutIndexService({ controlHome, routerPort: options.routerPort, registry, runner, logger, invocation });
  return {
    commandName,
    invocation,
    logger,
    runner,
    loaded,
    controlHome,
    registry,
    runtime,
    runtimeFor,
    router,
    index,
    process: options.process ?? realProcess(logger)
  };
}

function realProcess(logger: Logger): ProcessControl {
  return {
    source: process,
    reenact: (signal) => process.kill(process.pid, signal),
    forceExit: (error) => {
      logger.error(`Could not prove the interrupted operation stopped: ${error instanceof Error ? error.message : String(error)}`);
      process.exit(1);
    }
  };
}
