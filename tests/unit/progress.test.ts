import { afterEach, describe, expect, it, vi } from 'vitest';
import { silentLogger } from '../../src/logger.ts';
import { withProgress } from '../../src/progress.ts';

afterEach(() => {
  vi.useRealTimers();
});

describe('withProgress', () => {
  it('reports elapsed time while a phase is still running and stops after completion', async () => {
    vi.useFakeTimers();
    const messages: string[] = [];
    let complete: ((value: string) => void) | undefined;
    const operation = withProgress(
      { ...silentLogger, info(message) { messages.push(message); } },
      'Preparing services',
      () => new Promise<string>((resolve) => { complete = resolve; })
    );

    await vi.advanceTimersByTimeAsync(20_000);
    expect(messages).toEqual([
      'Preparing services.',
      'Preparing services (10s elapsed).',
      'Preparing services (20s elapsed).'
    ]);

    complete?.('ready');
    await expect(operation).resolves.toBe('ready');
    await vi.advanceTimersByTimeAsync(10_000);
    expect(messages).toHaveLength(3);
  });
});
