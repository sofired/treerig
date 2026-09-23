import { readFile } from 'node:fs/promises';
import { z } from 'zod';
import { BRAND } from '../brand.ts';
import { DockerUnavailableError, dockerSaid, runDocker } from '../docker.ts';
import { CliError, invariant } from '../errors.ts';
import { shellQuote } from '../shell.ts';
import { renderIndexHtml } from '../checkout-index/render.ts';
import { placeholderSnapshot } from '../checkout-index/snapshot.ts';
import { assertNotGoManaged, atomicWrite, pathExists, secureControlDirectories } from '../files.ts';
import { controlLayout } from '../layout.ts';
import type { Logger } from '../logger.ts';
import { routerLockTarget, withScopedLock } from '../operation-lock.ts';
import type { CommandRunner, RunOptions, RunResult } from '../process-runner.ts';
import { composeEnvironment } from '../runtime/compose.ts';
import {
  DOCKER_API_ACCESS,
  DOCKER_API_IMAGE,
  DOCKER_API_NETWORK,
  DOCKER_API_SERVICE,
  DOCKER_SOCKET,
  DYNAMIC_CONFIG_MOUNT,
  INDEX_COMMAND,
  INDEX_IMAGE,
  INDEX_MOUNT,
  INDEX_SERVICE,
  indexRouteLabels,
  indexUrl,
  ROUTED_BY_SINCE_PROTOCOL,
  ROUTER_IMAGE,
  ROUTER_PROTOCOL,
  ROUTER_SERVICE,
  routerCommand,
  routerStack
} from './stack.ts';

// One router per machine, shared by every worktree of every repository. It
// binds one loopback port and routes by hostname; worktree services reach it
// by joining the shared network and carrying labels the Compose runtime
// generates for them. Nothing here is per-project, which is why its
// settings come from the environment rather than from berth.toml.

/** What the rest of the tool needs to know about the router. */
export interface Router {
  readonly port: number;
  readonly network: string;
  /** Where the checkout index answers. */
  readonly indexUrl: string;
  /** Idempotent: start it if needed, verify it, and fail loudly if it cannot be made right. */
  ensure(): Promise<void>;
}

export interface RouterOptions {
  controlHome: string;
  port: number;
  runner: CommandRunner;
  logger: Logger;
  labelNamespace?: string;
  /** Overridable so tests can run a router beside a developer's real one. */
  projectName?: string;
  network?: string;
  /** The command line as invoked, arguments space-joined; names this process to whoever waits on the router lock. */
  invocation?: string;
  /** The name the CLI was invoked as; appears in advice. */
  commandName?: string;
}

export interface RouterStatus {
  /** False when Docker itself could not be asked; `running` is then unknown and reported false. */
  reachable: boolean;
  running: boolean;
  /** Empty when the running router is exactly what this version would start. */
  problems: string[];
  port: number;
  network: string;
}

const NOT_RUNNING = 'the router is not running';

/**
 * What `router status` says, and where. `running` only means a container
 * exists, so a stack whose containers have exited or gone unhealthy must not
 * read as running on the one stream a script is told to read; the reasons go
 * to stderr, and they are what makes the command fail.
 */
export function routerStatusReport(status: RouterStatus): { output: string[]; errors: string[]; failed: boolean } {
  const where = `127.0.0.1:${String(status.port)}, network ${status.network}`;
  if (!status.reachable) {
    return {
      output: [],
      errors: [`Router state unknown: ${status.problems.join('; ').replace(/\.$/, '')}`],
      failed: true
    };
  }
  // `verify()` answers `[NOT_RUNNING]` and nothing else when every container
  // is gone, so there is no other reason to drop here.
  if (!status.running) return { output: [`not running (would use ${where})`], errors: [], failed: false };
  return {
    output: [status.problems.length === 0
      ? `running on ${where}`
      : `degraded on ${where} (${String(status.problems.length)} problem(s) reported on stderr)`],
    errors: status.problems.map((problem) => `Router problem: ${problem}`),
    failed: status.problems.length > 0
  };
}

