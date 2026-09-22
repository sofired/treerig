import { describe, expect, it, vi } from 'vitest';
import { CliError } from '../../src/errors.ts';
import { runHook, runLifecycle, splitCommand, type HookContext } from '../../src/hooks/runner.ts';
import type { HookEnvironment } from '../../src/hooks/protocol.ts';
import { silentLogger } from '../../src/logger.ts';
import { CommandStartError, type CommandRunner, type RunOptions, type RunResult } from '../../src/process-runner.ts';

const environment: HookEnvironment = {
  STAGE: 'prepare',
  PROJECT: 'acme',
  REPOSITORY_ID: 'repo-a',
  WORKTREE_ID: 'wt-1',
  SLUG: 'feature-login',
  HOST: 'feature-login.acme.localhost',
  WORKTREE_HOME: '/control/repositories/repo-a/instances/wt-1',
  CONFIG: '/work/acme/berth.toml',
  ROOT: '/work/acme'
};

interface Call { command: string; args: readonly string[]; options: RunOptions }

class RecordingRunner implements CommandRunner {
  readonly calls: Call[] = [];
  private readonly exitCodes: Record<string, number>;

  constructor(exitCodes: Record<string, number> = {}) {
    this.exitCodes = exitCodes;
  }

  run(command: string, args: readonly string[] = [], options: RunOptions = {}): Promise<RunResult> {
    this.calls.push({ command, args, options });
    const exitCode = this.exitCodes[command] ?? 0;
    return Promise.resolve({ stdout: '', stderr: '', exitCode });
  }

  which(): Promise<boolean> {
    return Promise.resolve(true);
  }
}

/** A runner whose every command never runs (spawn failure) or is ended by a signal. */
type Outcome = 'ENOENT' | 'EACCES' | 'SIGKILL' | 'TIMEOUT';

class BrokenRunner implements CommandRunner {
  private readonly outcome: Outcome;

  constructor(outcome: Outcome) {
    this.outcome = outcome;
  }

  run(command: string): Promise<RunResult> {
    if (this.outcome === 'SIGKILL') return Promise.resolve({ stdout: '', stderr: '', exitCode: 137, signal: 'SIGKILL' });
    if (this.outcome === 'TIMEOUT') return Promise.resolve({ stdout: '', stderr: '', exitCode: 0, timedOut: true });
    return Promise.reject(new CommandStartError(command, this.outcome, `spawn ${command} ${this.outcome}`));
  }

  which(): Promise<boolean> {
    return Promise.resolve(false);
  }
}

function context(runner: CommandRunner, hooks: HookContext['hooks']): HookContext {
  return { environment, hooks, logger: silentLogger, runner };
}

describe('runHook', () => {
  it('resolves a relative command against the project root and passes the stage environment', async () => {
    const runner = new RecordingRunner();
    const ran = await runHook('prepare', context(runner, { prepare: './scripts/migrate.sh --fast' }));

    expect(ran).toBe(true);
    const [call] = runner.calls;
    expect(call?.command).toBe('/work/acme/scripts/migrate.sh');
    expect(call?.args).toEqual(['--fast']);
    expect(call?.options.cwd).toBe('/work/acme');
    expect(call?.options.env).toMatchObject({
      BERTH_STAGE: 'prepare',
      BERTH_SLUG: 'feature-login',
      BERTH_HOST: 'feature-login.acme.localhost',
      BERTH_WORKTREE_HOME: '/control/repositories/repo-a/instances/wt-1',
      BERTH_ROOT: '/work/acme'
    });
  });

  it("passes the user's control-home override through untouched, so a hook can call the tool", async () => {
    const previous = process.env.BERTH_HOME;
    process.env.BERTH_HOME = '/custom/control-home';
    try {
      const runner = new RecordingRunner();
      await runHook('prepare', context(runner, { prepare: './run.sh' }));
      expect(runner.calls[0]?.options.env?.BERTH_HOME).toBe('/custom/control-home');
    } finally {
      if (previous === undefined) delete process.env.BERTH_HOME;
      else process.env.BERTH_HOME = previous;
    }
  });

  it('leaves bare command names to PATH resolution and appends the bundle argument', async () => {
    const runner = new RecordingRunner();
    await runHook('backup', context(runner, { backup: 'pg_dump' }), { argument: '/bundles/one' });

    expect(runner.calls[0]?.command).toBe('pg_dump');
    expect(runner.calls[0]?.args).toEqual(['/bundles/one']);
  });

  it('returns false without spawning when the stage is not configured', async () => {
    const runner = new RecordingRunner();
    expect(await runHook('seed', context(runner, {}))).toBe(false);
    expect(runner.calls).toEqual([]);
  });

  it('attributes a non-zero exit to the stage and preserves the exit code', async () => {
    const runner = new RecordingRunner({ '/work/acme/seed.py': 3 });
    const failure = runHook('seed', context(runner, { seed: './seed.py' }));

    await expect(failure).rejects.toBeInstanceOf(CliError);
    await expect(failure).rejects.toMatchObject({ exitCode: 3 });
    await expect(failure).rejects.toThrow('The seed hook failed (exit 3)');
  });
});

