import { beforeEach, describe, expect, it, vi } from 'vitest';
import { OperationCancelledError } from '../../src/errors.ts';
import { ExecaRunner } from '../../src/process-runner.ts';

const execa = vi.hoisted(() => vi.fn());

vi.mock('execa', () => ({ execa }));

describe('ExecaRunner output', () => {
  beforeEach(() => execa.mockReset());

  it('tees requested output to the terminal while retaining a captured copy', async () => {
    execa.mockResolvedValue({ stdout: '', stderr: 'compose progress', exitCode: 0 });
    await new ExecaRunner().run('docker', ['compose', 'up'], { printOutput: true });

    expect(execa).toHaveBeenCalledWith(
      'docker',
      ['compose', 'up'],
      expect.objectContaining({
        stdout: ['inherit', 'pipe'],
        stderr: ['inherit', 'pipe']
      })
    );
  });

  it('returns captured output from a failed streamed command', async () => {
    execa.mockResolvedValue({ stdout: '', stderr: 'command failed', exitCode: 1 });
    const result = await new ExecaRunner().run('docker', ['compose', 'up'], {
      printOutput: true,
      reject: false
    });

    expect(result).toMatchObject({ stderr: 'command failed', exitCode: 1 });
  });

  it('keeps interactive commands in the terminal process group', async () => {
    execa.mockResolvedValue({ stdout: '', stderr: '', exitCode: 0 });

    await new ExecaRunner().run('less', ['README.md'], { interactive: true });

    expect(execa).toHaveBeenCalledWith(
      'less',
      ['README.md'],
      expect.objectContaining({ killDescendants: false })
    );
  });

  it('passes command deadlines to Execa and reports timeouts distinctly', async () => {
    execa.mockResolvedValue({ stdout: '', stderr: '', exitCode: undefined, timedOut: true });

    await expect(new ExecaRunner().run('docker', ['compose', 'exec'], {
      timeoutMs: 15_000
    })).rejects.toThrow('timed out after 15000 ms');
    expect(execa).toHaveBeenCalledWith(
      'docker',
      ['compose', 'exec'],
      expect.objectContaining({ timeout: 15_000 })
    );
  });

  it('forwards shared cancellation and force-kill budgets to Execa', async () => {
    execa.mockResolvedValue({ stdout: '', stderr: '', exitCode: 0 });
    const controller = new AbortController();

    await new ExecaRunner().run('docker', ['compose', 'exec'], {
      cancelSignal: controller.signal,
      forceKillAfterDelayMs: 500
    });

    expect(execa).toHaveBeenCalledWith(
      'docker',
      ['compose', 'exec'],
      expect.objectContaining({
        cancelSignal: controller.signal,
        forceKillAfterDelay: 500
      })
    );
  });

  it('does not start a subprocess with an already aborted command signal', async () => {
    const controller = new AbortController();
    const cancellationError = new Error('command deadline expired');
    controller.abort(cancellationError);

    await expect(new ExecaRunner().run('docker', ['compose', 'exec'], {
      cancelSignal: controller.signal,
      reject: false
    })).rejects.toBe(cancellationError);

    expect(execa).not.toHaveBeenCalled();
  });

  it('preserves command cancellation after an active subprocess settles', async () => {
    let settle: ((value: unknown) => void) | undefined;
    const child = Object.assign(new Promise((resolve) => {
      settle = resolve;
    }), { kill: vi.fn() });
    execa.mockReturnValue(child);
    const controller = new AbortController();
    const cancellationError = new Error('command deadline expired');
    const running = new ExecaRunner().run('docker', ['compose', 'exec'], {
      cancelSignal: controller.signal,
      reject: false
    });
    const assertion = expect(running).rejects.toBe(cancellationError);

    controller.abort(cancellationError);
    settle?.({ stdout: '', stderr: '', exitCode: 1 });

    await assertion;
  });

  it('uses bounded SIGTERM and reaps every active subprocess before cancellation completes', async () => {
    let settle: ((value: unknown) => void) | undefined;
    const child = Object.assign(new Promise((resolve) => {
      settle = resolve;
    }), {
      kill: vi.fn(() => {
        settle?.({ stdout: '', stderr: '', exitCode: 130 });
        return true;
      })
    });
    execa.mockReturnValue(child);
    const runner = new ExecaRunner();
    const running = runner.run('make', ['build'], { reject: false });
    const assertion = expect(running).rejects.toBeInstanceOf(OperationCancelledError);

    await runner.cancelActive('SIGINT');
    await assertion;

    expect(child.kill).toHaveBeenCalledWith('SIGTERM');
    expect(runner.cancellationSignal.reason).toMatchObject({ signal: 'SIGINT' });
    expect(execa).toHaveBeenCalledWith(
      'make',
      ['build'],
      expect.objectContaining({
        forceKillAfterDelay: 10_000,
        killSignal: 'SIGTERM',
        killDescendants: true
      })
    );
  });

  it('does not start subprocesses after cancellation was requested', async () => {
    const runner = new ExecaRunner();

    await runner.cancelActive('SIGINT');
    await expect(runner.run('docker', ['compose', 'up'], { reject: false })).rejects
      .toBeInstanceOf(OperationCancelledError);

    expect(execa).not.toHaveBeenCalled();
  });
});
