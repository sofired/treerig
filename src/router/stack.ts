import { BRAND } from '../brand.ts';

// The shared router, as a Compose document built from constants. It is
// written to the control home and started from there; the same constants
// drive the post-start verification in router.ts, so what is checked is by
// construction what was asked for. Everything is pinned by digest, runs
// read-only, drops every capability, and cannot escalate.
//
// Traefik never sees the Docker socket. It talks to a proxy that exposes the
// read-only subset of the API a router needs — containers, networks, events —
// and refuses everything else, including every POST.

/** Bumped whenever the stack changes shape; a newer router is never downgraded. */
export const ROUTER_PROTOCOL = 2;

export const ROUTER_IMAGE = 'traefik:v3.7.9@sha256:652929a140a32d7cafafb13c6cdfab5376cfeff800f51397b87b524501ed02a8';
export const DOCKER_API_IMAGE = 'tecnativa/docker-socket-proxy:v0.5.0@sha256:1f5038b54f06c3e18422902cf00ba21803d1c97805aae032e5e6673d532d3459';
export const INDEX_IMAGE = 'busybox:1.37.0@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0';

export const ROUTER_SERVICE = 'router';
export const DOCKER_API_SERVICE = 'docker-api';
export const INDEX_SERVICE = 'index';
/** The machine-wide checkout index answers here; a storage-contract name like the others. */
export const INDEX_HOST = `${BRAND.name}.localhost`;
export const INDEX_MOUNT = '/srv/index';
export const INDEX_PORT = 8080;
export const INDEX_COMMAND = ['httpd', '-f', '-p', String(INDEX_PORT), '-h', INDEX_MOUNT];
export const DOCKER_API_NETWORK = 'docker-api';
export const DOCKER_SOCKET = '/var/run/docker.sock';
/** Where the file provider looks inside the container; the control home's `router/dynamic` is mounted here. */
export const DYNAMIC_CONFIG_MOUNT = `/etc/${BRAND.name}/dynamic`;
export const ROUTER_ENTRYPOINT = 'web';

/** What the socket proxy lets through. Everything not listed here is denied. */
export const DOCKER_API_ACCESS = {
  ALLOW_PAUSE: '0',
  ALLOW_RESTARTS: '0',
  ALLOW_START: '0',
  ALLOW_STOP: '0',
  ALLOW_UNPAUSE: '0',
  AUTH: '0',
  BUILD: '0',
  COMMIT: '0',
  CONFIGS: '0',
  CONTAINERS: '1',
  DISTRIBUTION: '0',
  EVENTS: '1',
  EXEC: '0',
  GRPC: '0',
  IMAGES: '0',
  INFO: '0',
  NETWORKS: '1',
  NODES: '0',
  PING: '1',
  PLUGINS: '0',
  POST: '0',
  SECRETS: '0',
  SERVICES: '0',
  SESSION: '0',
  SWARM: '0',
  SYSTEM: '0',
  TASKS: '0',
  VERSION: '1',
  VOLUMES: '0'
} as const;

export interface RouterStackOptions {
  /** Compose project name; also prefixes the internal network. */
  projectName: string;
  /** Name of the shared network worktree services join to be routed. */
  network: string;
  /** Loopback port to publish. */
  port: number;
  /** Host directory watched by the file provider. */
  dynamicDir: string;
  /** Host directory the index container serves. */
  indexDir: string;
  labelNamespace: string;
}

export function indexUrl(port: number): string {
  return `http://${INDEX_HOST}${port === 80 ? '' : `:${port}`}`;
}

/** The index is routed like any worktree service: by labels, through the shared network. */
export function indexRouteLabels(): Record<string, string> {
  const name = `${BRAND.name}-index`;
  return {
    'traefik.enable': 'true',
    [`traefik.http.routers.${name}.rule`]: `Host(\`${INDEX_HOST}\`)`,
    [`traefik.http.routers.${name}.entrypoints`]: ROUTER_ENTRYPOINT,
    [`traefik.http.services.${name}.loadbalancer.server.port`]: String(INDEX_PORT)
  };
}

export function routerLabels(labelNamespace: string, component: string): Record<string, string> {
  return {
    [`${labelNamespace}.component`]: component,
    [`${labelNamespace}.router-protocol`]: String(ROUTER_PROTOCOL)
  };
}

/** Traefik's full command line; verification checks the running container has exactly this. */
export function routerCommand(options: Pick<RouterStackOptions, 'network'>): string[] {
  return [
    `--entrypoints.${ROUTER_ENTRYPOINT}.address=:80`,
    '--providers.docker=true',
    `--providers.docker.endpoint=tcp://${DOCKER_API_SERVICE}:2375`,
    '--providers.docker.exposedbydefault=false',
    `--providers.docker.network=${options.network}`,
    `--providers.file.directory=${DYNAMIC_CONFIG_MOUNT}`,
    '--providers.file.watch=true',
    '--api.dashboard=false',
    '--ping=true'
  ];
}

const HARDENED = {
  read_only: true,
  cap_drop: ['ALL'],
  security_opt: ['no-new-privileges:true']
};

const HEALTHCHECK = { interval: '2s', timeout: '3s', retries: 30 };

export function routerStack(options: RouterStackOptions): Record<string, unknown> {
  return {
    services: {
      [DOCKER_API_SERVICE]: {
        image: DOCKER_API_IMAGE,
        restart: 'unless-stopped',
        environment: DOCKER_API_ACCESS,
        ...HARDENED,
        tmpfs: ['/run', '/tmp'],
        volumes: [{ type: 'bind', source: DOCKER_SOCKET, target: DOCKER_SOCKET, read_only: true }],
        healthcheck: { test: ['CMD-SHELL', 'wget -qO- http://127.0.0.1:2375/_ping | grep -qx OK'], ...HEALTHCHECK },
        labels: routerLabels(options.labelNamespace, DOCKER_API_SERVICE),
        networks: [DOCKER_API_NETWORK]
      },
      [ROUTER_SERVICE]: {
        image: ROUTER_IMAGE,
        restart: 'unless-stopped',
        command: routerCommand(options),
        ports: [`127.0.0.1:${options.port}:80`],
        ...HARDENED,
        cap_add: ['NET_BIND_SERVICE'],
        tmpfs: ['/tmp'],
        volumes: [{ type: 'bind', source: options.dynamicDir, target: DYNAMIC_CONFIG_MOUNT, read_only: true }],
        depends_on: { [DOCKER_API_SERVICE]: { condition: 'service_healthy' } },
        healthcheck: { test: ['CMD', 'traefik', 'healthcheck', '--ping'], ...HEALTHCHECK },
        labels: routerLabels(options.labelNamespace, ROUTER_SERVICE),
        networks: [ROUTER_SERVICE, DOCKER_API_NETWORK]
      },
      [INDEX_SERVICE]: {
        image: INDEX_IMAGE,
        restart: 'unless-stopped',
        command: INDEX_COMMAND,
        ...HARDENED,
        volumes: [{ type: 'bind', source: options.indexDir, target: INDEX_MOUNT, read_only: true }],
        healthcheck: { test: ['CMD', 'wget', '-qO-', `http://127.0.0.1:${INDEX_PORT}/index.html`], ...HEALTHCHECK },
        labels: { ...routerLabels(options.labelNamespace, INDEX_SERVICE), ...indexRouteLabels() },
        networks: [ROUTER_SERVICE]
      }
    },
    networks: {
      [ROUTER_SERVICE]: { name: options.network },
      [DOCKER_API_NETWORK]: { internal: true }
    }
  };
}