describe('runHook when the hook never runs', () => {
  it('names the resolved path of a hook that does not exist, with the shell\'s exit code', async () => {
    const attempt = runHook('prepare', context(new BrokenRunner('ENOENT'), { prepare: './scripts/missing.sh' }));
    await expect(attempt).rejects.toBeInstanceOf(CliError);
    await expect(attempt).rejects.toMatchObject({
      message: 'The prepare hook could not be started: `/work/acme/scripts/missing.sh` does not exist — or the interpreter on its #! line is missing, as when the file has CRLF line endings (ENOENT).',
      exitCode: 127
    });
  });

  it('says when a hook is not executable, rather than reporting a failed exit', async () => {
    await expect(runHook('seed', context(new BrokenRunner('EACCES'), { seed: './scripts/seed' }))).rejects.toMatchObject({
      message: 'The seed hook could not be started: `/work/acme/scripts/seed` is not executable (EACCES).',
      exitCode: 126
    });
  });

  it('reports the signal that ended a hook', async () => {
    await expect(runHook('prepare', context(new BrokenRunner('SIGKILL'), { prepare: 'make prepare' }))).rejects.toMatchObject({
      message: 'The prepare hook was terminated by SIGKILL.',
      exitCode: 137
    });
  });

  it('reports a hook that drowned its own output', async () => {
    const runner: CommandRunner = {
      run: () => Promise.resolve({ stdout: '', stderr: '', exitCode: 1, outputOverflow: true }),
      which: () => Promise.resolve(true)
    };
    await expect(runHook('prepare', context(runner, { prepare: 'make noisy' }))).rejects.toMatchObject({
      message: 'The prepare hook produced more output than can be captured and was stopped.',
      exitCode: 1
    });
  });

  it('reports a timeout as a timeout, whatever the hook managed to exit with', async () => {
    await expect(runHook('seed', context(new BrokenRunner('TIMEOUT'), { seed: 'make seed' }), { timeoutMs: 50 })).rejects.toMatchObject({
      message: 'The seed hook timed out after 50 ms and was terminated.',
      exitCode: 124
    });
  });
});

describe('runLifecycle', () => {
  it('runs prepare, seed, and verify in order', async () => {
    const runner = new RecordingRunner();
    const ran = await runLifecycle(context(runner, {
      prepare: 'a', seed: 'b', verify: 'c'
    }));

    expect(ran).toEqual(['prepare', 'seed', 'verify']);
    expect(runner.calls.map((call) => call.command)).toEqual(['a', 'b', 'c']);
  });

  it('never runs seed when prepare fails', async () => {
    const runner = new RecordingRunner({ a: 1 });
    const seed = vi.fn();

    await expect(runLifecycle(context(runner, { prepare: 'a', seed: 'b' })))
      .rejects.toThrow('The prepare hook failed');
    expect(runner.calls.map((call) => call.command)).toEqual(['a']);
    expect(seed).not.toHaveBeenCalled();
  });

  it('skips unconfigured stages without treating them as failures', async () => {
    const runner = new RecordingRunner();
    expect(await runLifecycle(context(runner, { verify: 'c' }))).toEqual(['verify']);
  });
});

describe('splitCommand', () => {
  it('splits on whitespace and keeps double-quoted segments whole', () => {
    expect(splitCommand('./run.sh --name "two words" last')).toEqual([
      './run.sh', '--name', 'two words', 'last'
    ]);
    expect(splitCommand('')).toEqual([]);
  });
});
