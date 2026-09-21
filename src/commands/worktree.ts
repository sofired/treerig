import { BRAND } from '../brand.ts';
import type { ProjectConfig } from '../config/schema.ts';
import { CliError } from '../errors.ts';
import { secureWorktreeDirectories } from '../files.ts';
import { worktreeEnvironment } from '../hooks/protocol.ts';
import { runLifecycle, type HookContext } from '../hooks/runner.ts';
import { worktreeHost, worktreeLayout } from '../layout.ts';
import { withScopedLock, worktreeLockTarget } from '../operation-lock.ts';
import { withProgress } from '../progress.ts';
import type { ServiceStatus, WorktreeRef } from '../runtime/runtime.ts';
import type { RegisteredWorktree, WorktreeRegistration } from '../worktree-registry.ts';
import type { CommandContext } from './context.ts';
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
  const registration = await registry.ensure();
  await context.router.ensure();
  const ref = toRef(registration);
  const store = registry.store(ref.repositoryId);
  const services = (options.services ?? []).map((name) => ({ name }));

  const hooks = await withWorktreeMutation(context, ref, async () => {
    await secureWorktreeDirectories(worktreeLayout(context.controlHome, ref.repositoryId, ref.worktreeId));
    const what = services.length === 0 ? 'all services' : services.map((s) => s.name).join(', ');
    await withProgress(logger, `Starting ${what} for ${worktreeHost(ref.slug, ref.domain)}`, () =>
      runtime.up(ref, services, { signal: runner.cancellationSignal })
    );
    const firstTime = await store.initialization(ref.worktreeId, ref.slug) === 'pending';
    const ran = await runLifecycle(hookContext(context, ref), {
      stages: firstTime ? ['prepare', 'seed'] : ['prepare'],
      cancelSignal: runner.cancellationSignal
    });
    await store.completeInitialization(ref.worktreeId, ref.slug, logger);
    return ran;
  });

  const urls = worktreeUrls(context.loaded.config, ref, context.router.port);
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
    lockOptions(context)
  );
  logger.info(`${worktreeHost(ref.slug, ref.domain)} is down.`);
}

/** Per-service state, or `undefined` when this worktree has never been brought up. */
export async function status(context: CommandContext): Promise<ServiceStatus[] | undefined> {
  const { logger, runtime, registry } = context;
  const registration = await registry.current();
  if (!registration) {
    logger.output(`not registered — run '${context.commandName} up' to create this worktree's environment`);
    return undefined;
  }
  const ref = toRef(registration);
  const statuses = await runtime.status(ref);
  logger.output(worktreeHost(ref.slug, ref.domain));
  if (statuses.length === 0) {
    logger.output('  (no services)');
  }
  const width = Math.max(0, ...statuses.map((entry) => entry.service.length));
  for (const entry of statuses) {
    const detail = entry.detail ? `  (${entry.detail})` : '';
    logger.output(`  ${entry.service.padEnd(width)}  ${entry.state}${detail}`);
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

function hookContext(context: CommandContext, ref: WorktreeRef & { domain: string }): HookContext {
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
    context.logger.output(`${entry.service.padEnd(width)}  ${entry.url}`);
  }
}
