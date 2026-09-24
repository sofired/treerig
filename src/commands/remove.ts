import { rename } from 'node:fs/promises';
import { recordedLabelNamespace } from '../checkout-index/card.ts';
import { BRAND } from '../brand.ts';
import { LABEL_NAMESPACE } from '../config/schema.ts';
import { CliError } from '../errors.ts';
import { printable } from '../text.ts';
import { assertNotGoManaged, assertSafeDirectoryPath, pathExists, removeWithin } from '../files.ts';
import { worktreeHost, worktreeLayout, type WorktreeLayout } from '../layout.ts';
import { withProgress } from '../progress.ts';
import { confirm } from '../prompts.ts';
import { INDEX_IMAGE } from '../router/stack.ts';
import type { OwnedResource, WorktreeRef } from '../runtime/runtime.ts';
import type { RegisteredWorktree, WorktreeIdentity } from '../worktree-registry.ts';
import { labelNamespace, type CommandContext } from './context.ts';
import { withWorktreeMutation } from './mutation.ts';
import { shellQuote } from '../shell.ts';

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
  /** Which labels the target's resources carry, when nothing on disk can say. */
  labelNamespace?: string;
}

export interface RemoveResult {
  target: RegisteredWorktree;
  /** Everything the worktree owned that is now gone, in inventory order: what stopping it removed and what was reaped. */
  resources: OwnedResource[];
  home: boolean;
  registration: boolean;
  /** The worktree's identity marker was removed, so its next `up` mints a fresh identity. */
  marker: boolean;
  /** Another removal finished first; nothing was left for this one. */
  alreadyGone: boolean;
}

