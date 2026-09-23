import { BRAND } from '../brand.ts';
import { recordedLabelNamespace, writeIndexCard } from '../checkout-index/card.ts';
import type { ProjectConfig } from '../config/schema.ts';
import { CliError } from '../errors.ts';
import { secureWorktreeDirectories } from '../files.ts';
import { LIFECYCLE_ORDER, UP_STAGES, worktreeEnvironment, type LifecycleStage } from '../hooks/protocol.ts';
import { runLifecycle, type HookContext } from '../hooks/runner.ts';
import { worktreeHost, worktreeLayout } from '../layout.ts';
import { withScopedLock, worktreeLockTarget } from '../operation-lock.ts';
import { withProgress } from '../progress.ts';
import { printable } from '../text.ts';
import type { ServiceStatus, WorktreeRef } from '../runtime/runtime.ts';
import type { RegisteredWorktree, WorktreeRegistration } from '../worktree-registry.ts';
import { labelNamespace, type CommandContext } from './context.ts';
import { lockOptions, withWorktreeMutation } from './mutation.ts';

// The four everyday verbs. `up` is the only one that mutates the registry —
// through `ensure()` — and the only one that runs hooks; the rest read the
// record as it stands and never repair it.

export interface ServiceUrl {
  service: string;
  url: string;
  label?: string;
  purpose?: string;
}

/** Pure: the URL each routed service will answer on, in config order. */
export function worktreeUrls(
  config: ProjectConfig,
  registration: Pick<RegisteredWorktree, 'slug' | 'domain'>,
  port = 80
): ServiceUrl[] {
  const host = worktreeHost(registration.slug, registration.domain);
  const suffix = port === 80 ? '' : `:${port}`;
  return config.service.map((service) => ({
    service: service.name,
    url: `http://${service.subdomain ? `${service.subdomain}.` : ''}${host}${suffix}${service.path ?? ''}`,
    ...(service.index ? { label: service.index.label, purpose: service.index.purpose } : {})
  }));
}

export interface UpOptions {
  /** Compose service names to start; empty means all of them. */
  services?: readonly string[];
}

/** The stages `up` asks the lifecycle sequencer for, in its own terms. */
export function lifecycleStages(firstTime: boolean): LifecycleStage[] {
  return LIFECYCLE_ORDER.filter((stage) =>
    UP_STAGES.includes(stage) && (firstTime || stage !== 'seed')
  );
}

export interface UpResult {
  registration: WorktreeRegistration;
  /** Hook stages that ran, in order. */
  hooks: string[];
  urls: ServiceUrl[];
}

/**
 * Register (or reconcile), make sure the shared router is up, start, then
 * run hooks: `prepare` on every `up`, `seed` only the first time this
 * worktree comes up. The worktree lock is held for all of it, and an
 * interrupt is deferred until the lock can be released cleanly — a
 * half-started environment is never left locked.
 */
export async function up(context: CommandContext, options: UpOptions = {}): Promise<UpResult> {
  const { logger, runner, runtime, registry } = context;
  await refuseRelabelling(context);
  const registration = await registry.ensure();
  await context.router.ensure();
  const ref = toRef(registration);
  const store = registry.store(ref.repositoryId);
  const services = (options.services ?? []).map((name) => ({ name }));

  const urls = worktreeUrls(context.loaded.config, ref, context.router.port);
  const hooks = await withWorktreeMutation(context, ref, async () => {
    // The router may have taken minutes to start, and the lock was free
    // meanwhile: a removal can have finished. Nothing is created unless the
    // registration and the identity are still the ones `ensure()` saw.
    const found = await store.find(ref.worktreeId);
    if (!found) {
      throw new CliError(
        `${worktreeHost(ref.slug, ref.domain)} was removed while '${context.commandName} up' was waiting for the shared router; ` +
        'run it again to re-register.'
      );
    }
    if (found.labelNamespace !== undefined && found.labelNamespace !== labelNamespace(context.loaded.config)) {
      throw new CliError(
        `${worktreeHost(ref.slug, ref.domain)} was re-labelled under ${found.labelNamespace} while '${context.commandName} up' ` +
        'was waiting for the shared router; run it again.'
      );
    }
    const identity = await registry.identities.inspect();
    if (identity?.worktreeId !== ref.worktreeId) {
      throw new CliError(
        `This checkout's ${BRAND.name} markers changed or were removed while '${context.commandName} up' was waiting ` +
        'for the shared router; run it again.'
      );
    }
    const firstTime = await store.initialization(ref.worktreeId, ref.slug) === 'pending';
    await secureWorktreeDirectories(worktreeLayout(context.controlHome, ref.repositoryId, ref.worktreeId));
    await writeIndexCard(context.controlHome, ref.repositoryId, ref.worktreeId, {
      version: 1,
      project: context.loaded.config.project.name,
      domain: ref.domain,
      slug: ref.slug,
      host: worktreeHost(ref.slug, ref.domain),
      root: registration.root,
      branch: registration.branch,
      labelNamespace: labelNamespace(context.loaded.config),
      services: urls.flatMap((entry) => entry.label === undefined
        ? []
        : [{ name: entry.service, url: entry.url, label: entry.label, purpose: entry.purpose ?? '' }]),
      updatedAt: new Date().toISOString()
    });
    const what = services.length === 0 ? 'all services' : services.map((s) => s.name).join(', ');
    await withProgress(logger, `Starting ${what} for ${worktreeHost(ref.slug, ref.domain)}`, () =>
      runtime.up(ref, services, { signal: runner.cancellationSignal })
    );
    const ran = await runLifecycle(hookContext(context, ref), {
      // Everything invoked, or everything but the one that seeds once. The
      // lifecycle sequencer only knows its own three stages, so the list is
      // narrowed to those it can be asked for.
      stages: lifecycleStages(firstTime),
      cancelSignal: runner.cancellationSignal
    });
    await store.completeInitialization(ref.worktreeId, ref.slug, logger);
    return ran;
  });

  await context.index.refreshBestEffort('up');
  logger.info(`${worktreeHost(ref.slug, ref.domain)} is up.`);
  printUrls(context, urls);
  return { registration, hooks, urls };
}

