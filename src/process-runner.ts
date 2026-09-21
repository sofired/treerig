import { access } from 'node:fs/promises';
import path from 'node:path';
import { execa, type ResultPromise } from 'execa';
import { CliError, OperationCancelledError, signalAbortError } from './errors.ts';

export interface RunOptions {
  cancelSignal?: AbortSignal;
  cwd?: string;
  env?: NodeJS.ProcessEnv;
  input?: string | Uint8Array;
  /** Throw on a non-zero exit. Defaults to true. */
  reject?: boolean;
  /** Tee stdout/stderr to the terminal while still capturing them. */
  printOutput?: boolean;
  /** Hand the terminal to the child; nothing is captured. */
  interactive?: boolean;
  forceKillAfterDelayMs?: number;
  timeoutMs?: number;
}

export interface RunResult {
  stdout: string;
  stderr: string;
  exitCode: number;
  timedOut?: boolean;
}

/**
 * The seam every subprocess goes through. Injecting it is what makes the rest
 * of the codebase testable without spawning anything.
 */
export interface CommandRunner {
  readonly cancellationSignal?: AbortSignal;
  cancelActive?(signal: NodeJS.Signals): Promise<void>;
  run(command: string, args?: readonly string[], options?: RunOptions): Promise<RunResult>;
  which(command: string): Promise<boolean>;
}

export class ExecaRunner implements CommandRunner {
  readonly cancellationSignal: AbortSignal;
  private readonly active = new Set<ResultPromise>();
  private readonly cancellation = new AbortController();
  private cancellationError: OperationCancelledError | undefined;

  constructor() {
    this.cancellationSignal = this.cancellation.signal;
  }

  private operationCancellation(signal: AbortSignal | undefined): Error | undefined {
    return this.cancellationError ?? signalAbortError(signal);
  }

  /** SIGTERM every live child and reap them all before returning. */
  async cancelActive(signal: NodeJS.Signals): Promise<void> {
    if (!this.cancellationError) {
      this.cancellationError = new OperationCancelledError(signal);
      this.cancellation.abort(this.cancellationError);
    }
    const active = [...this.active];
    for (const child of active) {
      try {
        child.kill('SIGTERM');
      } catch {
        // Continue signalling and reaping the remaining owned subprocesses.
      }
    }
    await Promise.allSettled(active);
  }

  async run(
    command: string,
    args: readonly string[] = [],
    options: RunOptions = {}
  ): Promise<RunResult> {
    const initialCancellation = this.operationCancellation(options.cancelSignal);
    if (initialCancellation) throw initialCancellation;
    const execaOptions = {
      cwd: options.cwd,
      env: options.env,
      reject: false,
      all: false,
      cancelSignal: options.cancelSignal,
      timeout: options.timeoutMs,
      forceKillAfterDelay: options.forceKillAfterDelayMs ?? 10_000,
      killSignal: 'SIGTERM' as const,
      killDescendants: !options.interactive,
      ...(options.interactive ? { stdio: 'inherit' as const } : {}),
      ...(!options.interactive && options.printOutput ? {
        stdout: ['inherit', 'pipe'] as const,
        stderr: ['inherit', 'pipe'] as const
      } : {}),
      ...(options.input === undefined
        ? options.interactive ? {} : { stdin: 'ignore' as const }
        : { input: options.input })
    };
    const child = execa(command, [...args], execaOptions);
    this.active.add(child);
    let result: Awaited<ResultPromise>;
    try {
      result = await child;
    } catch (error) {
      const cancellationError = this.operationCancellation(options.cancelSignal);
      if (cancellationError) throw cancellationError;
      if (error instanceof Error) throw error;
      throw new Error(String(error), { cause: error });
    } finally {
      this.active.delete(child);
    }
    const cancellationError = this.operationCancellation(options.cancelSignal);
    if (cancellationError) throw cancellationError;
    const stdout = typeof result.stdout === 'string' ? result.stdout : '';
    const stderr = typeof result.stderr === 'string' ? result.stderr : '';
    const timedOut = result.timedOut === true;
    if (result.exitCode !== 0 && options.reject !== false) {
      const detail = stderr.trim() || stdout.trim();
      throw new CliError(
        `Command failed (${command} ${args.join(' ')}): ${timedOut
          ? `timed out after ${options.timeoutMs} ms`
          : detail || `exit ${result.exitCode}`}`
      );
    }
    return { stdout, stderr, exitCode: result.exitCode ?? 1, timedOut };
  }

  async which(command: string): Promise<boolean> {
    const candidates = (process.env.PATH ?? '').split(path.delimiter).map((directory) =>
      path.join(directory, command)
    );
    for (const candidate of candidates) {
      try {
        await access(candidate);
        return true;
      } catch {
        // Keep looking along PATH.
      }
    }
    return false;
  }
}