/** Removes a worktree end to end: stops and reaps its Docker resources, then its home, its registration, and its identity marker. */
export async function remove(context: CommandContext, options: RemoveOptions = {}): Promise<RemoveResult> {
  const { logger, registry, controlHome } = context;
  await assertNotGoManaged(controlHome);
  const target = await resolveTarget(context, options);
  const ref: WorktreeRef = { repositoryId: target.repositoryId, worktreeId: target.worktreeId, slug: target.slug };
  const host = worktreeHost(target.slug, target.domain);
  const layout = worktreeLayout(controlHome, target.repositoryId, target.worktreeId);
  const local = await registry.identities.repositoryId() === target.repositoryId;
  const namespace = await targetNamespace(context, target, local, options);
  const runtime = context.runtimeFor(namespace);

  // Everything that will go, verified before anything is touched. A resource
  // that cannot be proven ours fails here, before the prompt.
  const planned = await runtime.inventory(ref);
  const hasHome = await pathExists(layout.home) || await pathExists(layout.quarantinePath);
  await confirm({ nonInteractive: false }, describePlan(host, planned, hasHome), host, options.approved);

  const result = await withWorktreeMutation(context, ref, async () => {
    // The prompt was open for as long as a person took to answer it, and the
    // lock above may then have waited on another holder: checked again here,
    // right before the first write below, since the reads that follow are
    // local and fast.
    await assertNotGoManaged(controlHome);
    // The lock was free while the prompt was up: another removal may have
    // finished, or an `up` may have re-labelled the worktree's resources.
    const found = await registry.store(target.repositoryId).find(target.worktreeId);
    if (!found) {
      logger.info(`${host} was already removed while '${context.commandName} worktree remove' was waiting; nothing left to do.`);
      return { target, resources: [], home: false, registration: false, marker: false, alreadyGone: true };
    }
    if (found.labelNamespace !== undefined && found.labelNamespace !== namespace) {
      throw new CliError(
        `${host} was re-labelled under ${found.labelNamespace} while '${context.commandName} worktree remove' was waiting; run it again.`
      );
    }
    const identity = local ? await targetIdentity(context, target) : undefined;
    // `down` acts on the project name, so any target can be stopped gracefully
    // before its containers are reaped; skipped when there is nothing to stop.
    // Decided under the lock, so a container that appeared since the prompt
    // is stopped rather than killed.
    const current = await runtime.inventory(ref);
    if (current.some((resource) => resource.kind === 'container')) {
      await withProgress(logger, `Stopping ${host}`, () => runtime.down(ref));
    }
    const resources = await withProgress(logger, `Removing ${host}`, async () => {
      // Re-verified after `down`; every resource is checked before any is removed.
      const owned = await runtime.inventory(ref);
      // From here on data is destroyed. Should this be interrupted, the next
      // `up` must seed again rather than start on empty volumes.
      await registry.store(target.repositoryId).markPending(target.worktreeId, target.slug, logger);
      for (const resource of owned) await runtime.reap(ref, resource);
      // Everything now gone, in inventory order — what `down` took (containers
      // and their networks, which the second inventory no longer sees) together
      // with what was reaped — so the summary lists kinds the way the plan did.
      const gone = new Map(current.map((resource) => [`${resource.kind}:${resource.id}`, resource]));
      // `owned` is normally a subset of `current`; the second pass is for anything
      // that appeared between the two inventories and was reaped all the same.
      for (const resource of owned) gone.set(`${resource.kind}:${resource.id}`, resource);
      return [...gone.values()];
    });
    const home = await removeHome(layout, context.commandName);
    const registration = await registry.store(target.repositoryId).remove(target.worktreeId, target.slug, logger);
    let marker = false;
    if (identity) {
      await registry.identities.removeWorktreeMarker(identity);
      marker = true;
    }
    return { target, resources, home, registration, marker, alreadyGone: false };
  }, { exclusive: true });

  await context.index.refreshBestEffort('worktree remove');
  if (!result.alreadyGone) logger.info(`Removed ${host}: ${summarize(result)}.`);
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

/**
 * The target's Git identity among this repository's live worktrees. Only
 * meaningful for a local target; callers gate on that.
 */
async function targetIdentity(context: CommandContext, target: RegisteredWorktree): Promise<WorktreeIdentity | undefined> {
  return (await context.registry.identities.activeIdentities()).find((identity) => identity.worktreeId === target.worktreeId);
}

/**
 * Which labels the target's resources carry: the registration (deleted last,
 * so it survives an interrupted removal); then the index card, wherever the
 * home currently is; then, for a worktree of this repository, this config.
 * `--label-namespace` is for a worktree none of those describe — never an
 * override of one they do, because inventorying under the wrong namespace
 * finds nothing and would delete the registration that points at the data.
 */
async function targetNamespace(
  context: CommandContext,
  target: RegisteredWorktree,
  local: boolean,
  options: RemoveOptions
): Promise<string> {
  const flag = options.labelNamespace;
  if (flag !== undefined && !LABEL_NAMESPACE.test(flag)) {
    throw new CliError(`--label-namespace must look like a reverse domain, e.g. dev.example, not "${flag}".`);
  }
  const recorded = await recordedLabelNamespace(context.controlHome, target);
  if (recorded !== undefined) {
    if (flag !== undefined && flag !== recorded) {
      throw new CliError(
        `${worktreeHost(target.slug, target.domain)} records its resources as labelled under ${recorded}, not ${flag}. ` +
        'Drop --label-namespace, or pass the recorded one.'
      );
    }
    return recorded;
  }
  if (flag !== undefined) return flag;
  if (local) return labelNamespace(context.loaded.config);
  throw new CliError(
    `Cannot remove ${worktreeHost(target.slug, target.domain)} from this repository: it belongs to a different one, ` +
    'and nothing on disk records which Docker labels its resources carry. Run ' +
    `'${context.commandName} worktree remove' from a checkout of the repository that created it, or pass ` +
    `--label-namespace (${BRAND.labelNamespace} unless that project set project.label-namespace).`
  );
}

/**
 * Rename-then-delete. The rename is atomic, so a crash leaves either the home
 * or a `.deleting-` directory — never a half-deleted home that `up` might
 * adopt as complete. A leftover quarantine from an earlier attempt is finished
 * first.
 */
async function removeHome(layout: WorktreeLayout, commandName: string): Promise<boolean> {
  let removed = false;
  if (await pathExists(layout.quarantinePath)) {
    await deleteQuarantine(layout, commandName);
    removed = true;
  }
  if (await pathExists(layout.home)) {
    await assertSafeDirectoryPath(layout.controlHome, layout.home);
    await rename(layout.home, layout.quarantinePath);
    await deleteQuarantine(layout, commandName);
    removed = true;
  }
  return removed;
}

/**
 * A container that ran as another uid may have left files the user cannot
 * delete (a database's data directory, typically). Say what happened and how
 * to get out, rather than reporting a generic interrupted removal forever.
 * The recovery runs the same digest-pinned image the router stack already
 * pulled, as real root, on the quarantine directory alone.
 */
async function deleteQuarantine(layout: WorktreeLayout, commandName: string): Promise<void> {
  try {
    await removeWithin(layout.controlHome, layout.quarantinePath);
  } catch (error) {
    const failure = error as NodeJS.ErrnoException;
    if (failure.code !== 'EACCES' && failure.code !== 'EPERM') throw error;
    const where = failure.path === undefined ? '' : ` (first refusal: ${failure.path})`;
    // The container runs as root, so the owner must be spelled out as the
    // user's own uid:gid — a `$(id -u)` evaluated inside the container would say 0.
    const uid = process.getuid?.();
    const gid = process.getgid?.();
    const owner = uid === undefined || gid === undefined ? '$(id -u):$(id -g)' : `${uid}:${gid}`;
    throw new CliError(
      `Could not delete ${layout.quarantinePath}${where}. Some of its files are most likely owned ` +
      'by another user — written by a container running as a different uid. Take ownership back, ' +
      'for example with\n\n' +
      `  docker run --rm --network none -v ${shellQuote(`${layout.quarantinePath}:/d`)} ${INDEX_IMAGE} ` +
      `sh -c "chown -R ${owner} /d && chmod -R u+rwX /d"\n\n` +
      `then run '${commandName} worktree remove' again to finish.`
    );
  }
}

function countResources(resources: readonly OwnedResource[]): string {
  const counts = new Map<string, number>();
  for (const resource of resources) counts.set(resource.kind, (counts.get(resource.kind) ?? 0) + 1);
  if (counts.size === 0) return 'no Docker resources';
  return [...counts]
    .map(([kind, count]) => `${count} ${kind}${count === 1 ? '' : 's'}`)
    .join(', ');
}

export function describePlan(host: string, planned: readonly OwnedResource[], hasHome: boolean): string {
  const lines = [
    // Filtered here, where it is borrowed: a hostname that carried a line
    // break would otherwise write its own line into this list.
    `This will permanently remove ${printable(host)}:`,
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
