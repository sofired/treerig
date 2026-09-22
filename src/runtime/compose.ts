import { lstat } from 'node:fs/promises';
import path from 'node:path';
import { z } from 'zod';
import { BRAND } from '../brand.ts';
import { runDocker } from '../docker.ts';
import { CliError, invariant } from '../errors.ts';
import { assertSafeDirectoryPath, atomicWrite } from '../files.ts';
import { hookEnvironmentVariables, worktreeEnvironment } from '../hooks/protocol.ts';
import { composeProjectName, worktreeHost, worktreeLayout } from '../layout.ts';
import type { Logger } from '../logger.ts';
import type { CommandRunner, RunOptions, RunResult } from '../process-runner.ts';
import { ROUTED_BY_LABEL, ROUTER_ENTRYPOINT } from '../router/stack.ts';
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
// them; a generated override file does it for every service, volume,
// network, and built image, so forgetting a label — and silently exempting a
// resource from cleanup — is not possible. `inventory` finds resources by
// those labels, verifies the full tuple on each one, and refuses everything
// if any single resource fails. `reap` verifies again before removing.
//
// The same override carries routing. Each `[[service]]` becomes a set of
// Traefik labels and a membership in the shared router network, so the
// consumer's Compose file never mentions the router at all.
//
// Only `up` writes the override. `status` and `endpoints` read the model and
// nothing else; `down` needs no model at all — it acts on the project name,
// so a Compose file that no longer parses cannot stop you from stopping it.

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
  /**
   * Absolute path to the consumer's Compose file. Its directory is Compose's
   * project directory, exactly as with `docker compose -f`, so relative
   * paths in the file resolve the way the consumer already expects.
   */
  composeFile: string;
  /** Directory containing the config file; reaches Compose and hooks as `BERTH_ROOT`. */
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
/**
 * Shell variables that would let the caller's environment change what Compose
 * loads or how it reconciles: which files, which `.env`, whether orphans are
 * kept. `DOCKER_HOST` and friends are deliberately not here — the plain
 * `docker` calls inherit them too, so stripping them would split `up` and
 * `remove` across daemons.
 */
const COMPOSE_FILE_VARIABLES = ['COMPOSE_FILE', 'COMPOSE_PATH_SEPARATOR', 'COMPOSE_ENV_FILES', 'COMPOSE_IGNORE_ORPHANS'];

/**
 * The environment every Compose invocation gets — the worktree runtime's and
 * the router's alike, so a shell setting cannot split behaviour between the
 * two projects the tool manages. The redirecting variables are dropped; plain
 * progress output keeps Compose's lines and the tool's own from fighting over
 * the terminal (older releases ignore the variable; they already fall back to
 * plain on a pipe). The runner passes this through verbatim, so what is
 * absent here is absent in the child.
 */
export function composeEnvironment(extra: NodeJS.ProcessEnv = {}): NodeJS.ProcessEnv {
  const inherited = Object.fromEntries(
    Object.entries(process.env).filter(([name]) => !COMPOSE_FILE_VARIABLES.includes(name))
  );
  return { ...inherited, COMPOSE_PROGRESS: 'plain', ...extra };
}

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
    build: z.record(z.string(), z.unknown()).optional(),
    expose: z.array(z.union([z.string(), z.number()])).optional(),
    ports: z.array(z.object({ target: z.number().optional() }).passthrough()).optional(),
    networks: z.unknown().optional()
  }).passthrough()).default({}),
  volumes: z.record(z.string(), z.object({ external: externalSchema }).passthrough()).default({}),
  networks: z.record(z.string(), z.object({ external: externalSchema }).passthrough()).default({})
}).passthrough();

type ComposeConfig = z.infer<typeof composeConfigSchema>;
type ComposeService = ComposeConfig['services'][string];

/** What the override says about one service; `build.labels` is what reaches the image. */
interface OverrideService {
  labels: Record<string, string>;
  networks?: Record<string, object>;
  build?: { labels: Record<string, string> };
}

interface OverrideDocument {
  services: Record<string, OverrideService>;
  volumes: Record<string, object>;
  networks: Record<string, object>;
}

