import { mkdir, mkdtemp, readdir, readFile, rm, stat, symlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { CliError } from '../../src/errors.ts';
import {
  assertSafeDirectoryPath,
  atomicJson,
  atomicWrite,
  copySecure,
  listFiles,
  nonemptyFile,
  pathExists,
  readJson,
  readOwnerOnlyFile,
  removeWithin,
  secureControlDirectories,
  secureWorktreeDirectories,
  sha256,
  touchSecure
} from '../../src/files.ts';
import { controlLayout, worktreeLayout } from '../../src/layout.ts';

const temporary: string[] = [];
const mode = async (file: string): Promise<number> => (await stat(file)).mode & 0o777;

async function scratch(): Promise<string> {
  const directory = await mkdtemp(path.join(tmpdir(), 'berth-files-'));
  temporary.push(directory);
  return directory;
}

afterEach(async () => {
  await Promise.all(
    temporary.splice(0).map((directory) => rm(directory, { recursive: true, force: true }))
  );
});

describe('existence probes', () => {
  it('distinguish missing, empty, and populated files', async () => {
    const root = await scratch();
    const empty = path.join(root, 'empty');
    const full = path.join(root, 'full');
    await writeFile(empty, '');
    await writeFile(full, 'contents');

    expect(await pathExists(path.join(root, 'missing'))).toBe(false);
    expect(await pathExists(empty)).toBe(true);
    expect(await nonemptyFile(empty)).toBe(false);
    expect(await nonemptyFile(full)).toBe(true);
    expect(await nonemptyFile(path.join(root, 'missing'))).toBe(false);
  });
});

describe('atomicWrite', () => {
  it('creates the parent directory, writes owner-only, and leaves no temp file', async () => {
    const root = await scratch();
    const destination = path.join(root, 'nested', 'deep', 'record.json');

    await atomicWrite(destination, 'payload');

    expect(await readFile(destination, 'utf8')).toBe('payload');
    expect(await mode(destination)).toBe(0o600);
    expect(await mode(path.join(root, 'nested', 'deep'))).toBe(0o700);
    expect(await readdir(path.dirname(destination))).toEqual(['record.json']);
  });

  it('round-trips JSON and rejects a corrupt file with a readable error', async () => {
    const root = await scratch();
    const file = path.join(root, 'state.json');

    await atomicJson(file, { version: 2, slug: 'feature-login' });
    expect(await readJson(file)).toEqual({ version: 2, slug: 'feature-login' });

    await writeFile(file, '{ not json');
    await expect(readJson(file)).rejects.toBeInstanceOf(CliError);
    await expect(readJson(file)).rejects.toThrow('Could not read valid JSON');
  });
});

describe('readOwnerOnlyFile', () => {
  it('returns undefined when the file is absent', async () => {
    const root = await scratch();
    expect(await readOwnerOnlyFile(path.join(root, 'missing'), 'secret')).toBeUndefined();
  });

  it('reads a regular file and tightens it to owner-only', async () => {
    const root = await scratch();
    const file = path.join(root, 'secret');
    await writeFile(file, 'hunter2', { mode: 0o644 });

    expect(await readOwnerOnlyFile(file, 'secret')).toBe('hunter2');
    expect(await mode(file)).toBe(0o600);
  });

  it('refuses to follow a symlink', async () => {
    const root = await scratch();
    const real = path.join(root, 'real');
    const link = path.join(root, 'link');
    await writeFile(real, 'contents');
    await symlink(real, link);

    await expect(readOwnerOnlyFile(link, 'secret')).rejects.toThrow(
      'secret must be a regular, non-symlinked file'
    );
  });
});

describe('removeWithin', () => {
  it('removes a nested target inside the root', async () => {
    const root = await scratch();
    const target = path.join(root, 'instance');
    await mkdir(path.join(target, 'db'), { recursive: true });
    await writeFile(path.join(target, 'db', 'data'), 'x');

    await removeWithin(root, target);

    expect(await pathExists(target)).toBe(false);
  });

  it('refuses the root itself, its parent, and anything outside it', async () => {
    const root = await scratch();
    const outside = await scratch();
    await writeFile(path.join(outside, 'keep'), 'x');

    await expect(removeWithin(root, root)).rejects.toThrow('Refusing to remove unsafe path');
    await expect(removeWithin(root, path.dirname(root))).rejects.toThrow('Refusing to remove unsafe path');
    await expect(removeWithin(root, path.join(root, '..', path.basename(outside)))).rejects.toThrow('Refusing to remove unsafe path');
    expect(await pathExists(path.join(outside, 'keep'))).toBe(true);
  });
});

describe('secure copies and touches', () => {
  it('copySecure creates the parent and writes owner-only', async () => {
    const root = await scratch();
    const source = path.join(root, 'source');
    const destination = path.join(root, 'out', 'copy');
    await writeFile(source, 'data', { mode: 0o644 });

    await copySecure(source, destination);

    expect(await readFile(destination, 'utf8')).toBe('data');
    expect(await mode(destination)).toBe(0o600);
    expect(await mode(path.join(root, 'out'))).toBe(0o700);
  });

  it('touchSecure creates an empty owner-only file', async () => {
    const root = await scratch();
    const file = path.join(root, 'marker');

    await touchSecure(file);

    expect(await readFile(file, 'utf8')).toBe('');
    expect(await mode(file)).toBe(0o600);
  });
});

describe('listFiles and hashing', () => {
  it('lists files recursively in sorted order and tolerates a missing root', async () => {
    const root = await scratch();
    await mkdir(path.join(root, 'b'), { recursive: true });
    await writeFile(path.join(root, 'b', 'two'), '');
    await writeFile(path.join(root, 'a'), '');
    await writeFile(path.join(root, 'b', 'one'), '');

    expect(await listFiles(root)).toEqual([
      path.join(root, 'a'),
      path.join(root, 'b', 'one'),
      path.join(root, 'b', 'two')
    ]);
    expect(await listFiles(path.join(root, 'missing'))).toEqual([]);
  });

  it('hashes deterministically', () => {
    expect(sha256('abc')).toBe('ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad');
  });
});

describe('path containment', () => {
  it('accepts paths inside the root, including ones that do not exist yet', async () => {
    const root = await scratch();
    await expect(assertSafeDirectoryPath(root, root)).resolves.toBeUndefined();
    await expect(assertSafeDirectoryPath(root, path.join(root, 'not', 'yet'))).resolves.toBeUndefined();
  });

  it('refuses paths outside the root', async () => {
    const root = await scratch();
    await expect(assertSafeDirectoryPath(root, path.dirname(root))).rejects.toThrow(
      'Refusing directory outside the control home'
    );
    await expect(assertSafeDirectoryPath(root, path.join(root, '..', 'sibling'))).rejects.toThrow(
      'Refusing directory outside the control home'
    );
  });

  it('refuses a symlink anywhere in the chain', async () => {
    const root = await scratch();
    const elsewhere = await scratch();
    await symlink(elsewhere, path.join(root, 'linked'));

    await expect(assertSafeDirectoryPath(root, path.join(root, 'linked', 'child'))).rejects.toThrow(
      'Refusing symlinked or non-directory managed path'
    );
  });
});

describe('secure directory creation', () => {
  it('does not change the process umask, which hooks would inherit', async () => {
    const original = process.umask(0o022);
    try {
      await secureControlDirectories(controlLayout(path.join(await scratch(), 'control')));
      expect(process.umask()).toBe(0o022);
    } finally {
      process.umask(original);
    }
  });

  it('creates the control layout owner-only', async () => {
    const controlHome = path.join(await scratch(), 'control');
    const layout = controlLayout(controlHome);

    await secureControlDirectories(layout);

    for (const directory of [layout.controlHome, layout.repositoriesDir, layout.locksDir, layout.indexDir]) {
      expect(await mode(directory)).toBe(0o700);
    }
  });

  it('creates a worktree home owner-only and refuses while a removal is half-finished', async () => {
    const controlHome = path.join(await scratch(), 'control');
    const layout = worktreeLayout(
      controlHome,
      'a08b4892-5111-49f1-a47a-5ab988c7f676',
      '2b41d9d4-ed88-49c1-be9c-5439bdf6a431'
    );

    await secureWorktreeDirectories(layout);
    expect(await mode(layout.home)).toBe(0o700);
    expect(await mode(layout.worktreeLocksDir)).toBe(0o700);

    await mkdir(layout.quarantinePath, { recursive: true });
    await expect(secureWorktreeDirectories(layout)).rejects.toThrow('was interrupted; retry the removal');
  });
});
