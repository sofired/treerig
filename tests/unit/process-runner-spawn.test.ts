import { chmod, mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { CommandStartError, ExecaRunner } from '../../src/process-runner.ts';

// No mock here: these spawn for real, because the shape Execa reports for a
// process that never started is exactly what process-runner.test.ts assumes.

const temporary: string[] = [];
afterEach(async () => {
  await Promise.all(temporary.splice(0).map((directory) => rm(directory, { recursive: true, force: true })));
});

async function scratch(): Promise<string> {
  const directory = await mkdtemp(path.join(tmpdir(), 'berth-spawn-'));
  temporary.push(directory);
  return directory;
}

/** Runs `action` with PATH set to exactly `directory`, and puts PATH back afterwards. */
async function withPath(directory: string, action: () => Promise<void>): Promise<void> {
  const previous = process.env.PATH;
  process.env.PATH = directory;
  try {
    await action();
  } finally {
    if (previous === undefined) delete process.env.PATH;
    else process.env.PATH = previous;
  }
}

describe('ExecaRunner against real processes', () => {
  it('says a missing executable could not be started', async () => {
    await withPath(await scratch(), async () => {
      await expect(new ExecaRunner().run('berth-definitely-missing-tool', [], { reject: false })).rejects.toMatchObject({
        name: 'CommandStartError',
        message: '`berth-definitely-missing-tool` could not be started: it is not on PATH (ENOENT).',
        exitCode: 127
      });
    });
  });

  it('says a file that is not executable could not be started', async () => {
    const script = path.join(await scratch(), 'hook');
    await writeFile(script, '#!/bin/sh\necho hi\n');
    await chmod(script, 0o644);

    const attempt = new ExecaRunner().run(script, [], { reject: false });
    await expect(attempt).rejects.toBeInstanceOf(CommandStartError);
    await expect(attempt).rejects.toMatchObject({
      message: `\`${script}\` could not be started: it is not executable (EACCES).`,
      exitCode: 126
    });
  });

  it('names a working directory that does not exist, on one line', async () => {
    const gone = path.join(await scratch(), 'gone');
    await expect(new ExecaRunner().run('sh', ['-c', 'true'], { cwd: gone, reject: false })).rejects.toMatchObject({
      name: 'CommandStartError',
      message: `\`sh\` could not be started: it has no working directory: ${gone} does not exist (ENOENT).`,
      exitCode: 126
    });
  });

  it('still returns a plain exit code for a command that ran and failed', async () => {
    await expect(new ExecaRunner().run('sh', ['-c', 'exit 3'], { reject: false })).resolves.toMatchObject({ exitCode: 3 });
  });

  it('finds only executable regular files on PATH', async () => {
    const directory = await scratch();
    await writeFile(path.join(directory, 'tool'), '#!/bin/sh\n');
    await chmod(path.join(directory, 'tool'), 0o755);
    await writeFile(path.join(directory, 'plain'), '#!/bin/sh\n');
    await chmod(path.join(directory, 'plain'), 0o644);
    await mkdir(path.join(directory, 'folder'), { mode: 0o755 });
    await withPath(directory, async () => {
      const runner = new ExecaRunner();
      await expect(runner.which('tool')).resolves.toBe(true);
      await expect(runner.which('plain')).resolves.toBe(false);
      await expect(runner.which('folder')).resolves.toBe(false);
      await expect(runner.which('absent')).resolves.toBe(false);
    });
  });
});
