import path from 'node:path';
import type { HookStage, HooksConfig } from '../config/schema.ts';
import { CliError } from '../errors.ts';
import type { Logger } from '../logger.ts';
import type { CommandRunner } from '../process-runner.ts';
import { withProgress } from '../progress.ts';
import { hookEnvironmentVariables, LIFECYCLE_ORDER, type HookEnvironment } from './protocol.ts';

export interface HookRunOptions {
  /** Passed as the hook's first argument (backup and restore bundle directory). */
  argument?: string;
  cancelSignal?: AbortSignal;
  timeoutMs?: number;
}

export interface HookContext {
  environment: HookEnvironment;
  hooks: HooksConfig;
  logger: Logger;
  runner: CommandRunner;
}

/**
 * Runs one hook. The command is resolved relative to the project root, runs
 * with the project root as its working directory, sees the hook environment on
 * top of the parent's, and has its output streamed to the terminal. Any
 * non-zero exit is a failure attributed to the stage.
 */
export async function runHook(
  stage: HookStage,
  context: HookContext,
  options: HookRunOptions = {}
): Promise<boolean> {
  const command = context.hooks[stage];
  if (!command) return false;

  const [executable, ...args] = splitCommand(command);
  if (!executable) throw new CliError(`Hook "${stage}" is empty.`);
  const resolved = executable.startsWith('.') || executable.includes('/')
    ? path.resolve(context.environment.ROOT, executable)
    : executable;
  if (options.argument !== undefined) args.push(options.argument);

  await withProgress(context.logger, `Running ${stage} hook`, async () => {
    const result = await context.runner.run(resolved, args, {
      cwd: context.environment.ROOT,
      env: { ...process.env, ...hookEnvironmentVariables({ ...context.environment, STAGE: stage }) },
      printOutput: true,
      reject: false,
      cancelSignal: options.cancelSignal,
      timeoutMs: options.timeoutMs
    });
    if (result.exitCode !== 0) {
      throw new CliError(
        `The ${stage} hook failed (exit ${result.exitCode})${result.timedOut ? ' — timed out' : ''}.`,
        result.exitCode
      );
    }
  });
  return true;
}

/**
 * Runs prepare → seed → verify in order, stopping at the first failure.
 * Returns the stages that actually ran (unconfigured stages are skipped).
 */
export async function runLifecycle(
  context: HookContext,
  options: Pick<HookRunOptions, 'cancelSignal' | 'timeoutMs'> = {}
): Promise<HookStage[]> {
  const ran: HookStage[] = [];
  for (const stage of LIFECYCLE_ORDER) {
    if (await runHook(stage, context, options)) ran.push(stage);
  }
  return ran;
}

/**
 * Minimal shell-free splitting: whitespace-separated, with double-quoted
 * segments kept intact. Hooks needing anything richer should be scripts.
 */
export function splitCommand(command: string): string[] {
  const parts: string[] = [];
  const pattern = /"([^"]*)"|(\S+)/g;
  for (const match of command.matchAll(pattern)) {
    parts.push(match[1] ?? match[2] ?? '');
  }
  return parts;
}