export const DEFAULT_ROUTER_NAME = `${BRAND.name}-router`;

/** What Docker accepts for a project or network name; nothing else may reach a Traefik predicate. */
const DOCKER_NAME = /^[A-Za-z0-9][A-Za-z0-9_.-]*$/;
const ROUTER_LOCK_TIMEOUT_MS = 5 * 60_000;

/* ------------------------------------------------ what `docker inspect` says */

const inspectSchema = z.object({
  Config: z.object({
    Cmd: z.array(z.string()).nullable().optional(),
    Env: z.array(z.string()).nullable().optional(),
    Image: z.string().optional(),
    Labels: z.record(z.string(), z.string()).nullable().optional()
  }).optional(),
  HostConfig: z.object({
    CapAdd: z.array(z.string()).nullable().optional(),
    CapDrop: z.array(z.string()).nullable().optional(),
    ReadonlyRootfs: z.boolean().optional(),
    SecurityOpt: z.array(z.string()).nullable().optional()
  }).optional(),
  Mounts: z.array(z.object({
    Destination: z.string().optional(),
    RW: z.boolean().optional(),
    Source: z.string().optional(),
    Type: z.string().optional()
  })).optional(),
  NetworkSettings: z.object({
    Networks: z.record(z.string(), z.unknown()).nullable().optional(),
    Ports: z.record(z.string(), z.array(z.object({ HostIp: z.string().optional(), HostPort: z.string().optional() })).nullable()).nullable().optional()
  }).optional(),
  State: z.object({
    Health: z.object({ Status: z.string().optional() }).nullable().optional(),
    Status: z.string().optional()
  }).optional()
}).passthrough();

export type ContainerInspect = z.infer<typeof inspectSchema>;

const sameSet = (actual: readonly string[], expected: readonly string[]): boolean =>
  [...actual].sort().join('\0') === [...expected].sort().join('\0');

/* ------------------------------------------------------------------ service */

/** A protocol number as it appears on disk or in a label; anything else is no answer at all. */
function toProtocol(value: unknown): number | undefined {
  if (typeof value !== 'string' && typeof value !== 'number') return undefined;
  // Digits only: `0x10` and `1e3` are corruption, not protocols.
  const text = String(value).trim();
  if (!/^\d+$/.test(text)) return undefined;
  const protocol = Number(text);
  return Number.isSafeInteger(protocol) && protocol >= 1 ? protocol : undefined;
}

export class RouterService implements Router {
  readonly port: number;
  readonly network: string;
  readonly indexUrl: string;
  readonly projectName: string;
  private readonly options: RouterOptions;
  private readonly labelNamespace: string;

  constructor(options: RouterOptions) {
    this.options = options;
    this.port = options.port;
    this.projectName = options.projectName ?? DEFAULT_ROUTER_NAME;
    this.network = options.network ?? this.projectName;
    // The network name is interpolated into Traefik's discovery predicate;
    // nothing that could rewrite it may reach there.
    invariant(DOCKER_NAME.test(this.projectName), `Invalid router project name: ${this.projectName}.`);
    invariant(DOCKER_NAME.test(this.network), `Invalid router network name: ${this.network}.`);
    this.indexUrl = indexUrl(options.port);
    this.labelNamespace = options.labelNamespace ?? BRAND.labelNamespace;
  }

  private get layout() {
    return controlLayout(this.options.controlHome);
  }

  private compose(args: readonly string[], options: Omit<RunOptions, 'cwd'> = {}): Promise<RunResult> {
    return runDocker(this.options.runner, [
      'compose',
      '--project-directory', this.layout.routerDir,
      '--project-name', this.projectName,
      '-f', this.layout.routerComposeFile,
      ...args
    ], { ...options, env: composeEnvironment(options.env) });
  }

