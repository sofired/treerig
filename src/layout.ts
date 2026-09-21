import { createHash } from 'node:crypto';
import { homedir } from 'node:os';
import path from 'node:path';
import { BRAND } from './brand.ts';

/**
 * On-disk layout of the control home. Nothing here touches the filesystem;
 * these are pure path derivations so they can be reasoned about in tests.
 *
 *   <controlHome>/
 *     locks/registry                    registryLockPath
 *     locks/router                      routerLockPath
 *     locks/worktrees/<repo>/<wt>       worktree operation lock
 *     locks/worktrees/<repo>/<wt>.dev   development-session lock
 *     router/index/index.html           the shared checkout index
 *     repositories/<repo>/worktrees/    registry records, one JSON per worktree
 *     repositories/<repo>/instances/<wt>/   the worktree's home
 */

export interface ControlLayout {
  controlHome: string;
  indexDir: string;
  indexFile: string;
  repositoriesDir: string;
  locksDir: string;
  registryLockPath: string;
  routerLockPath: string;
}

export interface RepositoryLayout extends ControlLayout {
  repositoryDir: string;
  registryDir: string;
  instancesDir: string;
  worktreeLocksDir: string;
}

export interface WorktreeLayout extends RepositoryLayout {
  recordPath: string;
  home: string;
  /** Renamed-to during removal so a crash mid-delete leaves a recognizable artifact. */
  quarantinePath: string;
  lockPath: string;
  developmentLockPath: string;
}

export function resolveControlHome(
  environment: NodeJS.ProcessEnv = process.env,
  override?: string
): string {
  return path.resolve(
    override ?? environment[BRAND.homeEnv] ?? path.join(homedir(), BRAND.defaultHomeDir)
  );
}

export function controlLayout(controlHome: string): ControlLayout {
  const resolved = path.resolve(controlHome);
  const locksDir = path.join(resolved, 'locks');
  const indexDir = path.join(resolved, 'router', 'index');
  return {
    controlHome: resolved,
    indexDir,
    indexFile: path.join(indexDir, 'index.html'),
    repositoriesDir: path.join(resolved, 'repositories'),
    locksDir,
    registryLockPath: path.join(locksDir, 'registry'),
    routerLockPath: path.join(locksDir, 'router')
  };
}

export function repositoryLayout(
  controlHome: string,
  repositoryId: string
): RepositoryLayout {
  const control = controlLayout(controlHome);
  const repositoryDir = path.join(control.repositoriesDir, repositoryId);
  return {
    ...control,
    repositoryDir,
    registryDir: path.join(repositoryDir, 'worktrees'),
    instancesDir: path.join(repositoryDir, 'instances'),
    worktreeLocksDir: path.join(control.locksDir, 'worktrees', repositoryId)
  };
}

export function worktreeLayout(
  controlHome: string,
  repositoryId: string,
  worktreeId: string
): WorktreeLayout {
  const repository = repositoryLayout(controlHome, repositoryId);
  const home = path.join(repository.instancesDir, worktreeId);
  return {
    ...repository,
    recordPath: path.join(repository.registryDir, `${worktreeId}.json`),
    home,
    quarantinePath: path.join(repository.instancesDir, `.deleting-${worktreeId}`),
    lockPath: path.join(repository.worktreeLocksDir, worktreeId),
    developmentLockPath: path.join(repository.worktreeLocksDir, `${worktreeId}.dev`)
  };
}

/**
 * A Compose project name that is stable for the (repository, worktree) pair
 * and cannot collide across repositories. Kept short because Docker prefixes
 * every container, volume, and network with it.
 */
export function composeProjectName(repositoryId: string, worktreeId: string): string {
  const identity = createHash('sha256')
    .update(`${repositoryId}:${worktreeId}`)
    .digest('hex')
    .slice(0, 24);
  return `${BRAND.name}-${identity}`;
}

/** The routed hostname for a worktree: `<slug>.<domain>`. */
export function worktreeHost(slug: string, domain: string): string {
  return `${slug}.${domain}`;
}
