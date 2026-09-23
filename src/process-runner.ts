import { access, constants, stat } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { execa, type ResultPromise } from 'execa';
import { CliError, OperationCancelledError, signalAbortError } from './errors.ts';

export interface RunOptions {
  cancelSignal?: AbortSignal;
  cwd?: string;
  /**
   * The child's entire environment. Spread `process.env` (or a filtered copy)
   * yourself; nothing is merged in. Omit it to inherit this process's.
   */
  env?: NodeJS.ProcessEnv;
  input?: string | Uint8Array;
  /** Throw on a non-zero exit. Defaults to true. */
  reject?: boolean;
  /**
   * Tee stdout/stderr to the terminal while still capturing them. `'stderr'`
   * sends both to this process's stderr, for a caller whose own stdout is a
   * report that the child's output must not interleave with.
   */
  printOutput?: boolean | 'stderr';
  /** Hand the terminal to the child; nothing is captured. */
  interactive?: boolean;
  forceKillAfterDelayMs?: number;
  timeoutMs?: number;
}

export interface RunResult {
  stdout: string;
  stderr: string;
  /**
   * The child's exit code; a child ended by a signal reports 128 plus the
   * signal number, as a shell would, and a child killed for a timeout or
   * for overflowing the output buffer reports 1 with the flag set.
   */
  exitCode: number;
  timedOut?: boolean;
  /** The child was stopped for producing more output than can be captured. */
  outputOverflow?: boolean;
  /** Set when a signal ended the child rather than an exit; a timeout is reported as `timedOut` alone. */
  signal?: NodeJS.Signals;
}

/** Execa's default `maxBuffer`, per stream. */
const OUTPUT_LIMIT_MB = 100;

/**
 * The child never ran: the executable is missing, not executable, or could not
 * be spawned. Distinct from a failed exit, which is the command's own verdict.
 * Exit codes follow the shell: 127 for "not found", 126 for "not executable".
 */
export class CommandStartError extends CliError {
  readonly command: string;
  readonly code: string | undefined;
  /** The reason as a predicate — "is not executable (EACCES)." — for callers that name the command themselves. */
  readonly detail: string;

  constructor(command: string, code: string | undefined, message: string) {
    const { detail, exitCode } = describeStartFailure(command, code, message);
    super(`\`${command}\` could not be started: it ${detail}`, exitCode);
    this.name = 'CommandStartError';
    this.command = command;
    this.code = code;
    this.detail = detail;
  }
}

/** Execa's first line when the working directory, not the command, is the problem: `The "cwd" option is invalid: /x.` */
const CWD_COMPLAINT = /^The "cwd" option is (?:invalid|not a directory): (.*?)\.?$/;

function describeStartFailure(command: string, code: string | undefined, message: string): { detail: string; exitCode: number } {
  const suffix = code === undefined ? '' : ` (${code})`;
  // Node reports a missing working directory as ENOENT on the command itself; that is not "command not found".
  const cwd = CWD_COMPLAINT.exec(message);
  if (cwd) {
    const problem = code === 'ENOTDIR' ? 'is not a directory' : 'does not exist';
    return { detail: `has no working directory: ${cwd[1]} ${problem}${suffix}.`, exitCode: 126 };
  }
  switch (code) {
    case 'ENOENT':
      return {
        detail: command.includes('/')
          ? 'does not exist — or the interpreter on its #! line is missing, as when the file has CRLF line endings (ENOENT).'
          : 'is not on PATH (ENOENT).',
        exitCode: 127
      };
    case 'EACCES':
      return { detail: 'is not executable (EACCES).', exitCode: 126 };
    case 'ENOEXEC':
      return { detail: 'is not a valid executable (ENOEXEC) — a script needs a #! line.', exitCode: 126 };
    default:
      return { detail: code === undefined ? `failed to spawn: ${message.replace(/\.$/, '')}.` : `failed to spawn${suffix}.`, exitCode: 126 };
  }
}

/**
 * What Execa reports when the process never started: no exit code, no
 * timeout, no terminating signal — only the spawn's own error. Execa's own
 * first line is kept when it explains more than Node's (a bad `cwd`).
 */