  /** Writes the stack document and makes sure the directories it mounts exist and serve. */
  private async write(): Promise<void> {
    await secureControlDirectories(this.layout);
    // The index container's health check fetches the page, so one must exist before `up --wait`.
    if (!(await pathExists(this.layout.indexFile))) {
      await atomicWrite(this.layout.indexFile, renderIndexHtml(placeholderSnapshot(new Date().toISOString())), 0o644);
    }
    const document = routerStack({
      projectName: this.projectName,
      network: this.network,
      port: this.port,
      dynamicDir: this.layout.routerDynamicDir,
      indexDir: this.layout.indexDir,
      labelNamespace: this.labelNamespace
    });
    // JSON is YAML; Compose reads it as such.
    await atomicWrite(this.layout.routerComposeFile, `${JSON.stringify(document, null, 2)}\n`);
  }

  /**
   * By project name alone, with no stack file: this runs before the file
   * exists, and a failure then can only mean Docker itself — a daemon that
   * is not running, a socket this user cannot open, a `docker` that is not
   * installed — never a stopped router. Compose given a project name and no
   * file looks for none, so the consumer's own compose file in the current
   * directory is never loaded; keep it that way (no `cwd`, no `-f`).
   */
  private async containerId(service: string): Promise<string | undefined> {
    const result = await runDocker(
      this.options.runner,
      ['compose', '--project-name', this.projectName, 'ps', '--all', '--quiet', service],
      { reject: false, env: composeEnvironment() }
    );
    if (result.exitCode !== 0) {
      throw new DockerUnavailableError(dockerSaid(result.stderr, `\`docker compose ps\` exited ${result.exitCode} without saying why`));
    }
    return result.stdout.trim().split('\n')[0] || undefined;
  }

  private async inspect(container: string): Promise<ContainerInspect | undefined> {
    const result = await runDocker(this.options.runner, ['inspect', container], { reject: false });
    if (result.exitCode !== 0) return undefined;
    try {
      const parsed = z.array(inspectSchema).safeParse(JSON.parse(result.stdout));
      return parsed.success ? parsed.data[0] : undefined;
    } catch {
      return undefined;
    }
  }

  /**
   * Settles what protocol the router was at before this run: refuses a
   * downgrade, announces an upgrade, and records what it found so a failed
   * start cannot erase it. Returns that protocol, or `undefined` when this
   * machine has no router history at all.
   */
  private async previousProtocol(): Promise<number | undefined> {
    const recorded = await this.recordedProtocol();
    const id = await this.containerId(ROUTER_SERVICE);
    const container = id === undefined ? undefined : await this.inspect(id);
    if (id !== undefined && container === undefined) throw new CliError('The shared router exists but could not be inspected.');
    const running = container === undefined ? undefined : this.protocolOf(container);
    // `ps --all` also finds a container that was created and never started —
    // a failed `up`'s leftover, which has served nothing.
    const serving = container?.State?.Status === 'running';
    // Refuse on the newest evidence there is, each case naming the remedy that
    // actually frees the router. The router is machine-wide while the record is
    // per control home, so either can be the newer one.
    const command = this.options.commandName ?? BRAND.name;
    const alsoRecorded = recorded !== undefined && recorded > ROUTER_PROTOCOL
      ? ` and this control home's record with \`rm ${shellQuote(this.layout.routerProtocolFile)}\``
      : '';
    if (running !== undefined && running > ROUTER_PROTOCOL) {
      throw new CliError(
        `The shared router is at protocol ${running}; this version speaks ${ROUTER_PROTOCOL}. ` +
        'Upgrade rather than downgrading the router the other worktrees on this machine expect, ' +
        `or hand the router to this one: remove it with '${command} router down'${alsoRecorded}.`
      );
    }
    if (recorded !== undefined && recorded > ROUTER_PROTOCOL) {
      throw new CliError(
        `This control home last recorded the shared router at protocol ${recorded}; this version speaks ${ROUTER_PROTOCOL}. ` +
        `Run that version again, or remove the record with \`rm ${shellQuote(this.layout.routerProtocolFile)}\` ` +
        'to hand the router to this one.'
      );
    }
    // Announce on the oldest evidence, from all three sources together. A
    // record ahead of an older running router means another control home
    // downgraded it; a record behind a container is a failed start's
    // leftover; and this home's own stack file is the only witness left when
    // another control home upgraded the machine-wide router first. The
    // worktrees started under the oldest of these are the ones about to lose
    // their routes, so that is the number to speak from.
    const evidence = [recorded, running, await this.legacyProtocol()]
      .filter((protocol): protocol is number => protocol !== undefined);
    if (evidence.length === 0) return undefined;
    const previous = Math.min(...evidence);
    // Make it durable before anything overwrites the evidence: `write()` is
    // about to stamp the new protocol into the stack file, and a start that
    // fails afterwards leaves a container carrying it too. Fatal, unlike the
    // promotion at the end: `write()` would fail on the same directory two
    // steps later, so there is no run this could have cost a router.
    if (recorded !== previous) {
      await atomicWrite(this.layout.routerProtocolFile, `${previous}\n`).catch((error: unknown) => {
        throw new CliError(
          `Could not record the router protocol in ${this.layout.routerProtocolFile}: ` +
          `${error instanceof Error ? error.message : String(error)}. Fix its permissions, or remove it and run this again.`
        );
      });
    }
    if (previous < ROUTER_PROTOCOL) {
      // Another control home may have done the upgrading; say so rather than claiming this run did.
      this.options.logger.info(serving && (running ?? 0) >= ROUTER_PROTOCOL
        ? `The shared router is already at protocol ${ROUTER_PROTOCOL}, but this control home last saw ${previous}.`
        : `Upgrading the shared router from protocol ${previous} to ${ROUTER_PROTOCOL}.`);
    }
    return previous;
  }

