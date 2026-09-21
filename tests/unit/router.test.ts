import { mkdtemp, readFile, rm, stat } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { controlLayout } from '../../src/layout.ts';
import type { CommandRunner, RunOptions, RunResult } from '../../src/process-runner.ts';
import { RouterService, type ContainerInspect } from '../../src/router/router.ts';
import {
  DOCKER_API_ACCESS,
  DOCKER_API_IMAGE,
  DOCKER_SOCKET,
  DYNAMIC_CONFIG_MOUNT,
  INDEX_COMMAND,
  INDEX_IMAGE,
  INDEX_MOUNT,
  indexRouteLabels,
  ROUTER_IMAGE,
  ROUTER_PROTOCOL,
  routerCommand
} from '../../src/router/stack.ts';
import { RecordingLogger } from '../helpers/command-doubles.ts';

const PROJECT = 'berth-router';
const NETWORK = 'berth-router';
const NS = 'dev.berth';

interface Call { args: readonly string[]; options?: RunOptions }

/** Answers the compose/inspect calls the router makes; `docs` is what `docker inspect` would return. */
class DockerFake implements CommandRunner {
  readonly calls: Call[] = [];
  ids: Record<string, string> = { router: 'router-1', 'docker-api': 'api-1', index: 'index-1' };
  docs = new Map<string, ContainerInspect>();
  upExitCode = 0;
  upStderr = '';
  downExitCode = 0;
  downStderr = '';
  /** Called when `up --force-recreate` runs, so a test can make the recreation "fix" things. */
  onRecreate?: () => void;

  run(_command: string, args: readonly string[] = [], options?: RunOptions): Promise<RunResult> {
    this.calls.push({ args, options });
    const ok = (stdout = ''): Promise<RunResult> => Promise.resolve({ stdout, stderr: '', exitCode: 0 });
    if (args[0] === 'compose') {
      if (args.includes('ps')) return ok(this.ids[args.at(-1) ?? ''] ?? '');
      if (args.includes('up')) {
        if (args.includes('--force-recreate')) this.onRecreate?.();
        return Promise.resolve({ stdout: '', stderr: this.upStderr, exitCode: this.upExitCode });
      }
      if (args.includes('down')) return Promise.resolve({ stdout: '', stderr: this.downStderr, exitCode: this.downExitCode });
      return ok();
    }
    if (args[0] === 'inspect') {
      const doc = this.docs.get(args[1] ?? '');
      return doc ? ok(JSON.stringify([doc])) : Promise.resolve({ stdout: '', stderr: 'no such object', exitCode: 1 });
    }
    return ok();
  }

  which(): Promise<boolean> {
    return Promise.resolve(true);
  }
}

const temporary: string[] = [];
afterEach(async () => {
  await Promise.all(temporary.splice(0).map((d) => rm(d, { recursive: true, force: true })));
});

function labels(service: string): Record<string, string> {
  return {
    'com.docker.compose.project': PROJECT,
    'com.docker.compose.service': service,
    [`${NS}.component`]: service,
    [`${NS}.router-protocol`]: String(ROUTER_PROTOCOL)
  };
}

/** What Docker reports for a router started exactly as the stack asks. */
function routerDoc(dynamicDir: string, port = 80): ContainerInspect {
  return {
    Config: { Cmd: routerCommand({ network: NETWORK }), Image: ROUTER_IMAGE, Labels: labels('router') },
    HostConfig: { CapAdd: ['CAP_NET_BIND_SERVICE'], CapDrop: ['ALL'], ReadonlyRootfs: true, SecurityOpt: ['no-new-privileges:true'] },
    Mounts: [{ Type: 'bind', Source: dynamicDir, Destination: DYNAMIC_CONFIG_MOUNT, RW: false }],
    NetworkSettings: {
      Networks: { [NETWORK]: {}, [`${PROJECT}_docker-api`]: {} },
      Ports: { '80/tcp': [{ HostIp: '127.0.0.1', HostPort: String(port) }] }
    },
    State: { Status: 'running', Health: { Status: 'healthy' } }
  };
}

