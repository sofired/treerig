import pRetry from 'p-retry';
import { signalAbortError } from './errors.ts';

export class DeadlineExceededError extends Error {
  constructor() {
    super('The operation deadline expired.');
    this.name = 'DeadlineExceededError';
  }
}

/**
 * A wall-clock budget that can be shared across several operations and
 * combined with an external cancellation signal. Callers must `close()` it when
 * finished so the internal timer does not keep the process alive.
 */
export class Deadline {
  readonly signal: AbortSignal;
  private readonly controller = new AbortController();
  private readonly expiresAt: number;
  private readonly timer: NodeJS.Timeout;

  constructor(timeoutMs: number, externalSignal?: AbortSignal) {
    this.expiresAt = performance.now() + Math.max(0, timeoutMs);
    this.signal = externalSignal
      ? AbortSignal.any([this.controller.signal, externalSignal])
      : this.controller.signal;
    this.timer = setTimeout(() => {
      this.controller.abort(new DeadlineExceededError());
    }, Math.max(0, timeoutMs));
  }

  close(): void {
    clearTimeout(this.timer);
  }

  expired(): boolean {
    return this.signal.aborted || performance.now() >= this.expiresAt;
  }

  assertOpen(): void {
    const abortError = signalAbortError(this.signal);
    if (abortError) throw abortError;
    if (performance.now() >= this.expiresAt) throw new DeadlineExceededError();
  }

  remainingMs(maximum = Number.POSITIVE_INFINITY): number {
    return Math.max(0, Math.min(maximum, Math.ceil(this.expiresAt - performance.now())));
  }

  timeoutMs(maximum: number): number {
    this.assertOpen();
    return Math.max(1, this.remainingMs(maximum));
  }

  boundedSignal(maximum: number): AbortSignal {
    return AbortSignal.any([this.signal, AbortSignal.timeout(this.timeoutMs(maximum))]);
  }
}

/**
 * Like `Promise.all`, but waits for every sibling to settle before rejecting.
 * Callers that close a shared deadline in `finally` would otherwise abort work
 * that was still running.
 */
export async function settleTogether<T extends readonly unknown[] | []>(
  operations: T
): Promise<{ -readonly [P in keyof T]: Awaited<T[P]> }> {
  try {
    return await Promise.all(operations);
  } catch (error) {
    // Preserve the first failure, but let sibling work settle before callers close their shared deadline.
    await Promise.allSettled(operations);
    throw error;
  }
}

export interface WaitOptions {
  attempts: number;
  deadline: Deadline;
  delayMs: number;
  shouldRetry?: (error: Error) => boolean;
}

export async function waitFor<T>(
  operation: (deadline: Deadline) => Promise<T>,
  options: WaitOptions
): Promise<T> {
  return pRetry(async () => {
    options.deadline.assertOpen();
    const result = await operation(options.deadline);
    options.deadline.assertOpen();
    return result;
  }, {
    retries: Math.max(0, options.attempts - 1),
    factor: 1,
    minTimeout: options.delayMs,
    maxTimeout: options.delayMs,
    maxRetryTime: options.deadline.remainingMs(),
    randomize: false,
    shouldRetry: ({ error }) => options.shouldRetry?.(error) ?? true,
    signal: options.deadline.signal
  });
}