  /** What this control home last brought up and verified, if it has recorded anything legible. */
  private async recordedProtocol(): Promise<number | undefined> {
    const text = await this.read(this.layout.routerProtocolFile);
    if (text === undefined) return undefined;
    const recorded = toProtocol(text);
    if (recorded === undefined) {
      this.options.logger.warn(
        `Ignoring ${this.layout.routerProtocolFile}: it does not name a router protocol. ` +
        `This run takes the previous protocol from the router itself, or from ${this.layout.routerComposeFile}, if either is still there.`
      );
    }
    return recorded;
  }

  /** The protocol label of the router container, running or not, if it carries one. */
  private protocolOf(router: ContainerInspect): number | undefined {
    const value = router.Config?.Labels?.[this.protocolLabel];
    if (value === undefined) return undefined;
    const protocol = toProtocol(value);
    if (protocol === undefined) {
      throw new CliError(
        `The shared router carries an invalid protocol label: ${value}. ` +
        `Remove the router with '${this.options.commandName ?? BRAND.name} router down' and run this again.`
      );
    }
    return protocol;
  }

  /**
   * The stack file records intent, not what ran: a version that wrote it and
   * then failed to start left its number behind. Only an older protocol is
   * taken from it — intent is never a reason to refuse this version the
   * router, so removing the record really does hand the router over. It is
   * replaced by `write()` on every run, so a stale number survives at most
   * until this version's first successful `ensure`. Droppable once
   * `ROUTER_PROTOCOL` is past 3: by then every control home still in use has
   * a record, and no version that lacked one can still run.
   */
  private async legacyProtocol(): Promise<number | undefined> {
    let label: unknown;
    try {
      // Unreadable is no answer here: `write()` replaces this file in a moment.
      const text = await this.read(this.layout.routerComposeFile);
      if (text === undefined) return undefined;
      const document = JSON.parse(text) as { services?: Record<string, { labels?: Record<string, unknown> } | undefined> };
      label = document.services?.[ROUTER_SERVICE]?.labels?.[this.protocolLabel];
    } catch {
      return undefined;
    }
    const protocol = toProtocol(label);
    return protocol !== undefined && protocol < ROUTER_PROTOCOL ? protocol : undefined;
  }

