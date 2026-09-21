import { createHash } from 'node:crypto';
import { constants } from 'node:fs';
import {
  chmod,
  copyFile,
  cp,
  lstat,
  mkdir,
  open,
  readFile,
  readdir,
  rename,
  rm,
  stat
} from 'node:fs/promises';
import type { FileHandle } from 'node:fs/promises';
import path from 'node:path';
import { CliError, invariant } from './errors.ts';
import type { ControlLayout, WorktreeLayout } from './layout.ts';

// Everything under the control home is created owner-only (0700 directories,
// 0600 files) and every path is verified to sit inside the control home with
// no symlink in the chain before it is created or removed. That containment is
// what makes recursive removal during cleanup safe.

export async function pathExists(file: string): Promise<boolean> {
  try {
    await stat(file);
    return true;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return false;
    throw error;
  }
}

export async function nonemptyFile(file: string): Promise<boolean> {
  try {
    return (await stat(file)).isFile() && (await stat(file)).size > 0;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return false;
    throw error;
  }
}

export async function assertSafeDirectoryPath(root: string, candidate: string): Promise<void> {
  const resolvedRoot = path.resolve(root);
  const resolvedCandidate = path.resolve(candidate);
  const relative = path.relative(resolvedRoot, resolvedCandidate);
  invariant(
    relative === '' || (!relative.startsWith('..') && !path.isAbsolute(relative)),
    `Refusing directory outside the control home: ${resolvedCandidate}`
  );
  const segments = relative === '' ? [] : relative.split(path.sep);
  let current = resolvedRoot;
  for (let index = -1; index < segments.length; index += 1) {
    if (index >= 0) current = path.join(current, segments[index] ?? '');
    try {
      const metadata = await lstat(current);
      invariant(
        metadata.isDirectory() && !metadata.isSymbolicLink(),
        `Refusing symlinked or non-directory managed path: ${current}`
      );
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === 'ENOENT') return;
      throw error;
    }
  }
}

export async function assertSafeFilePath(root: string, candidate: string): Promise<void> {
  await assertSafeDirectoryPath(root, path.dirname(candidate));
  try {
    const metadata = await lstat(candidate);
    invariant(
      metadata.isFile() && !metadata.isSymbolicLink(),
      `Refusing symlinked or non-file managed path: ${candidate}`
    );
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error;
  }
}

// Deliberately no process.umask() here: it is process-wide and would be
// inherited by every hook the tool spawns, making files a hook creates
// unreadable inside containers. Explicit modes plus the chmod below are enough.
async function createSecureDirectories(
  controlHome: string,
  directories: readonly string[]
): Promise<void> {
  for (const directory of directories) {
    await assertSafeDirectoryPath(controlHome, directory);
    await mkdir(directory, { recursive: true, mode: 0o700 });
    await assertSafeDirectoryPath(controlHome, directory);
    await chmod(directory, 0o700);
  }
}

export async function secureControlDirectories(layout: ControlLayout): Promise<void> {
  await createSecureDirectories(layout.controlHome, [
    layout.controlHome,
    layout.repositoriesDir,
    layout.locksDir,
    layout.indexDir
  ]);
  await assertSafeFilePath(layout.controlHome, layout.registryLockPath);
  await assertSafeFilePath(layout.controlHome, layout.routerLockPath);
  await assertSafeFilePath(layout.controlHome, layout.indexFile);
}

/** Directories a worktree needs before it can be registered — no home yet. */
export async function secureRegistrationDirectories(layout: WorktreeLayout): Promise<void> {
  await createSecureDirectories(layout.controlHome, [
    layout.controlHome,
    layout.repositoriesDir,
    layout.locksDir,
    layout.repositoryDir,
    layout.registryDir,
    path.dirname(layout.worktreeLocksDir),
    layout.worktreeLocksDir
  ]);
  await assertSafeFilePath(layout.controlHome, layout.registryLockPath);
  await assertSafeFilePath(layout.controlHome, layout.lockPath);
  await assertSafeFilePath(layout.controlHome, layout.developmentLockPath);
}

/**
 * Directories a worktree needs to run. Refuses if a previous removal was
 * interrupted, since the quarantine directory means state is half-deleted.
 */
