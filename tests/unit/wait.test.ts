import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Deadline, DeadlineExceededError, settleTogether, waitFor } from '../../src/wait.ts';

const execFileAsync = promisify(execFile);

afterEach(() => {
  vi.useRealTimers();
});

describe('bounded retry', () => {
  it('waits for sibling operations before propagating the first failure', async () => {
    const firstError = new Error('first probe failed');
    let releaseSibling: (() => void) | undefined;
    const sibling = new Promise<void>((resolve) => {
      releaseSibling = resolve;
    });
    const combined = settleTogether([Promise.reject(firstError), sibling] as const);
    let settled = false;
    void combined.then(
      () => { settled = true; },
      () => { settled = true; }
    );
    const assertion = expect(combined).rejects.toBe(firstError);

    await Promise.resolve();
    await Promise.resolve();
    expect(settled).toBe(false);

    releaseSibling?.();
    await assertion;
    expect(settled).toBe(true);
  });

  it('does not begin an operation after its deadline', async () => {
    const deadline = new Deadline(0);
    const operation = vi.fn().mockResolvedValue(undefined);
    try {
      await expect(waitFor(operation, {
        attempts: 1,
        deadline,
        delayMs: 0
      })).rejects.toBeInstanceOf(DeadlineExceededError);
      expect(operation).not.toHaveBeenCalled();
    } finally {
      deadline.close();
    }
  });

  it('rejects a successful operation that completes after the deadline', async () => {
    vi.useFakeTimers();
    const deadline = new Deadline(10);
    try {
      const waiting = waitFor(async () => {
        await new Promise((resolve) => setTimeout(resolve, 20));
        return 'late success';
      }, {
        attempts: 1,
        deadline,
        delayMs: 0
      });
      const assertion = expect(waiting).rejects.toBeInstanceOf(DeadlineExceededError);
      await vi.advanceTimersByTimeAsync(20);
      await assertion;
    } finally {
      deadline.close();
    }
  });

  it('cancels a retry delay when the deadline expires', async () => {
    vi.useFakeTimers();
    const deadline = new Deadline(10);
    const operation = vi.fn().mockRejectedValue(new Error('pending'));
    try {
      const waiting = waitFor(operation, {
        attempts: 5,
        deadline,
        delayMs: 1_000,
        shouldRetry: () => true
      });
      const assertion = expect(waiting).rejects.toBeInstanceOf(DeadlineExceededError);
      await vi.advanceTimersByTimeAsync(10);
      await assertion;
      expect(operation).toHaveBeenCalledOnce();
    } finally {
      deadline.close();
    }
  });

  it('propagates external cancellation while waiting between attempts', async () => {
    vi.useFakeTimers();
    const cancellation = new AbortController();
    const cancellationError = new Error('cancel readiness');
    const deadline = new Deadline(10_000, cancellation.signal);
    const operation = vi.fn().mockRejectedValue(new Error('pending'));
    try {
      const waiting = waitFor(operation, {
        attempts: 5,
        deadline,
        delayMs: 1_000
      });
      const assertion = expect(waiting).rejects.toBe(cancellationError);
      await vi.advanceTimersByTimeAsync(0);
      expect(operation).toHaveBeenCalledOnce();

      cancellation.abort(cancellationError);

      await assertion;
      expect(operation).toHaveBeenCalledOnce();
    } finally {
      deadline.close();
    }
  });

  it('keeps the process alive while waiting between retry attempts', async () => {
    const waitModule = new URL('../../src/wait.ts', import.meta.url).href;
    const script = `
      import { Deadline, waitFor } from ${JSON.stringify(waitModule)};

      const deadline = new Deadline(1_000);
      let attempts = 0;
      try {
        const result = await waitFor(async () => {
          attempts += 1;
          if (attempts === 1) throw new Error('pending');
          return 'ready';
        }, {
          attempts: 2,
          deadline,
          delayMs: 50
        });
        process.stdout.write(result);
      } finally {
        deadline.close();
      }
    `;

    const { stdout } = await execFileAsync(process.execPath, [
      '--input-type=module',
      '--eval',
      script
    ], {
      encoding: 'utf8'
    });

    expect(stdout).toBe('ready');
  });
});
