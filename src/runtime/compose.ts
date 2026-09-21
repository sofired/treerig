import path from 'node:path';
import { z } from 'zod';
import { BRAND } from '../brand.ts';
import { CliError, invariant } from '../errors.ts';
import { atomicWrite } from '../files.ts';
import { hookEnvironmentVariables, worktreeEnvironment } from '../hooks/protocol.ts';
import { composeProjectName, worktreeHost, worktreeLayout } from '../layout.ts';
import type { Logger } from '../logger.ts';
import type { CommandRunner, RunOptions, RunResult } from '../process-runner.ts';
import { ROUTER_ENTRYPOINT } from '../router/stack.ts';
import type {
  Endpoint,
  OwnedResource,
  Runtime,
  ServiceSpec,
  ServiceStatus,
  UpOptions,
  WorktreeRef
} from './runtime.ts';

// The shipping backend. Everything goes through the `docker` CLI — never an
// SDK — which is what makes this layer portable to any consumer.
//
// Ownership is the whole point. Every resource Compose creates for a worktree
// is stamped with three labels: `<namespace>.managed`, `.repository-uuid`,
// and `.worktree-uuid`. The consumer's Compose file is never asked to add
// them; a generated override file does it for every service, volume, and
// network, so forgetting a label — and silently exempting a container from
// cleanup — is not possible. `inventory` finds resources by those labels,
// verifies the full tuple on each one, and refuses everything if any single
// resource fails. `reap` verifies again before removing.
//
// The same override carries routing. Each `[[service]]` becomes a set of
// Traefik labels and a membership in the shared router network, so the
// consumer's Compose file never mentions the router at all.

/** One `[[service]]` entry, as the runtime needs it. */
export interface RouteSpec {
  service: string;
  /** Empty routes at the worktree host itself. */
  subdomain: string;
  /** Route only this prefix; lets several services share one host. */
  path?: string;
  /** Container port; defaults to the first port the service exposes or publishes. */
  port?: number;
}

export interface ComposeRuntimeOptions {
  /** Absolute path to the consumer's Compose file. */
  composeFile: string;
  /** Directory containing the config file; Compose's project directory. */
  projectRoot: string;
  /** Absolute path of the loaded config file, handed to Compose as an env var. */
  configFile: string;
  /** `project.name` from config. */
  projectName: string;
  /** `project.domain` from config; worktrees route as `<slug>.<domain>`. */
  domain: string;
  controlHome: string;
  /** Docker label namespace, e.g. `dev.berth`. */
  labelNamespace: string;
  /** Services to route, from `[[service]]` in config. */
  routes: readonly RouteSpec[];
  /** The shared router's network; routed services join it. */
  routerNetwork: string;
  runner: CommandRunner;
  logger: Logger;
  /** How long `up --wait` waits for health before failing. */
  waitTimeoutSeconds?: number;
}

const COMPOSE_PROJECT_LABEL = 'com.docker.compose.project';
const COMPOSE_SERVICE_LABEL = 'com.docker.compose.service';
const DEFAULT_WAIT_TIMEOUT_SECONDS = 120;

type ResourceKind = OwnedResource['kind'];

interface ResourceType {
  kind: ResourceKind;
  listArguments: readonly string[];
  inspectCommand: readonly string[];
  removeArguments: readonly string[];
  /** Where Docker keeps labels in the inspect document for this kind. */
  labelPrefix: '.Config' | '';
}

/** In removal order: containers hold volumes and networks; images last. */
const RESOURCE_TYPES: readonly ResourceType[] = [
  { kind: 'container', listArguments: ['ps', '-aq'], inspectCommand: ['inspect'], removeArguments: ['rm', '--force'], labelPrefix: '.Config' },
  { kind: 'volume', listArguments: ['volume', 'ls', '-q'], inspectCommand: ['volume', 'inspect'], removeArguments: ['volume', 'rm'], labelPrefix: '' },
  { kind: 'network', listArguments: ['network', 'ls', '-q'], inspectCommand: ['network', 'inspect'], removeArguments: ['network', 'rm'], labelPrefix: '' },
  { kind: 'image', listArguments: ['image', 'ls', '-q'], inspectCommand: ['image', 'inspect'], removeArguments: ['image', 'rm'], labelPrefix: '.Config' }
];

/* ------------------------------------------------ compose document shapes */