function apiDoc(): ContainerInspect {
  return {
    Config: {
      Env: Object.entries(DOCKER_API_ACCESS).map(([name, value]) => `${name}=${value}`),
      Image: DOCKER_API_IMAGE,
      Labels: labels('docker-api')
    },
    HostConfig: { CapAdd: [], CapDrop: ['ALL'], ReadonlyRootfs: true, SecurityOpt: ['no-new-privileges:true'] },
    Mounts: [{ Type: 'bind', Source: DOCKER_SOCKET, Destination: DOCKER_SOCKET, RW: false }],
    NetworkSettings: { Networks: { [`${PROJECT}_docker-api`]: {} }, Ports: {} },
    State: { Status: 'running', Health: { Status: 'healthy' } }
  };
}

function indexDoc(indexDir: string): ContainerInspect {
  return {
    Config: { Cmd: [...INDEX_COMMAND], Image: INDEX_IMAGE, Labels: { ...labels('index'), ...indexRouteLabels() } },
    HostConfig: { CapAdd: [], CapDrop: ['ALL'], ReadonlyRootfs: true, SecurityOpt: ['no-new-privileges:true'] },
    Mounts: [{ Type: 'bind', Source: indexDir, Destination: INDEX_MOUNT, RW: false }],
    NetworkSettings: { Networks: { [NETWORK]: {} }, Ports: {} },
    State: { Status: 'running', Health: { Status: 'healthy' } }
  };
}

async function world(port = 80) {
  const controlHome = await mkdtemp(path.join(tmpdir(), 'berth-router-'));
  temporary.push(controlHome);
  const fake = new DockerFake();
  const logger = new RecordingLogger();
  const layout = controlLayout(controlHome);
  const healthy = (): void => {
    fake.docs.set('router-1', routerDoc(layout.routerDynamicDir, port));
    fake.docs.set('api-1', apiDoc());
    fake.docs.set('index-1', indexDoc(layout.indexDir));
  };
  healthy();
  const router = new RouterService({ controlHome, port, runner: fake, logger });
  const ups = () => fake.calls.filter((c) => c.args[0] === 'compose' && c.args.includes('up')).map((c) => c.args);
  return { controlHome, layout, fake, logger, router, healthy, ups };
}