  /** The file's text, or `undefined` when it is simply not there. */
  private async read(file: string): Promise<string | undefined> {
    try {
      return await readFile(file, 'utf8');
    } catch (error) {
      const code = (error as NodeJS.ErrnoException).code;
      if (code === 'ENOENT') return undefined;
      const why = code ?? (error instanceof Error ? error.message : String(error));
      throw new CliError(`${file} could not be read (${why}). Fix its permissions, or remove it and run this again.`);
    }
  }

  private get protocolLabel(): string {
    return `${this.labelNamespace}.router-protocol`;
  }

  /**
   * A router from before the routed-by label discovered every container with
   * Traefik labels; the new one discovers only containers labelled for it,
   * and a label cannot be added to a container already running. Each
   * worktree's next `up` recreates its own. Said only after the upgrade is a fact.
   */
  private noteUnroutedAfterUpgrade(previous: number | undefined): void {
    if (previous === undefined || previous >= ROUTED_BY_SINCE_PROTOCOL) return;
    this.options.logger.warn(
      `Worktrees already up elsewhere stay unrouted until their next '${this.options.commandName ?? BRAND.name} up'.`
    );
  }

  /* ---- verification: every property the stack asked for, checked on the running container */

  private commonProblems(inspected: ContainerInspect, service: string): string[] {
    const problems: string[] = [];
    const labels = inspected.Config?.Labels ?? {};
    if (labels['com.docker.compose.project'] !== this.projectName || labels['com.docker.compose.service'] !== service) {
      problems.push(`${service}: not the container Compose started for this project`);
    }
    if (labels[`${this.labelNamespace}.component`] !== service) problems.push(`${service}: component label`);
    if (labels[this.protocolLabel] !== String(ROUTER_PROTOCOL)) problems.push(`${service}: protocol label`);
    if (inspected.State?.Status !== 'running') problems.push(`${service}: not running`);
    if (inspected.State?.Health?.Status !== 'healthy') problems.push(`${service}: not healthy`);
    if (inspected.HostConfig?.ReadonlyRootfs !== true) problems.push(`${service}: root filesystem is writable`);
    if (!inspected.HostConfig?.SecurityOpt?.includes('no-new-privileges:true')) problems.push(`${service}: no-new-privileges is not set`);
    if (!inspected.HostConfig?.CapDrop?.includes('ALL')) problems.push(`${service}: capabilities are not dropped`);
    return problems;
  }

  private routerProblems(inspected: ContainerInspect): string[] {
    const problems = this.commonProblems(inspected, ROUTER_SERVICE);
    const bindings = inspected.NetworkSettings?.Ports?.['80/tcp'] ?? [];
    if (bindings.length !== 1 || bindings[0]?.HostIp !== '127.0.0.1' || bindings[0].HostPort !== String(this.port)) {
      problems.push(`router: port 80 is not published only on 127.0.0.1:${this.port}`);
    }
    const otherPorts = Object.entries(inspected.NetworkSettings?.Ports ?? {}).filter(([port, list]) => port !== '80/tcp' && list && list.length > 0);
    if (otherPorts.length > 0) problems.push(`router: publishes unexpected ports (${otherPorts.map(([port]) => port).join(', ')})`);
    const networks = Object.keys(inspected.NetworkSettings?.Networks ?? {});
    if (!sameSet(networks, [this.network, `${this.projectName}_${DOCKER_API_NETWORK}`])) {
      problems.push(`router: attached to networks ${networks.join(', ') || '(none)'} instead of the shared and proxy networks`);
    }
    const command = inspected.Config?.Cmd ?? [];
    if (command.join('\0') !== routerCommand({ network: this.network }).join('\0')) problems.push('router: command line differs');
    if (inspected.Config?.Image !== ROUTER_IMAGE) problems.push('router: image is not the pinned digest');
    if (!sameSet(inspected.HostConfig?.CapAdd ?? [], ['CAP_NET_BIND_SERVICE'])) problems.push('router: capabilities beyond NET_BIND_SERVICE');
    const mounts = inspected.Mounts ?? [];
    const dynamic = mounts.find((mount) => mount.Destination === DYNAMIC_CONFIG_MOUNT);
    if (mounts.length !== 1 || !dynamic || dynamic.Type !== 'bind' || dynamic.Source !== this.layout.routerDynamicDir || dynamic.RW !== false) {
      problems.push('router: mounts are not exactly the read-only dynamic-configuration directory');
    }
    return problems;
  }