const externalSchema = z.union([z.boolean(), z.object({}).passthrough()]).optional();
const composeConfigSchema = z.object({
  services: z.record(z.string(), z.object({
    expose: z.array(z.union([z.string(), z.number()])).optional(),
    ports: z.array(z.object({ target: z.number().optional() }).passthrough()).optional(),
    networks: z.unknown().optional()
  }).passthrough()).default({}),
  volumes: z.record(z.string(), z.object({ external: externalSchema }).passthrough()).default({}),
  networks: z.record(z.string(), z.object({ external: externalSchema }).passthrough()).default({})
}).passthrough();

type ComposeConfig = z.infer<typeof composeConfigSchema>;
type ComposeService = ComposeConfig['services'][string];

const psRecordSchema = z.object({
  Service: z.string(),
  State: z.string(),
  Health: z.string().optional()
}).passthrough();

function isExternal(resource: { external?: boolean | object } | undefined): boolean {
  return resource?.external !== undefined && resource.external !== false;
}

/** A service with no `networks` key, or one naming `default`, joins the default network. */
function usesDefaultNetwork(service: ComposeService): boolean {
  const networks = service.networks;
  if (networks === undefined || networks === null) return true;
  if (Array.isArray(networks)) return networks.includes('default');
  if (typeof networks === 'object') return 'default' in networks;
  return false;
}

/** The networks a service is on, as Compose normalized them; no key means the implicit default. */
function serviceNetworks(service: ComposeService): Record<string, object> {
  const networks = service.networks;
  if (Array.isArray(networks)) return Object.fromEntries(networks.map((name) => [String(name), {}]));
  if (networks !== null && typeof networks === 'object') return Object.fromEntries(Object.keys(networks).map((name) => [name, {}]));
  return { default: {} };
}

function routeLabels(name: string, route: RouteSpec, network: string, host: string, port: number): Record<string, string> {
  const routeHost = route.subdomain ? `${route.subdomain}.${host}` : host;
  const rule = route.path === undefined
    ? `Host(\`${routeHost}\`)`
    : `Host(\`${routeHost}\`) && PathPrefix(\`${route.path}\`)`;
  return {
    'traefik.enable': 'true',
    'traefik.docker.network': network,
    [`traefik.http.routers.${name}.rule`]: rule,
    [`traefik.http.routers.${name}.entrypoints`]: ROUTER_ENTRYPOINT,
    [`traefik.http.services.${name}.loadbalancer.server.port`]: String(port)
  };
}

function firstPort(service: ComposeService): number | undefined {
  const exposed = service.expose?.[0];
  if (exposed !== undefined) {
    const port = Number.parseInt(String(exposed).split('/')[0] ?? '', 10);
    if (Number.isInteger(port)) return port;
  }
  return service.ports?.find((entry) => entry.target !== undefined)?.target;
}

function serviceState(record: z.infer<typeof psRecordSchema>): Pick<ServiceStatus, 'state' | 'detail'> {
  const state = record.State.toLowerCase();
  if (state === 'running') {
    if (record.Health === 'starting') return { state: 'starting' };
    if (record.Health === 'unhealthy') return { state: 'running', detail: 'unhealthy' };
    return { state: 'running' };
  }
  if (state === 'restarting') return { state: 'starting' };
  if (['created', 'exited', 'dead', 'paused', 'removing'].includes(state)) return { state: 'stopped' };
  return { state: 'unknown', detail: record.State };
}

/* -------------------------------------------------------------- runtime */

export class ComposeRuntime implements Runtime {
  readonly kind = 'compose';
  private readonly options: ComposeRuntimeOptions;

  constructor(options: ComposeRuntimeOptions) {
    this.options = options;
  }

  private project(ref: WorktreeRef): string {
    return composeProjectName(ref.repositoryId, ref.worktreeId);
  }

  private overridePath(ref: WorktreeRef): string {
    const { home } = worktreeLayout(this.options.controlHome, ref.repositoryId, ref.worktreeId);
    // JSON content under a .yaml name: JSON is valid YAML, Compose reads it
    // as such, and it spares us a YAML emitter and every quoting bug one brings.
    return path.join(home, `${BRAND.name}.override.yaml`);
  }

  private ownershipLabels(ref: WorktreeRef): Record<string, string> {
    const namespace = this.options.labelNamespace;
    return {
      [`${namespace}.managed`]: 'true',
      [`${namespace}.repository-uuid`]: ref.repositoryId,
      [`${namespace}.worktree-uuid`]: ref.worktreeId
    };
  }

  /** The same `BERTH_*` vocabulary hooks receive, so one name means one thing. */
  private environment(ref: WorktreeRef): NodeJS.ProcessEnv {
    return {
      ...process.env,
      ...hookEnvironmentVariables(worktreeEnvironment(this.options, ref)),
      COMPOSE_PROJECT_NAME: this.project(ref)
    };
  }

