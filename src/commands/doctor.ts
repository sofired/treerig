import { BRAND } from '../brand.ts';
import { findConfigFile, loadConfig, type LoadedConfig } from '../config/load.ts';
import { dockerSaid, DockerUnavailableError, runDocker } from '../docker.ts';
import { CliError, OperationCancelledError } from '../errors.ts';
import { assertAdoptableControlHome, pathExists } from '../files.ts';
import { runHook } from '../hooks/runner.ts';
import { worktreeHost } from '../layout.ts';
import type { Logger } from '../logger.ts';
import { describeLock, formatDuration, holderLabel, lockedWorktrees, registryLockTarget, routerLockTarget, worktreeLocks, worktreeUnlockCommand } from '../operation-lock.ts';
import type { CommandRunner, RunResult } from '../process-runner.ts';
import { routerStatusReport, type RouterStatus } from '../router/router.ts';
import { INDEX_HOST, ROUTED_BY_LABEL } from '../router/stack.ts';
import { COMPOSE_PROJECT_LABEL, COMPOSE_SERVICE_LABEL } from '../runtime/compose.ts';
import { shellQuote } from '../shell.ts';
import { oneLine, printable } from '../text.ts';
import { scanRegistrations, type RegistryScan, type WorktreeRegistration, type WorktreeRegistry } from '../worktree-registry.ts';
import { checkoutRegistry } from './context.ts';
import { hookContext } from './worktree.ts';

// Read-only, start to finish: no lock is taken, nothing is written, and
// nothing is repaired. Every check ends as a line in the report — a check
// that throws is reported as failed rather than ending the command, since the
// point of a diagnosis is to see everything that is wrong at once.

/** `engines.node` in package.json; a unit test keeps the two identical. */
export const NODE_ENGINE = '>=24.14.1 <25';
/** `git worktree list --porcelain -z` arrived in Git 2.36. */
export const GIT_MINIMUM = '2.36';
/** The oldest Compose the README promises to work with. */
export const COMPOSE_MINIMUM = '2.21';
/** How long Docker gets to answer before it counts as unavailable. */
export const DOCKER_PROBE_TIMEOUT_MS = 30_000;

export type CheckStatus = 'ok' | 'warn' | 'fail' | 'skip';

export interface Check {
  status: CheckStatus;
  subject: string;
  detail: string;
  /** A failure that carries its own exit code — the verify hook's — makes it the command's. */
  exitCode?: number;
}

export interface Section {
  title: string;
  checks: Check[];
}

/** Where the checkout section comes from: no config at all, one that does not load, or one that does. */
export type CheckoutSource =
  | { kind: 'none'; searchedFrom: string }
  | { kind: 'unloadable'; error: Error }
  | { kind: 'loaded'; loaded: LoadedConfig };

export interface RouterSource {
  status(): Promise<RouterStatus>;
  readonly network: string;
}

export type CheckoutRegistry = Pick<WorktreeRegistry, 'current' | 'pruneCandidates'>;

export interface DoctorOptions {
  commandName: string;
  controlHome: string;
  runner: CommandRunner;
  logger: Logger;
  /** The shared router, or why one could not even be described (a bad port in the environment). */
  router: RouterSource | Error;
  checkout: CheckoutSource;
  /** Defaults to the running Node. */
  nodeVersion?: string;
  /** Defaults to the registry the other commands use for this checkout. */
  registryFor?(loaded: LoadedConfig): CheckoutRegistry;
  invocation?: string;
}

/* ------------------------------------------------------------ versions ---- */

export interface Version {
  /** As the tool printed it. */
  text: string;
  parts: [number, number, number];
}

/** The first `major.minor[.patch]` in a tool's version output. */
export function readVersion(output: string): Version | undefined {
  const match = /(\d+)\.(\d+)(?:\.(\d+))?/.exec(output);
  if (!match) return undefined;
  return { text: match[0], parts: [Number(match[1]), Number(match[2]), Number(match[3] ?? 0)] };
}

export function atLeast(version: readonly number[], minimum: readonly number[]): boolean {
  for (let index = 0; index < Math.max(version.length, minimum.length); index++) {
    const have = version[index] ?? 0;
    const need = minimum[index] ?? 0;
    if (have !== need) return have > need;
  }
  return true;
}