describe('RouterService.ensure', () => {
  it('writes the stack, starts it once, verifies it, and leaves a correct router alone', async () => {
    const w = await world();
    await w.router.ensure();

    const document = JSON.parse(await readFile(w.layout.routerComposeFile, 'utf8')) as {
      services: Record<string, { image: string; ports?: string[]; command?: string[] }>;
      networks: Record<string, { name?: string; internal?: boolean }>;
    };
    expect(Object.keys(document.services).sort()).toEqual(['docker-api', 'index', 'router']);
    expect((await stat(w.layout.indexFile)).isFile()).toBe(true);   // a page exists before the health check needs one
    expect(document.services.router?.ports).toEqual(['127.0.0.1:80:80']);
    expect(document.services.router?.command).toContain(`--providers.file.directory=${DYNAMIC_CONFIG_MOUNT}`);
    expect(document.networks).toEqual({ router: { name: NETWORK }, 'docker-api': { internal: true } });
    expect((await stat(w.layout.routerDynamicDir)).isDirectory()).toBe(true);

    expect(w.ups()).toEqual([[
      'compose', '--project-directory', w.layout.routerDir, '--project-name', PROJECT, '-f', w.layout.routerComposeFile,
      'up', '--detach', '--wait', '--remove-orphans'
    ]]);
    expect(w.logger.lines.filter((l) => l.startsWith('warn:'))).toEqual([]);
  });

  it('recreates a drifted router once, and accepts it when the recreation fixes it', async () => {
    const w = await world();
    w.fake.docs.set('router-1', { ...routerDoc(w.layout.routerDynamicDir), HostConfig: { ...routerDoc(w.layout.routerDynamicDir).HostConfig, ReadonlyRootfs: false } });
    w.fake.onRecreate = w.healthy;

    await w.router.ensure();

    expect(w.ups().map((args) => args.includes('--force-recreate'))).toEqual([false, true]);
    expect(w.logger.lines).toContainEqual(expect.stringContaining('warn: The shared router is not configured as expected (router: root filesystem is writable)'));
  });

  it('fails, listing every problem, when recreation does not help', async () => {
    const w = await world();
    const bad = routerDoc(w.layout.routerDynamicDir);
    bad.HostConfig = { ...bad.HostConfig, CapDrop: [] };
    bad.NetworkSettings = { ...bad.NetworkSettings, Ports: { '80/tcp': [{ HostIp: '0.0.0.0', HostPort: '80' }] } };
    w.fake.docs.set('router-1', bad);

    await expect(w.router.ensure()).rejects.toThrow(
      'could not be brought to its expected configuration:\n  router: capabilities are not dropped\n  router: port 80 is not published only on 127.0.0.1:80'
    );
    expect(w.ups()).toHaveLength(2);
  });

  it('refuses to downgrade a router started by a newer version', async () => {
    const w = await world();
    const newer = routerDoc(w.layout.routerDynamicDir);
    newer.Config = { ...newer.Config, Labels: { ...labels('router'), [`${NS}.router-protocol`]: String(ROUTER_PROTOCOL + 1) } };
    w.fake.docs.set('router-1', newer);

    await expect(w.router.ensure()).rejects.toThrow(`started by a newer version (protocol ${ROUTER_PROTOCOL + 1}; this version speaks ${ROUTER_PROTOCOL})`);
    expect(w.ups()).toEqual([]);
  });

  it('explains a failed start in terms of the port', async () => {
    const w = await world(8080);
    w.fake.upExitCode = 1;
    w.fake.upStderr = 'Error response from daemon: Bind for 127.0.0.1:8080 failed: port is already allocated';
    await expect(w.router.ensure()).rejects.toThrow(/127\.0\.0\.1:8080 is free, or choose another port with BERTH_ROUTER_PORT[\s\S]*port is already allocated/);
  });
});