  private compose(
    ref: WorktreeRef,
    files: readonly string[],
    args: readonly string[],
    options: RunOptions = {}
  ): Promise<RunResult> {
    return this.options.runner.run('docker', [
      'compose',
      '--project-directory', this.options.projectRoot,
      '--project-name', this.project(ref),
      ...files.flatMap((file) => ['-f', file]),
      ...args
    ], { ...options, env: { ...this.environment(ref), ...options.env } });
  }

  /**
   * Reads the consumer's Compose file as Compose itself resolves it, then
   * writes the override that stamps ownership labels onto every service and
   * every non-external volume and network, and routes the declared services.
   * Idempotent; every command calls it so the override always matches the
   * current Compose file.
   */
  private async prepare(ref: WorktreeRef): Promise<{ config: ComposeConfig; files: string[] }> {
    const { composeFile } = this.options;
    const result = await this.compose(ref, [composeFile], ['config', '--format', 'json'], { reject: false });
    if (result.exitCode !== 0) {
      throw new CliError(`Compose could not read ${composeFile}:\n${result.stderr.trim() || result.stdout.trim()}`);
    }
    let raw: unknown;
    try {
      raw = JSON.parse(result.stdout);
    } catch {
      throw new CliError(`Compose returned invalid JSON for ${composeFile}.`);
    }
    const parsed = composeConfigSchema.safeParse(raw);
    if (!parsed.success) throw new CliError(`Compose returned an unrecognized document for ${composeFile}.`);
    const config = parsed.data;

    const labels = this.ownershipLabels(ref);
    const stamp = () => ({ labels });
    const services: Record<string, { labels: Record<string, string>; networks?: Record<string, object> }> =
      Object.fromEntries(Object.keys(config.services).map((name) => [name, stamp()]));
    const volumes = Object.fromEntries(
      Object.entries(config.volumes).filter(([, volume]) => !isExternal(volume)).map(([name]) => [name, stamp()])
    );
    const networks: Record<string, object> = Object.fromEntries(
      Object.entries(config.networks).filter(([, network]) => !isExternal(network)).map(([name]) => [name, stamp()])
    );
    const defaultDeclared = 'default' in config.networks;
    if (!defaultDeclared && Object.values(config.services).some(usesDefaultNetwork)) {
      networks.default = stamp();
    }

    if (this.options.routes.length > 0) {
      const host = worktreeHost(ref.slug, this.options.domain);
      const { routerNetwork } = this.options;
      for (const route of this.options.routes) {
        const service = config.services[route.service];
        if (!service) throw new CliError(`[[service]] ${route.service} is not defined in ${composeFile}.`);
        const port = route.port ?? firstPort(service);
        if (port === undefined) {
          throw new CliError(
            `Service ${route.service} declares no port: set port in its [[service]] entry, or expose one in ${composeFile}.`
          );
        }
        services[route.service] = {
          labels: { ...labels, ...routeLabels(`${this.project(ref)}-${route.service}`, route, routerNetwork, host, port) },
          // Adding the router network must not cost the service its own networks.
          networks: { ...serviceNetworks(service), [routerNetwork]: {} }
        };
      }
      networks[routerNetwork] = { external: true };
    }

    const override = this.overridePath(ref);
    await atomicWrite(override, `${JSON.stringify({ services, volumes, networks }, null, 2)}\n`);
    return { config, files: [composeFile, override] };
  }

  async up(ref: WorktreeRef, services: readonly ServiceSpec[], options: UpOptions = {}): Promise<void> {
    const { files } = await this.prepare(ref);
    const timeout = this.options.waitTimeoutSeconds ?? DEFAULT_WAIT_TIMEOUT_SECONDS;
    await this.compose(ref, files, [
      'up', '--detach', '--build', '--remove-orphans',
      '--wait', '--wait-timeout', String(timeout),
      ...services.map((service) => service.name)
    ], { printOutput: true, cancelSignal: options.signal });
  }

  /** Stops and removes containers and networks. Named volumes survive; that is what `reap` is for. */
  async down(ref: WorktreeRef): Promise<void> {
    const { files } = await this.prepare(ref);
    await this.compose(ref, files, ['down', '--remove-orphans'], { printOutput: true });
  }