/** Whether `version` satisfies a range of the one form package.json uses: `>=A.B.C <N`. */
export function satisfiesEngine(version: string, range: string): boolean {
  const match = /^>=(\d+\.\d+\.\d+) <(\d+)$/.exec(range);
  if (!match) throw new Error(`Unsupported engines range: ${range}`);
  const have = readVersion(version);
  const floor = readVersion(match[1] ?? '');
  return have !== undefined && floor !== undefined &&
    atLeast(have.parts, floor.parts) && have.parts[0] < Number(match[2]);
}

export function nodeCheck(version: string, commandName: string, range = NODE_ENGINE): Check {
  return satisfiesEngine(version, range)
    ? { status: 'ok', subject: 'Node.js', detail: version }
    : { status: 'fail', subject: 'Node.js', detail: `${version}; ${commandName} needs ${range}` };
}

/** A tool's version output against the oldest version that works. */
export function versionCheck(subject: string, output: string, minimum: string): Check {
  const version = readVersion(output);
  const floor = readVersion(minimum);
  if (version === undefined || floor === undefined) {
    return { status: 'fail', subject, detail: `could not read a version from "${oneLine(output, 80)}"` };
  }
  return atLeast(version.parts, floor.parts)
    ? { status: 'ok', subject, detail: version.text }
    : { status: 'fail', subject, detail: `${version.text}; ${minimum} or newer is needed` };
}

/* ------------------------------------------------------- prerequisites ---- */

async function gitCheck(runner: CommandRunner): Promise<Check> {
  const result = await runner.run('git', ['--version'], { reject: false });
  if (result.exitCode !== 0) {
    return { status: 'fail', subject: 'Git', detail: `\`git --version\` failed: ${oneLine(result.stderr.trim() || `exit ${result.exitCode}`)}` };
  }
  return versionCheck('Git', result.stdout, GIT_MINIMUM);
}

/** One `docker` query, bounded in time; Docker being unavailable is its answer, not an exception. */
async function askDocker(runner: CommandRunner, subject: string, args: readonly string[]): Promise<Check | RunResult> {
  let result: RunResult;
  try {
    result = await runDocker(runner, args, { reject: false, timeoutMs: DOCKER_PROBE_TIMEOUT_MS });
  } catch (error) {
    if (error instanceof DockerUnavailableError) return { status: 'fail', subject, detail: error.message };
    throw error;
  }
  if (result.timedOut) {
    return { status: 'fail', subject, detail: `\`docker ${args.join(' ')}\` did not answer within ${DOCKER_PROBE_TIMEOUT_MS / 1000} s` };
  }
  if (result.exitCode !== 0) {
    return { status: 'fail', subject, detail: oneLine(dockerSaid(result.stderr, `\`docker ${args.join(' ')}\` exited ${result.exitCode}`)) };
  }
  return result;
}

/** The Docker lines, and whether each half of Docker answered at all — what the router checks need. */
interface DockerState {
  checks: Check[];
  engineAnswered: boolean;
  composeAnswered: boolean;
}

async function dockerChecks(runner: CommandRunner): Promise<DockerState> {
  if (!await runner.which('docker')) {
    return {
      checks: [
        { status: 'fail', subject: 'Docker Engine', detail: 'the `docker` command is not on PATH; is Docker installed?' },
        { status: 'skip', subject: 'Docker Compose', detail: 'needs the `docker` command' }
      ],
      engineAnswered: false,
      composeAnswered: false
    };
  }
  // Together, so a Docker that answers neither costs one timeout rather than two.
  const [engine, compose] = await Promise.all([
    askDocker(runner, 'Docker Engine', ['version', '--format', '{{.Server.Version}}']),
    askDocker(runner, 'Docker Compose', ['compose', 'version', '--short'])
  ]);
  return {
    checks: [
      'status' in engine ? engine : { status: 'ok', subject: 'Docker Engine', detail: `${oneLine(engine.stdout.trim(), 80)}, daemon answering` },
      'status' in compose ? compose : versionCheck('Docker Compose', compose.stdout, COMPOSE_MINIMUM)
    ],
    engineAnswered: !('status' in engine),
    composeAnswered: !('status' in compose)
  };
}

/* ------------------------------------------------------------ registry ---- */

async function controlHomeCheck(controlHome: string): Promise<Check> {
  const subject = 'control home';
  if (!await pathExists(controlHome)) {
    return { status: 'ok', subject, detail: `${controlHome} (not created yet; the first up creates it)` };
  }
  await assertAdoptableControlHome(controlHome);
  return { status: 'ok', subject, detail: controlHome };
}

