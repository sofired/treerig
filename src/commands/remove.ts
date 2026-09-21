import { rename } from 'node:fs/promises';
import { CliError } from '../errors.ts';
import { assertSafeDirectoryPath, pathExists, removeWithin } from '../files.ts';
import { worktreeHost, worktreeLayout, type WorktreeLayout } from '../layout.ts';
import { withProgress } from '../progress.ts';
import { confirm } from '../prompts.ts';
import type { OwnedResource, WorktreeRef } from '../runtime/runtime.ts';
import type { RegisteredWorktree, WorktreeIdentity } from '../worktree-registry.ts';
import type { CommandContext } from './context.ts';
import { withWorktreeMutation } from './mutation.ts';

// The destructive verb. Order matters for crash safety: Docker resources go
// first, then the home, then the registration, then the identity marker — so
// an interrupted removal leaves a record that still points at whatever is
// left, and running `remove` again finishes the job. Nothing is deleted
// until every resource has been verified as owned by the target; a single
// resource that fails verification stops the whole thing before it starts.

export interface RemoveOptions {
  /** A slug or worktree UUID; omitted means the current worktree. */
  identifier?: string;
  /** Narrows a slug registered under more than one domain. */
  domain?: string;
  /** Skip the confirmation phrase (`--yes`). */
  approved?: boolean;
}

export interface RemoveResult {
  target: RegisteredWorktree;
  resources: OwnedResource[];
  home: boolean;
  registration: boolean;
  /** The worktree's identity marker was removed, so its next `up` mints a fresh identity. */
  marker: boolean;
}

export async function remove(context: CommandContext, options: RemoveOptions = {}): Promise<RemoveResult> {
  const { logger, runtime, registry, controlHome } = context;
  const target = await resolveTarget(context, options);
  const ref: WorktreeRef = { repositoryId: target.repositoryId, worktreeId: target.worktreeId, slug: target.slug };
  const host = worktreeHost(target.slug, target.domain);
  const layout = worktreeLayout(controlHome, target.repositoryId, target.worktreeId);

  // Everything that will go, verified before anything is touched. A resource
  // that cannot be proven ours fails here, before the prompt.
  const planned = await runtime.inventory(ref);
  const hasHome = await pathExists(layout.home) || await pathExists(layout.quarantinePath);
  await confirm({ nonInteractive: false }, describePlan(host, planned, hasHome), host, options.approved);

  const result = await withWorktreeMutation(context, ref, async () => {
    const identity = await targetIdentity(context, target);
    // A graceful stop only makes sense with this repository's Compose file.
    if (identity) {
      await withProgress(logger, `Stopping ${host}`, () => runtime.down(ref));
    }
    const resources = await withProgress(logger, `Removing ${host}`, async () => {
      // Re-verified after `down`; every resource is checked before any is removed.
      const owned = await runtime.inventory(ref);
      for (const resource of owned) await runtime.reap(ref, resource);
      return owned;
    });
    const home = await removeHome(layout);
    const registration = await registry.store(target.repositoryId).remove(target.worktreeId, target.slug, logger);
    let marker = false;
    if (identity) {
      await registry.identities.removeWorktreeMarker(identity);
      marker = true;
    }
    return { target, resources, home, registration, marker };
  }, { exclusive: true });

  logger.info(`Removed ${host}: ${summarize(result)}.`);
  return result;
}

/* ------------------------------------------------------------------ helpers */

async function resolveTarget(context: CommandContext, options: RemoveOptions): Promise<RegisteredWorktree> {
  if (options.identifier === undefined) {
    const current = await context.registry.current();
    if (!current) {
      throw new CliError(
        `This worktree is not registered; nothing to remove. ` +
        `Name another with '${context.commandName} worktree remove <slug|uuid>'.`
      );
    }
    return current;
  }
  const found = await context.registry.findGlobal(options.identifier, options.domain);
  if (!found) {
    const where = options.domain === undefined ? '' : ` under ${options.domain}`;
    throw new CliError(`No worktree is registered as ${options.identifier}${where}.`);
  }
  return found;
}

/** The target's Git identity, when it is a live worktree of this repository; otherwise undefined. */
async function targetIdentity(context: CommandContext, target: RegisteredWorktree): Promise<WorktreeIdentity | undefined> {
  const { identities } = context.registry;
  if (await identities.repositoryId() !== target.repositoryId) return undefined;
  return (await identities.activeIdentities()).find((identity) => identity.worktreeId === target.worktreeId);
}

/**
 * Rename-then-delete. The rename is atomic, so a crash leaves either the home
 * or a `.deleting-` directory — never a half-deleted home that `up` might
 * adopt as complete. A leftover quarantine from an earlier attempt is finished
 * first.
 */
async function removeHome(layout: WorktreeLayout): Promise<boolean> {
  let removed = false;
  if (await pathExists(layout.quarantinePath)) {
    await removeWithin(layout.controlHome, layout.quarantinePath);
    removed = true;
  }
  if (await pathExists(layout.home)) {
    await assertSafeDirectoryPath(layout.controlHome, layout.home);
    await rename(layout.home, layout.quarantinePath);
    await removeWithin(layout.controlHome, layout.quarantinePath);
    removed = true;
  }
  return removed;
}

function countResources(resources: readonly OwnedResource[]): string {
  const counts = new Map<string, number>();
  for (const resource of resources) counts.set(resource.kind, (counts.get(resource.kind) ?? 0) + 1);
  if (counts.size === 0) return 'no Docker resources';
  return [...counts]
    .map(([kind, count]) => `${count} ${kind}${count === 1 ? '' : 's'}`)
    .join(', ');
}

function describePlan(host: string, planned: readonly OwnedResource[], hasHome: boolean): string {
  const lines = [
    `This will permanently remove ${host}:`,
    `  ${countResources(planned)}`,
    hasHome ? '  its home directory under the control home' : '  (no home directory)',
    '  its registration'
  ];
  return lines.join('\n');
}

function summarize(result: RemoveResult): string {
  const parts = [countResources(result.resources)];
  if (result.home) parts.push('home directory');
  if (result.registration) parts.push('registration');
  if (result.marker) parts.push('identity marker');
  return parts.join(', ');
}