  async status(ref: WorktreeRef): Promise<ServiceStatus[]> {
    const { config, files } = await this.prepare(ref);
    const result = await this.compose(ref, files, ['ps', '--format', 'json', '--all'], { reject: false });
    if (result.exitCode !== 0) {
      throw new CliError(`Compose could not list services:\n${result.stderr.trim() || result.stdout.trim()}`);
    }
    // One JSON document per line.
    const seen = new Map<string, ServiceStatus>();
    for (const line of result.stdout.split('\n')) {
      if (!line.trim()) continue;
      let raw: unknown;
      try {
        raw = JSON.parse(line);
      } catch {
        continue;
      }
      const record = psRecordSchema.safeParse(raw);
      if (!record.success) continue;
      seen.set(record.data.Service, { service: record.data.Service, ...serviceState(record.data) });
    }
    return Object.keys(config.services).sort().map((name) =>
      seen.get(name) ?? { service: name, state: 'missing' }
    );
  }

  /** Each service is reachable by name on the project network; the port is its first exposed one. */
  async endpoints(ref: WorktreeRef): Promise<Endpoint[]> {
    const { config } = await this.prepare(ref);
    const endpoints: Endpoint[] = [];
    for (const [name, service] of Object.entries(config.services)) {
      const port = firstPort(service);
      if (port !== undefined) endpoints.push({ service: name, host: name, port });
    }
    return endpoints.sort((left, right) => left.service.localeCompare(right.service));
  }

  private async list(type: ResourceType, labelFilters: readonly string[]): Promise<string[]> {
    const result = await this.options.runner.run('docker', [
      ...type.listArguments,
      ...labelFilters.flatMap((filter) => ['--filter', `label=${filter}`])
    ]);
    return [...new Set(result.stdout.split(/\s+/).filter(Boolean))].sort();
  }

  /**
   * One inspect per resource, reading all four ownership labels and the
   * Compose service name together. Any mismatch is a refusal; a resource
   * whose labels cannot be read is treated the same way.
   */
  private async verify(ref: WorktreeRef, type: ResourceType, id: string): Promise<string> {
    const namespace = this.options.labelNamespace;
    const label = (name: string) => `{{ index ${type.labelPrefix}.Labels "${name}" }}`;
    const format = [
      label(`${namespace}.managed`),
      label(COMPOSE_PROJECT_LABEL),
      label(`${namespace}.repository-uuid`),
      label(`${namespace}.worktree-uuid`),
      label(COMPOSE_SERVICE_LABEL)
    ].join('\t');
    const result = await this.options.runner.run('docker', [
      ...type.inspectCommand, '--format', format, id
    ], { reject: false });
    invariant(
      result.exitCode === 0,
      `Refusing to touch ${type.kind} ${id}: its labels could not be inspected.`
    );
    const [managed, project, repositoryId, worktreeId, service = ''] = result.stdout.trim().split('\t');
    invariant(
      managed === 'true' &&
        project === this.project(ref) &&
        repositoryId === ref.repositoryId &&
        worktreeId === ref.worktreeId,
      `Refusing to touch ${type.kind} ${id}: its ownership labels do not match this worktree.`
    );
    return service;
  }

  /**
   * Everything this worktree owns, in removal order, every item verified.
   * Resources are found two ways — by Compose project and by ownership
   * labels — so a resource with the right owner but a mangled project label
   * is still found, and then refused by verification rather than orphaned.
   * Verification happens for all resources before any is returned, so a
   * caller that reaps the result cannot remove half and then hit a refusal.
   */
  async inventory(ref: WorktreeRef): Promise<OwnedResource[]> {
    const namespace = this.options.labelNamespace;
    const ownership = [
      `${namespace}.managed=true`,
      `${namespace}.repository-uuid=${ref.repositoryId}`,
      `${namespace}.worktree-uuid=${ref.worktreeId}`
    ];
    const owned: OwnedResource[] = [];
    for (const type of RESOURCE_TYPES) {
      // Images carry no Compose project label, and a project-only match would
      // sweep in images built before ownership labels existed.
      const byProject = type.kind === 'image'
        ? []
        : await this.list(type, [`${COMPOSE_PROJECT_LABEL}=${this.project(ref)}`]);
      const byOwnership = await this.list(type, ownership);
      for (const id of [...new Set([...byProject, ...byOwnership])].sort()) {
        const service = await this.verify(ref, type, id);
        owned.push(service ? { kind: type.kind, id, service } : { kind: type.kind, id });
      }
    }
    return owned;
  }

  async reap(ref: WorktreeRef, resource: OwnedResource): Promise<void> {
    const type = RESOURCE_TYPES.find((candidate) => candidate.kind === resource.kind);
    invariant(type, `Unknown resource kind: ${resource.kind}`);
    await this.verify(ref, type, resource.id);
    await this.options.runner.run('docker', [...type.removeArguments, resource.id]);
    this.options.logger.info(`Removed ${resource.kind} ${resource.id}.`);
  }
}