/** Stops the worktree's services. Data survives; `worktree remove` is the destructive verb. */
export async function down(context: CommandContext): Promise<void> {
  const { logger, runtime } = context;
  const registration = await requireRegistration(context);
  const ref = toRef(registration);
  await withScopedLock(
    worktreeLockTarget(context.controlHome, ref.repositoryId, ref.worktreeId),
    `worktree ${ref.slug}`,
    logger,
    () => withProgress(logger, `Stopping ${worktreeHost(ref.slug, ref.domain)}`, () => runtime.down(ref)),
    lockOptions(context, ref)
  );
  await context.index.refreshBestEffort('down');
  logger.info(`${worktreeHost(ref.slug, ref.domain)} is down.`);
}

/** Per-service state, or `undefined` when this worktree has never been brought up. */
export async function status(context: CommandContext): Promise<ServiceStatus[] | undefined> {
  const { logger, runtime, registry } = context;
  const registration = await registry.current();
  if (!registration) {
    // Prose, not data: stdout stays empty, which is the honest answer.
    logger.info(`Not registered — run '${context.commandName} up' to create this worktree's environment.`);
    return undefined;
  }
  const ref = toRef(registration);
  const statuses = await runtime.status(ref);
  logger.output(printable(worktreeHost(ref.slug, ref.domain)));
  if (statuses.length === 0) {
    logger.output('  (no services)');
  }
  const width = Math.max(0, ...statuses.map((entry) => entry.service.length));
  for (const entry of statuses) {
    const detail = entry.detail ? `  (${entry.detail})` : '';
    // The service names come from the branch's own compose file.
    logger.output(printable(`  ${entry.service.padEnd(width)}  ${entry.state}${detail}`));
  }
  return statuses;
}

export async function urls(context: CommandContext): Promise<ServiceUrl[]> {
  const registration = await requireRegistration(context);
  const list = worktreeUrls(context.loaded.config, registration, context.router.port);
  if (list.length === 0) {
    context.logger.warn(`No [[service]] entries in ${BRAND.configFile}; nothing is routed.`);
  }
  printUrls(context, list);
  return list;
}

/* ------------------------------------------------------------------ helpers */

/**
 * Compose relabels containers on the next `up` but never volumes or networks,
 * so once resources exist under one namespace the record must keep naming
 * it — otherwise `remove` could no longer find them. A changed
 * `label-namespace` is therefore honoured only for a worktree that owns
 * nothing under the old one; anything else must be removed first.
 *
 * Checked before `ensure()` rewrites the record, and so before the worktree
 * lock. Two `up`s racing from one checkout with a config edit between them
 * could still slip resources in under the old namespace; the under-lock
 * re-check catches the record changing, not that narrower case.
 */
async function refuseRelabelling(context: CommandContext): Promise<void> {
  // The record is read directly: `current()` would add a domain-mismatch hint
  // to run `up`, which is what is running; `ensure()` handles that next.
  const identity = await context.registry.identities.inspect();
  if (!identity) return;
  const current = await context.registry.store(identity.repositoryId).find(identity.worktreeId);
  if (!current) return;
  const own = labelNamespace(context.loaded.config);
  // Records from before the namespace was tracked still have a card saying what `up` used.
  const recorded = await recordedLabelNamespace(context.controlHome, current);
  if (recorded === undefined || recorded === own) return;
  const leftovers = await context.runtimeFor(recorded).inventory(toRef(current));
  if (leftovers.length === 0) return;
  throw new CliError(
    `This worktree's resources are labelled under ${recorded}, but ${BRAND.configFile} now says ${own}. ` +
    `Run '${context.commandName} worktree remove' first — it uses the recorded labels — then '${context.commandName} up' again.`
  );
}

function toRef(registration: RegisteredWorktree): WorktreeRef & Pick<RegisteredWorktree, 'domain'> {
  return {
    repositoryId: registration.repositoryId,
    worktreeId: registration.worktreeId,
    slug: registration.slug,
    domain: registration.domain
  };
}

async function requireRegistration(context: CommandContext): Promise<WorktreeRegistration> {
  const registration = await context.registry.current();
  if (!registration) {
    throw new CliError(`This worktree is not registered. Run '${context.commandName} up' first.`);
  }
  return registration;
}

export function hookContext(
  context: Pick<CommandContext, 'loaded' | 'controlHome' | 'logger' | 'runner'>,
  ref: WorktreeRef & { domain: string }
): HookContext {
  const { loaded } = context;
  return {
    environment: worktreeEnvironment({
      projectName: loaded.config.project.name,
      // The registered domain, not the config's: hooks address the hostname that is actually routed.
      domain: ref.domain,
      controlHome: context.controlHome,
      configFile: loaded.file,
      projectRoot: loaded.root
    }, ref),
    hooks: loaded.config.hooks,
    logger: context.logger,
    runner: context.runner
  };
}

function printUrls(context: CommandContext, list: readonly ServiceUrl[]): void {
  const width = Math.max(0, ...list.map((entry) => entry.service.length));
  for (const entry of list) {
    context.logger.output(printable(`${entry.service.padEnd(width)}  ${entry.url}`));
  }
}
