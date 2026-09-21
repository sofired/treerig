/**
 * An error the CLI should report to the user and exit with, rather than a
 * crash. Carries the process exit code the caller should use.
 */
export class CliError extends Error {
  readonly exitCode: number;

  constructor(message: string, exitCode = 1) {
    super(message);
    this.name = 'CliError';
    this.exitCode = exitCode;
  }
}

export class OperationCancelledError extends CliError {
  readonly signal: NodeJS.Signals;

  constructor(signal: NodeJS.Signals) {
    super(`Operation cancelled by ${signal}.`);
    this.name = 'OperationCancelledError';
    this.signal = signal;
  }
}

export function signalAbortError(signal: AbortSignal | undefined): Error | undefined {
  if (!signal?.aborted) return undefined;
  if (signal.reason instanceof Error) return signal.reason;
  return new Error('The operation was aborted.', { cause: signal.reason });
}

export function invariant(condition: unknown, message: string): asserts condition {
  if (!condition) {
    throw new CliError(message);
  }
}
