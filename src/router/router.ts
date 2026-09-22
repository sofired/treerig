import { z } from 'zod';
import { BRAND } from '../brand.ts';
import { CliError } from '../errors.ts';
import { renderIndexHtml } from '../checkout-index/render.ts';
import { atomicWrite, pathExists, secureControlDirectories } from '../files.ts';
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
}

export interface RouterStatus {
  running: boolean;
  /** Empty when the running router is exactly what this version would start. */
  problems: string[];
  port: number;
  network: string;
}

export const DEFAULT_ROUTER_NAME = `${BRAND.name}-router`;
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
    this.indexUrl = indexUrl(options.port);
    this.labelNamespace = options.labelNamespace ?? BRAND.labelNamespace;
  }

  private get layout() {
    return controlLayout(this.options.controlHome);
  }

  private compose(args: readonly string[], options: RunOptions = {}): Promise<RunResult> {
    return this.options.runner.run('docker', [
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
      await atomicWrite(this.layout.indexFile, renderIndexHtml({ entries: [], generatedAt: new Date().toISOString() }), 0o644);
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

  private async containerId(service: string): Promise<string | undefined> {
    const result = await this.compose(['ps', '--all', '--quiet', service], { reject: false });
    return result.exitCode === 0 ? result.stdout.trim().split('\n')[0] || undefined : undefined;
  }

  private async inspect(container: string): Promise<ContainerInspect | undefined> {
    const result = await this.options.runner.run('docker', ['inspect', container], { reject: false });
    if (result.exitCode !== 0) return undefined;
    try {
      const parsed = z.array(inspectSchema).safeParse(JSON.parse(result.stdout));
      return parsed.success ? parsed.data[0] : undefined;
    } catch {
      return undefined;
    }
  }

  /**
   * A router started by a newer version of the tool is left alone: its
   * protocol label says the worktrees on this machine may depend on
   * behaviour this version does not have.
   */
  private async assertCompatibleProtocol(): Promise<void> {
    const id = await this.containerId(ROUTER_SERVICE);
    if (!id) return;
    const router = await this.inspect(id);
    if (!router) throw new CliError('The shared router exists but could not be inspected.');
    const value = router.Config?.Labels?.[`${this.labelNamespace}.router-protocol`];
    if (value === undefined) return;
    const protocol = Number(value);
    if (!Number.isSafeInteger(protocol) || protocol < 1) {
      throw new CliError(`The shared router carries an invalid protocol label: ${value}.`);
    }
    if (protocol > ROUTER_PROTOCOL) {
      throw new CliError(
        `The shared router was started by a newer version (protocol ${protocol}; this version speaks ${ROUTER_PROTOCOL}). ` +
        'Upgrade rather than downgrading the router other worktrees depend on.'
      );
    }
    if (protocol < ROUTER_PROTOCOL) {
      this.options.logger.info(`Upgrading the shared router from protocol ${protocol} to ${ROUTER_PROTOCOL}.`);
    }
  }

  /* ---- verification: every property the stack asked for, checked on the running container */

  private commonProblems(inspected: ContainerInspect, service: string): string[] {
    const problems: string[] = [];
    const labels = inspected.Config?.Labels ?? {};
    if (labels['com.docker.compose.project'] !== this.projectName || labels['com.docker.compose.service'] !== service) {
      problems.push(`${service}: not the container Compose started for this project`);
    }
    if (labels[`${this.labelNamespace}.component`] !== service) problems.push(`${service}: component label`);
    if (labels[`${this.labelNamespace}.router-protocol`] !== String(ROUTER_PROTOCOL)) problems.push(`${service}: protocol label`);
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
    if (Object.entries(indexRouteLabels()).some(([name, value]) => labels[name] !== value)) problems.push('index: routing labels differ');
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

  /** Everything that differs between the running router and what this version would start. */
  async verify(): Promise<string[]> {
    const ids = await Promise.all([ROUTER_SERVICE, DOCKER_API_SERVICE, INDEX_SERVICE].map((service) => this.containerId(service)));
    if (ids.some((id) => !id)) return ['the router is not running'];
    const [router, api, index] = await Promise.all(ids.map((id) => this.inspect(id ?? '')));
    if (!router || !api || !index) return ['the router could not be inspected'];
    return [...this.routerProblems(router), ...this.dockerApiProblems(api), ...this.indexProblems(index)];
  }

  async ensure(): Promise<void> {
    // The lock file lands under the control home, so the home must be ours — and marked — before it is taken.
    await secureControlDirectories(this.layout);
    await withScopedLock(routerLockTarget(this.options.controlHome), 'shared router', this.options.logger, async () => {
      await this.assertCompatibleProtocol();
      await this.write();
      await this.start([]);
      const problems = await this.verify();
      if (problems.length === 0) return;
      this.options.logger.warn(`The shared router is not configured as expected (${problems.join('; ')}); recreating it once.`);
      await this.start(['--force-recreate']);
      const remaining = await this.verify();
      if (remaining.length > 0) {
        throw new CliError(`The shared router could not be brought to its expected configuration:\n  ${remaining.join('\n  ')}`);
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
    const problems = await this.verify();
    const running = !problems.includes('the router is not running');
    return { running, problems, port: this.port, network: this.network };
  }

  /**
   * Stops the router. The shared network stays while any worktree service is
   * still attached to it; Compose reports that as a failure, and here it is
   * the expected outcome.
   */
  async down(): Promise<void> {
    const result = await this.compose(['down', '--remove-orphans'], { reject: false, printOutput: true });
    if (result.exitCode === 0) return;
    if (/has active endpoints/i.test(result.stderr)) {
      this.options.logger.info(`Network ${this.network} is kept while worktree services are attached to it.`);
      return;
    }
    throw new CliError(`The shared router did not stop cleanly:\n${result.stderr.trim() || result.stdout.trim()}`);
  }
}
