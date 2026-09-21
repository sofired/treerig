import { mkdir, mkdtemp, realpath, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { createCommandContext, type CommandContext } from '../../src/commands/context.ts';
import { parseConfig } from '../../src/config/load.ts';
import { MemoryRuntime } from '../../src/runtime/memory.ts';
import { FakeProcess, RecordingLogger } from './command-doubles.ts';
import { GitRunner } from './git-runner.ts';

// A hermetic world for command tests: fake Git, in-memory runtime, real
// filesystem for the control home and identity markers.

export interface TestContext extends CommandContext {
  logger: RecordingLogger;
  fakeProcess: FakeProcess;
}

export interface CommandWorld {
  base: string;
  controlHome: string;
  root: string;
  runner: GitRunner;
  runtime: MemoryRuntime;
  /** Adds another Git worktree; `common` defaults to this repository's. */
  worktree(name: string, branch: string, common?: string): Promise<string>;
  /** A context whose config lives in `root`, with `domain` in its config. */
  context(root?: string, domain?: string): TestContext;
  dispose(): Promise<void>;
}

const CONFIG = (name: string, domain: string): string => `
[project]
name = "${name}"
domain = "${domain}"
[compose]
file = "compose.yaml"
[[service]]
name = "api"
[[service]]
name = "mail"
subdomain = "mail"
`;

export async function commandWorld(branch = 'feature/login', domain = 'acme.localhost'): Promise<CommandWorld> {
  const base = await realpath(await mkdtemp(path.join(tmpdir(), 'berth-commands-')));
  const controlHome = path.join(base, 'control');
  const common = path.join(base, 'common');
  const runner = new GitRunner();
  const runtime = new MemoryRuntime();
  const worktree = async (name: string, wtBranch: string, wtCommon = common): Promise<string> => {
    const root = path.join(base, name);
    await mkdir(root, { recursive: true });
    await mkdir(path.join(wtCommon, 'worktrees', name), { recursive: true });
    runner.add(root, wtBranch, wtCommon);
    return root;
  };
  const root = await worktree('worktree', branch);
  return {
    base,
    controlHome,
    root,
    runner,
    runtime,
    worktree,
    context(contextRoot = root, contextDomain = domain) {
      const file = path.join(contextRoot, 'berth.toml');
      const logger = new RecordingLogger();
      const fakeProcess = new FakeProcess();
      const context = createCommandContext({
        loaded: { config: parseConfig(CONFIG(path.basename(contextDomain, '.localhost'), contextDomain), file), file, root: contextRoot },
        controlHome,
        commandName: 'berth',
        logger,
        runner,
        runtime,
        process: fakeProcess.control()
      });
      return { ...context, logger, fakeProcess };
    },
    dispose: () => rm(base, { recursive: true, force: true })
  };
}
