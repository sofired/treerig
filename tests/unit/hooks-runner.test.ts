import { describe, expect, it, vi } from 'vitest';
import { CliError } from '../../src/errors.ts';
import { runHook, runLifecycle, splitCommand, type HookContext } from '../../src/hooks/runner.ts';
import type { HookEnvironment } from '../../src/hooks/protocol.ts';
import { silentLogger } from '../../src/logger.ts';
import type { CommandRunner, RunOptions, RunResult } from '../../src/process-runner.ts';

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
