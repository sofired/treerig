import { execFile } from 'node:child_process';
import { mkdtemp, readFile, rm, stat, symlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';
import { afterEach, describe, expect, it } from 'vitest';

// The CLI as a process, because which stream a line lands on is only a
// contract if it survives the entry point. A unit test can prove `console.log`
// was called; only this can prove a pipe gets the data and nothing else.

const run = promisify(execFile);
const CLI = fileURLToPath(new URL('../../src/cli/main.ts', import.meta.url));

const temporary: string[] = [];
afterEach(async () => {
  await Promise.all(temporary.splice(0).map((directory) => rm(directory, { recursive: true, force: true })));
});

interface Invocation {
  stdout: string;
  stderr: string;
  code: number;
}

async function berth(
  cwd: string,
  args: readonly string[],
  options: { path?: string; debug?: boolean } = {}
): Promise<Invocation> {
  try {
    const { stdout, stderr } = await run(process.execPath, [CLI, ...args], {
      cwd,
      env: {
        BERTH_HOME: path.join(cwd, 'control-home'),
        // Node is started by absolute path, so an empty PATH only decides
        // whether the tool can find `docker`.
        PATH: options.path ?? (process.env['PATH'] ?? ''),
        HOME: cwd,
        ...(options.debug === true ? { BERTH_DEBUG: '1' } : {})
      }
    });
    return { stdout, stderr, code: 0 };
  } catch (error) {
    const failure = error as { stdout?: string; stderr?: string; code?: number };
    return { stdout: failure.stdout ?? '', stderr: failure.stderr ?? '', code: failure.code ?? 1 };
  }
}

async function git(cwd: string, args: readonly string[]): Promise<void> {
  // Nothing of the developer's own git: a global `commit.gpgsign` would
  // otherwise fail these on their machine and not in CI.
  await run('git', [...args], {
    cwd,
    env: {
      ...process.env,
      GIT_CONFIG_GLOBAL: '/dev/null',
      GIT_CONFIG_SYSTEM: '/dev/null',
      GIT_CONFIG_NOSYSTEM: '1',
      GIT_TERMINAL_PROMPT: '0'
    }
  });
}

async function project(config: string): Promise<string> {
  const directory = await mkdtemp(path.join(tmpdir(), 'berth-cli-'));
  temporary.push(directory);
  await writeFile(path.join(directory, 'compose.yaml'), 'services:\n  api:\n    image: busybox:1.37.0\n');
  await writeFile(path.join(directory, 'berth.toml'), config);
  return directory;
}

const CONFIG = `
[project]
name = "acme"
domain = "acme.localhost"
[compose]
file = "compose.yaml"
[[service]]
name = "api"
`;

describe('what each stream carries', () => {
  it('puts a validator report on stdout and keeps stderr empty', async () => {
    const cwd = await project(CONFIG);

    const result = await berth(cwd, ['config', 'validate']);

    expect(result.code).toBe(0);
    expect(result.stderr).toBe('');
    expect(result.stdout).toContain('berth.toml is valid.');
    expect(result.stdout).toContain('project acme · domain acme.localhost · 1 service(s) · 0 hook(s)');
  });

  it('counts only the hooks that run, and names the ones that do not', async () => {
    const cwd = await project(`${CONFIG}
[hooks]
prepare = "./prepare.sh"
seed    = "./seed.sh"
verify  = "./verify.sh"
backup  = "./backup.sh"
`);

    const result = await berth(cwd, ['config', 'validate']);

    // Both invoked stages counted, including the one only a first `up` asks for.
    expect(result.stdout).toContain('1 service(s) · 2 hook(s)');
    expect(result.stdout).toContain('accepted but not invoked yet: verify, backup');
  });

  it('prints JSON a terminal cannot be driven by, that parses back exactly', async () => {
    const cwd = await project(`${CONFIG}
[service.index]
label   = "API\u202e"
purpose = "surface"
`);

    const result = await berth(cwd, ['config', 'show']);

    expect(result.stderr).toBe('');
    expect(result.stdout).not.toMatch(/[\u007f-\uffff]/);
    expect(result.stdout).toContain('\\u202e');
    const parsed = JSON.parse(result.stdout) as { service: { index?: { label: string } }[] };
    expect(parsed.service[0]?.index?.label).toBe('API\u202e');
  });

  it('keeps a failure, and what it names, off stdout', async () => {
    const cwd = await mkdtemp(path.join(tmpdir(), 'berth-cli-'));
    temporary.push(cwd);

    const result = await berth(cwd, ['config', 'validate']);

    expect(result.code).toBe(1);
    expect(result.stdout).toBe('');
    expect(result.stderr).toContain('No berth.toml found');
    // Everything it points at exists: a template, and a file in this repository.
    expect(result.stderr).toContain('berth.example.toml');
    expect(result.stderr).not.toContain('berth init');
  });

  it('filters the stack trace it prints when asked for one', async () => {
    const cwd = await project(CONFIG);
    // The path is echoed back in the failure, and with BERTH_DEBUG=1 it is
    // echoed again inside the stack — which is printed outside the logger.
    const hostile = path.join(cwd, 'missing\u001b[2K.toml');

    const result = await berth(cwd, ['--config', hostile, 'config', 'validate'], { debug: true });

    expect(result.code).toBe(1);
    expect(result.stdout).toBe('');
    expect(result.stderr).toContain('\uFFFD');
    expect([...result.stderr].some((character) => character === '\u001b')).toBe(false);
  });

  it('refuses a verb it does not have by name, on stderr', async () => {
    const cwd = await project(CONFIG);

    const result = await berth(cwd, ['doctor']);

    expect(result.code).toBe(2);
    expect(result.stdout).toBe('');
    expect(result.stderr).toContain("'berth doctor' is not implemented yet");
  });

  it('leaves stdout empty when the answer is a notice, not data', async () => {
    const cwd = await project(CONFIG);
    // `status` needs a worktree to be asked about, and this one was never
    // brought up: the answer is a sentence, which belongs on the notice stream.
    await git(cwd, ['init', '--initial-branch=feature/login', '.']);
    await git(cwd, ['-c', 'user.name=t', '-c', 'user.email=t@example.invalid', 'commit', '--allow-empty', '-m', 'initial']);

    const result = await berth(cwd, ['status']);

    expect(result.code).toBe(0);
    expect(result.stdout).toBe('');
    expect(result.stderr).toContain('Not registered');
  });

  it('pipes a machine-readable list with nothing else in it', async () => {
    const cwd = await project(CONFIG);
    await git(cwd, ['init', '--initial-branch=feature/login', '.']);
    await git(cwd, ['-c', 'user.name=t', '-c', 'user.email=t@example.invalid', 'commit', '--allow-empty', '-m', 'initial']);

    const result = await berth(cwd, ['worktree', 'list', '--json']);

    expect(result.code).toBe(0);
    expect(result.stderr).toBe('');
    expect(JSON.parse(result.stdout)).toEqual([]);
  });

  it('reports an unreachable router as a failure rather than as output', async () => {
    const cwd = await project(CONFIG);
    const empty = await mkdtemp(path.join(tmpdir(), 'berth-nopath-'));
    temporary.push(empty);

    // No `docker` to be found, which is what an unreachable router looks like
    // from here.
    const result = await berth(cwd, ['router', 'status'], { path: empty });

    expect(result.code).toBe(1);
    expect(result.stdout).toBe('');
    expect(result.stderr).toContain('Router state unknown');
  });
});

// The README's install step is `ln -s <this file> somewhere-on-PATH`, and the
// rename story is that the symlink's own name is what people see. Both are
// claims about a file mode and about `argv[1]`, and neither survives a commit
// that drops the executable bit.
describe('the entry point as an installed command', () => {
  it('is executable and starts with a shebang', async () => {
    const source = await readFile(CLI, 'utf8');
    expect(source.startsWith('#!/usr/bin/env node\n')).toBe(true);
    // Mode 0755 for everyone, not just the owner: a clone is installed by
    // symlink, so the bit is whatever Git recorded.
    expect((await stat(CLI)).mode & 0o111).toBe(0o111);
  });

  it('answers to the name it was symlinked as, and to the product name otherwise', async () => {
    const cwd = await project(CONFIG);
    const link = path.join(cwd, 'duke');
    await symlink(CLI, link);

    const named = await run(link, ['--help'], { env: { PATH: process.env['PATH'] ?? '' } });
    expect(named.stdout).toContain('Usage: duke [options] [command]');

    const byPath = await berth(cwd, ['--help']);
    expect(byPath.stdout).toContain('Usage: berth [options] [command]');
  });
});
