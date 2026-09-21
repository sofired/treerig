import { EventEmitter } from 'node:events';
import { describe, expect, it, vi } from 'vitest';
import { runWithDeferredSignals } from '../../src/signal-lifecycle.ts';

describe('runWithDeferredSignals', () => {
  it('cancels active work but retains ownership until the action settles', async () => {
    const signals = new EventEmitter();
    const events: string[] = [];
    let settle: (() => void) | undefined;
    const action = new Promise<void>((resolve) => {
      settle = resolve;
    });
    let settleCancellation: (() => void) | undefined;
    const cancellation = new Promise<void>((resolve) => {
      settleCancellation = resolve;
    });
    const cancel = vi.fn(() => {
      events.push('cancel');
      return cancellation;
    });
    const release = vi.fn(() => {
      events.push('release');
      return Promise.resolve();
    });
    const reenact = vi.fn((signal: NodeJS.Signals) => {
      events.push(`reenact:${signal}`);
    });
    const quiesce = vi.fn(() => {
      events.push('quiesce');
      return Promise.resolve();
    });
    const finalize = vi.fn(() => {
      events.push('finalize');
      return Promise.resolve();
    });

    const running = runWithDeferredSignals(async () => {
      await action;
      events.push('settled');
    }, {
      cancel,
      finalize,
      forceExit: (error): never => {
        throw error;
      },
      postActionQuiesce: quiesce,
      reenact,
      release,
      source: signals
    });

    signals.emit('SIGTERM');
    await vi.waitFor(() => expect(cancel).toHaveBeenCalledWith('SIGTERM'));
    expect(release).not.toHaveBeenCalled();

    settle?.();
    await vi.waitFor(() => expect(events).toContain('settled'));
    expect(quiesce).not.toHaveBeenCalled();
    expect(release).not.toHaveBeenCalled();
    settleCancellation?.();
    await running;

    expect(events).toEqual([
      'cancel',
      'settled',
      'quiesce',
      'finalize',
      'release',
      'reenact:SIGTERM'
    ]);
  });

  it('suppresses repeated termination signals until the protected action settles', async () => {
    const signals = new EventEmitter();
    let settle: (() => void) | undefined;
    const action = new Promise<void>((resolve) => {
      settle = resolve;
    });
    const cancel = vi.fn(() => Promise.resolve());
    const reenact = vi.fn();
    const quiesce = vi.fn(() => Promise.resolve());

    const running = runWithDeferredSignals(() => action, {
      cancel,
      forceExit: (error): never => {
        throw error;
      },
      postActionQuiesce: quiesce,
      reenact,
      release: () => Promise.resolve(),
      source: signals
    });
    signals.emit('SIGINT');
    signals.emit('SIGTERM');
    settle?.();
    await running;

    expect(cancel).toHaveBeenCalledTimes(1);
    expect(cancel).toHaveBeenCalledWith('SIGINT');
    expect(quiesce).toHaveBeenCalledTimes(1);
    expect(reenact).toHaveBeenCalledWith('SIGINT');
  });

  it('does not quiesce an ordinary successful action', async () => {
    const signals = new EventEmitter();
    const quiesce = vi.fn(() => Promise.resolve());
    const release = vi.fn(() => Promise.resolve());

    await runWithDeferredSignals(() => Promise.resolve('complete'), {
      cancel: () => Promise.resolve(),
      forceExit: (error): never => {
        throw error;
      },
      postActionQuiesce: quiesce,
      reenact: vi.fn(),
      release,
      source: signals
    });

    expect(quiesce).not.toHaveBeenCalled();
    expect(release).toHaveBeenCalledTimes(1);
  });

  it('forces exit without releasing or reenacting when quiescence fails', async () => {
    const signals = new EventEmitter();
    const events: string[] = [];
    let settle: (() => void) | undefined;
    const action = new Promise<void>((resolve) => {
      settle = resolve;
    });
    const quiescenceError = new Error('background work is ambiguous');
    const release = vi.fn(() => {
      events.push('release');
      return Promise.resolve();
    });
    const reenact = vi.fn();
    const finalize = vi.fn(() => Promise.resolve());
    const forceError = new Error('forced exit');
    const forceExit = vi.fn((error: unknown): never => {
      events.push('force-exit');
      expect(error).toBe(quiescenceError);
      throw forceError;
    });

    const running = runWithDeferredSignals(async () => {
      await action;
      events.push('settled');
    }, {
      cancel: () => {
        events.push('cancel');
        return Promise.resolve();
      },
      finalize,
      forceExit,
      postActionQuiesce: () => {
        events.push('quiesce');
        return Promise.reject(quiescenceError);
      },
      reenact,
      release,
      source: signals
    });

    signals.emit('SIGTERM');
    settle?.();

    await expect(running).rejects.toBe(forceError);
    expect(events).toEqual(['cancel', 'settled', 'quiesce', 'force-exit']);
    expect(release).not.toHaveBeenCalled();
    expect(reenact).not.toHaveBeenCalled();
    expect(finalize).not.toHaveBeenCalled();
  });
});
