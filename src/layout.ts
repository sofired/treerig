import { createHash } from 'node:crypto';
import { homedir } from 'node:os';
import path from 'node:path';
import { BRAND } from './brand.ts';
import { CliError } from './errors.ts';

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
  routerDir: string;
  routerComposeFile: string;
  /**
   * Bind-mounted read-only into the router; 0755, and files written there
   * must be 0644 and must never contain secrets (TLS keys, tokens) — those
   * belong in Docker secrets or a named volume, not a served directory.
   */
  routerDynamicDir: string;
  /** Bind-mounted read-only into the index server; 0755, 0644 files, never secrets. */
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

/** Loopback port the shared router publishes; `BERTH_ROUTER_PORT` overrides the default of 80. */
export function resolveRouterPort(environment: NodeJS.ProcessEnv = process.env): number {
  const raw = environment[`${BRAND.envPrefix}_ROUTER_PORT`];
  if (raw === undefined || raw === '') return 80;
  const port = Number(raw);
  if (!Number.isInteger(port) || port < 1 || port > 65_535) {
    throw new CliError(`${BRAND.envPrefix}_ROUTER_PORT must be a port number between 1 and 65535, not "${raw}".`);
  }
  return port;
}

export function controlLayout(controlHome: string): ControlLayout {
  const resolved = path.resolve(controlHome);
  const locksDir = path.join(resolved, 'locks');
  const routerDir = path.join(resolved, 'router');
  const indexDir = path.join(routerDir, 'index');
  return {
    controlHome: resolved,
    routerDir,
    routerComposeFile: path.join(routerDir, 'compose.yaml'),
    routerDynamicDir: path.join(routerDir, 'dynamic'),
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
