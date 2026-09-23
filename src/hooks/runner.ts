import path from 'node:path';
import type { HookStage, HooksConfig } from '../config/schema.ts';
import { CliError } from '../errors.ts';
import type { Logger } from '../logger.ts';
import { CommandStartError, type CommandRunner, type RunResult } from '../process-runner.ts';
import { withProgress } from '../progress.ts';
import { hookEnvironmentVariables, LIFECYCLE_ORDER, type LifecycleStage, type WorktreeEnvironment } from './protocol.ts';

export interface HookRunOptions {
  /** Passed as the hook's first argument (backup and restore bundle directory). */
  argument?: string;
  cancelSignal?: AbortSignal;
  timeoutMs?: number;
  /** Where the hook's output is streamed: the terminal as-is by default, or this process's stderr. */
  printOutput?: 'stderr';
}

export interface HookContext {
  /** The runner adds `STAGE` itself; callers never set it. */
  environment: WorktreeEnvironment;
  hooks: HooksConfig;
  logger: Logger;
  runner: CommandRunner;
}

/**
 * Runs one hook. The command is resolved relative to the project root, runs
 * with the project root as its working directory, sees the hook environment on
 * top of the parent's, and has its output streamed to the terminal. Any
 * non-zero exit is a failure attributed to the stage — and so, distinctly, is
 * a hook that never ran at all or was ended by a signal.
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
    let result: RunResult;
    try {
      result = await context.runner.run(resolved, args, {
        cwd: context.environment.ROOT,
        env: { ...process.env, ...hookEnvironmentVariables({ ...context.environment, STAGE: stage }) },
        printOutput: options.printOutput ?? true,
        reject: false,
        cancelSignal: options.cancelSignal,
        timeoutMs: options.timeoutMs
      });
    } catch (error) {
      if (error instanceof CommandStartError) {
        throw new CliError(`The ${stage} hook could not be started: \`${resolved}\` ${error.detail}`, error.exitCode);
      }
      throw error;
    }
    if (result.timedOut) {
      // 124 is what `timeout(1)` reports; the hook's own exit, if any, was not its verdict.
      throw new CliError(`The ${stage} hook timed out after ${options.timeoutMs} ms and was terminated.`, 124);
    }
    if (result.outputOverflow) {
      throw new CliError(`The ${stage} hook produced more output than can be captured and was stopped.`, 1);
    }
    if (result.signal !== undefined) {
      throw new CliError(`The ${stage} hook was terminated by ${result.signal}.`, result.exitCode);
    }
    if (result.exitCode !== 0) {
      throw new CliError(`The ${stage} hook failed (exit ${result.exitCode}).`, result.exitCode);
    }
  });
  return true;
}

export interface LifecycleOptions extends Pick<HookRunOptions, 'cancelSignal' | 'timeoutMs'> {
  /** Which lifecycle stages to run; always in lifecycle order regardless of how they are listed. */
  stages?: readonly LifecycleStage[];
}

/**
 * Runs prepare → seed → verify in order, stopping at the first failure.
 * Returns the stages that actually ran (unconfigured stages are skipped).
 * A subset of stages still runs in lifecycle order: `seed` cannot precede
 * `prepare`. `up` is the only caller today, and asks for `prepare` and `seed`;
 * `verify` is ordered here, but only `doctor` runs it, on its own.
 */
export async function runLifecycle(
  context: HookContext,
  options: LifecycleOptions = {}
): Promise<HookStage[]> {
  const wanted = new Set(options.stages ?? LIFECYCLE_ORDER);
  const ran: HookStage[] = [];
  for (const stage of LIFECYCLE_ORDER) {
    if (!wanted.has(stage)) continue;
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
