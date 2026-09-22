import { beforeEach, describe, expect, it, vi } from 'vitest';
import { OperationCancelledError } from '../../src/errors.ts';
import { CommandStartError, ExecaRunner } from '../../src/process-runner.ts';

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
    // Execa kills a timed-out child itself; that signal is not the child's story.
    execa.mockResolvedValue({ stdout: '', stderr: '', exitCode: undefined, timedOut: true, isTerminated: true, signal: 'SIGTERM' });

    await expect(new ExecaRunner().run('docker', ['compose', 'exec'], {
      timeoutMs: 15_000
    })).rejects.toThrow('timed out after 15000 ms');
    await expect(new ExecaRunner().run('docker', ['compose', 'exec'], {
      timeoutMs: 15_000, reject: false
    })).resolves.toEqual({ stdout: '', stderr: '', exitCode: 1, timedOut: true });
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

  it('diagnoses a command that never started instead of inventing an exit code', async () => {
    execa.mockResolvedValue({
      stdout: '', stderr: '', exitCode: undefined, failed: true, code: 'ENOENT',
      cause: Object.assign(new Error('spawn missing-tool ENOENT'), { code: 'ENOENT' })
    });

    const attempt = new ExecaRunner().run('missing-tool', ['--version'], { reject: false });
    await expect(attempt).rejects.toBeInstanceOf(CommandStartError);
    await expect(attempt).rejects.toMatchObject({
      message: '`missing-tool` could not be started: it is not on PATH (ENOENT).',
      exitCode: 127,
      code: 'ENOENT'
    });
  });

  it('names the file, not PATH, when a command given by path cannot be started', async () => {
    execa.mockResolvedValue({ stdout: '', stderr: '', exitCode: undefined, failed: true, code: 'EACCES' });

    await expect(new ExecaRunner().run('/work/acme/hooks/prepare', [], { reject: false })).rejects.toMatchObject({
      message: '`/work/acme/hooks/prepare` could not be started: it is not executable (EACCES).',
      exitCode: 126
    });
  });

  it('tells a script without a #! line what it is missing', async () => {
    execa.mockResolvedValue({ stdout: '', stderr: '', exitCode: undefined, failed: true, code: 'ENOEXEC' });

    await expect(new ExecaRunner().run('/work/acme/hooks/seed', [], { reject: false })).rejects.toMatchObject({
      message: '`/work/acme/hooks/seed` could not be started: it is not a valid executable (ENOEXEC) — a script needs a #! line.',
      exitCode: 126
    });
  });

  it('blames the working directory, not PATH, when that is what is missing', async () => {
    // Execa's message is three lines; only the first says what is wrong.
    execa.mockResolvedValue({
      stdout: '', stderr: '', exitCode: undefined, failed: true, code: 'ENOENT',
      originalMessage: 'The "cwd" option is invalid: /work/gone.\nENOENT: no such file or directory, stat \'/work/gone\'\nspawn make ENOENT',
      cause: Object.assign(new Error('spawn make ENOENT'), { code: 'ENOENT' })
    });

    await expect(new ExecaRunner().run('make', [], { cwd: '/work/gone', reject: false })).rejects.toMatchObject({
      message: '`make` could not be started: it has no working directory: /work/gone does not exist (ENOENT).',
      exitCode: 126
    });
  });

  it('explains a working directory that is a file the same way', async () => {
    execa.mockResolvedValue({
      stdout: '', stderr: '', exitCode: undefined, failed: true, code: 'ENOTDIR',
      originalMessage: 'The "cwd" option is not a directory: /work/acme/berth.toml.\nspawn make ENOTDIR',
      cause: Object.assign(new Error('spawn make ENOTDIR'), { code: 'ENOTDIR' })
    });

    await expect(new ExecaRunner().run('make', [], { cwd: '/work/acme/berth.toml', reject: false })).rejects.toMatchObject({
      message: '`make` could not be started: it has no working directory: /work/acme/berth.toml is not a directory (ENOTDIR).'
    });
  });

  it('falls back to the error code alone for a spawn failure it has no words for', async () => {
    execa.mockResolvedValue({ stdout: '', stderr: '', exitCode: undefined, failed: true, code: 'EPERM', cause: Object.assign(new Error('spawn make EPERM'), { code: 'EPERM' }) });

    await expect(new ExecaRunner().run('make', [], { reject: false })).rejects.toMatchObject({
      message: '`make` could not be started: it failed to spawn (EPERM).',
      exitCode: 126
    });
  });

  it('reports a child stopped for overflowing the output buffer as such, not as a signal', async () => {
    execa.mockResolvedValue({ stdout: '', stderr: '', exitCode: undefined, failed: true, isMaxBuffer: true, isTerminated: true, signal: 'SIGTERM' });

    await expect(new ExecaRunner().run('make', ['build'], { reject: false })).resolves.toEqual({ stdout: '', stderr: '', exitCode: 1, timedOut: false, outputOverflow: true });
    await expect(new ExecaRunner().run('make', ['build'])).rejects.toThrow('Command failed (make build): produced more than 100 MB of output and was stopped');
  });

  it('treats a timed-out child as failed even when it managed to exit zero', async () => {
    execa.mockResolvedValue({ stdout: '', stderr: '', exitCode: 0, timedOut: true, isTerminated: false });

    await expect(new ExecaRunner().run('make', [], { timeoutMs: 50 })).rejects.toMatchObject({ message: expect.stringContaining('timed out after 50 ms') as string, exitCode: 124 });
    await expect(new ExecaRunner().run('make', [], { timeoutMs: 50, reject: false })).resolves.toMatchObject({ exitCode: 0, timedOut: true });
  });

  it("reports a child ended by a signal as such, with the shell's exit code", async () => {
    execa.mockResolvedValue({ stdout: '', stderr: '', exitCode: undefined, failed: true, isTerminated: true, signal: 'SIGKILL' });

    await expect(new ExecaRunner().run('make', ['build'], { reject: false })).resolves.toMatchObject({ exitCode: 137, signal: 'SIGKILL' });
    await expect(new ExecaRunner().run('make', ['build'])).rejects.toThrow('Command failed (make build): terminated by SIGKILL');
  });

  it('does not start subprocesses after cancellation was requested', async () => {
    const runner = new ExecaRunner();

    await runner.cancelActive('SIGINT');
    await expect(runner.run('docker', ['compose', 'up'], { reject: false })).rejects
      .toBeInstanceOf(OperationCancelledError);

    expect(execa).not.toHaveBeenCalled();
  });
});
