import { randomUUID } from 'node:crypto';
import { mkdtemp, realpath, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { execa } from 'execa';
import { afterEach, beforeAll, describe, expect, it } from 'vitest';
import { composeProjectName, worktreeLayout } from '../../src/layout.ts';
import { silentLogger } from '../../src/logger.ts';
import { ExecaRunner } from '../../src/process-runner.ts';
import { ComposeRuntime } from '../../src/runtime/compose.ts';
import type { WorktreeRef } from '../../src/runtime/runtime.ts';

// A namespace no real installation uses, so the sweep in afterEach can never
// touch a developer's actual environments.
const NS = 'dev.berth-test';
const FIXTURE = path.resolve(import.meta.dirname, 'fixtures', 'compose');
const COMPOSE_FILE = path.join(FIXTURE, 'compose.yaml');

const temporary: string[] = [];
const projects: string[] = [];

async function docker(args: readonly string[], reject = true): Promise<string> {
  const result = await execa('docker', [...args], { reject });
  return result.stdout.trim();
}

async function exists(kind: 'container' | 'volume' | 'network', name: string): Promise<boolean> {
  return (await execa('docker', [kind, 'inspect', name], { reject: false })).exitCode === 0;
}

beforeAll(async () => {
  await docker(['info']);
});

afterEach(async () => {
  for (const project of projects.splice(0)) {
    await execa('docker', ['compose', '-p', project, 'down', '--volumes', '--remove-orphans'], { reject: false });
  }
  // Belt and braces: anything still carrying the test namespace goes too.
  for (const [kind, list] of [['container', ['ps', '-aq']], ['volume', ['volume', 'ls', '-q']], ['network', ['network', 'ls', '-q']]] as const) {
    const ids = (await docker([...list, '--filter', `label=${NS}.managed=true`], false)).split(/\s+/).filter(Boolean);
    for (const id of ids) await docker([kind, 'rm', ...(kind === 'network' ? [] : ['--force']), id], false);
  }
  await Promise.all(temporary.splice(0).map((d) => rm(d, { recursive: true, force: true })));
});

async function world() {
  const controlHome = await realpath(await mkdtemp(path.join(tmpdir(), 'berth-compose-it-')));
  temporary.push(controlHome);
  const runtime = new ComposeRuntime({
    composeFile: COMPOSE_FILE,
    projectRoot: FIXTURE,
    configFile: path.join(FIXTURE, 'berth.toml'),
    projectName: 'berth-test',
    domain: 'test.localhost',
    controlHome,
    labelNamespace: NS,
    routes: [],
    routerNetwork: 'berth-router-unused',
    runner: new ExecaRunner(),
    logger: silentLogger,
    waitTimeoutSeconds: 60
  });
  const ref = (slug: string): WorktreeRef => {
    const worktree = { repositoryId: randomUUID(), worktreeId: randomUUID(), slug };
    projects.push(composeProjectName(worktree.repositoryId, worktree.worktreeId));
    return worktree;
  };
  return { controlHome, runtime, ref };
}

describe('ComposeRuntime against a real daemon', () => {
  it('brings a worktree up with ownership labels and injected environment, reports it, takes it down, and reaps it', async () => {
    const w = await world();
    const subject = w.ref('feature-subject');
    const project = composeProjectName(subject.repositoryId, subject.worktreeId);

    await w.runtime.up(subject, [{ name: 'api' }, { name: 'worker' }]);

    // Labels landed on the container, the volume, and the default network — via the generated override.
    const apiId = await docker(['compose', '-p', project, 'ps', '-q', 'api']);
    const containerLabels = await docker(['inspect', '--format', `{{ index .Config.Labels "${NS}.managed" }} {{ index .Config.Labels "${NS}.worktree-uuid" }}`, apiId]);
    expect(containerLabels).toBe(`true ${subject.worktreeId}`);
    expect(await docker(['volume', 'inspect', '--format', `{{ index .Labels "${NS}.worktree-uuid" }}`, `${project}_data`])).toBe(subject.worktreeId);
    expect(await docker(['network', 'inspect', '--format', `{{ index .Labels "${NS}.worktree-uuid" }}`, `${project}_default`])).toBe(subject.worktreeId);

    // The consumer's Compose file saw the BERTH_* environment.
    const env = await docker(['inspect', '--format', '{{ range .Config.Env }}{{ println . }}{{ end }}', apiId]);
    expect(env).toContain('SLUG=feature-subject');
    expect(env).toContain('HOST=feature-subject.test.localhost');
    expect(env).toContain(`WORKTREE_HOME=${worktreeLayout(w.controlHome, subject.repositoryId, subject.worktreeId).home}`);

    await expect(w.runtime.status(subject)).resolves.toEqual([
      { service: 'api', state: 'running' },
      { service: 'worker', state: 'running' }
    ]);
    await expect(w.runtime.endpoints(subject)).resolves.toEqual([
      { service: 'api', host: 'api', port: 8080 },
      { service: 'worker', host: 'worker', port: 9090 }
    ]);

    await w.runtime.down(subject);
    await expect(w.runtime.status(subject)).resolves.toEqual([
      { service: 'api', state: 'missing' },
      { service: 'worker', state: 'missing' }
    ]);
    // Down is reversible: the named volume is still there for reap to find.
    expect(await exists('volume', `${project}_data`)).toBe(true);

    const owned = await w.runtime.inventory(subject);
    expect(owned.map((r) => r.kind)).toEqual(['volume']);
    for (const resource of owned) await w.runtime.reap(subject, resource);
    expect(await exists('volume', `${project}_data`)).toBe(false);
    await expect(w.runtime.inventory(subject)).resolves.toEqual([]);
  }, 120_000);

  it('reaps one worktree without touching a sibling in the same repository', async () => {
    const w = await world();
    const subject = w.ref('feature-a');
    const sibling = { ...w.ref('feature-b'), repositoryId: subject.repositoryId };
    projects.push(composeProjectName(sibling.repositoryId, sibling.worktreeId));
    // `api` mounts the named volume, so both worktrees own a container, a volume, and a network.
    await w.runtime.up(subject, [{ name: 'api' }]);
    await w.runtime.up(sibling, [{ name: 'api' }]);
    const siblingProject = composeProjectName(sibling.repositoryId, sibling.worktreeId);

    await w.runtime.down(subject);
    for (const resource of await w.runtime.inventory(subject)) await w.runtime.reap(subject, resource);

    await expect(w.runtime.inventory(subject)).resolves.toEqual([]);
    expect((await w.runtime.inventory(sibling)).map((r) => r.kind)).toEqual(['container', 'volume', 'network']);
    expect(await exists('container', `${siblingProject}-api-1`)).toBe(true);
    expect(await exists('volume', `${siblingProject}_data`)).toBe(true);
  }, 120_000);

  it('refuses the whole inventory when a project-labelled resource belongs to a different worktree', async () => {
    const w = await world();
    const subject = w.ref('feature-guarded');
    const project = composeProjectName(subject.repositoryId, subject.worktreeId);
    await w.runtime.up(subject, [{ name: 'worker' }]);

    // Right project, right repository, wrong worktree — exactly the shape a bug elsewhere would produce.
    const impostor = `${project}_impostor`;
    await docker([
      'network', 'create',
      '--label', `${NS}.managed=true`,
      '--label', `com.docker.compose.project=${project}`,
      '--label', `${NS}.repository-uuid=${subject.repositoryId}`,
      '--label', `${NS}.worktree-uuid=${randomUUID()}`,
      impostor
    ]);

    await expect(w.runtime.inventory(subject)).rejects.toThrow('ownership labels do not match this worktree');
    expect(await exists('container', `${project}-worker-1`)).toBe(true);
    expect(await exists('network', impostor)).toBe(true);
  }, 120_000);
});