  private dockerApiProblems(inspected: ContainerInspect): string[] {
    const problems = this.commonProblems(inspected, DOCKER_API_SERVICE);
    const environment = new Map((inspected.Config?.Env ?? []).map((entry) => {
      const separator = entry.indexOf('=');
      return separator === -1 ? [entry, ''] : [entry.slice(0, separator), entry.slice(separator + 1)];
    }));
    const wrong = Object.entries(DOCKER_API_ACCESS).filter(([name, expected]) => environment.get(name) !== expected);
    if (wrong.length > 0) problems.push(`docker-api: API access differs (${wrong.map(([name]) => name).join(', ')})`);
    const networks = Object.keys(inspected.NetworkSettings?.Networks ?? {});
    if (!sameSet(networks, [`${this.projectName}_${DOCKER_API_NETWORK}`])) problems.push('docker-api: attached to a network other than the internal proxy network');
    if (inspected.Config?.Image !== DOCKER_API_IMAGE) problems.push('docker-api: image is not the pinned digest');
    if ((inspected.HostConfig?.CapAdd ?? []).length !== 0) problems.push('docker-api: has added capabilities');
    if (Object.values(inspected.NetworkSettings?.Ports ?? {}).some((list) => list && list.length > 0)) problems.push('docker-api: publishes a port');
    const mounts = inspected.Mounts ?? [];
    const socket = mounts.find((mount) => mount.Destination === DOCKER_SOCKET);
    if (mounts.length !== 1 || !socket || socket.Type !== 'bind' || socket.Source !== DOCKER_SOCKET || socket.RW !== false) {
      problems.push('docker-api: mounts are not exactly the read-only Docker socket');
    }
    return problems;
  }

  private indexProblems(inspected: ContainerInspect): string[] {
    const problems = this.commonProblems(inspected, INDEX_SERVICE);
    if (inspected.Config?.Image !== INDEX_IMAGE) problems.push('index: image is not the pinned digest');
    if ((inspected.Config?.Cmd ?? []).join('\0') !== INDEX_COMMAND.join('\0')) problems.push('index: command line differs');
    const labels = inspected.Config?.Labels ?? {};
    if (Object.entries(indexRouteLabels(this.network)).some(([name, value]) => labels[name] !== value)) problems.push('index: routing labels differ');
    const networks = Object.keys(inspected.NetworkSettings?.Networks ?? {});
    if (!sameSet(networks, [this.network])) problems.push('index: attached to a network other than the shared one');
    if (Object.values(inspected.NetworkSettings?.Ports ?? {}).some((list) => list && list.length > 0)) problems.push('index: publishes a port');
    if ((inspected.HostConfig?.CapAdd ?? []).length !== 0) problems.push('index: has added capabilities');
    const mounts = inspected.Mounts ?? [];
    const served = mounts.find((mount) => mount.Destination === INDEX_MOUNT);
    if (mounts.length !== 1 || !served || served.Type !== 'bind' || served.Source !== this.layout.indexDir || served.RW !== false) {
      problems.push('index: mounts are not exactly the read-only index directory');
    }
    return problems;
  }

  /**
   * Everything that differs between the running router and what this version
   * would start. Throws `DockerUnavailableError` when Docker cannot say.
   */
  async verify(): Promise<string[]> {
    const services = [ROUTER_SERVICE, DOCKER_API_SERVICE, INDEX_SERVICE];
    const ids = await Promise.all(services.map((service) => this.containerId(service)));
    // All three gone is a router that is not running. One or two gone is a
    // router that is still bound to its port and still routing, with a hole in
    // it — reporting that as "not running" would tell an operator the opposite
    // of what is true.
    if (ids.every((id) => !id)) return [NOT_RUNNING];
    const missing = services.filter((_, index) => !ids[index]).map((service) => `${service}: container is missing`);
    if (missing.length > 0) return missing;
    const [router, api, index] = await Promise.all(ids.map((id) => this.inspect(id ?? '')));
    if (!router || !api || !index) return ['the router could not be inspected'];
    return [...this.routerProblems(router), ...this.dockerApiProblems(api), ...this.indexProblems(index)];
  }