const psRecordSchema = z.object({
  Service: z.string(),
  State: z.string(),
  Health: z.string().optional()
}).passthrough();
type PsRecord = z.infer<typeof psRecordSchema>;

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
    [ROUTED_BY_LABEL]: network,
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

function serviceState(record: PsRecord): Pick<ServiceStatus, 'state' | 'detail'> {
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

/**
 * `docker compose ps --format json` has printed one document per line since
 * Compose 2.21, the documented floor, and a single document — an array, or
 * `null` for none — before that; the older shape is accepted as well since
 * it costs nothing. Refuse to turn output nobody can parse into "every
 * service is absent": an empty list is a complete answer, garbage is not.
 */
function parsePsOutput(stdout: string): PsRecord[] {
  const text = stdout.trim();
  if (text === '') return [];
  const unreadable = (): CliError => new CliError(
    `Compose returned service state in a form this version of ${BRAND.name} cannot read. ` +
    'Run `docker compose version` and report it with the output below:\n' +
    `${text.slice(0, 400)}${text.length > 400 ? '\n…' : ''}`
  );
  const record = (candidate: unknown): PsRecord => {
    const parsed = psRecordSchema.safeParse(candidate);
    if (!parsed.success) throw unreadable();
    return parsed.data;
  };
  const parse = (fragment: string): unknown => {
    try {
      return JSON.parse(fragment);
    } catch {
      throw unreadable();
    }
  };

  let whole: unknown;
  try {
    whole = JSON.parse(text);
  } catch {
    // One document per line; every non-blank line must be a record.
    return text.split('\n').filter((line) => line.trim() !== '').map((line) => record(parse(line)));
  }
  if (whole === null) return [];
  if (Array.isArray(whole)) return (whole as unknown[]).map(record);
  return [record(whole)];
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

  private home(ref: WorktreeRef): string {
    return worktreeLayout(this.options.controlHome, ref.repositoryId, ref.worktreeId).home;
  }

  private overridePath(ref: WorktreeRef): string {
    // JSON content under a .yaml name: JSON is valid YAML, Compose reads it
    // as such, and it spares us a YAML emitter and every quoting bug one brings.
    return path.join(this.home(ref), `${BRAND.name}.override.yaml`);
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
    return composeEnvironment({
      ...hookEnvironmentVariables(worktreeEnvironment(this.options, ref)),
      COMPOSE_PROJECT_NAME: this.project(ref)
    });
  }

  /**
   * One Compose invocation. Files are passed only when a verb needs the
   * model; the project directory is then the consumer file's own directory,
   * as it would be with a plain `docker compose -f`.
   */
  private compose(
    ref: WorktreeRef,
    args: readonly string[],
    options: Omit<RunOptions, 'cwd'> & { files?: readonly string[] } = {}
  ): Promise<RunResult> {
    const { files, ...run } = options;
    return runDocker(this.options.runner, [
      'compose',
      '--project-name', this.project(ref),
      ...(files === undefined ? [] : ['--project-directory', path.dirname(this.options.composeFile)]),
      ...(files ?? []).flatMap((file) => ['-f', file]),
      ...args
    ], { ...run, env: { ...this.environment(ref), ...run.env } });
  }

  /** The consumer's Compose file as Compose itself resolves it. Reads nothing else, writes nothing. */
  private async readModel(ref: WorktreeRef): Promise<ComposeConfig> {
    const { composeFile } = this.options;
    const result = await this.compose(ref, ['config', '--format', 'json'], { files: [composeFile], reject: false });
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
    return parsed.data;
  }

  /**
   * Pure: the override that stamps ownership labels onto every service, every
   * built image, and every non-external volume and network, and routes the
   * declared services.
   */
  private overrideDocument(ref: WorktreeRef, config: ComposeConfig): OverrideDocument {
    const { composeFile } = this.options;
    const labels = this.ownershipLabels(ref);
    const stamp = () => ({ labels });
    const services: Record<string, OverrideService> = Object.fromEntries(
      Object.entries(config.services).map(([name, service]) => [
        name,
        // Service labels reach the container; only `build.labels` reach the image it builds.
        service.build === undefined ? stamp() : { ...stamp(), build: { labels } }
      ])
    );
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
          ...services[route.service],
          labels: { ...labels, ...routeLabels(`${this.project(ref)}-${route.service}`, route, routerNetwork, host, port) },
          // Adding the router network must not cost the service its own networks.
          networks: { ...serviceNetworks(service), [routerNetwork]: {} }
        };
      }
      networks[routerNetwork] = { external: true };
    }
    return { services, volumes, networks };
  }

  /**
   * Reads the model and writes the override into the worktree home. Only
   * `up` calls this: the home must already exist, created by the command
   * layer with its containment checks — the runtime never creates it.
   */
  private async prepare(ref: WorktreeRef): Promise<string[]> {
    const home = this.home(ref);
    await assertSafeDirectoryPath(this.options.controlHome, home);
    const present = await lstat(home).then((entry) => entry.isDirectory(), () => false);
    if (!present) {
      throw new CliError(
        `Worktree home ${home} does not exist. \`up\` creates it before starting services, ` +
        `so this is a bug in ${BRAND.name}; please report it.`
      );
    }
    const config = await this.readModel(ref);
    const document = this.overrideDocument(ref, config);
    const override = this.overridePath(ref);
    await atomicWrite(override, `${JSON.stringify(document, null, 2)}\n`);
    return [this.options.composeFile, override];
  }

  async up(ref: WorktreeRef, services: readonly ServiceSpec[], options: UpOptions = {}): Promise<void> {
    const files = await this.prepare(ref);
    const timeout = this.options.waitTimeoutSeconds ?? DEFAULT_WAIT_TIMEOUT_SECONDS;
    await this.compose(ref, [
      'up', '--detach', '--build', '--remove-orphans',
      '--wait', '--wait-timeout', String(timeout),
      ...services.map((service) => service.name)
    ], { files, printOutput: true, cancelSignal: options.signal });
  }

  /**
   * Stops and removes containers and networks by project name alone, so it
   * works whatever state the Compose file is in. Named volumes survive; that
   * is what `reap` is for.
   */
  async down(ref: WorktreeRef): Promise<void> {
    await this.compose(ref, ['down', '--remove-orphans'], { printOutput: true });
  }

  /** Every declared service, `absent` when it has no container at all. */
  async status(ref: WorktreeRef): Promise<ServiceStatus[]> {
    const config = await this.readModel(ref);
    const result = await this.compose(ref, ['ps', '--format', 'json', '--all'], { reject: false });
    if (result.exitCode !== 0) {
      throw new CliError(`Compose could not list services:\n${result.stderr.trim() || result.stdout.trim()}`);
    }
    const seen = new Map<string, ServiceStatus>();
    for (const record of parsePsOutput(result.stdout)) {
      seen.set(record.Service, { service: record.Service, ...serviceState(record) });
    }
    return Object.keys(config.services).sort().map((name) =>
      seen.get(name) ?? { service: name, state: 'absent' }
    );
  }

  /** Each service is reachable by name on the project network; the port is its first exposed one. */
  async endpoints(ref: WorktreeRef): Promise<Endpoint[]> {
    const config = await this.readModel(ref);
    const endpoints: Endpoint[] = [];
    for (const [name, service] of Object.entries(config.services)) {
      const port = firstPort(service);
      if (port !== undefined) endpoints.push({ service: name, host: name, port });
    }
    return endpoints.sort((left, right) => left.service.localeCompare(right.service));
  }

  private async list(type: ResourceType, labelFilters: readonly string[]): Promise<string[]> {
    const result = await runDocker(this.options.runner, [
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
    const result = await runDocker(this.options.runner, [
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
      // Built images carry the Compose project label too, but they are found
      // by ownership alone: a project-only match would sweep in images built
      // before the override stamped them, which verification would then refuse.
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
    if (!type) throw new CliError(`Unknown resource kind: ${resource.kind}`);
    await this.verify(ref, type, resource.id);
    await runDocker(this.options.runner, [...type.removeArguments, resource.id]);
    this.options.logger.info(`Removed ${resource.kind} ${resource.id}.`);
  }
}
