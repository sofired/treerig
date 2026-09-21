import path from 'node:path';
import { BRAND } from '../brand.ts';
import type { LoadedConfig } from '../config/load.ts';
import type { ProjectConfig } from '../config/schema.ts';
import type { Logger } from '../logger.ts';
import type { CommandRunner } from '../process-runner.ts';
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
  logger: Logger;
  runner: CommandRunner;
  loaded: LoadedConfig;
  controlHome: string;
  registry: WorktreeRegistry;
  runtime: Runtime;
  process: ProcessControl;
}

export interface CommandContextOptions {
  loaded: LoadedConfig;
  controlHome: string;
  commandName: string;
  logger: Logger;
  runner: CommandRunner;
  /** Defaults to the Compose runtime over `runner`. */
  runtime?: Runtime;
  /** Defaults to the real process. */
  process?: ProcessControl;
}

export function labelNamespace(config: ProjectConfig): string {
  return config.project['label-namespace'] ?? BRAND.labelNamespace;
}

export function composeFilePath(loaded: LoadedConfig): string {
  return path.resolve(loaded.root, loaded.config.compose.file);
}

export function createCommandContext(options: CommandContextOptions): CommandContext {
  const { loaded, controlHome, commandName, logger, runner } = options;
  const registry = new WorktreeRegistry({
    controlHome,
    root: loaded.root,
    domain: loaded.config.project.domain,
    runner,
    logger,
    commandName
  });
  const runtime = options.runtime ?? new ComposeRuntime({
    composeFile: composeFilePath(loaded),
    projectRoot: loaded.root,
    configFile: loaded.file,
    projectName: loaded.config.project.name,
    domain: loaded.config.project.domain,
    controlHome,
    labelNamespace: labelNamespace(loaded.config),
    runner,
    logger
  });
  return {
    commandName,
    logger,
    runner,
    loaded,
    controlHome,
    registry,
    runtime,
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