  async ensure(): Promise<void> {
    await assertNotGoManaged(this.options.controlHome);
    // The lock file lands under the control home, so the home must be ours — and marked — before it is taken.
    await secureControlDirectories(this.layout);
    await withScopedLock(routerLockTarget(this.options.controlHome), 'shared router', this.options.logger, async () => {
      const previous = await this.previousProtocol();
      await this.write();
      await this.start([]);
      const problems = await this.verify();
      if (problems.length > 0) {
        this.options.logger.warn(`The shared router is not configured as expected (${problems.join('; ')}); recreating it once.`);
        await this.start(['--force-recreate']);
        const remaining = await this.verify();
        if (remaining.length > 0) {
          throw new CliError(`The shared router could not be brought to its expected configuration:\n  ${remaining.join('\n  ')}`);
        }
      }
      // The warning is about what `verify()` just established, so it is said
      // even if recording the new protocol fails.
      this.noteUnroutedAfterUpgrade(previous);
      if (previous !== ROUTER_PROTOCOL) {
        // A record that will not write costs a repeated announcement next
        // time, not a router: never fail a verified `up` over it.
        await atomicWrite(this.layout.routerProtocolFile, `${ROUTER_PROTOCOL}\n`).catch((error: unknown) => {
          this.options.logger.warn(
            `Could not record the router protocol in ${this.layout.routerProtocolFile}: ` +
            `${error instanceof Error ? error.message : String(error)}`
          );
        });
      }
    }, { acquireTimeoutMs: ROUTER_LOCK_TIMEOUT_MS, holder: this.options.invocation });
  }

  private async start(extra: readonly string[]): Promise<void> {
    const result = await this.compose(['up', '--detach', '--wait', '--remove-orphans', ...extra], { reject: false });
    if (result.exitCode !== 0) {
      throw new CliError(
        `The shared router failed to start. Make sure 127.0.0.1:${this.port} is free, or choose another port with ` +
        `${BRAND.envPrefix}_ROUTER_PORT.\n${result.stderr.trim() || result.stdout.trim()}`
      );
    }
  }

  async status(): Promise<RouterStatus> {
    let problems: string[];
    try {
      problems = await this.verify();
    } catch (error) {
      if (!(error instanceof DockerUnavailableError)) throw error;
      return { reachable: false, running: false, problems: [error.message], port: this.port, network: this.network };
    }
    return { reachable: true, running: !problems.includes(NOT_RUNNING), problems, port: this.port, network: this.network };
  }

  /**
   * Stops the router. The shared network stays while any worktree service is
   * still attached to it; Compose reports that as a failure, and here it is
   * the expected outcome.
   */
  async down(): Promise<void> {
    await assertNotGoManaged(this.options.controlHome);
    // By project name alone, as `containerId` reads it: the router is
    // machine-wide, so it must be removable from a control home that never
    // wrote a stack file — the one a refusal tells the user to run this from.
    // `--remove-orphans` no longer sweeps orphans without a model to compare
    // against; it stays because it still catches one-off `compose run`
    // containers of the project.
    const result = await runDocker(
      this.options.runner,
      ['compose', '--project-name', this.projectName, 'down', '--remove-orphans'],
      { reject: false, printOutput: true, env: composeEnvironment() }
    );
    if (result.exitCode === 0) return;
    if (/has active endpoints/i.test(result.stderr)) {
      this.options.logger.info(`Network ${this.network} is kept while worktree services are attached to it.`);
      return;
    }
    throw new CliError(`The shared router did not stop cleanly:\n${result.stderr.trim() || result.stdout.trim()}`);
  }
}
