import { randomUUID } from 'node:crypto';
import { mkdtemp, realpath, rm } from 'node:fs/promises';
import http from 'node:http';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { execa } from 'execa';
import { afterAll, beforeAll, describe, expect, inject, it } from 'vitest';
import { writeIndexCard } from '../../src/checkout-index/card.ts';
import { CheckoutIndexService } from '../../src/checkout-index/service.ts';
import { secureWorktreeDirectories } from '../../src/files.ts';
import { composeProjectName, worktreeLayout } from '../../src/layout.ts';
import { silentLogger } from '../../src/logger.ts';
import { ExecaRunner } from '../../src/process-runner.ts';
import { RouterService } from '../../src/router/router.ts';
import { INDEX_HOST, ROUTED_BY_LABEL } from '../../src/router/stack.ts';
import { ComposeRuntime } from '../../src/runtime/compose.ts';
import type { WorktreeRef } from '../../src/runtime/runtime.ts';
import { freePort } from '../helpers/ports.ts';

// The shared router against a real daemon: it starts hardened, routes a
// worktree by hostname, and its socket proxy refuses everything a router
// does not need. Names carry a random suffix so this never touches a
// developer's real router.

const NS = 'dev.berth-test-router';   // per file, so one suite's sweep can never touch another's resources
const SUFFIX = randomUUID().slice(0, 8);
const PROJECT = `berth-router-test-${SUFFIX}`;
let PORT = 0;   // chosen from what is free when the suite starts
const DOCKER = inject('docker');
// A config that forgot the global setup would otherwise skip everything in silence.
if (typeof DOCKER !== 'boolean') throw new Error('The integration config must load tests/helpers/docker-setup.ts.');
const FIXTURE = path.resolve(import.meta.dirname, 'fixtures', 'compose');

let controlHome: string;
let router: RouterService;
let runtime: ComposeRuntime;
const worktrees: WorktreeRef[] = [];
const intruders: string[] = [];

async function docker(args: readonly string[], reject = true) {
  return execa('docker', [...args], { reject });
}

/**
 * Everything a killed run of this suite left behind: the router stack, the
 * worktrees it started, and the intruder — all under a namespace no
 * installation uses. It takes no account of age, so one Docker integration
 * run per machine at a time; `fileParallelism: false` already assumes as much.
 */
async function sweep(): Promise<void> {
  for (const filter of [`label=${NS}.component`, `label=${NS}.managed=true`]) {
    const containers = (await docker(['ps', '-aq', '--filter', filter], false)).stdout.split(/\s+/).filter(Boolean);
    for (const id of containers) await docker(['rm', '--force', id], false);
    const volumes = (await docker(['volume', 'ls', '-q', '--filter', filter], false)).stdout.split(/\s+/).filter(Boolean);
    for (const id of volumes) await docker(['volume', 'rm', '--force', id], false);
    const labelled = (await docker(['network', 'ls', '-q', '--filter', filter], false)).stdout.split(/\s+/).filter(Boolean);
    for (const id of labelled) await docker(['network', 'rm', id], false);
  }
  // Last: a network with endpoints still attached refuses to go, and that failure is swallowed.
  const networks = (await docker(['network', 'ls', '--format', '{{.Name}}'], false)).stdout
    .split('\n').map((name) => name.trim()).filter((name) => name.startsWith('berth-router-test-'));
  for (const name of networks) await docker(['network', 'rm', name], false);
}

/** GET through the router with an explicit Host header; node:http lets us set one where fetch does not. */
function get(host: string, pathname = '/'): Promise<{ status: number; body: string }> {
  return new Promise((resolve, reject) => {
    const request = http.request({ host: '127.0.0.1', port: PORT, path: pathname, headers: { Host: host } }, (response) => {
      let body = '';
      response.setEncoding('utf8');
      response.on('data', (chunk: string) => { body += chunk; });
      response.on('end', () => resolve({ status: response.statusCode ?? 0, body }));
    });
    request.on('error', reject);
    request.end();
  });
}

