import { mkdir, mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { pathExists } from '../../src/files.ts';
import { composeProjectName, worktreeLayout } from '../../src/layout.ts';
import { silentLogger } from '../../src/logger.ts';
import type { CommandRunner, RunOptions, RunResult } from '../../src/process-runner.ts';
import { ComposeRuntime, type RouteSpec } from '../../src/runtime/compose.ts';
import type { OwnedResource, WorktreeRef } from '../../src/runtime/runtime.ts';
import { ROUTED_BY_LABEL } from '../../src/router/stack.ts';

const NS = 'dev.berth';
const REF: WorktreeRef = {
  repositoryId: 'a08b4892-5111-49f1-a47a-5ab988c7f676',
  worktreeId: '2b41d9d4-ed88-49c1-be9c-5439bdf6a431',
  slug: 'feature-login'
};
const PROJECT = composeProjectName(REF.repositoryId, REF.worktreeId);

const CONFIG = {
  name: PROJECT,
  services: {
    api: { image: 'busybox', expose: ['8080/tcp'], volumes: [{ type: 'volume', source: 'data', target: '/data' }] },
    worker: { image: 'busybox', ports: [{ mode: 'ingress', target: 9090, protocol: 'tcp' }] },
    quiet: { image: 'busybox', networks: { backend: null } },
    builder: { build: { context: '/work/acme/docker/builder', dockerfile: 'Dockerfile' }, expose: ['7000'] }
  },
  volumes: {
    data: { name: `${PROJECT}_data` },
    shared: { name: 'shared-external', external: true }
  },
  networks: {
    backend: { name: `${PROJECT}_backend` },
    corp: { name: 'corp', external: true }
  }
};

interface Call { command: string; args: readonly string[]; options?: RunOptions }

/** Answers every docker/compose invocation the runtime makes, with knobs for the failure modes. */
class DockerFake implements CommandRunner {
  readonly calls: Call[] = [];
  config: object = CONFIG;
  psLines: object[] = [];
  /** Raw `ps` stdout, overriding `psLines`, for output shapes that are not line-delimited. */
  psOutput?: string;
  resources: Record<string, string[]> = { ps: [], volume: [], network: [], image: [] };
  labels = new Map<string, Record<string, string>>();
  uninspectable = new Set<string>();

  /** `composeLabels: false` is an image from Compose's bake builder, which stamps only our `build.labels`. */
  own(id: string, service = '', { composeLabels = true } = {}): void {
    this.labels.set(id, {
      [`${NS}.managed`]: 'true',
      [`${NS}.repository-uuid`]: REF.repositoryId,
      [`${NS}.worktree-uuid`]: REF.worktreeId,
      ...(composeLabels ? { 'com.docker.compose.project': PROJECT, 'com.docker.compose.service': service } : {})
    });
  }

  run(command: string, args: readonly string[] = [], options?: RunOptions): Promise<RunResult> {
    this.calls.push({ command, args, options });
    const ok = (stdout = ''): Promise<RunResult> => Promise.resolve({ stdout, stderr: '', exitCode: 0 });

    if (args[0] === 'compose') {
      const verb = ['config', 'up', 'down', 'ps'].find((candidate) => args.includes(candidate));
      if (verb === 'config') return ok(JSON.stringify(this.config));
      if (verb === 'ps') return ok(this.psOutput ?? this.psLines.map((line) => JSON.stringify(line)).join('\n'));
      return ok();
    }
    if (args.includes('--filter')) {
      const kind = args[0] === 'ps' ? 'ps' : args[0] ?? '';
      return ok((this.resources[kind] ?? []).join('\n'));
    }
    if (args.includes('--format')) {
      const id = args.at(-1) ?? '';
      if (this.uninspectable.has(id)) return Promise.resolve({ stdout: '', stderr: 'no such object', exitCode: 1 });
      const format = args[args.indexOf('--format') + 1] ?? '';
      const names = [...format.matchAll(/"([^"]+)"/g)].map((m) => m[1] ?? '');
      const labels = this.labels.get(id) ?? {};
      return ok(names.map((name) => labels[name] ?? '').join('\t'));
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

const overrideIn = (controlHome: string): string =>
  path.join(worktreeLayout(controlHome, REF.repositoryId, REF.worktreeId).home, 'berth.override.yaml');

async function runtime(fake = new DockerFake(), routes: readonly RouteSpec[] = [], { home = true } = {}) {
  const controlHome = await mkdtemp(path.join(tmpdir(), 'berth-compose-'));
  temporary.push(controlHome);
  // The command layer creates the home before `up`; the runtime never does.
  if (home) await mkdir(worktreeLayout(controlHome, REF.repositoryId, REF.worktreeId).home, { recursive: true });
  return {
    fake,
    controlHome,
    runtime: new ComposeRuntime({
      composeFile: '/work/acme/docker/compose.yaml',
      projectRoot: '/work/acme',
      configFile: '/work/acme/berth.toml',
      projectName: 'acme',
      domain: 'acme.localhost',
      controlHome,
      labelNamespace: NS,
      routes,
      routerNetwork: 'berth-router',
      runner: fake,
      logger: silentLogger,
      waitTimeoutSeconds: 45
    })
  };
}

const composeCalls = (fake: DockerFake, verb: string) =>
  fake.calls.filter((c) => c.args[0] === 'compose' && c.args.includes(verb));

describe('ComposeRuntime up', () => {
  it('stamps ownership labels onto every service and every non-external volume and network', async () => {
    const { runtime: rt, controlHome } = await runtime();
    await rt.up(REF, [{ name: 'api' }, { name: 'worker' }, { name: 'quiet' }]);

    const override = path.join(worktreeLayout(controlHome, REF.repositoryId, REF.worktreeId).home, 'berth.override.yaml');
    const document = JSON.parse(await readFile(override, 'utf8')) as {
      services: Record<string, { labels: Record<string, string> }>;
      volumes: Record<string, unknown>;
      networks: Record<string, unknown>;
    };
    const expectedLabels = {
      [`${NS}.managed`]: 'true',
      [`${NS}.repository-uuid`]: REF.repositoryId,
      [`${NS}.worktree-uuid`]: REF.worktreeId
    };
    expect(Object.keys(document.services).sort()).toEqual(['api', 'builder', 'quiet', 'worker']);
    expect(document.services.api?.labels).toEqual(expectedLabels);
    expect(Object.keys(document.volumes)).toEqual(['data']);          // `shared` is external
    expect(Object.keys(document.networks).sort()).toEqual(['backend', 'default']);  // `corp` is external; default is used by api/worker
  });

  it('stamps build labels on services that build, and only those, so the images they produce are owned too', async () => {
    const { runtime: rt, controlHome } = await runtime(new DockerFake(), [{ service: 'builder', subdomain: 'b' }]);
    await rt.up(REF, []);

    const document = JSON.parse(await readFile(overrideIn(controlHome), 'utf8')) as {
      services: Record<string, { labels: Record<string, string>; build?: { labels: Record<string, string> } }>;
    };
    const ownership = {
      [`${NS}.managed`]: 'true',
      [`${NS}.repository-uuid`]: REF.repositoryId,
      [`${NS}.worktree-uuid`]: REF.worktreeId
    };
    expect(document.services.builder?.build).toEqual({ labels: ownership });
    expect(document.services.builder?.labels).toMatchObject({ ...ownership, 'traefik.enable': 'true' });   // routed and built
    expect(document.services.api?.build).toBeUndefined();
  });

  it('refuses to run without the worktree home, which only the command layer creates', async () => {
    const { fake, runtime: rt, controlHome } = await runtime(new DockerFake(), [], { home: false });
    await expect(rt.up(REF, [])).rejects.toThrow('does not exist. `up` creates it before starting services, so this is a bug in berth');
    expect(await pathExists(overrideIn(controlHome))).toBe(false);
    expect(fake.calls).toEqual([]);   // refused before spawning anything
  });

  it('routes declared services with Traefik labels and the shared network, leaving the rest alone', async () => {
    const { runtime: rt, controlHome } = await runtime(new DockerFake(), [
      { service: 'api', subdomain: '' },
      { service: 'worker', subdomain: 'jobs', path: '/v2', port: 9091 }
    ]);
    await rt.up(REF, []);

    const override = path.join(worktreeLayout(controlHome, REF.repositoryId, REF.worktreeId).home, 'berth.override.yaml');
    const document = JSON.parse(await readFile(override, 'utf8')) as {
      services: Record<string, { labels: Record<string, string>; networks?: Record<string, object> }>;
      networks: Record<string, unknown>;
    };
    expect(document.services.api).toEqual({
      labels: {
        [`${NS}.managed`]: 'true',
        [`${NS}.repository-uuid`]: REF.repositoryId,
        [`${NS}.worktree-uuid`]: REF.worktreeId,
        'traefik.enable': 'true',
        'traefik.docker.network': 'berth-router',
        [ROUTED_BY_LABEL]: 'berth-router',
        [`traefik.http.routers.${PROJECT}-api.rule`]: 'Host(`feature-login.acme.localhost`)',
        [`traefik.http.routers.${PROJECT}-api.entrypoints`]: 'web',
        [`traefik.http.services.${PROJECT}-api.loadbalancer.server.port`]: '8080'   // from `expose`
      },
      networks: { default: {}, 'berth-router': {} }                                   // keeps its implicit default
    });
    expect(document.services.worker?.labels).toMatchObject({
      [`traefik.http.routers.${PROJECT}-worker.rule`]: 'Host(`jobs.feature-login.acme.localhost`) && PathPrefix(`/v2`)',
      [`traefik.http.services.${PROJECT}-worker.loadbalancer.server.port`]: '9091'   // explicit port wins
    });
    expect(document.services.quiet).toEqual({ labels: expect.objectContaining({ [`${NS}.managed`]: 'true' }) as object });
    expect(Object.keys(document.services.quiet?.labels ?? {}).some((k) => k.startsWith('traefik.'))).toBe(false);
    expect(document.networks['berth-router']).toEqual({ external: true });
  });

  it('refuses to route a service with no port, and one the Compose file does not define', async () => {
    const noPort = await runtime(new DockerFake(), [{ service: 'quiet', subdomain: 'q' }]);
    await expect(noPort.runtime.up(REF, [])).rejects.toThrow('Service quiet declares no port');
    const missing = await runtime(new DockerFake(), [{ service: 'ghost', subdomain: '' }]);
    await expect(missing.runtime.up(REF, [])).rejects.toThrow('[[service]] ghost is not defined');
  });

  it('runs compose up detached with build, orphan removal, and a bounded wait, naming the requested services', async () => {
    const { fake, runtime: rt, controlHome } = await runtime();
    await rt.up(REF, [{ name: 'api' }]);

    const [up] = composeCalls(fake, 'up');
    expect(up?.args).toEqual([
      'compose',
      '--project-name', PROJECT,
      // The Compose file's own directory, as with `docker compose -f`: relative paths in it resolve unchanged.
      '--project-directory', '/work/acme/docker',
      '-f', '/work/acme/docker/compose.yaml',
      '-f', overrideIn(controlHome),
      'up', '--detach', '--build', '--remove-orphans', '--wait', '--wait-timeout', '45',
      'api'
    ]);
    expect(up?.options?.printOutput).toBe(true);
  });

  it('hands Compose the same BERTH_* vocabulary hooks receive, plus the project name', async () => {
    const { fake, runtime: rt, controlHome } = await runtime();
    await rt.up(REF, []);

    const [up] = composeCalls(fake, 'up');
    expect(up?.options?.env).toMatchObject({
      BERTH_PROJECT: 'acme',
      BERTH_REPOSITORY_ID: REF.repositoryId,
      BERTH_WORKTREE_ID: REF.worktreeId,
      BERTH_SLUG: 'feature-login',
      BERTH_HOST: 'feature-login.acme.localhost',
      BERTH_WORKTREE_HOME: worktreeLayout(controlHome, REF.repositoryId, REF.worktreeId).home,
      BERTH_CONFIG: '/work/acme/berth.toml',
      BERTH_ROOT: '/work/acme',
      COMPOSE_PROJECT_NAME: PROJECT,
      COMPOSE_PROGRESS: 'plain'
    });
    expect(up?.options?.env).not.toHaveProperty('BERTH_STAGE');
  });

  it('keeps the shell from redirecting Compose to other files', async () => {
    const { fake, runtime: rt } = await runtime();
    const redirecting = ['COMPOSE_FILE', 'COMPOSE_PATH_SEPARATOR', 'COMPOSE_ENV_FILES', 'COMPOSE_IGNORE_ORPHANS'];
    for (const name of redirecting) process.env[name] = 'from-the-shell';
    try {
      await rt.up(REF, []);
    } finally {
      for (const name of redirecting) delete process.env[name];
    }
    const [up] = composeCalls(fake, 'up');
    for (const name of redirecting) expect(up?.options?.env).not.toHaveProperty(name);
    // The runner passes a supplied environment through verbatim (see process-runner tests), so this is the child's env.
  });

  it('reads the config without the override, since the override does not exist yet', async () => {
    const { fake, runtime: rt } = await runtime();
    await rt.up(REF, []);
    const [config] = composeCalls(fake, 'config');
    expect(config?.args.filter((a) => a === '-f')).toHaveLength(1);
    expect(config?.args).toContain('--project-directory');
  });
});

describe('ComposeRuntime down, status, endpoints', () => {
  it('down acts on the project name alone — no model, no files, no override — and leaves named volumes to reap', async () => {
    const { fake, runtime: rt, controlHome } = await runtime();
    await rt.down(REF);
    const [down] = composeCalls(fake, 'down');
    expect(down?.args).toEqual(['compose', '--project-name', PROJECT, 'down', '--remove-orphans']);
    expect(composeCalls(fake, 'config')).toEqual([]);
    expect(await pathExists(overrideIn(controlHome))).toBe(false);
  });

  it('status maps Compose states and reports declared services that have no container as absent', async () => {
    const fake = new DockerFake();
    fake.psLines = [
      { Service: 'api', State: 'running', Health: 'healthy' },
      { Service: 'worker', State: 'running', Health: 'starting' },
      { Service: 'noise', State: 'exited', Health: '' }
    ];
    const { runtime: rt, controlHome } = await runtime(fake);
    await expect(rt.status(REF)).resolves.toEqual([
      { service: 'api', state: 'running' },
      { service: 'builder', state: 'absent' },
      { service: 'quiet', state: 'absent' },
      { service: 'worker', state: 'starting' }
    ]);
    // Read-only verbs never write the override.
    expect(await pathExists(overrideIn(controlHome))).toBe(false);
    const [ps] = composeCalls(fake, 'ps');
    expect(ps?.args).not.toContain('-f');
  });

  it('status accepts the single-array form older Compose releases print', async () => {
    const fake = new DockerFake();
    fake.psOutput = JSON.stringify([{ Service: 'api', State: 'running' }, { Service: 'worker', State: 'exited' }]);
    const { runtime: rt } = await runtime(fake);
    const status = await rt.status(REF);
    expect(status.find((s) => s.service === 'api')?.state).toBe('running');
    expect(status.find((s) => s.service === 'worker')?.state).toBe('stopped');
  });

  it('status treats an empty array or null — older Compose with no containers — as everything absent', async () => {
    for (const output of ['[]', '[]\n', 'null']) {
      const fake = new DockerFake();
      fake.psOutput = output;
      const { runtime: rt } = await runtime(fake);
      const states = (await rt.status(REF)).map((s) => s.state);
      expect(states, output).toEqual(['absent', 'absent', 'absent', 'absent']);
    }
  });

  it('status refuses to report anything absent when the ps output is unreadable, even partially', async () => {
    for (const output of [
      'NAME   IMAGE   COMMAND\nberth-x-api-1   busybox   "sleep"',
      '{"Service":"api","State":"running"}\n{"Service":"worker","State":"runn',   // truncated second line
      '[{"Service":"api","State":"running"},{"nope":true}]'
    ]) {
      const fake = new DockerFake();
      fake.psOutput = output;
      const { runtime: rt } = await runtime(fake);
      await expect(rt.status(REF), output).rejects.toThrow('Run `docker compose version` and report it');
    }
  });

  it('status surfaces an unhealthy running container without inventing a state', async () => {
    const fake = new DockerFake();
    fake.psLines = [{ Service: 'api', State: 'running', Health: 'unhealthy' }, { Service: 'worker', State: 'exited' }];
    const { runtime: rt } = await runtime(fake);
    const status = await rt.status(REF);
    expect(status.find((s) => s.service === 'api')).toEqual({ service: 'api', state: 'running', detail: 'unhealthy' });
    expect(status.find((s) => s.service === 'worker')).toEqual({ service: 'worker', state: 'stopped' });
  });

  it('endpoints use the service name as host and the first exposed or published port', async () => {
    const { runtime: rt, controlHome } = await runtime();
    await expect(rt.endpoints(REF)).resolves.toEqual([
      { service: 'api', host: 'api', port: 8080 },
      { service: 'builder', host: 'builder', port: 7000 },
      { service: 'worker', host: 'worker', port: 9090 }
    ]);
    expect(await pathExists(overrideIn(controlHome))).toBe(false);
  });
});

describe('ComposeRuntime inventory and reap', () => {
  function populated(): DockerFake {
    const fake = new DockerFake();
    fake.resources = { ps: ['c1', 'c2'], volume: ['v1'], network: ['n1'], image: ['i1'] };
    fake.own('c1', 'api');
    fake.own('c2', 'worker');
    fake.own('v1');
    fake.own('n1');
    fake.own('i1');
    return fake;
  }

  it('returns every owned resource in removal order, each verified with a single inspect', async () => {
    const fake = populated();
    const { runtime: rt } = await runtime(fake);
    const owned = await rt.inventory(REF);

    expect(owned).toEqual([
      { kind: 'container', id: 'c1', service: 'api' },
      { kind: 'container', id: 'c2', service: 'worker' },
      { kind: 'volume', id: 'v1' },
      { kind: 'network', id: 'n1' },
      { kind: 'image', id: 'i1' }
    ]);
    const inspects = fake.calls.filter((c) => c.args.includes('--format'));
    expect(inspects).toHaveLength(5);
    expect(fake.calls.some((c) => c.args[0] === 'rm' || c.args[1] === 'rm')).toBe(false);
  });

  it('lists images by ownership only, never by project label', async () => {
    const fake = populated();
    const { runtime: rt } = await runtime(fake);
    await rt.inventory(REF);
    const imageLists = fake.calls.filter((c) => c.args[0] === 'image' && c.args[1] === 'ls');
    expect(imageLists).toHaveLength(1);
    expect(imageLists[0]?.args.filter((a) => a === '--filter')).toHaveLength(3);
  });

  it('refuses the whole inventory when one resource belongs to another worktree, removing nothing', async () => {
    const fake = populated();
    fake.labels.set('v1', { ...fake.labels.get('v1'), [`${NS}.worktree-uuid`]: 'ffffffff-ffff-4fff-8fff-ffffffffffff' });
    const { runtime: rt } = await runtime(fake);

    await expect(rt.inventory(REF)).rejects.toThrow('Refusing to touch volume v1: its ownership labels do not match this worktree.');
    expect(fake.calls.some((c) => c.args[0] === 'rm' || c.args[1] === 'rm')).toBe(false);
  });

  it('refuses a resource whose labels cannot be inspected', async () => {
    const fake = populated();
    fake.uninspectable.add('n1');
    const { runtime: rt } = await runtime(fake);
    await expect(rt.inventory(REF)).rejects.toThrow('Refusing to touch network n1: its labels could not be inspected.');
  });

  it('finds a resource with the right owner but the wrong project label, and then refuses it', async () => {
    const fake = populated();
    fake.resources.ps = ['c1', 'stray'];
    fake.own('stray', 'api');
    fake.labels.set('stray', { ...fake.labels.get('stray'), 'com.docker.compose.project': 'somebody-else' });
    const { runtime: rt } = await runtime(fake);
    await expect(rt.inventory(REF)).rejects.toThrow('Refusing to touch container stray');
  });

  it('accepts and reaps an image the bake builder left without Compose\'s own labels', async () => {
    const fake = populated();
    fake.own('i1', '', { composeLabels: false });
    const { runtime: rt } = await runtime(fake);

    await expect(rt.inventory(REF)).resolves.toContainEqual({ kind: 'image', id: 'i1' });
    await rt.reap(REF, { kind: 'image', id: 'i1' });
    expect(fake.calls.some((c) => c.args.join(' ') === 'image rm i1')).toBe(true);
  });

  it('still refuses an image whose Compose project label names another project', async () => {
    const fake = populated();
    fake.labels.set('i1', { ...fake.labels.get('i1'), 'com.docker.compose.project': 'somebody-else' });
    const { runtime: rt } = await runtime(fake);
    await expect(rt.inventory(REF)).rejects.toThrow('Refusing to touch image i1: its ownership labels do not match this worktree.');
  });

  it('refuses a volume without a Compose project label: only images may lack one', async () => {
    const fake = populated();
    fake.own('v1', '', { composeLabels: false });
    const { runtime: rt } = await runtime(fake);
    await expect(rt.inventory(REF)).rejects.toThrow('Refusing to touch volume v1: its ownership labels do not match this worktree.');
  });

  it('reap re-verifies and then removes with the right verb for each kind', async () => {
    const fake = populated();
    const { runtime: rt } = await runtime(fake);
    const resources: OwnedResource[] = [
      { kind: 'container', id: 'c1' }, { kind: 'volume', id: 'v1' },
      { kind: 'network', id: 'n1' }, { kind: 'image', id: 'i1' }
    ];
    for (const resource of resources) await rt.reap(REF, resource);

    const removals = fake.calls
      .filter((c) => c.command === 'docker' && !c.args.includes('--format') && !c.args.includes('--filter') && c.args[0] !== 'compose')
      .map((c) => c.args);
    expect(removals).toEqual([
      ['rm', '--force', 'c1'],
      ['volume', 'rm', 'v1'],
      ['network', 'rm', 'n1'],
      ['image', 'rm', 'i1']
    ]);
  });

  it('reap refuses a resource that no longer verifies, even if inventory once returned it', async () => {
    const fake = populated();
    const { runtime: rt } = await runtime(fake);
    fake.labels.set('c1', { ...fake.labels.get('c1'), [`${NS}.managed`]: 'false' });
    await expect(rt.reap(REF, { kind: 'container', id: 'c1' })).rejects.toThrow('ownership labels do not match');
    expect(fake.calls.some((c) => c.args[0] === 'rm')).toBe(false);
  });
});
