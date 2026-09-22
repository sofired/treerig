import { chmod, mkdir, mkdtemp, readFile, realpath, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { placeholderSnapshot } from '../../src/checkout-index/snapshot.ts';
import { createCommandContext, type CommandContext, type ProcessControl } from '../../src/commands/context.ts';
import { down, status, up, urls } from '../../src/commands/worktree.ts';
import { loadConfig } from '../../src/config/load.ts';
import { CliError, OperationCancelledError } from '../../src/errors.ts';
import { ScopedLock, worktreeLockTarget } from '../../src/operation-lock.ts';
import { ExecaRunner } from '../../src/process-runner.ts';
import { MemoryRuntime } from '../../src/runtime/memory.ts';
import type { Runtime, ServiceSpec, WorktreeRef } from '../../src/runtime/runtime.ts';
import { FakeProcess, RecordingLogger } from '../helpers/command-doubles.ts';

// The command layer end to end: a real Git repository, real hook executables,
// real locks — and an in-memory runtime in place of Docker.

const temporary: string[] = [];
afterEach(async () => {
  await Promise.all(temporary.splice(0).map((d) => rm(d, { recursive: true, force: true })));
});

const CONFIG = `
[project]
name = "acme"
domain = "acme.localhost"
[compose]
file = "compose.yaml"
[[service]]
name = "api"
[service.index]
label = "API"
[[service]]
name = "mail"
subdomain = "mail"
path = "/inbox"
[hooks]
prepare = "./hooks/record"
seed = "./hooks/record"
`;

interface World {
  base: string;
  root: string;
  controlHome: string;
  log: string;
  failFile: string;
  runner: ExecaRunner;
  context(overrides?: { runtime?: Runtime; process?: ProcessControl; runner?: ExecaRunner }): Promise<CommandContext & { logger: RecordingLogger; fakeProcess: FakeProcess }>;
}

async function world(): Promise<World> {
  const base = await realpath(await mkdtemp(path.join(tmpdir(), 'berth-commands-')));
  temporary.push(base);
  const root = path.join(base, 'repo');
  const controlHome = path.join(base, 'control');
  const log = path.join(base, 'hooks.log');
  const failFile = path.join(base, 'fail-prepare');
  const runner = new ExecaRunner();
  await runner.run('git', ['init', '--initial-branch=feature/login', root]);
  await runner.run('git', ['-C', root, '-c', 'user.name=t', '-c', 'user.email=t@example.invalid', 'commit', '--allow-empty', '-m', 'initial']);
  await writeFile(path.join(root, 'berth.toml'), CONFIG);
  await mkdir(path.join(root, 'hooks'));
  const hook = path.join(root, 'hooks', 'record');
  // Records what it saw; fails prepare with exit 7 while the fail file exists; proves WORKTREE_HOME is writable.
  await writeFile(hook, [
    '#!/bin/sh',
    `echo "$BERTH_STAGE $BERTH_SLUG $BERTH_HOST $BERTH_PROJECT $(pwd)" >> "${log}"`,
    `touch "$BERTH_WORKTREE_HOME/touched-by-$BERTH_STAGE" || exit 9`,
    `if [ "$BERTH_STAGE" = prepare ] && [ -e "${failFile}" ]; then exit 7; fi`,
    'exit 0',
    ''
  ].join('\n'));
  await chmod(hook, 0o755);
  const shared = new MemoryRuntime({ services: ['api', 'mail'] });

  return {
    base, root, controlHome, log, failFile, runner,
    async context(overrides = {}) {
      const logger = new RecordingLogger();
      const fakeProcess = new FakeProcess();
      const context = createCommandContext({
        loaded: await loadConfig(path.join(root, 'berth.toml')),
        controlHome,
        routerPort: 80,
        router: { port: 80, network: 'berth-router', indexUrl: 'http://berth.localhost', ensure: () => Promise.resolve() },
        index: { url: 'http://berth.localhost', refresh: () => Promise.resolve(placeholderSnapshot('')), refreshBestEffort: () => Promise.resolve() },
        commandName: 'berth',
        logger,
        runner: overrides.runner ?? runner,
        runtime: overrides.runtime ?? shared,
        process: overrides.process ?? fakeProcess.control()
      });
      return { ...context, logger, fakeProcess };
    }
  };
}

const hookLog = async (w: World): Promise<string[]> =>
  (await readFile(w.log, 'utf8').catch(() => '')).split('\n').filter(Boolean);

describe('up', () => {
  it('registers the worktree, starts services, runs prepare then seed the first time, and prints URLs', async () => {
    const w = await world();
    const context = await w.context();

    const result = await up(context, { services: ['api', 'mail'] });

    expect(result.registration.slug).toBe('feature-login');
    expect(result.hooks).toEqual(['prepare', 'seed']);
    expect(result.urls.map((u) => u.url)).toEqual([
      'http://feature-login.acme.localhost',
      'http://mail.feature-login.acme.localhost/inbox'
    ]);
    expect(context.logger.printed).toEqual([
      'api   http://feature-login.acme.localhost',
      'mail  http://mail.feature-login.acme.localhost/inbox'
    ]);
    // Hooks saw the contract: stage, slug, host, project, and the project root as cwd.
    expect(await hookLog(w)).toEqual([
      `prepare feature-login feature-login.acme.localhost acme ${w.root}`,
      `seed feature-login feature-login.acme.localhost acme ${w.root}`
    ]);
    const store = context.registry.store(result.registration.repositoryId);
    await expect(store.initialization(result.registration.worktreeId, 'feature-login')).resolves.toBe('complete');
    await expect(status(context)).resolves.toEqual([
      { service: 'api', state: 'running' },
      { service: 'mail', state: 'running' }
    ]);
  });

  it('reports a hook that cannot be started, not a failed exit', async () => {
    const w = await world();
    const hook = path.join(w.root, 'hooks', 'record');
    await chmod(hook, 0o644);
    const context = await w.context();

    await expect(up(context)).rejects.toMatchObject({
      message: `The prepare hook could not be started: \`${hook}\` is not executable (EACCES).`,
      exitCode: 126
    });
    expect(await hookLog(w)).toEqual([]);
  });

  it('runs only prepare on later ups', async () => {
    const w = await world();
    await up(await w.context());
    const second = await up(await w.context());

    expect(second.hooks).toEqual(['prepare']);
    expect((await hookLog(w)).map((line) => line.split(' ')[0])).toEqual(['prepare', 'seed', 'prepare']);
  });

  it('never runs seed when prepare fails, keeps the worktree pending, preserves the exit code, and releases the lock', async () => {
    const w = await world();
    await writeFile(w.failFile, '');
    const context = await w.context();

    const failure = await up(context).catch((error: unknown) => error);
    expect(failure).toBeInstanceOf(CliError);
    expect((failure as CliError).exitCode).toBe(7);
    expect((failure as CliError).message).toContain('prepare hook failed');
    expect((await hookLog(w)).map((line) => line.split(' ')[0])).toEqual(['prepare']);

    const registration = await context.registry.current();
    expect(registration).toBeDefined();
    const store = context.registry.store(registration!.repositoryId);
    await expect(store.initialization(registration!.worktreeId, registration!.slug)).resolves.toBe('pending');

    // The lock was released on the failure path; fixing the hook and retrying seeds as a first run.
    await rm(w.failFile);
    const retry = await up(await w.context());
    expect(retry.hooks).toEqual(['prepare', 'seed']);
  });

  it('defers an interrupt until the lock is released, then re-raises it', async () => {
    const w = await world();
    const fakeProcess = new FakeProcess();
    const runner = new ExecaRunner();
    const memory = new MemoryRuntime({ services: ['api', 'mail'] });
    // A backend that gets interrupted mid-start.
    const interrupted: Runtime = {
      kind: 'interrupted',
      up: async (ref: WorktreeRef, services: readonly ServiceSpec[]) => {
        await memory.up(ref, services);
        fakeProcess.emit('SIGINT');
        await new Promise((resolve) => setTimeout(resolve, 20));
      },
      down: (ref) => memory.down(ref),
      status: (ref) => memory.status(ref),
      endpoints: (ref) => memory.endpoints(ref),
      inventory: (ref) => memory.inventory(ref),
      reap: (ref, resource) => memory.reap(ref, resource)
    };
    const context = await w.context({ runtime: interrupted, process: fakeProcess.control(), runner });

    await expect(up(context, { services: ['api'] })).rejects.toBeInstanceOf(OperationCancelledError);

    expect(fakeProcess.reenacted).toEqual(['SIGINT']);
    expect(await hookLog(w)).toEqual([]);   // cancelled before any hook could run
    // The interrupted runner is poisoned by design; a fresh context sees the aftermath.
    const after = await w.context();
    const registration = (await after.registry.current())!;
    const lock = new ScopedLock(
      worktreeLockTarget(w.controlHome, registration.repositoryId, registration.worktreeId),
      'probe', after.logger, { acquireTimeoutMs: 500 }
    );
    await expect(lock.run(() => Promise.resolve('free'))).resolves.toBe('free');
    const store = after.registry.store(registration.repositoryId);
    await expect(store.initialization(registration.worktreeId, registration.slug)).resolves.toBe('pending');
  });
});

describe('down, status, urls', () => {
  it('refuse to act on a worktree that was never brought up', async () => {
    const w = await world();
    const context = await w.context();
    await expect(down(context)).rejects.toThrow("This worktree is not registered. Run 'berth up' first.");
    await expect(urls(context)).rejects.toThrow('not registered');
    await expect(status(context)).resolves.toBeUndefined();
    // Prose, not data: it goes to the notice stream, and stdout stays empty so
    // `berth status | …` gets no rows rather than a sentence.
    expect(context.logger.printed).toEqual([]);
    expect(context.logger.lines).toContainEqual(
      "info: Not registered — run 'berth up' to create this worktree's environment."
    );
    await expect(context.registry.current()).resolves.toBeUndefined();   // read-only verbs never register
  });

  it('stop services and report them absent without touching the registry', async () => {
    const w = await world();
    await up(await w.context(), { services: ['api', 'mail'] });
    const context = await w.context();

    await down(context);
    const statuses = await status(context);

    expect(statuses).toEqual([
      { service: 'api', state: 'absent' },
      { service: 'mail', state: 'absent' }
    ]);
    expect(context.logger.printed).toEqual([
      'feature-login.acme.localhost',
      '  api   absent',
      '  mail  absent'
    ]);
    expect(await urls(context)).toHaveLength(2);
  });
});
