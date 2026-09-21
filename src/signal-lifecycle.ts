export interface SignalSource {
  on(signal: NodeJS.Signals, listener: () => void): unknown;
  removeListener(signal: NodeJS.Signals, listener: () => void): unknown;
}

export interface DeferredSignalOptions {
  /** Cancel in-flight work. Called at most once, for the first signal. */
  cancel: (signal: NodeJS.Signals) => Promise<void>;
  /** Runs only when the action and cancellation both succeeded. */
  finalize?: () => Promise<void>;
  /** Escape hatch when quiescence cannot be proven; must not return. */
  forceExit: (error: unknown) => never;
  /** Prove that nothing owned by this operation is still mutating state. */
  postActionQuiesce: () => Promise<void>;
  /** Release whatever lock the action was holding. */
  release: () => Promise<void>;
  /** Re-raise the original signal now that handlers are removed. */
  reenact: (signal: NodeJS.Signals) => void;
  source: SignalSource;
}

const DEFERRED_SIGNALS = ['SIGINT', 'SIGTERM'] as const;

/**
 * Runs an action while deferring SIGINT/SIGTERM. The first signal cancels the
 * action; the process then waits for the action to settle, proves it is
 * quiescent, optionally finalizes, releases its lock, removes these handlers,
 * and only then re-raises the original signal. Repeated signals are ignored
 * until that sequence completes — a held lock is never dropped mid-mutation.
 */
export async function runWithDeferredSignals<T>(
  action: () => Promise<T>,
  options: DeferredSignalOptions
): Promise<T> {
  let pendingSignal: NodeJS.Signals | undefined;
  let cancellation = Promise.resolve();
  const handlers = new Map<NodeJS.Signals, () => void>();

  for (const signal of DEFERRED_SIGNALS) {
    const handler = (): void => {
      if (pendingSignal) return;
      pendingSignal = signal;
      cancellation = Promise.resolve().then(() => options.cancel(signal));
    };
    handlers.set(signal, handler);
    options.source.on(signal, handler);
  }

  let actionError: unknown;
  let result: T | undefined;
  try {
    result = await action();
  } catch (error) {
    actionError = error;
  }

  let cancellationError: unknown;
  try {
    await cancellation;
  } catch (error) {
    cancellationError = error;
  }

  if (pendingSignal) {
    try {
      await options.postActionQuiesce();
    } catch (error) {
      options.forceExit(error);
    }
  }

  let finalizationError: unknown;
  if (actionError === undefined && cancellationError === undefined) {
    try {
      await options.finalize?.();
    } catch (error) {
      finalizationError = error;
    }
  }

  let releaseError: unknown;
  try {
    await options.release();
  } catch (error) {
    releaseError = error;
  }
  for (const [signal, handler] of handlers) {
    options.source.removeListener(signal, handler);
  }
  if (pendingSignal) options.reenact(pendingSignal);
  // The action's own error is the one the user acted on — a hook's exit code,
  // say — so it wins. Every other failure rides along on it (mutating the
  // caller's error object): as `cause` when it has none, and always in a
  // `suppressed` list, so a lock that could not be released is never silent.
  const errors = [actionError, cancellationError, finalizationError, releaseError].filter((error) => error !== undefined);
  const [primary, ...suppressed] = errors;
  if (primary !== undefined) {
    const thrown = primary instanceof Error ? primary : new Error('Deferred signal handling failed.', { cause: primary });
    if (suppressed.length > 0) {
      if (thrown.cause === undefined) thrown.cause = suppressed[0];
      const existing = (thrown as { suppressed?: unknown[] }).suppressed ?? [];
      Object.assign(thrown, { suppressed: [...existing, ...suppressed] });
    }
    throw thrown;
  }
  return result as T;
}
