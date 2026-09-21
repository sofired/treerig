import { randomUUID } from 'node:crypto';
import { mkdtemp, realpath, rm } from 'node:fs/promises';
import http from 'node:http';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { execa } from 'execa';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { writeIndexCard } from '../../src/checkout-index/card.ts';
import { CheckoutIndexService } from '../../src/checkout-index/service.ts';
import { composeProjectName } from '../../src/layout.ts';
import { silentLogger } from '../../src/logger.ts';
import { ExecaRunner } from '../../src/process-runner.ts';
import { RouterService } from '../../src/router/router.ts';
import { INDEX_HOST } from '../../src/router/stack.ts';
import { ComposeRuntime } from '../../src/runtime/compose.ts';
import type { WorktreeRef } from '../../src/runtime/runtime.ts';

// The shared router against a real daemon: it starts hardened, routes a
// worktree by hostname, and its socket proxy refuses everything a router
// does not need. Names carry a random suffix so this never touches a
// developer's real router.

const NS = 'dev.berth-test';
const SUFFIX = randomUUID().slice(0, 8);
const PROJECT = `berth-router-test-${SUFFIX}`;
const PORT = 18_000 + (process.pid % 1000);
const FIXTURE = path.resolve(import.meta.dirname, 'fixtures', 'compose');

let controlHome: string;
let router: RouterService;
let runtime: ComposeRuntime;
const worktrees: WorktreeRef[] = [];

async function docker(args: readonly string[], reject = true) {
  return execa('docker', [...args], { reject });
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

beforeAll(async () => {
  await docker(['info']);
  controlHome = await realpath(await mkdtemp(path.join(tmpdir(), 'berth-router-it-')));
  router = new RouterService({ controlHome, port: PORT, runner: new ExecaRunner(), logger: silentLogger, projectName: PROJECT, network: PROJECT });
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
    await execa('docker', ['compose', '-p', composeProjectName(worktree.repositoryId, worktree.worktreeId), 'down', '--volumes', '--remove-orphans'], { reject: false });
  }
  await execa('docker', ['compose', '-p', PROJECT, 'down', '--remove-orphans'], { reject: false });
  await execa('docker', ['network', 'rm', PROJECT], { reject: false });
  await rm(controlHome, { recursive: true, force: true });
});

describe('the shared router', () => {
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
    const worktree: WorktreeRef = { repositoryId: randomUUID(), worktreeId: randomUUID(), slug: 'feature-route' };
    worktrees.push(worktree);
    await runtime.up(worktree, []);

    const api = await eventually(() => get('feature-route.test.localhost'), (r) => r.status === 200);
    expect(api.body).toBe('hello from api\n');
    const worker = await eventually(() => get('jobs.feature-route.test.localhost'), (r) => r.status === 200);
    expect(worker.body).toBe('hello from worker\n');
    expect((await get('nobody.test.localhost')).status).toBe(404);

    // Service discovery inside the worktree still works: api reaches worker by name.
    const apiId = (await docker(['compose', '-p', composeProjectName(worktree.repositoryId, worktree.worktreeId), 'ps', '-q', 'api'])).stdout.trim();
    expect((await docker(['exec', apiId, 'wget', '-qO-', 'http://worker:9090/'])).stdout).toBe('hello from worker');

    // A refreshed index — real `docker ps`, real file, real httpd — lists the worktree as running.
    const index = new CheckoutIndexService({
      controlHome,
      routerPort: PORT,
      registry: {
        listAll: () => Promise.resolve([{ ...worktree, domain: 'test.localhost' }]),
        identities: { activeIdentities: () => Promise.resolve([]), repositoryId: () => Promise.resolve(undefined) }
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
