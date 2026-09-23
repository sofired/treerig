import { randomUUID } from 'node:crypto';
import { mkdir, mkdtemp, readFile, realpath, rm, utimes, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import {
  atLeast,
  doctor,
  doctorExitCode,
  doctorSummary,
  findCheckout,
  formatDoctorReport,
  NODE_ENGINE,
  nodeCheck,
  parseRoutedContainers,
  readVersion,
  registryChecks,
  routeConflicts,
  routerChecks,
  satisfiesEngine,
  versionCheck,
  type CheckoutSource,
  type DoctorOptions,
  type RouterSource,
  type Section
} from '../../src/commands/doctor.ts';
import { up } from '../../src/commands/worktree.ts';
import { OperationCancelledError } from '../../src/errors.ts';
import { CONTROL_HOME_MARKER } from '../../src/files.ts';
import { silentLogger } from '../../src/logger.ts';
import { developmentLockTarget, registryLockTarget, ScopedLock, worktreeLockTarget, worktreeRecoveryLockTarget } from '../../src/operation-lock.ts';
import { CommandStartError, type CommandRunner, type RunOptions, type RunResult } from '../../src/process-runner.ts';
import type { RouterStatus } from '../../src/router/router.ts';
import { INDEX_HOST } from '../../src/router/stack.ts';
import { registryDirectory, type RegisteredWorktree, type RegistryScan } from '../../src/worktree-registry.ts';
import { commandWorld, type CommandWorld } from '../helpers/command-world.ts';
import { GitRunner } from '../helpers/git-runner.ts';

const temporary: string[] = [];
const worlds: CommandWorld[] = [];
afterEach(async () => {
  await Promise.all(temporary.splice(0).map((d) => rm(d, { recursive: true, force: true })));
  await Promise.all(worlds.splice(0).map((w) => w.dispose()));
});

async function scratch(): Promise<string> {
  const directory = await realpath(await mkdtemp(path.join(tmpdir(), 'berth-doctor-')));
  temporary.push(directory);
  return directory;
}

const ok = (stdout = ''): RunResult => ({ stdout, stderr: '', exitCode: 0 });

/** Answers `git --version`, `docker …`, and any other command as the verify hook; Git identity queries go to `git`. */
class DoctorRunner implements CommandRunner {
  readonly calls: { command: string; args: readonly string[]; options?: RunOptions }[] = [];
  readonly git: GitRunner;
  gitVersion: RunResult | Error = ok('git version 2.43.0\n');
  dockerOnPath: boolean | Error = true;
  docker = new Map<string, RunResult>([
    ['version', ok('29.7.2\n')],
    ['compose', ok('2.38.2\n')],
    ['ps', ok('')],
    ['inspect', ok('')]
  ]);
  hook: RunResult | Error = ok();

  constructor(git = new GitRunner()) {
    this.git = git;
  }

  run(command: string, args: readonly string[] = [], options?: RunOptions): Promise<RunResult> {
    this.calls.push({ command, args, options });
    if (command === 'git' && args[0] === '--version') {
      return this.gitVersion instanceof Error ? Promise.reject(this.gitVersion) : Promise.resolve(this.gitVersion);
    }
    if (command === 'git') return this.git.run(command, args);
    if (command === 'docker') return Promise.resolve(this.docker.get(args[0] ?? '') ?? ok());
    return this.hook instanceof Error ? Promise.reject(this.hook) : Promise.resolve(this.hook);
  }

  which(command: string): Promise<boolean> {
    if (command !== 'docker') return Promise.resolve(true);
    return this.dockerOnPath instanceof Error ? Promise.reject(this.dockerOnPath) : Promise.resolve(this.dockerOnPath);
  }
}

const STATUS: RouterStatus = { reachable: true, running: true, problems: [], port: 80, network: 'berth-router' };

function routerSource(status: RouterStatus | Error = STATUS): RouterSource & { asked: number } {
  const source = {
    network: 'berth-router',
    asked: 0,
    status() {
      source.asked += 1;
      return status instanceof Error ? Promise.reject(status) : Promise.resolve(status);
    }
  };
  return source;
}

async function options(overrides: Partial<DoctorOptions> = {}): Promise<DoctorOptions> {
  return {
    commandName: 'berth',
    controlHome: path.join(await scratch(), 'control'),
    runner: new DoctorRunner(),
    logger: silentLogger,
    router: routerSource(),
    checkout: { kind: 'none', searchedFrom: '/nowhere' },
    nodeVersion: '24.14.1',
    ...overrides
  };
}

const section = (sections: readonly Section[], title: string) => sections.find((s) => s.title === title)?.checks ?? [];
const statuses = (sections: readonly Section[], title: string) => section(sections, title).map((c) => `${c.status} ${c.subject}`);

function worktree(slug: string, domain: string, worktreeId = randomUUID()): RegisteredWorktree {
  return { repositoryId: randomUUID(), worktreeId, slug, domain };
}

describe('versions', () => {
  it('reads the first version in what a tool prints, patch optional', () => {
    expect(readVersion('git version 2.43.0 (Apple Git-145)')).toEqual({ text: '2.43.0', parts: [2, 43, 0] });
    expect(readVersion('v2.21.0')).toEqual({ text: '2.21.0', parts: [2, 21, 0] });
    expect(readVersion('2.29.2-desktop.1')).toEqual({ text: '2.29.2', parts: [2, 29, 2] });
    expect(readVersion('2.36')).toEqual({ text: '2.36', parts: [2, 36, 0] });
    expect(readVersion('no version here')).toBeUndefined();
  });

  it('compares versions part by part, equal counting as enough', () => {
    expect(atLeast([2, 36, 0], [2, 36])).toBe(true);
    expect(atLeast([2, 36, 1], [2, 36, 0])).toBe(true);
    expect(atLeast([2, 35, 9], [2, 36, 0])).toBe(false);
    expect(atLeast([3, 0, 0], [2, 36, 0])).toBe(true);
    expect(atLeast([1, 99, 99], [2, 0, 0])).toBe(false);
  });

  it('holds Node to the engines range: the floor included, the next major excluded', () => {
    expect(satisfiesEngine('24.14.1', NODE_ENGINE)).toBe(true);
    expect(satisfiesEngine('24.99.0', NODE_ENGINE)).toBe(true);
    expect(satisfiesEngine('24.14.0', NODE_ENGINE)).toBe(false);
    expect(satisfiesEngine('25.0.0', NODE_ENGINE)).toBe(false);
    expect(satisfiesEngine('22.20.0', NODE_ENGINE)).toBe(false);
    expect(() => satisfiesEngine('24.14.1', '^24.14.1')).toThrow('Unsupported engines range');
  });

  it('checks Node against exactly the range package.json declares', async () => {
    const manifest = JSON.parse(await readFile(path.resolve(import.meta.dirname, '../../package.json'), 'utf8')) as { engines: { node: string } };
    expect(NODE_ENGINE).toBe(manifest.engines.node);
  });

  it('names the command and the range when Node is outside it', () => {
    expect(nodeCheck('24.14.1', 'duke')).toEqual({ status: 'ok', subject: 'Node.js', detail: '24.14.1' });
    expect(nodeCheck('22.1.0', 'duke')).toEqual({ status: 'fail', subject: 'Node.js', detail: `22.1.0; duke needs ${NODE_ENGINE}` });
  });

  it('passes a tool at the minimum, fails one below it, and fails output it cannot read', () => {
    expect(versionCheck('Git', 'git version 2.36.0', '2.36')).toEqual({ status: 'ok', subject: 'Git', detail: '2.36.0' });
    expect(versionCheck('Git', 'git version 2.35.1', '2.36')).toEqual({ status: 'fail', subject: 'Git', detail: '2.35.1; 2.36 or newer is needed' });
    expect(versionCheck('Git', 'garbled', '2.36')).toMatchObject({ status: 'fail', detail: 'could not read a version from "garbled"' });
  });
});

describe('registryChecks', () => {
  const scan = (overrides: Partial<RegistryScan> = {}): RegistryScan => ({ records: [], problems: [], conflicts: new Map(), ...overrides });

  it('says there is nothing yet, or how many records read cleanly', () => {
    expect(registryChecks(scan(), 'berth')).toEqual([{ status: 'ok', subject: 'registrations', detail: 'none yet' }]);
    expect(registryChecks(scan({ records: [worktree('a', 'acme.localhost'), worktree('b', 'acme.localhost')] }), 'berth'))
      .toEqual([{ status: 'ok', subject: 'registrations', detail: '2 readable' }]);
  });

  it('fails each unreadable record with its reason', () => {
    const checks = registryChecks(scan({ problems: [{ file: '/x.json', reason: 'Registry record is not valid JSON: /x.json' }] }), 'berth');
    expect(checks[1]).toEqual({ status: 'fail', subject: 'unreadable record', detail: 'Registry record is not valid JSON: /x.json' });
  });

  it('fails a hostname two worktrees claim, naming both', () => {
    const first = worktree('shared', 'acme.localhost');
    const second = worktree('shared', 'acme.localhost');
    const checks = registryChecks(scan({ records: [first, second], conflicts: new Map([['shared.acme.localhost', [first, second]]]) }), 'duke');
    expect(checks[1]).toMatchObject({ status: 'fail', subject: 'shared.acme.localhost' });
    expect(checks[1]?.detail).toBe(
      `registered to 2 worktrees (${first.worktreeId}, ${second.worktreeId}); requests for it may reach any of them ` +
      `that is running. Remove all but one with 'duke worktree remove <uuid>'.`
    );
  });

  it('bounds the list of worktrees claiming a hostname', () => {
    const claimants = Array.from({ length: 12 }, () => worktree('shared', 'acme.localhost'));
    const checks = registryChecks(scan({ records: claimants, conflicts: new Map([['shared.acme.localhost', claimants]]) }), 'berth');
    const listed = /\((.*)\); requests/.exec(checks[1]?.detail ?? '')?.[1] ?? '';
    expect(listed).toHaveLength(200);
    expect(listed.endsWith('…')).toBe(true);
  });

  it('fails a registration that sits on the index\'s own hostname, and only that one', () => {
    const squatter = worktree(INDEX_HOST.split('.')[0] ?? '', INDEX_HOST.split('.').slice(1).join('.'));
    const namesake = worktree(INDEX_HOST.split('.')[0] ?? '', 'acme.localhost');
    const checks = registryChecks(scan({ records: [squatter, namesake] }), 'berth');
    expect(checks.filter((check) => check.status === 'fail')).toEqual([expect.objectContaining({ subject: INDEX_HOST })]);
    expect(checks[1]?.detail).toContain(squatter.worktreeId);
  });
});

describe('routerChecks', () => {
  it('reports a running, clean router and a stopped one as healthy, in router status\'s words', () => {
    expect(routerChecks(STATUS)).toEqual([{ status: 'ok', subject: 'router', detail: 'running on 127.0.0.1:80, network berth-router' }]);
    expect(routerChecks({ ...STATUS, running: false, problems: ['the router is not running'] }))
      .toEqual([{ status: 'ok', subject: 'router', detail: 'not running (would use 127.0.0.1:80, network berth-router)' }]);
  });

  it('fails each problem of a degraded router, and a router Docker could not be asked about', () => {
    expect(routerChecks({ ...STATUS, problems: ['router: not healthy', 'index: image is not pinned'] })).toEqual([
      { status: 'fail', subject: 'router', detail: 'router: not healthy' },
      { status: 'fail', subject: 'router', detail: 'index: image is not pinned' }
    ]);
    expect(routerChecks({ ...STATUS, reachable: false, running: false, problems: ['Docker is not available: boom.'] }))
      .toEqual([{ status: 'fail', subject: 'router', detail: 'Router state unknown: Docker is not available: boom' }]);
  });
});

describe('routeConflicts', () => {
  const routed = (name: string, router: string, rule: string) => ({
    name,
    labels: { [`traefik.http.routers.${router}.rule`]: rule, [`traefik.http.routers.${router}.entrypoints`]: 'web' }
  });

  it('finds a rule two routers claim, naming the containers behind them', () => {
    const conflicts = routeConflicts([
      routed('b-api-1', 'b-api', 'Host(`mail.main.acme.localhost`)'),
      routed('a-mail-1', 'a-mail', 'Host(`mail.main.acme.localhost`)'),
      routed('c-api-1', 'c-api', 'Host(`other.acme.localhost`)')
    ]);
    expect([...conflicts]).toEqual([['Host(`mail.main.acme.localhost`)', ['a-mail-1', 'b-api-1']]]);
  });

  it('does not count replicas of one service, which share a router, as a conflict', () => {
    const replica = (name: string) => {
      const container = routed(name, 'a-api', 'Host(`x`)');
      return { ...container, labels: { ...container.labels, 'com.docker.compose.project': 'a', 'com.docker.compose.service': 'api' } };
    };
    expect(routeConflicts([replica('a-api-1'), replica('a-api-2')]).size).toBe(0);
    expect(routeConflicts([routed('a-api-1', 'a-api', 'Host(`x`)'), routed('a-api-2', 'a-api', 'Host(`x`)')]).size).toBe(0);
  });

  it('sees through a container that copies a service\'s router name and rule from another project', () => {
    const genuine = routed('a-api-1', 'a-api', 'Host(`x`)');
    const copy = routed('impostor', 'a-api', 'Host(`x`)');
    const conflicts = routeConflicts([
      { ...genuine, labels: { ...genuine.labels, 'com.docker.compose.project': 'a', 'com.docker.compose.service': 'api' } },
      { ...copy, labels: { ...copy.labels, 'com.docker.compose.project': 'b', 'com.docker.compose.service': 'api' } }
    ]);
    expect([...conflicts]).toEqual([['Host(`x`)', ['a-api-1', 'impostor']]]);
    const otherService = routeConflicts([
      { ...genuine, labels: { ...genuine.labels, 'com.docker.compose.project': 'a', 'com.docker.compose.service': 'api' } },
      { ...copy, labels: { ...copy.labels, 'com.docker.compose.project': 'a', 'com.docker.compose.service': 'web' } }
    ]);
    expect(otherService.size).toBe(1);
  });

  it('reads names and labels from inspect output, skipping what it cannot parse', () => {
    const stdout = [
      '/a-api-1\t{"traefik.http.routers.a-api.rule":"Host(`x`)","n":1}',
      'no tab here',
      '/broken\t{not json',
      '/empty\tnull',
      ''
    ].join('\n');
    expect(parseRoutedContainers(stdout)).toEqual([{ name: 'a-api-1', labels: { 'traefik.http.routers.a-api.rule': 'Host(`x`)' } }]);
  });
});

describe('report', () => {
  const sections: Section[] = [
    { title: 'First', checks: [{ status: 'ok', subject: 'a', detail: 'fine' }, { status: 'warn', subject: 'longer', detail: 'hmm' }] },
    { title: 'Second', checks: [{ status: 'skip', subject: 'x\u001b[31m', detail: 'line\nbreak' }] }
  ];

  it('aligns each section and filters everything borrowed', () => {
    expect(formatDoctorReport(sections)).toEqual([
      'First',
      '  ok    a       fine',
      '  warn  longer  hmm',
      '',
      'Second',
      '  skip  x�[31m  line break'
    ]);
  });

  it('exits 0 without failures, 1 with one, and with the hook\'s own code when the hook failed', () => {
    expect(doctorExitCode(sections)).toBe(0);
    const failing = (exitCode?: number): Section[] => [...sections, { title: 'T', checks: [
      { status: 'fail', subject: 'router', detail: 'x' },
      { status: 'fail', subject: 'verify hook', detail: 'y', ...(exitCode === undefined ? {} : { exitCode }) }
    ] }];
    expect(doctorExitCode(failing())).toBe(1);
    expect(doctorExitCode(failing(3))).toBe(3);
    // A failure never exits 0, whatever code it carries.
    expect(doctorExitCode(failing(0))).toBe(1);
  });

  it('sums up failures as an error, or says as information that nothing failed and what was only noted', () => {
    expect(doctorSummary(sections)).toEqual({ level: 'info', message: 'No checks failed (1 warning(s), 1 skipped).' });
    expect(doctorSummary([{ title: 'T', checks: [{ status: 'ok', subject: 'a', detail: '' }] }])).toEqual({ level: 'info', message: 'No checks failed.' });
    expect(doctorSummary([...sections, { title: 'T', checks: [{ status: 'fail', subject: 'a', detail: '' }] }]))
      .toEqual({ level: 'error', message: '1 check(s) failed.' });
  });
});

describe('doctor', () => {
  it('reports a healthy machine outside any checkout, and exits 0', async () => {
    const opts = await options();
    const sections = await doctor(opts);
    expect(sections.map((s) => s.title)).toEqual(['Prerequisites', 'Registry', 'Locks', 'Router', 'This checkout']);
    expect(statuses(sections, 'Prerequisites')).toEqual(['ok Node.js', 'ok Git', 'ok Docker Engine', 'ok Docker Compose']);
    expect(section(sections, 'Registry')[0]?.detail).toContain('not created yet');
    expect(statuses(sections, 'Router')).toEqual(['ok router', 'ok routes']);
    expect(section(sections, 'This checkout')).toEqual([expect.objectContaining({ status: 'skip', subject: 'berth.toml' })]);
    expect(doctorExitCode(sections)).toBe(0);
  });

  it('bounds every Docker question in time', async () => {
    const runner = new DoctorRunner();
    await doctor(await options({ runner }));
    const docker = runner.calls.filter((call) => call.command === 'docker');
    expect(docker.map((call) => call.args[0])).toEqual(['version', 'compose', 'ps']);
    expect(docker.every((call) => call.options?.timeoutMs === 30_000)).toBe(true);
  });

  it('skips the router\'s own state, which is read through Compose, when Compose does not answer, and still reads routes', async () => {
    const runner = new DoctorRunner();
    runner.docker.set('compose', { stdout: '', stderr: "docker: 'compose' is not a docker command.", exitCode: 1 });
    const router = routerSource();
    const sections = await doctor(await options({ runner, router }));
    expect(section(sections, 'Router')).toEqual([
      { status: 'skip', subject: 'router', detail: 'needs Docker Compose' },
      { status: 'ok', subject: 'routes', detail: 'no running container is routed' }
    ]);
    expect(router.asked).toBe(0);
  });

  it('reports Docker checks that could not even be made, and skips the router rather than guessing', async () => {
    const runner = new DoctorRunner();
    runner.dockerOnPath = new Error('PATH could not be searched');
    const router = routerSource();
    const sections = await doctor(await options({ runner, router }));
    expect(section(sections, 'Prerequisites').slice(2)).toEqual([{ status: 'fail', subject: 'Docker', detail: 'PATH could not be searched' }]);
    expect(section(sections, 'Router')).toEqual([{ status: 'skip', subject: 'router', detail: 'needs Docker' }]);
    expect(router.asked).toBe(0);
  });

  it('fails Docker off PATH, skips Compose and the router, and never asks the router', async () => {
    const runner = new DoctorRunner();
    runner.dockerOnPath = false;
    const router = routerSource();
    const sections = await doctor(await options({ runner, router }));
    expect(statuses(sections, 'Prerequisites').slice(2)).toEqual(['fail Docker Engine', 'skip Docker Compose']);
    expect(section(sections, 'Router')).toEqual([{ status: 'skip', subject: 'router', detail: 'needs Docker' }]);
    expect(router.asked).toBe(0);
    expect(doctorExitCode(sections)).toBe(1);
  });

  it('fails a daemon that does not answer in Docker\'s own words, and skips the router', async () => {
    const runner = new DoctorRunner();
    runner.docker.set('version', { stdout: '', stderr: 'Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?', exitCode: 1 });
    const sections = await doctor(await options({ runner }));
    expect(section(sections, 'Prerequisites')[2]).toEqual({
      status: 'fail',
      subject: 'Docker Engine',
      detail: 'Docker is not available: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?'
    });
    expect(statuses(sections, 'Router')).toEqual(['skip router']);
  });

  it('fails a Docker that does not answer in time, and one that refuses for another reason', async () => {
    const runner = new DoctorRunner();
    runner.docker.set('version', { stdout: '', stderr: '', exitCode: 1, timedOut: true });
    runner.docker.set('compose', { stdout: '', stderr: 'WARNING: noise\nunexpected failure', exitCode: 2 });
    const sections = await doctor(await options({ runner }));
    expect(section(sections, 'Prerequisites').slice(2)).toEqual([
      { status: 'fail', subject: 'Docker Engine', detail: '`docker version --format {{.Server.Version}}` did not answer within 30 s' },
      { status: 'fail', subject: 'Docker Compose', detail: 'unexpected failure' }
    ]);
  });

  it('fails a Compose or Git older than the minimum, and a Git that cannot be started', async () => {
    const runner = new DoctorRunner();
    runner.docker.set('compose', ok('2.20.3\n'));
    runner.gitVersion = ok('git version 2.30.1\n');
    let sections = await doctor(await options({ runner }));
    expect(section(sections, 'Prerequisites').map((c) => c.detail)).toEqual(
      expect.arrayContaining(['2.20.3; 2.21 or newer is needed', '2.30.1; 2.36 or newer is needed'])
    );
    runner.gitVersion = new CommandStartError('git', 'ENOENT', 'spawn git ENOENT');
    sections = await doctor(await options({ runner }));
    expect(section(sections, 'Prerequisites')[1]).toEqual({ status: 'fail', subject: 'Git', detail: '`git` could not be started: it is not on PATH (ENOENT).' });
    runner.gitVersion = { stdout: '', stderr: 'fatal: bad\n', exitCode: 128 };
    sections = await doctor(await options({ runner }));
    expect(section(sections, 'Prerequisites')[1]).toEqual({ status: 'fail', subject: 'Git', detail: '`git --version` failed: fatal: bad' });
  });

  it('fails Node outside the engines range', async () => {
    const sections = await doctor(await options({ nodeVersion: '22.12.0' }));
    expect(section(sections, 'Prerequisites')[0]).toMatchObject({ status: 'fail', subject: 'Node.js' });
  });

  it('refuses a control home berth did not create, and reads registry problems from it', async () => {
    const controlHome = await scratch();
    await writeFile(path.join(controlHome, 'someone-elses.txt'), 'x');
    let sections = await doctor(await options({ controlHome }));
    expect(section(sections, 'Registry')[0]).toMatchObject({ status: 'fail', subject: 'control home' });
    expect(section(sections, 'Registry')[0]?.detail).toContain('Refusing to use');

    await rm(path.join(controlHome, 'someone-elses.txt'));
    await writeFile(path.join(controlHome, CONTROL_HOME_MARKER), '');
    const repositoryId = randomUUID();
    await mkdir(registryDirectory(controlHome, repositoryId), { recursive: true });
    await writeFile(path.join(registryDirectory(controlHome, repositoryId), `${randomUUID()}.json`), '{not json');
    sections = await doctor(await options({ controlHome }));
    expect(statuses(sections, 'Registry')).toEqual(['ok control home', 'ok registrations', 'fail unreadable record']);
  });

  it('fails a control home that cannot be looked at, rather than calling it not created yet', async () => {
    const file = path.join(await scratch(), 'a-file');
    await writeFile(file, 'x');
    const sections = await doctor(await options({ controlHome: path.join(file, 'control') }));
    expect(section(sections, 'Registry')[0]).toMatchObject({ status: 'fail', subject: 'control home' });
    expect(section(sections, 'Registry')[0]?.detail).toContain('ENOTDIR');
  });

  it('reports a router it could not describe, and one whose status call throws, without losing the rest', async () => {
    let sections = await doctor(await options({ router: new Error('BERTH_ROUTER_PORT must be a port number between 1 and 65535, not "x".') }));
    expect(section(sections, 'Router')).toEqual([{ status: 'fail', subject: 'router', detail: 'BERTH_ROUTER_PORT must be a port number between 1 and 65535, not "x".' }]);
    sections = await doctor(await options({ router: routerSource(new Error('exploded')) }));
    expect(section(sections, 'Router')).toEqual([
      { status: 'fail', subject: 'router', detail: 'exploded' },
      { status: 'ok', subject: 'routes', detail: 'no running container is routed' }
    ]);
    expect(sections.map((s) => s.title)).toContain('This checkout');
  });

  it('lets a cancellation end the command instead of becoming a line', async () => {
    await expect(doctor(await options({ router: routerSource(new OperationCancelledError('SIGINT')) })))
      .rejects.toThrow('Operation cancelled by SIGINT.');
  });

  it('fails two routers that claim the same rule, read from the routed containers', async () => {
    const runner = new DoctorRunner();
    runner.docker.set('ps', ok('c1\nc2\n'));
    runner.docker.set('inspect', ok([
      '/berth-aaa-mail-1\t{"traefik.http.routers.berth-aaa-mail.rule":"Host(`mail.main.acme.localhost`)"}',
      '/berth-bbb-api-1\t{"traefik.http.routers.berth-bbb-api.rule":"Host(`mail.main.acme.localhost`)"}'
    ].join('\n')));
    const sections = await doctor(await options({ runner }));
    expect(section(sections, 'Router')[1]).toEqual({
      status: 'fail',
      subject: 'routes',
      detail: 'Host(`mail.main.acme.localhost`) is claimed by berth-aaa-mail-1, berth-bbb-api-1; requests for it may reach any of them'
    });
    const ps = runner.calls.find((call) => call.command === 'docker' && call.args[0] === 'ps');
    expect(ps?.args).toEqual(['ps', '-q', '--filter', 'label=dev.berth.router=berth-router']);
    const inspect = runner.calls.find((call) => call.command === 'docker' && call.args[0] === 'inspect');
    expect(inspect?.options?.timeoutMs).toBe(30_000);
  });

  it('bounds a rule any container could have written before it reaches the report', async () => {
    const runner = new DoctorRunner();
    const rule = `Host(\`${'a'.repeat(500)}\`)`;
    runner.docker.set('ps', ok('c1\nc2\n'));
    runner.docker.set('inspect', ok([
      `/one\t${JSON.stringify({ 'traefik.http.routers.one.rule': rule })}`,
      `/two\t${JSON.stringify({ 'traefik.http.routers.two.rule': rule })}`
    ].join('\n')));
    const detail = section(await doctor(await options({ runner })), 'Router')[1]?.detail ?? '';
    expect(detail.startsWith(`${rule.slice(0, 199)}… is claimed by one, two`)).toBe(true);
  });

  it('bounds the list of containers claiming a rule, however many there are', async () => {
    const runner = new DoctorRunner();
    const names = Array.from({ length: 60 }, (_, index) => `claimant-${String(index).padStart(2, '0')}`);
    runner.docker.set('ps', ok(names.join('\n')));
    runner.docker.set('inspect', ok(names.map((name) => `/${name}\t${JSON.stringify({ [`traefik.http.routers.${name}.rule`]: 'Host(`x`)' })}`).join('\n')));
    const detail = section(await doctor(await options({ runner })), 'Router')[1]?.detail ?? '';
    const claimed = detail.slice('Host(`x`) is claimed by '.length, detail.indexOf('; requests'));
    expect(claimed).toHaveLength(200);
    expect(claimed.endsWith('…')).toBe(true);
  });

  it('fails rather than half-reads an inspect that ran out of time part-way', async () => {
    const runner = new DoctorRunner();
    runner.docker.set('ps', ok('c1\nc2\n'));
    runner.docker.set('inspect', { stdout: '/one\t{"traefik.http.routers.one.rule":"Host(`a`)"}\n', stderr: '', exitCode: 1, timedOut: true });
    const sections = await doctor(await options({ runner }));
    expect(section(sections, 'Router')[1]).toEqual({
      status: 'fail', subject: 'routes', detail: '`docker inspect` did not answer within 30 s, so routes were not checked'
    });
  });

  it('fails rather than half-reads an inspect whose output could not all be captured', async () => {
    const runner = new DoctorRunner();
    runner.docker.set('ps', ok('c1\n'));
    runner.docker.set('inspect', { stdout: '/one\t{"traefik.http.routers.one.rule":"Host(`a`)"}\n', stderr: '', exitCode: 1, outputOverflow: true });
    const sections = await doctor(await options({ runner }));
    expect(section(sections, 'Router')[1]).toEqual({
      status: 'fail', subject: 'routes', detail: '`docker inspect` produced more output than can be read, so routes were not checked'
    });
  });

  it('counts routed containers when no rule is shared, and fails an inspect that answered nothing', async () => {
    const runner = new DoctorRunner();
    runner.docker.set('ps', ok('c1\n'));
    runner.docker.set('inspect', ok('/berth-aaa-api-1\t{"traefik.http.routers.berth-aaa-api.rule":"Host(`a`)"}\n'));
    let sections = await doctor(await options({ runner }));
    expect(section(sections, 'Router')[1]).toEqual({ status: 'ok', subject: 'routes', detail: '1 routed container(s); no two routers share a rule' });
    runner.docker.set('inspect', { stdout: '', stderr: 'Error: No such object: c1', exitCode: 1 });
    sections = await doctor(await options({ runner }));
    expect(section(sections, 'Router')[1]).toEqual({ status: 'fail', subject: 'routes', detail: 'Error: No such object: c1' });
  });

  it('reads the containers inspect did describe when another stopped in between', async () => {
    const runner = new DoctorRunner();
    runner.docker.set('ps', ok('c1\nc2\n'));
    runner.docker.set('inspect', {
      stdout: '/berth-aaa-api-1\t{"traefik.http.routers.berth-aaa-api.rule":"Host(`a`)"}\n',
      stderr: 'Error: No such object: c2',
      exitCode: 1
    });
    const sections = await doctor(await options({ runner }));
    expect(section(sections, 'Router')[1]).toEqual({ status: 'ok', subject: 'routes', detail: '1 routed container(s); no two routers share a rule' });
  });

  it('fails a config file that does not load', async () => {
    const sections = await doctor(await options({ checkout: { kind: 'unloadable', error: new Error('berth.toml: bad') } }));
    expect(section(sections, 'This checkout')).toEqual([{ status: 'fail', subject: 'berth.toml', detail: 'berth.toml: bad' }]);
    expect(doctorExitCode(sections)).toBe(1);
  });
});

describe('doctor in a checkout', () => {
  async function checkout(verify?: string) {
    const world = await commandWorld();
    worlds.push(world);
    const context = world.context();
    const loaded = verify === undefined
      ? context.loaded
      : { ...context.loaded, config: { ...context.loaded.config, hooks: { verify } } };
    const runner = new DoctorRunner(world.runner);
    const run = (source: CheckoutSource = { kind: 'loaded', loaded }) => doctor({
      commandName: 'berth',
      controlHome: world.controlHome,
      runner,
      logger: silentLogger,
      router: routerSource(),
      checkout: source,
      nodeVersion: '24.14.1',
      registryFor: () => context.registry
    });
    return { world, context, runner, run };
  }

  it('skips the worktree and its verify hook until the worktree is registered', async () => {
    const { run } = await checkout('./verify');
    const checks = section(await run(), 'This checkout');
    expect(checks.map((c) => `${c.status} ${c.subject}`)).toEqual(['ok berth.toml', 'skip this worktree', 'skip verify hook']);
    expect(checks[2]?.detail).toBe(`needs a registered worktree; run 'berth up' first`);
  });

  it('says so when no verify hook is configured', async () => {
    const { run, context } = await checkout();
    await up(context);
    expect(section(await run(), 'This checkout').at(-1)).toEqual({ status: 'skip', subject: 'verify hook', detail: 'none configured' });
  });

  it('runs the verify hook with its output on stderr, and passes when it exits 0', async () => {
    const { run, context, runner, world } = await checkout('./verify');
    await up(context);
    const checks = section(await run(), 'This checkout');
    expect(checks.map((c) => `${c.status} ${c.subject}`)).toEqual(['ok berth.toml', 'ok this worktree', 'ok verify hook']);
    expect(checks[1]?.detail).toBe('registered as feature-login.acme.localhost');
    const hook = runner.calls.find((call) => call.command === path.join(world.root, 'verify'));
    expect(hook?.options?.printOutput).toBe('stderr');
    expect(hook?.options?.env?.BERTH_STAGE).toBe('verify');
  });

  it('fails a verify hook that exits non-zero, and keeps its code', async () => {
    const { run, context, runner } = await checkout('./verify');
    await up(context);
    runner.hook = { stdout: '', stderr: '', exitCode: 3 };
    const sections = await run();
    expect(section(sections, 'This checkout').at(-1)).toEqual({
      status: 'fail', subject: 'verify hook', detail: 'The verify hook failed (exit 3).', exitCode: 3
    });
    expect(doctorExitCode(sections)).toBe(3);
  });

  it('lets a cancellation during the verify hook end the command', async () => {
    const { run, context, runner } = await checkout('./verify');
    await up(context);
    runner.hook = new OperationCancelledError('SIGINT');
    await expect(run()).rejects.toThrow('Operation cancelled by SIGINT.');
  });

  it('reports a verify hook that could not be run at all as its own failure, keeping the lines before it', async () => {
    const { run, context, runner } = await checkout('./verify');
    await up(context);
    runner.hook = new Error('spawn went wrong');
    const sections = await run();
    expect(section(sections, 'This checkout').map((c) => `${c.status} ${c.subject}`)).toEqual(['ok berth.toml', 'ok this worktree', 'fail verify hook']);
    // Strictly: no exit code of its own, since no hook verdict was reached.
    expect(section(sections, 'This checkout').at(-1)).toStrictEqual({ status: 'fail', subject: 'verify hook', detail: 'spawn went wrong' });
    expect(doctorExitCode(sections)).toBe(1);
  });

  it('keeps what it learned about the checkout when Git cannot identify it, and says why verify did not run', async () => {
    const { world, context } = await checkout('./verify');
    const loaded = { ...context.loaded, config: { ...context.loaded.config, hooks: { verify: './verify' } } };
    const sections = await doctor({
      commandName: 'berth',
      controlHome: world.controlHome,
      runner: new DoctorRunner(world.runner),
      logger: silentLogger,
      router: routerSource(),
      checkout: { kind: 'loaded', loaded },
      nodeVersion: '24.14.1',
      registryFor: () => ({
        current: () => Promise.reject(new Error('Command failed (git rev-parse): not a git repository')),
        pruneCandidates: () => Promise.reject(new Error('Command failed (git worktree list): not a git repository'))
      })
    });
    expect(section(sections, 'This checkout')).toEqual([
      { status: 'ok', subject: 'berth.toml', detail: loaded.file },
      { status: 'fail', subject: 'this worktree', detail: 'Command failed (git rev-parse): not a git repository' },
      { status: 'fail', subject: 'missing checkouts', detail: 'Command failed (git worktree list): not a git repository' },
      { status: 'skip', subject: 'verify hook', detail: 'needs this worktree to be identified' }
    ]);
  });

  it('keeps the config line when the registry cannot even be built', async () => {
    const { world, context } = await checkout();
    const sections = await doctor({
      commandName: 'berth',
      controlHome: world.controlHome,
      runner: new DoctorRunner(world.runner),
      logger: silentLogger,
      router: routerSource(),
      checkout: { kind: 'loaded', loaded: context.loaded },
      nodeVersion: '24.14.1',
      registryFor: () => { throw new Error('no registry here'); }
    });
    expect(section(sections, 'This checkout')).toEqual([
      { status: 'ok', subject: 'berth.toml', detail: context.loaded.file },
      { status: 'fail', subject: 'this worktree', detail: 'no registry here' },
      { status: 'skip', subject: 'verify hook', detail: 'none configured' }
    ]);
  });

  it('still runs verify for a registered worktree when only the missing-checkout scan fails', async () => {
    const { world, context, runner } = await checkout('./verify');
    await up(context);
    const loaded = { ...context.loaded, config: { ...context.loaded.config, hooks: { verify: './verify' } } };
    const sections = await doctor({
      commandName: 'berth',
      controlHome: world.controlHome,
      runner,
      logger: silentLogger,
      router: routerSource(),
      checkout: { kind: 'loaded', loaded },
      nodeVersion: '24.14.1',
      registryFor: () => ({
        current: () => context.registry.current(),
        pruneCandidates: () => Promise.reject(new Error('git worktree list failed'))
      })
    });
    expect(section(sections, 'This checkout').map((c) => `${c.status} ${c.subject}`))
      .toEqual(['ok berth.toml', 'ok this worktree', 'fail missing checkouts', 'ok verify hook']);
  });

  it('lets a cancellation while identifying the worktree end the command', async () => {
    const { world, context } = await checkout();
    await expect(doctor({
      commandName: 'berth',
      controlHome: world.controlHome,
      runner: new DoctorRunner(world.runner),
      logger: silentLogger,
      router: routerSource(),
      checkout: { kind: 'loaded', loaded: context.loaded },
      nodeVersion: '24.14.1',
      registryFor: () => ({ current: () => Promise.reject(new OperationCancelledError('SIGTERM')), pruneCandidates: () => Promise.resolve([]) })
    })).rejects.toThrow('Operation cancelled by SIGTERM.');
  });

  it('warns about a registration whose checkout Git no longer lists', async () => {
    const { run, context, world } = await checkout();
    await up(context);
    const gone = await world.worktree('gone', 'feature/gone');
    const registered = await up(world.context(gone));
    world.runner.active = world.runner.active.filter((root) => root !== gone);
    await rm(gone, { recursive: true, force: true });
    const warning = section(await run(), 'This checkout').find((c) => c.status === 'warn');
    expect(warning).toEqual({
      status: 'warn',
      subject: 'feature-gone.acme.localhost',
      detail: `registered, but Git no longer lists its checkout. Remove what it left with 'berth worktree remove ${registered.registration.worktreeId}'.`
    });
  });
});

describe('findCheckout', () => {
  // The search walks up to the filesystem root, so these assume no berth.toml
  // above the system's temporary directory — as the CLI tests do.
  it('finds nothing where there is no config, and loads the nearest one otherwise', async () => {
    const empty = await scratch();
    await expect(findCheckout(undefined, empty)).resolves.toEqual({ kind: 'none', searchedFrom: empty });
    const project = await scratch();
    await writeFile(path.join(project, 'berth.toml'), '[project]\nname = "acme"\ndomain = "acme.localhost"\n[compose]\nfile = "compose.yaml"\n');
    const nested = path.join(project, 'src');
    await mkdir(nested);
    await expect(findCheckout(undefined, nested)).resolves.toMatchObject({ kind: 'loaded', loaded: { root: project } });
  });

  it('turns a config that does not load into a reportable error', async () => {
    const project = await scratch();
    await writeFile(path.join(project, 'berth.toml'), 'not = [valid');
    await expect(findCheckout(undefined, project)).resolves.toMatchObject({ kind: 'unloadable' });
  });

  it('uses a named config instead of the nearest one, even when the nearest one is fine', async () => {
    const valid = '[project]\nname = "acme"\ndomain = "acme.localhost"\n[compose]\nfile = "compose.yaml"\n';
    const project = await scratch();
    await writeFile(path.join(project, 'berth.toml'), valid);
    const elsewhere = path.join(await scratch(), 'other.toml');
    await writeFile(elsewhere, valid.replace('acme', 'other'));
    await expect(findCheckout(elsewhere, project)).resolves.toMatchObject({ kind: 'loaded', loaded: { file: elsewhere } });
    await expect(findCheckout(path.join(project, 'missing.toml'), project)).resolves.toMatchObject({ kind: 'unloadable' });
  });
});

describe('doctor Locks', () => {
  const REPOSITORY = 'a08b4892-5111-49f1-a47a-5ab988c7f676';
  const WORKTREE = '2b41d9d4-ed88-49c1-be9c-5439bdf6a431';

  async function locksOf(controlHome: string): Promise<Section['checks']> {
    return section(await doctor(await options({ controlHome })), 'Locks');
  }

  async function register(controlHome: string, slug: string): Promise<void> {
    const directory = registryDirectory(controlHome, REPOSITORY);
    await mkdir(directory, { recursive: true });
    await writeFile(path.join(directory, `${WORKTREE}.json`), `${JSON.stringify({
      version: 1, repositoryId: REPOSITORY, worktreeId: WORKTREE, slug, domain: 'acme.localhost', initialization: 'complete'
    })}\n`);
  }

  async function silentLock(target: string, record?: object): Promise<void> {
    await mkdir(path.dirname(target), { recursive: true });
    await writeFile(target, record === undefined ? '' : `${JSON.stringify(record)}\n`);
    await mkdir(`${target}.lock`);
    const silent = new Date(Date.now() - 125_000);
    await utimes(`${target}.lock`, silent, silent);
  }

  const holder = { pid: 4242, host: 'laptop', command: 'berth up', startedAt: 'x' };

  it('reports none held when no lock is taken', async () => {
    await expect(locksOf(await scratch())).resolves.toEqual([{ status: 'ok', subject: 'locks', detail: 'none held' }]);
  });

  it('reports a live holder as an answer, not a problem', async () => {
    const controlHome = await scratch();
    await register(controlHome, 'main');
    const lock = new ScopedLock(worktreeLockTarget(controlHome, REPOSITORY, WORKTREE), 'worktree main', silentLogger, { holder: 'berth up' });
    await lock.acquire();
    try {
      await expect(locksOf(controlHome)).resolves.toEqual([{
        status: 'ok', subject: 'main.acme.localhost', detail: expect.stringMatching(new RegExp(`^held by "berth up" \\(PID ${process.pid} on "[^"]+"\\); heartbeat \\ds ago$`)) as string
      }]);
    } finally {
      await lock.release();
    }
  });

  it('fails a silent worktree lock, and names the command that clears it safely', async () => {
    const controlHome = await scratch();
    await register(controlHome, 'main');
    await silentLock(worktreeLockTarget(controlHome, REPOSITORY, WORKTREE), holder);
    await silentLock(developmentLockTarget(controlHome, REPOSITORY, WORKTREE));
    const checks = await locksOf(controlHome);
    expect(checks.map((check) => `${check.status} ${check.subject}`).sort()).toEqual(['fail main.acme.localhost', 'fail main.acme.localhost (development session)']);
    expect(checks.find((check) => check.subject === 'main.acme.localhost')?.detail).toBe(
      'held by "berth up" (PID 4242 on "laptop"), whose heartbeat has been silent for 2m 5s; every command that needs it waits. ' +
      "Run 'berth worktree unlock main' to check that it, and every command it started on this machine, is gone, and to clear it."
    );
    expect(checks.find((check) => check.subject.endsWith('(development session)'))?.detail).toMatch(/^held by an unidentified holder, /);
    expect(doctorExitCode(await doctor(await options({ controlHome })))).toBe(1);
  });

  it('finds a worktree whose only lock left is its development session\'s', async () => {
    const controlHome = await scratch();
    await register(controlHome, 'main');
    await silentLock(developmentLockTarget(controlHome, REPOSITORY, WORKTREE), holder);
    await expect(locksOf(controlHome)).resolves.toEqual([
      expect.objectContaining({ status: 'fail', subject: 'main.acme.localhost (development session)' })
    ]);
  });

  it('points at the checkout for a worktree lock nothing is registered under', async () => {
    const controlHome = await scratch();
    await silentLock(worktreeLockTarget(controlHome, REPOSITORY, WORKTREE), holder);
    const [check] = await locksOf(controlHome);
    expect(check).toMatchObject({ status: 'fail', subject: `worktree ${WORKTREE}` });
    expect(check?.detail).toContain(`Run 'berth worktree unlock ${WORKTREE}' to check`);
  });

  it('keeps the hand-removal advice for the registry and router locks, which unlock does not clear', async () => {
    const controlHome = await scratch();
    await silentLock(registryLockTarget(controlHome), holder);
    const [check] = await locksOf(controlHome);
    expect(check).toMatchObject({ status: 'fail', subject: 'registry' });
    expect(check?.detail).toContain(`If that process is gone and nothing it started is still running, remove '${registryLockTarget(controlHome)}.lock' with rmdir.`);
  });

  it('is not misled by recovery mutexes, temporary files or directories that are not a worktree\'s', async () => {
    const controlHome = await scratch();
    const worktreeLocksDirectory = path.dirname(worktreeLockTarget(controlHome, REPOSITORY, WORKTREE));
    await silentLock(worktreeRecoveryLockTarget(controlHome, REPOSITORY, WORKTREE));
    await mkdir(worktreeLocksDirectory, { recursive: true });
    await writeFile(path.join(worktreeLocksDirectory, `${WORKTREE}.tmp.123`), '');
    await mkdir(path.join(path.dirname(worktreeLocksDirectory), 'not-a-repository', `${WORKTREE}.lock`), { recursive: true });
    await expect(locksOf(controlHome)).resolves.toEqual([{ status: 'ok', subject: 'locks', detail: 'none held' }]);
  });
});