export async function secureWorktreeDirectories(layout: WorktreeLayout): Promise<void> {
  await secureControlDirectories(layout);
  invariant(
    !(await pathExists(layout.quarantinePath)),
    `Removal of this worktree's environment was interrupted; retry the removal before starting it again.`
  );
  await createSecureDirectories(layout.controlHome, [
    layout.repositoryDir,
    layout.registryDir,
    path.dirname(layout.worktreeLocksDir),
    layout.worktreeLocksDir,
    layout.instancesDir,
    layout.home
  ]);
}

/** Write via a temp file and rename, then fsync the directory. */
export async function atomicWrite(
  destination: string,
  contents: string | Uint8Array,
  mode = 0o600
): Promise<void> {
  await mkdir(path.dirname(destination), { recursive: true, mode: 0o700 });
  const temporary = `${destination}.tmp.${process.pid}`;
  let temporaryHandle: FileHandle | undefined;
  try {
    temporaryHandle = await open(
      temporary,
      constants.O_CREAT | constants.O_TRUNC | constants.O_WRONLY,
      mode
    );
    await temporaryHandle.writeFile(contents);
    await temporaryHandle.chmod(mode);
    await temporaryHandle.sync();
  } finally {
    await temporaryHandle?.close();
  }
  await rename(temporary, destination);
  const directory = await open(path.dirname(destination), constants.O_RDONLY);
  try {
    await directory.sync();
  } finally {
    await directory.close();
  }
}

export async function atomicJson(destination: string, value: unknown): Promise<void> {
  await atomicWrite(destination, `${JSON.stringify(value, null, 2)}\n`);
}

export async function readJson<T = unknown>(file: string): Promise<T> {
  try {
    return JSON.parse(await readFile(file, 'utf8')) as T;
  } catch (error) {
    throw new CliError(`Could not read valid JSON from ${file}: ${(error as Error).message}`);
  }
}

/** Read a file that must be a regular, non-symlinked file, tightening it to 0600. */
export async function readOwnerOnlyFile(
  file: string,
  description: string
): Promise<string | undefined> {
  let handle: FileHandle | undefined;
  try {
    handle = await open(file, constants.O_RDONLY | constants.O_NOFOLLOW);
    const metadata = await handle.stat();
    invariant(
      metadata.isFile(),
      `${description} must be a regular, non-symlinked file: ${file}`
    );
    await handle.chmod(0o600);
    return await handle.readFile('utf8');
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return undefined;
    if (['ELOOP', 'EMLINK'].includes((error as NodeJS.ErrnoException).code ?? '')) {
      throw new CliError(`${description} must be a regular, non-symlinked file: ${file}`);
    }
    throw error;
  } finally {
    await handle?.close();
  }
}

export function sha256(contents: string | Uint8Array): string {
  return createHash('sha256').update(contents).digest('hex');
}

export async function sha256File(file: string): Promise<string> {
  return sha256(await readFile(file));
}

export async function listFiles(directory: string): Promise<string[]> {
  const results: string[] = [];
  if (!(await pathExists(directory))) return results;
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    const absolute = path.join(directory, entry.name);
    if (entry.isDirectory()) results.push(...(await listFiles(absolute)));
    else if (entry.isFile()) results.push(absolute);
  }
  return results.sort((left, right) => left.localeCompare(right));
}

export async function copySecure(source: string, destination: string): Promise<void> {
  await mkdir(path.dirname(destination), { recursive: true, mode: 0o700 });
  await copyFile(source, destination);
  await chmod(destination, 0o600);
}

export async function copyTreeSecure(source: string, destination: string): Promise<void> {
  await cp(source, destination, { recursive: true });
  for (const file of await listFiles(destination)) await chmod(file, 0o600);
  const directories = new Set<string>([destination]);
  for (const file of await listFiles(destination)) directories.add(path.dirname(file));
  for (const directory of directories) await chmod(directory, 0o700);
}

/** Recursive removal that refuses anything not strictly inside `root`. */
export async function removeWithin(root: string, target: string): Promise<void> {
  const relative = path.relative(path.resolve(root), path.resolve(target));
  invariant(
    relative !== '' && !relative.startsWith('..') && !path.isAbsolute(relative),
    `Refusing to remove unsafe path: ${target}`
  );
  await rm(target, { recursive: true, force: true });
}

export async function touchSecure(file: string): Promise<void> {
  const handle = await open(file, 'w', 0o600);
  await handle.close();
  await chmod(file, 0o600);
}