/**
 * What the registry says about itself: records that cannot be read, and
 * hostnames that more than one worktree claims — including the index's own,
 * which no worktree can share with it.
 */
export function registryChecks(scan: RegistryScan, commandName: string): Check[] {
  const checks: Check[] = [{
    status: 'ok',
    subject: 'registrations',
    detail: scan.records.length === 0 ? 'none yet' : `${scan.records.length} readable`
  }];
  for (const problem of scan.problems) {
    checks.push({ status: 'fail', subject: 'unreadable record', detail: problem.reason });
  }
  for (const [host, claimants] of scan.conflicts) {
    checks.push({
      status: 'fail',
      subject: host,
      // Records, not traffic: which of them is up, and so reachable, is the Router section's to say.
      detail: `registered to ${claimants.length} worktrees (${oneLine(claimants.map((record) => record.worktreeId).join(', '), 200)}); ` +
        `requests for it may reach any of them that is running. Remove all but one with '${commandName} worktree remove <uuid>'.`
    });
  }
  for (const record of scan.records) {
    if (worktreeHost(record.slug, record.domain) !== INDEX_HOST) continue;
    checks.push({
      status: 'fail',
      subject: INDEX_HOST,
      detail: `is the checkout index's own address, and worktree ${record.worktreeId} is registered to it. ` +
        `Set another domain in that checkout's ${BRAND.configFile} and run '${commandName} up' there.`
    });
  }
  return checks;
}

/* --------------------------------------------------------------- locks ---- */

interface HeldLock {
  subject: string;
  target: string;
  description: string;
  /** What to run once its holder is dead; absent where only the hand-removal advice applies. */
  unlock?: string;
}

/**
 * Every lock there is a `.lock` directory for: the registry's, the router's,
 * and each worktree's operation and development locks. Leftover records,
 * recovery mutexes and temporary files are not locks anyone waits on.
 */
async function heldLocks(controlHome: string, commandName: string, scan: RegistryScan): Promise<HeldLock[]> {
  const found: HeldLock[] = [];
  for (const [subject, target, description] of [
    ['registry', registryLockTarget(controlHome), 'global worktree registry'],
    ['router', routerLockTarget(controlHome), 'shared router']
  ] as const) {
    if (await pathExists(`${target}.lock`)) found.push({ subject, target, description });
  }
  const records = new Map(scan.records.map((record) => [`${record.repositoryId}/${record.worktreeId}`, record]));
  for (const { repositoryId, worktreeId } of await lockedWorktrees(controlHome)) {
    const record = records.get(`${repositoryId}/${worktreeId}`);
    // Named as unlock names them, so a refusal here reads the same as one there.
    const name = record?.slug ?? worktreeId;
    const shown = record ? worktreeHost(record.slug, record.domain) : `worktree ${worktreeId}`;
    for (const lock of worktreeLocks(controlHome, repositoryId, worktreeId, name)) {
      if (!await pathExists(`${lock.target}.lock`)) continue;
      found.push({
        subject: lock.kind === 'operation' ? shown : `${shown} (development session)`,
        target: lock.target,
        description: lock.description,
        unlock: `'${worktreeUnlockCommand(commandName, worktreeId)}'`
      });
    }
  }
  return found;
}

/**
 * Who holds what, read without taking or touching anything. A live holder is
 * an answer, not a problem; a silent one blocks every command that needs its
 * lock, and the report says how to clear it safely.
 */
async function lockChecks(controlHome: string, commandName: string, now: () => number = Date.now): Promise<Check[]> {
  const checks: Check[] = [];
  for (const lock of await heldLocks(controlHome, commandName, await scanRegistrations(controlHome))) {
    const described = await describeLock(lock.target, lock.description);
    if (described.state === 'missing') continue;   // released while looking
    const holder = holderLabel(described.record);
    const age = described.heartbeatMs === undefined ? 'unknown' : formatDuration(now() - described.heartbeatMs);
    if (described.state === 'active') {
      checks.push({ status: 'ok', subject: lock.subject, detail: `held by ${holder}; heartbeat ${age} ago` });
      continue;
    }
    const remedy = lock.unlock === undefined
      ? `If that process is gone and nothing it started is still running, remove ${shellQuote(described.lockDirectory)} with rmdir.`
      : `Run ${lock.unlock} to check that it, and every command it started on this machine, is gone, and to clear it.`;
    checks.push({
      status: 'fail',
      subject: lock.subject,
      detail: `held by ${holder}, whose heartbeat has been silent for ${age}; every command that needs it waits. ${remedy}`
    });
  }
  return checks.length === 0 ? [{ status: 'ok', subject: 'locks', detail: 'none held' }] : checks;
}