describe('RouterService.verify', () => {
  const tampering: [string, (router: ContainerInspect, api: ContainerInspect, index: ContainerInspect) => void, string][] = [
    ['the index serving another directory', (_r, _a, i) => { i.Mounts = [{ Type: 'bind', Source: '/etc', Destination: INDEX_MOUNT, RW: false }]; }, 'index: mounts are not exactly the read-only index directory'],
    ['the index publishing a port', (_r, _a, i) => { i.NetworkSettings = { ...i.NetworkSettings, Ports: { '8080/tcp': [{ HostIp: '0.0.0.0', HostPort: '8080' }] } }; }, 'index: publishes a port'],
    ['the index routed elsewhere', (_r, _a, i) => { i.Config = { ...i.Config, Labels: { ...i.Config?.Labels, 'traefik.http.routers.berth-index.rule': 'Host(`evil.localhost`)' } }; }, 'index: routing labels differ'],
    ['a writable root filesystem', (r) => { r.HostConfig = { ...r.HostConfig, ReadonlyRootfs: false }; }, 'router: root filesystem is writable'],
    ['kept capabilities', (r) => { r.HostConfig = { ...r.HostConfig, CapDrop: [] }; }, 'router: capabilities are not dropped'],
    ['extra capabilities', (r) => { r.HostConfig = { ...r.HostConfig, CapAdd: ['CAP_NET_BIND_SERVICE', 'CAP_SYS_ADMIN'] }; }, 'router: capabilities beyond NET_BIND_SERVICE'],
    ['a port on every interface', (r) => { r.NetworkSettings = { ...r.NetworkSettings, Ports: { '80/tcp': [{ HostIp: '0.0.0.0', HostPort: '80' }] } }; }, 'published only on 127.0.0.1:80'],
    ['an extra published port', (r) => { r.NetworkSettings = { ...r.NetworkSettings, Ports: { ...r.NetworkSettings?.Ports, '8080/tcp': [{ HostIp: '127.0.0.1', HostPort: '8080' }] } }; }, 'unexpected ports (8080/tcp)'],
    ['the file provider removed', (r) => { r.Config = { ...r.Config, Cmd: routerCommand({ network: NETWORK }).filter((a) => !a.startsWith('--providers.file')) }; }, 'router: command line differs'],
    ['a different image', (r) => { r.Config = { ...r.Config, Image: 'traefik:latest' }; }, 'router: image is not the pinned digest'],
    ['an extra mount', (r) => { r.Mounts = [...(r.Mounts ?? []), { Type: 'bind', Source: '/etc', Destination: '/host-etc', RW: false }]; }, 'router: mounts are not exactly'],
    ['a writable dynamic directory', (r) => { r.Mounts = (r.Mounts ?? []).map((m) => ({ ...m, RW: true })); }, 'router: mounts are not exactly'],
    ['an unhealthy router', (r) => { r.State = { Status: 'running', Health: { Status: 'unhealthy' } }; }, 'router: not healthy'],
    ['a foreign container', (r) => { r.Config = { ...r.Config, Labels: { ...labels('router'), 'com.docker.compose.project': 'other' } }; }, 'router: not the container Compose started for this project'],
    ['POST allowed through the proxy', (_r, a) => { a.Config = { ...a.Config, Env: (a.Config?.Env ?? []).map((e) => e === 'POST=0' ? 'POST=1' : e) }; }, 'docker-api: API access differs (POST)'],
    ['a writable socket', (_r, a) => { a.Mounts = (a.Mounts ?? []).map((m) => ({ ...m, RW: true })); }, 'docker-api: mounts are not exactly the read-only Docker socket'],
    ['the proxy on the shared network', (_r, a) => { a.NetworkSettings = { ...a.NetworkSettings, Networks: { ...a.NetworkSettings?.Networks, [NETWORK]: {} } }; }, 'docker-api: attached to a network other than the internal proxy network'],
    ['the proxy publishing a port', (_r, a) => { a.NetworkSettings = { ...a.NetworkSettings, Ports: { '2375/tcp': [{ HostIp: '127.0.0.1', HostPort: '2375' }] } }; }, 'docker-api: publishes a port']
  ];

  it.each(tampering)('reports %s', async (_name, tamper, expected) => {
    const w = await world();
    const router = routerDoc(w.layout.routerDynamicDir);
    const api = apiDoc();
    const index = indexDoc(w.layout.indexDir);
    tamper(router, api, index);
    w.fake.docs.set('router-1', router);
    w.fake.docs.set('api-1', api);
    w.fake.docs.set('index-1', index);
    const problems = await w.router.verify();
    expect(problems).toHaveLength(1);
    expect(problems[0]).toContain(expected);
  });

  it('is clean for a router exactly as the stack asked for it', async () => {
    const w = await world();
    await expect(w.router.verify()).resolves.toEqual([]);
  });
});

describe('RouterService.status and down', () => {
  it('reports a router that is not running', async () => {
    const w = await world();
    w.fake.ids = {};
    await expect(w.router.status()).resolves.toEqual({ running: false, problems: ['the router is not running'], port: 80, network: NETWORK });
  });

  it('treats the shared network staying behind as the expected outcome of down', async () => {
    const w = await world();
    w.fake.downExitCode = 1;
    w.fake.downStderr = 'Error response from daemon: error while removing network: network berth-router has active endpoints';
    await expect(w.router.down()).resolves.toBeUndefined();
    expect(w.logger.lines).toContainEqual('info: Network berth-router is kept while worktree services are attached to it.');

    w.fake.downStderr = 'permission denied';
    await expect(w.router.down()).rejects.toThrow('did not stop cleanly');
  });
});
