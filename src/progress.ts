import type { Logger } from './logger.ts';

const PROGRESS_HEARTBEAT_MS = 10_000;

function elapsed(started: number): string {
  const totalSeconds = Math.max(1, Math.floor((Date.now() - started) / 1_000));
  if (totalSeconds < 60) return `${totalSeconds}s`;
  return `${Math.floor(totalSeconds / 60)}m ${String(totalSeconds % 60).padStart(2, '0')}s`;
}

/** Logs a phase and emits an elapsed-time heartbeat while it runs. */
export async function withProgress<T>(
  logger: Logger,
  phase: string,
  action: () => Promise<T>
): Promise<T> {
  logger.info(`${phase}.`);
  const started = Date.now();
  const heartbeat = setInterval(() => {
    logger.info(`${phase} (${elapsed(started)} elapsed).`);
  }, PROGRESS_HEARTBEAT_MS);
  heartbeat.unref();
  try {
    return await action();
  } finally {
    clearInterval(heartbeat);
  }
}