/* -------------------------------------------------------------- router ---- */

/** The router in the words `router status` uses; not running is an answer, not a problem. */
export function routerChecks(status: RouterStatus): Check[] {
  const report = routerStatusReport(status);
  if (!status.reachable) return report.errors.map((error) => ({ status: 'fail', subject: 'router', detail: error }));
  if (!report.failed) return [{ status: 'ok', subject: 'router', detail: report.output.join('; ') }];
  return status.problems.map((problem) => ({ status: 'fail', subject: 'router', detail: problem }));
}

export interface RoutedContainer {
  name: string;
  labels: Record<string, string>;
}

const ROUTER_RULE = /^traefik\.http\.routers\.([^.]+)\.rule$/;

/**
 * Rules that more than one claimant routes, each with the containers behind
 * them. Replicas of one Compose service are one claimant: they share a router
 * name *and* the project and service Compose stamped on them. A router name
 * alone is only a label, and any container can copy one — Traefik would then
 * balance that host's requests onto it, which is exactly the conflict to see.
 */
export function routeConflicts(containers: readonly RoutedContainer[]): Map<string, string[]> {
  const byRule = new Map<string, { claimants: Set<string>; containers: Set<string> }>();
  for (const container of containers) {
    const origin = [container.labels[COMPOSE_PROJECT_LABEL] ?? '', container.labels[COMPOSE_SERVICE_LABEL] ?? ''];
    for (const [key, rule] of Object.entries(container.labels)) {
      const router = ROUTER_RULE.exec(key)?.[1];
      if (router === undefined) continue;
      const claim = byRule.get(rule) ?? { claimants: new Set<string>(), containers: new Set<string>() };
      claim.claimants.add(JSON.stringify([router, ...origin]));
      claim.containers.add(container.name);
      byRule.set(rule, claim);
    }
  }
  return new Map(
    [...byRule]
      .filter(([, claim]) => claim.claimants.size > 1)
      .map(([rule, claim]) => [rule, [...claim.containers].sort()])
  );
}