/** Traefik learns about containers from events; give it a moment. */
async function eventually<T>(probe: () => Promise<T>, accept: (value: T) => boolean, timeoutMs = 20_000): Promise<T> {
  const deadline = Date.now() + timeoutMs;
  let last: T | undefined;
  while (Date.now() < deadline) {
    try {
      last = await probe();
      if (accept(last)) return last;
    } catch {
      // Not up yet.
    }
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  throw new Error(`Condition not met within ${timeoutMs} ms; last value: ${JSON.stringify(last)}`);
}

describe.skipIf(!DOCKER)('the shared router', () => {
  beforeAll(async () => {
    PORT = await freePort();
    await sweep();
    controlHome = await realpath(await mkdtemp(path.join(tmpdir(), 'berth-router-it-')));
    // Its own label namespace, so a killed run leaves nothing a real installation would claim.
    router = new RouterService({
      controlHome, port: PORT, runner: new ExecaRunner(), logger: silentLogger,
      projectName: PROJECT, network: PROJECT, labelNamespace: NS
    });
    runtime = new ComposeRuntime({
      composeFile: path.join(FIXTURE, 'compose.yaml'),
      projectRoot: FIXTURE,
      configFile: path.join(FIXTURE, 'berth.toml'),
      projectName: 'berth-test',
      domain: 'test.localhost',
      controlHome,
      labelNamespace: NS,
      routes: [{ service: 'api', subdomain: '' }, { service: 'worker', subdomain: 'jobs' }],
      routerNetwork: PROJECT,
      runner: new ExecaRunner(),
      logger: silentLogger,
      waitTimeoutSeconds: 60
    });
  });

  afterAll(async () => {
    for (const worktree of worktrees) {
      await execa('docker', ['compose', '-p', composeProjectName(worktree.repositoryId, worktree.worktreeId), 'down', '--volumes', '--rmi', 'local', '--remove-orphans'], { reject: false });
    }
    for (const intruder of intruders) await execa('docker', ['rm', '--force', intruder], { reject: false });
    await execa('docker', ['compose', '-p', PROJECT, 'down', '--remove-orphans'], { reject: false });
    await execa('docker', ['network', 'rm', PROJECT], { reject: false });
    await rm(controlHome, { recursive: true, force: true });
  });

  it('starts hardened, verifies clean, and is left alone by a second ensure', async () => {
    await router.ensure();
    await expect(router.verify()).resolves.toEqual([]);
    const before = (await docker(['compose', '-p', PROJECT, 'ps', '-q', 'router'])).stdout.trim();

    await router.ensure();

    expect((await docker(['compose', '-p', PROJECT, 'ps', '-q', 'router'])).stdout.trim()).toBe(before);
    await expect(router.status()).resolves.toMatchObject({ running: true, problems: [], port: PORT, network: PROJECT });
    // The empty file-provider directory must not upset Traefik.
    const logs = await docker(['logs', before]);
    expect(`${logs.stdout}\n${logs.stderr}`).not.toMatch(/level=error|ERR /);

    // The index answers through the router before anything has been indexed.
    const page = await eventually(() => get(INDEX_HOST), (r) => r.status === 200);
    expect(page.body).toContain('No checkouts have been indexed yet');
  }, 120_000);

  it('routes a worktree by hostname over the shared network, without disturbing its own network', async () => {
    // A container on the shared network with Traefik labels, labelled for some other
    // router: what a second router stack's services look like. It answers unmistakably,
    // so a leaked route could never hide behind a 404 of its own. Started first: Traefik
    // rebuilds from a full listing on every event, so once the worktree below is routed,
    // the listing that produced that configuration already held the intruder.
    const intruder = (await docker(['run', '--detach', '--name', `intruder-${SUFFIX}`, '--network', PROJECT,
      '--label', `${NS}.component=intruder`,   // so a killed run leaves it for the sweep
      '--label', 'traefik.enable=true',
      '--label', `traefik.docker.network=${PROJECT}`,
      '--label', `${ROUTED_BY_LABEL}=not-${PROJECT}`,   // the only label that differs from a worktree service
      '--label', 'traefik.http.routers.intruder.rule=Host(`intruder.test.localhost`)',
      '--label', 'traefik.http.routers.intruder.entrypoints=web',
      '--label', 'traefik.http.services.intruder.loadbalancer.server.port=8080',
      'busybox:1.37.0', 'sh', '-c', 'echo intruder > /tmp/index.html && exec httpd -f -p 8080 -h /tmp'])).stdout.trim();
    intruders.push(intruder);
    const worktree: WorktreeRef = { repositoryId: randomUUID(), worktreeId: randomUUID(), slug: 'feature-route' };
    worktrees.push(worktree);
    await secureWorktreeDirectories(worktreeLayout(controlHome, worktree.repositoryId, worktree.worktreeId));
    try {
      await runtime.up(worktree, [{ name: 'api' }, { name: 'worker' }]);

      const api = await eventually(() => get('feature-route.test.localhost'), (r) => r.status === 200);
      expect(api.body).toBe('hello from api\n');
      const worker = await eventually(() => get('jobs.feature-route.test.localhost'), (r) => r.status === 200);
      expect(worker.body).toBe('hello from worker\n');
      const nobody = await get('nobody.test.localhost');
      expect(nobody.status).toBe(404);
      // The negative means something only if the intruder is alive and serving while Traefik lists it.
      expect((await docker(['exec', intruder, 'wget', '-qO-', 'http://127.0.0.1:8080/'])).stdout).toBe('intruder');
      // Traefik's own 404, byte for byte the one an unknown host gets — not the intruder's page.
      expect(await get('intruder.test.localhost')).toEqual(nobody);
      expect(nobody.body).not.toContain('intruder');
    } finally {
      // Gone now, so the later network-release case sees only the worktree; `afterAll` covers a killed run.
      await docker(['rm', '--force', intruder], false);
    }

    // Service discovery inside the worktree still works: api reaches worker by name.
    const apiId = (await docker(['compose', '-p', composeProjectName(worktree.repositoryId, worktree.worktreeId), 'ps', '-q', 'api'])).stdout.trim();
    expect((await docker(['exec', apiId, 'wget', '-qO-', 'http://worker:9090/'])).stdout).toBe('hello from worker');

    // A refreshed index — real `docker ps`, real file, real httpd — lists the worktree as running.
    const index = new CheckoutIndexService({
      controlHome,
      routerPort: PORT,
      registry: {
        listAll: () => Promise.resolve([{ ...worktree, domain: 'test.localhost' }]),
        identities: { root: FIXTURE, activeIdentities: () => Promise.resolve([]), repositoryId: () => Promise.resolve(undefined) }
      },
      runner: new ExecaRunner(),
      logger: silentLogger
    });
    await writeIndexCard(controlHome, worktree.repositoryId, worktree.worktreeId, {
      version: 1, project: 'berth-test', domain: 'test.localhost', slug: worktree.slug, host: 'feature-route.test.localhost',
      root: FIXTURE, branch: 'feature/route', labelNamespace: NS,
      services: [{ name: 'api', url: `http://feature-route.test.localhost:${PORT}`, label: 'API', purpose: 'fixture' }],
      updatedAt: new Date().toISOString()
    });
    const snapshot = await index.refresh();
    expect(snapshot.entries.map((e) => [e.host, e.status, e.services[0]?.available])).toEqual([['feature-route.test.localhost', 'running', true]]);
    const page = await eventually(() => get(INDEX_HOST), (r) => r.status === 200 && r.body.includes('feature-route.test.localhost'));
    expect(page.body).toContain(`href="http://feature-route.test.localhost:${PORT}"`);
    expect(page.body).toContain('status-running');
  }, 120_000);

  it('exposes only the read-only Docker API through the proxy', async () => {
    const network = `${PROJECT}_docker-api`;
    const allowed = await docker(['run', '--rm', '--network', network, 'busybox:1.37.0', 'wget', '-qO-', 'http://docker-api:2375/version']);
    expect(allowed.stdout).toContain('ApiVersion');

    const denied = await docker(['run', '--rm', '--network', network, 'busybox:1.37.0', 'wget', '-qO-', '--post-data=', 'http://docker-api:2375/containers/create'], false);
    expect(denied.exitCode).not.toBe(0);
    expect(denied.stderr).toContain('403');

    const info = await docker(['run', '--rm', '--network', network, 'busybox:1.37.0', 'wget', '-qO-', 'http://docker-api:2375/info'], false);
    expect(info.exitCode).not.toBe(0);
  }, 120_000);

  it('keeps the shared network on down while a worktree is attached, and lets it go once free', async () => {
    const attached = worktrees[0];
    if (!attached) throw new Error('the routing test did not leave a worktree behind');
    await router.down();
    expect((await docker(['network', 'inspect', PROJECT], false)).exitCode).toBe(0);

    await runtime.down(attached);
    for (const resource of await runtime.inventory(attached)) await runtime.reap(attached, resource);
    await router.down();
    expect((await docker(['network', 'inspect', PROJECT], false)).exitCode).not.toBe(0);
  }, 120_000);
});