function startFailure(result: Awaited<ResultPromise>): { code: string | undefined; message: string } | undefined {
  if (result.exitCode !== undefined || result.timedOut || result.isTerminated) return undefined;
  const cause = result.cause instanceof Error ? (result.cause as NodeJS.ErrnoException) : undefined;
  const firstLine = (text: string | undefined): string => text?.split('\n', 1)[0] ?? '';
  const original = firstLine(result.originalMessage);
  return {
    code: result.code ?? cause?.code,
    message: CWD_COMPLAINT.test(original) ? original : firstLine(cause?.message ?? result.shortMessage) || 'unknown spawn failure'
  };
}

/** The error a rejecting `run` throws for a command that ran and did not succeed. */
export function commandFailure(
  command: string,
  args: readonly string[],
  result: RunResult,
  options: Pick<RunOptions, 'timeoutMs'> = {}
): CliError {
  const detail = result.stderr.trim() || result.stdout.trim();
  const reason = result.timedOut
    ? `timed out after ${options.timeoutMs} ms`
    : result.outputOverflow
      ? `produced more than ${OUTPUT_LIMIT_MB} MB of output and was stopped`
      : result.signal === undefined ? detail || `exit ${result.exitCode}` : `terminated by ${result.signal}`;
  return new CliError(`Command failed (${command} ${args.join(' ')}): ${reason}`, result.timedOut ? 124 : 1);
}

/**
 * The seam every subprocess goes through. Injecting it is what makes the rest
 * of the codebase testable without spawning anything.
 */
export interface CommandRunner {
  readonly cancellationSignal?: AbortSignal;
  cancelActive?(signal: NodeJS.Signals): Promise<void>;
  run(command: string, args?: readonly string[], options?: RunOptions): Promise<RunResult>;
  /** Whether an executable regular file by that name is on PATH. */
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
      // A caller that supplies an environment has decided what the child
      // sees; nothing is merged back in from this process.
      extendEnv: options.env === undefined,
      reject: false,
      all: false,
      cancelSignal: options.cancelSignal,
      timeout: options.timeoutMs,
      forceKillAfterDelay: options.forceKillAfterDelayMs ?? 10_000,
      killSignal: 'SIGTERM' as const,
      killDescendants: !options.interactive,
      ...(options.interactive ? { stdio: 'inherit' as const } : {}),
      ...(!options.interactive && options.printOutput === 'stderr' ? {
        stdout: [2, 'pipe'] as const,
        stderr: [2, 'pipe'] as const
      } : !options.interactive && options.printOutput ? {
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
    const failure = startFailure(result);
    if (failure) throw new CommandStartError(command, failure.code, failure.message);
    const stdout = typeof result.stdout === 'string' ? result.stdout : '';
    const stderr = typeof result.stderr === 'string' ? result.stderr : '';
    const timedOut = result.timedOut === true;
    const outputOverflow = result.isMaxBuffer === true;
    // Execa ends both of those with a signal of its own; that is not the child's story.
    const signal = !timedOut && !outputOverflow && result.isTerminated === true && typeof result.signal === 'string'
      ? result.signal
      : undefined;
    const signalNumber = signal === undefined ? undefined : os.constants.signals[signal];
    const exitCode = result.exitCode ?? (signalNumber === undefined ? 1 : 128 + signalNumber);
    const outcome: RunResult = {
      stdout, stderr, exitCode, timedOut,
      ...(outputOverflow ? { outputOverflow } : {}),
      ...(signal === undefined ? {} : { signal })
    };
    // A timed-out child that caught the signal and exited 0 did not succeed.
    if ((exitCode !== 0 || timedOut || outputOverflow) && options.reject !== false) throw commandFailure(command, args, outcome, options);
    return outcome;
  }

  async which(command: string): Promise<boolean> {
    const candidates = (process.env.PATH ?? '').split(path.delimiter).map((directory) =>
      path.join(directory, command)
    );
    for (const candidate of candidates) {
      try {
        // A directory of that name passes an X_OK check; only a file can be run.
        if (!(await stat(candidate)).isFile()) continue;
        await access(candidate, constants.X_OK);
        return true;
      } catch {
        // Keep looking along PATH.
      }
    }
    return false;
  }
}