/** Parses `docker inspect --format '{{.Name}}\t{{json .Config.Labels}}'`, one container per line. */
export function parseRoutedContainers(stdout: string): RoutedContainer[] {
  const containers: RoutedContainer[] = [];
  for (const line of stdout.split('\n')) {
    const tab = line.indexOf('\t');
    if (tab < 0) continue;
    let labels: unknown;
    try {
      labels = JSON.parse(line.slice(tab + 1));
    } catch {
      continue;
    }
    if (labels === null || typeof labels !== 'object') continue;
    containers.push({
      name: line.slice(0, tab).replace(/^\//, ''),
      labels: Object.fromEntries(Object.entries(labels).filter((entry): entry is [string, string] => typeof entry[1] === 'string'))
    });
  }
  return containers;
}

/**
 * Every running container the router would route, read from Docker rather
 * than from any registry: the one view that sees worktrees of every control
 * home, and hostnames that collide only once a domain and a slug are joined.
 */
async function routeChecks(runner: CommandRunner, network: string): Promise<Check[]> {
  const listed = await askDocker(runner, 'routes', ['ps', '-q', '--filter', `label=${ROUTED_BY_LABEL}=${network}`]);
  if ('status' in listed) return [listed];
  const ids = listed.stdout.split(/\s+/).filter(Boolean);
  if (ids.length === 0) return [{ status: 'ok', subject: 'routes', detail: 'no running container is routed' }];
  // A container that stops between the two calls is simply not in the answer.
  const inspected = await runDocker(runner, ['inspect', '--format', '{{.Name}}\t{{json .Config.Labels}}', ...ids], {
    reject: false, timeoutMs: DOCKER_PROBE_TIMEOUT_MS
  });
  // Either way the output may stop part-way, and a partial list cannot show there is no conflict.
  if (inspected.outputOverflow) {
    return [{ status: 'fail', subject: 'routes', detail: '`docker inspect` produced more output than can be read, so routes were not checked' }];
  }
  if (inspected.timedOut) {
    return [{ status: 'fail', subject: 'routes', detail: `\`docker inspect\` did not answer within ${DOCKER_PROBE_TIMEOUT_MS / 1000} s, so routes were not checked` }];
  }
  const containers = parseRoutedContainers(inspected.stdout);
  if (containers.length === 0 && inspected.exitCode !== 0) {
    return [{ status: 'fail', subject: 'routes', detail: oneLine(dockerSaid(inspected.stderr, `\`docker inspect\` exited ${inspected.exitCode}`)) }];
  }
  const conflicts = routeConflicts(containers);
  if (conflicts.size === 0) {
    return [{ status: 'ok', subject: 'routes', detail: `${containers.length} routed container(s); no two routers share a rule` }];
  }
  return [...conflicts].map(([rule, names]) => ({
    status: 'fail',
    subject: 'routes',
    // Any container can carry a rule label, of any length.
    detail: `${oneLine(rule, 200)} is claimed by ${oneLine(names.join(', '), 200)}; requests for it may reach any of them`
  }));
}

/* ------------------------------------------------------------ checkout ---- */

/** The config the other commands would use here, without failing when there is none. */
export async function findCheckout(explicit: string | undefined, cwd: string): Promise<CheckoutSource> {
  try {
    const file = explicit ?? await findConfigFile(cwd);
    if (file === undefined) return { kind: 'none', searchedFrom: cwd };
    return { kind: 'loaded', loaded: await loadConfig(file) };
  } catch (error) {
    if (error instanceof Error) return { kind: 'unloadable', error };
    throw error;
  }
}

async function checkoutChecks(options: DoctorOptions, loaded: LoadedConfig): Promise<Check[]> {
  const { commandName, controlHome, logger, runner } = options;
  const checks: Check[] = [{ status: 'ok', subject: BRAND.configFile, detail: loaded.file }];

  // Each step on its own: a checkout Git cannot identify still has a config
  // worth reporting, and its verify hook still deserves a line saying why it
  // did not run.
  let registry: CheckoutRegistry | undefined;
  let registration: WorktreeRegistration | undefined;
  let identified = true;
  try {
    registry = options.registryFor?.(loaded) ??
      checkoutRegistry({ loaded, controlHome, commandName, logger, runner, invocation: options.invocation ?? commandName });
    registration = await registry.current();
    checks.push(registration
      ? { status: 'ok', subject: 'this worktree', detail: `registered as ${worktreeHost(registration.slug, registration.domain)}` }
      : { status: 'skip', subject: 'this worktree', detail: `not registered; '${commandName} up' registers it` });
  } catch (error) {
    if (error instanceof OperationCancelledError) throw error;
    identified = false;
    checks.push(failure('this worktree', error));
  }

  const found = registry;
  if (found) {
    checks.push(...await guarded('missing checkouts', async () => (await found.pruneCandidates()).map((record): Check => ({
      status: 'warn',
      subject: worktreeHost(record.slug, record.domain),
      detail: `registered, but Git no longer lists its checkout. Remove what it left with '${commandName} worktree remove ${record.worktreeId}'.`
    }))));
  }

  const command = loaded.config.hooks.verify;
  if (command === undefined) {
    checks.push({ status: 'skip', subject: 'verify hook', detail: 'none configured' });
  } else if (!identified) {
    checks.push({ status: 'skip', subject: 'verify hook', detail: 'needs this worktree to be identified' });
  } else if (!registration) {
    checks.push({ status: 'skip', subject: 'verify hook', detail: `needs a registered worktree; run '${commandName} up' first` });
  } else {
    try {
      // Its output goes to stderr: stdout is this report.
      await runHook('verify', hookContext({ loaded, controlHome, logger, runner }, registration), { printOutput: 'stderr' });
      checks.push({ status: 'ok', subject: 'verify hook', detail: `${command} exited 0` });
    } catch (error) {
      if (error instanceof OperationCancelledError) throw error;
      // The hook's own verdict carries its exit code; anything else is a failure to run it at all.
      checks.push(error instanceof CliError
        ? { status: 'fail', subject: 'verify hook', detail: error.message, exitCode: error.exitCode }
        : failure('verify hook', error));
    }
  }
  return checks;
}

/* -------------------------------------------------------------- report ---- */

function failure(subject: string, error: unknown): Check {
  return { status: 'fail', subject, detail: error instanceof Error ? error.message : String(error) };
}

/** Runs a group of checks; one that throws becomes a failed line instead of ending the report. */
async function guarded(subject: string, run: () => Promise<Check[]>): Promise<Check[]> {
  try {
    return await run();
  } catch (error) {
    if (error instanceof OperationCancelledError) throw error;
    return [failure(subject, error)];
  }
}

export async function doctor(options: DoctorOptions): Promise<Section[]> {
  const { commandName, controlHome, runner } = options;

  let docker: DockerState;
  try {
    docker = await dockerChecks(runner);
  } catch (error) {
    if (error instanceof OperationCancelledError) throw error;
    docker = { checks: [failure('Docker', error)], engineAnswered: false, composeAnswered: false };
  }
  const prerequisites: Section = {
    title: 'Prerequisites',
    checks: [
      nodeCheck(options.nodeVersion ?? process.versions.node, commandName),
      ...await guarded('Git', async () => [await gitCheck(runner)]),
      ...docker.checks
    ]
  };

  const registry: Section = {
    title: 'Registry',
    checks: [
      ...await guarded('control home', async () => [await controlHomeCheck(controlHome)]),
      ...await guarded('registrations', async () => registryChecks(await scanRegistrations(controlHome), commandName))
    ]
  };

  const { router: source } = options;
  const router: Section = {
    title: 'Router',
    checks: source instanceof Error
      ? [{ status: 'fail', subject: 'router', detail: source.message }]
      : !docker.engineAnswered
        ? [{ status: 'skip', subject: 'router', detail: 'needs Docker' }]
        : [
            // The router's own state is read through Compose; the routes, through Docker alone.
            ...docker.composeAnswered
              ? await guarded('router', async () => routerChecks(await source.status()))
              : [{ status: 'skip' as const, subject: 'router', detail: 'needs Docker Compose' }],
            ...await guarded('routes', () => routeChecks(runner, source.network))
          ]
  };

  const { checkout } = options;
  const checks = checkout.kind === 'none'
    ? [{
        status: 'skip' as const,
        subject: BRAND.configFile,
        detail: `none in ${checkout.searchedFrom} or above; run '${commandName} doctor' in a checkout to check it and its verify hook`
      }]
    : checkout.kind === 'unloadable'
      ? [{ status: 'fail' as const, subject: BRAND.configFile, detail: checkout.error.message }]
      : await guarded('this worktree', () => checkoutChecks(options, checkout.loaded));

  const locks: Section = { title: 'Locks', checks: await guarded('locks', () => lockChecks(controlHome, commandName)) };

  return [prerequisites, registry, locks, router, { title: 'This checkout', checks }];
}

/** 0 when nothing failed; the verify hook's own code when it failed, as the hook contract promises; otherwise 1. */
export function doctorExitCode(sections: readonly Section[]): number {
  const failures = sections.flatMap((section) => section.checks).filter((check) => check.status === 'fail');
  if (failures.length === 0) return 0;
  // A failure never exits 0, whatever code it carries.
  return failures.find((check) => check.exitCode !== undefined && check.exitCode !== 0)?.exitCode ?? 1;
}

export function tally(sections: readonly Section[]): Record<CheckStatus, number> {
  const counts: Record<CheckStatus, number> = { ok: 0, warn: 0, fail: 0, skip: 0 };
  for (const check of sections.flatMap((section) => section.checks)) counts[check.status] += 1;
  return counts;
}

/** The report, one section per area and one aligned line per check, every borrowed string made printable. */
export function formatDoctorReport(sections: readonly Section[]): string[] {
  const lines: string[] = [];
  for (const section of sections) {
    if (lines.length > 0) lines.push('');
    lines.push(printable(section.title));
    const subjects = section.checks.map((check) => printable(check.subject));
    const width = Math.max(0, ...subjects.map((subject) => subject.length));
    section.checks.forEach((check, index) => {
      lines.push(`  ${check.status.padEnd(4)}  ${(subjects[index] ?? '').padEnd(width)}  ${printable(check.detail)}`);
    });
  }
  return lines;
}

/**
 * The one line on stderr that says how it went, and how loudly: a failure is
 * an error line, as every failing verb's last word is.
 */
export function doctorSummary(sections: readonly Section[]): { level: 'error' | 'info'; message: string } {
  const counts = tally(sections);
  if (counts.fail > 0) return { level: 'error', message: `${counts.fail} check(s) failed.` };
  const notes = [
    ...(counts.warn > 0 ? [`${counts.warn} warning(s)`] : []),
    ...(counts.skip > 0 ? [`${counts.skip} skipped`] : [])
  ];
  return { level: 'info', message: `No checks failed${notes.length > 0 ? ` (${notes.join(', ')})` : ''}.` };
}
