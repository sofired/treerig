import { chmodSync } from 'node:fs';
import { mkdtemp, readFile, rm, stat, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { CONTROL_HOME_MARKER, pathExists, secureControlDirectories } from '../../src/files.ts';
import { controlLayout } from '../../src/layout.ts';
import { CommandStartError, type CommandRunner, type RunOptions, type RunResult } from '../../src/process-runner.ts';
import { RouterService, type ContainerInspect, routerStatusReport } from '../../src/router/router.ts';
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
  ROUTED_BY_SINCE_PROTOCOL,
  ROUTER_PROTOCOL,
  routerCommand
} from '../../src/router/stack.ts';
import { RecordingLogger } from '../helpers/command-doubles.ts';
import { fileMode } from '../helpers/fs.ts';

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
  /** What `compose ps` says when Docker itself is the problem. */
  psExitCode = 0;
  psStderr = '';
  /** Pretend `docker` is not installed at all. */
  dockerMissing = false;
  /** Called on every `up`, so a test can make the start itself bring containers back. */
  onUp?: () => void;
  /** Called when `up --force-recreate` runs, so a test can make the recreation "fix" things. */
  onRecreate?: () => void;

  run(command: string, args: readonly string[] = [], options?: RunOptions): Promise<RunResult> {
    this.calls.push({ args, options });
    if (this.dockerMissing) return Promise.reject(new CommandStartError(command, 'ENOENT', `spawn ${command} ENOENT`));
    const ok = (stdout = ''): Promise<RunResult> => Promise.resolve({ stdout, stderr: '', exitCode: 0 });
    if (args[0] === 'compose') {
      if (args.includes('ps')) {
        if (this.psExitCode !== 0) return Promise.resolve({ stdout: '', stderr: this.psStderr, exitCode: this.psExitCode });
        return ok(this.ids[args.at(-1) ?? ''] ?? '');
      }
      if (args.includes('up')) {
        // A failed start brings nothing back in this fake; a leftover container is staged by hand.
        if (this.upExitCode === 0) this.onUp?.();
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
    Config: { Cmd: [...INDEX_COMMAND], Image: INDEX_IMAGE, Labels: { ...labels('index'), ...indexRouteLabels(NETWORK) } },
    HostConfig: { CapAdd: [], CapDrop: ['ALL'], ReadonlyRootfs: true, SecurityOpt: ['no-new-privileges:true'] },
    Mounts: [{ Type: 'bind', Source: indexDir, Destination: INDEX_MOUNT, RW: false }],
    NetworkSettings: { Networks: { [NETWORK]: {} }, Ports: {} },
    State: { Status: 'running', Health: { Status: 'healthy' } }
  };
}

async function world(port = 80, existing?: string) {
  const controlHome = existing ?? await mkdtemp(path.join(tmpdir(), 'berth-router-'));
  if (existing === undefined) temporary.push(controlHome);
  const fake = new DockerFake();
  const logger = new RecordingLogger();
  const layout = controlLayout(controlHome);
  const healthy = (): void => {
    fake.docs.set('router-1', routerDoc(layout.routerDynamicDir, port));
    fake.docs.set('api-1', apiDoc());
    fake.docs.set('index-1', indexDoc(layout.indexDir));
  };
  healthy();
  const router = new RouterService({ controlHome, port, runner: fake, logger, commandName: 'duke' });
  const ups = () => fake.calls.filter((c) => c.args[0] === 'compose' && c.args.includes('up')).map((c) => c.args);
  return { controlHome, layout, fake, logger, router, healthy, ups };
}

/**
 * A control home whose router containers are gone — what `router down` leaves.
 * Pass a protocol to record one as last brought up, or `undefined` to leave
 * any existing record untouched. Starting it again brings the containers
 * back, as a real `up` would.
 */
async function taken(w: Awaited<ReturnType<typeof world>>, protocol: number | undefined): Promise<void> {
  w.fake.ids = {};
  // As in production: the control home is established (and marked) before anything is written into it.
  await secureControlDirectories(w.layout);
  if (protocol !== undefined) await writeFile(w.layout.routerProtocolFile, `${protocol}\n`);
  w.fake.onUp = () => {
    w.fake.ids = { router: 'router-1', 'docker-api': 'api-1', index: 'index-1' };
    w.healthy();
  };
}

/** The exact line `noteUnroutedAfterUpgrade` emits, so asserting its absence cannot go vacuous. */
const UNROUTED = "warn: Worktrees already up elsewhere stay unrouted until their next 'duke up'.";

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
    // What the containers read must be readable by root with no capabilities; what they don't, stays private.
    expect(await fileMode(w.layout.routerDynamicDir)).toBe(0o755);
    expect(await fileMode(w.layout.indexDir)).toBe(0o755);
    expect(await fileMode(w.layout.indexFile)).toBe(0o644);
    expect(await fileMode(w.layout.routerComposeFile)).toBe(0o600);
    expect(await readFile(w.layout.routerProtocolFile, 'utf8')).toBe(`${ROUTER_PROTOCOL}\n`);
    expect(await fileMode(w.layout.routerProtocolFile)).toBe(0o600);

    expect(w.ups()).toEqual([[
      'compose', '--project-directory', w.layout.routerDir, '--project-name', PROJECT, '-f', w.layout.routerComposeFile,
      'up', '--detach', '--wait', '--remove-orphans'
    ]]);
    expect(w.logger.lines.filter((l) => l.startsWith('warn:'))).toEqual([]);
  });

  it('runs Compose with the same guarded environment as worktrees, so a shell setting cannot split the two', async () => {
    const w = await world();
    process.env.COMPOSE_IGNORE_ORPHANS = 'true';   // would make `up --remove-orphans` a hard error
    process.env.COMPOSE_FILE = '/elsewhere/compose.yaml';
    try {
      await w.router.ensure();
    } finally {
      delete process.env.COMPOSE_IGNORE_ORPHANS;
      delete process.env.COMPOSE_FILE;
    }
    const [up] = w.fake.calls.filter((c) => c.args[0] === 'compose' && c.args.includes('up'));
    expect(up?.options?.env).not.toHaveProperty('COMPOSE_IGNORE_ORPHANS');
    expect(up?.options?.env).not.toHaveProperty('COMPOSE_FILE');
    expect(up?.options?.env).toMatchObject({ COMPOSE_PROGRESS: 'plain', PATH: process.env.PATH });
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

  it('says so when it upgrades a router started by an older version, and what that costs other worktrees', async () => {
    const w = await world();
    const older = routerDoc(w.layout.routerDynamicDir);
    older.Config = { ...older.Config, Labels: { ...labels('router'), [`${NS}.router-protocol`]: String(ROUTED_BY_SINCE_PROTOCOL - 1) } };
    w.fake.docs.set('router-1', older);
    w.fake.onRecreate = w.healthy;
    await w.router.ensure();
    expect(w.logger.lines).toContain(`info: Upgrading the shared router from protocol ${ROUTED_BY_SINCE_PROTOCOL - 1} to ${ROUTER_PROTOCOL}.`);
    expect(w.logger.lines).toContain(UNROUTED);
  });

  // Switches itself on at the next protocol bump: a router that already carries the label needs no such warning.
  it.skipIf(ROUTER_PROTOCOL === ROUTED_BY_SINCE_PROTOCOL)('upgrades a router that already routes by label without warning about routing', async () => {
    const w = await world();
    const older = routerDoc(w.layout.routerDynamicDir);
    older.Config = { ...older.Config, Labels: { ...labels('router'), [`${NS}.router-protocol`]: String(ROUTER_PROTOCOL - 1) } };
    w.fake.docs.set('router-1', older);
    w.fake.onRecreate = w.healthy;
    await w.router.ensure();
    expect(w.logger.lines).toContain(`info: Upgrading the shared router from protocol ${ROUTER_PROTOCOL - 1} to ${ROUTER_PROTOCOL}.`);
    // The drift warning still fires here (the fake's doc carries the old label until it is recreated); only routing must stay silent.
    expect(w.logger.lines).not.toContain(UNROUTED);
  });

  it('upgrades a router that was taken down across the upgrade, from what this control home last ran', async () => {
    const w = await world();
    await taken(w, ROUTED_BY_SINCE_PROTOCOL - 1);

    await w.router.ensure();

    expect(w.logger.lines).toContain(`info: Upgrading the shared router from protocol ${ROUTED_BY_SINCE_PROTOCOL - 1} to ${ROUTER_PROTOCOL}.`);
    expect(w.logger.lines).toContain(UNROUTED);
    expect(w.ups()).toHaveLength(1);   // the start itself brought it back; no drift repair was needed
    expect(w.logger.lines.some((line) => line.includes('not configured as expected'))).toBe(false);
    expect(await readFile(w.layout.routerProtocolFile, 'utf8')).toBe(`${ROUTER_PROTOCOL}\n`);   // promoted once it verified
  });

  it('tells a second control home its worktrees went dark when another home did the upgrading', async () => {
    const w = await world();
    // This home never recorded anything (no released version did) and its own stack
    // file is the only witness that it last ran an older router; the machine-wide
    // router is already upgraded, by some other control home.
    await secureControlDirectories(w.layout);
    await writeFile(w.layout.routerComposeFile, JSON.stringify({
      services: { router: { labels: { [`${NS}.router-protocol`]: String(ROUTED_BY_SINCE_PROTOCOL - 1) } } }
    }));

    await w.router.ensure();

    expect(w.logger.lines).toContain(
      `info: The shared router is already at protocol ${ROUTER_PROTOCOL}, but this control home last saw ${ROUTED_BY_SINCE_PROTOCOL - 1}.`
    );
    expect(w.logger.lines).toContain(UNROUTED);
  });

  it("does not let a newer version's stack file, which never started, poison the record", async () => {
    const w = await world();
    await taken(w, undefined);
    // A newer version wrote its stack file and then failed to start: intent, never a run.
    await writeFile(w.layout.routerComposeFile, JSON.stringify({
      services: { router: { labels: { [`${NS}.router-protocol`]: String(ROUTER_PROTOCOL + 1) } } }
    }));
    w.fake.upExitCode = 1;
    w.fake.upStderr = 'bind: address already in use';
    await expect(w.router.ensure()).rejects.toThrow('The shared router failed to start');

    const second = await world(80, w.controlHome);
    await taken(second, undefined);

    // If that number had been recorded, this control home would refuse itself for ever.
    await expect(second.router.ensure()).resolves.toBeUndefined();
  });

  it('names both steps when a newer router and a newer record are in the way, and both together hand it over', async () => {
    const w = await world();
    await secureControlDirectories(w.layout);
    await writeFile(w.layout.routerProtocolFile, `${ROUTER_PROTOCOL + 1}\n`);
    const newer = routerDoc(w.layout.routerDynamicDir);
    newer.Config = { ...newer.Config, Labels: { ...labels('router'), [`${NS}.router-protocol`]: String(ROUTER_PROTOCOL + 1) } };
    w.fake.docs.set('router-1', newer);

    await expect(w.router.ensure()).rejects.toThrow(
      `remove it with 'duke router down' and this control home's record with \`rm '${w.layout.routerProtocolFile}'\``
    );

    // Doing both of the things it named has to be enough.
    w.fake.ids = {};
    await rm(w.layout.routerProtocolFile);
    await taken(w, undefined);
    await expect(w.router.ensure()).resolves.toBeUndefined();
  });

  it('ignores a record that has been corrupted into something that is not a protocol', async () => {
    const w = await world();
    await taken(w, undefined);
    await writeFile(w.layout.routerProtocolFile, '0x10\n');   // reads as 16 to `Number`, but it is corruption

    await w.router.ensure();

    expect(w.logger.lines).toContain(
      `warn: Ignoring ${w.layout.routerProtocolFile}: it does not name a router protocol. ` +
      `This run takes the previous protocol from the router itself, or from ${w.layout.routerComposeFile}, if either is still there.`
    );
    expect(w.logger.lines.some((line) => line.includes('protocol 16'))).toBe(false);
    expect(w.logger.lines).not.toContain(UNROUTED);   // no history at all is not an upgrade
  });

  it.each(['0', '99999999999999999999'])('reads a record of %s as no protocol at all', async (recorded) => {
    const w = await world();
    await taken(w, undefined);
    await writeFile(w.layout.routerProtocolFile, `${recorded}\n`);

    await w.router.ensure();

    expect(w.logger.lines.some((line) => line.startsWith(`warn: Ignoring ${w.layout.routerProtocolFile}`))).toBe(true);
  });

  // Root reads a 0000 file regardless, so there would be nothing to refuse.
  it.skipIf(process.getuid?.() === 0)('refuses to guess when its own protocol record cannot be read', async () => {
    const w = await world();
    await taken(w, undefined);
    await writeFile(w.layout.routerProtocolFile, `${ROUTER_PROTOCOL}\n`);
    chmodSync(w.layout.routerProtocolFile, 0o000);

    try {
      // Reading it as "no record" is how a newer router gets silently downgraded.
      await expect(w.router.ensure()).rejects.toThrow(`${w.layout.routerProtocolFile} could not be read (EACCES)`);
      expect(w.ups()).toEqual([]);
    } finally {
      chmodSync(w.layout.routerProtocolFile, 0o600);
    }
  });

  it('ignores a stack file it cannot parse, which it is about to replace anyway', async () => {
    const w = await world();
    await taken(w, undefined);
    await writeFile(w.layout.routerComposeFile, '{ not json');

    // One corrupt byte there must not brick every `up` on the machine.
    await expect(w.router.ensure()).resolves.toBeUndefined();
  });

  // Root reads a 0000 file regardless, so there would be nothing to ignore.
  it.skipIf(process.getuid?.() === 0)('ignores a stack file it cannot read either', async () => {
    const w = await world();
    await taken(w, undefined);
    await writeFile(w.layout.routerComposeFile, '{}');
    chmodSync(w.layout.routerComposeFile, 0o000);

    try {
      // Unreadable is no answer here, where unreadable is fatal for the record.
      await expect(w.router.ensure()).resolves.toBeUndefined();
    } finally {
      chmodSync(w.layout.routerComposeFile, 0o600);
    }
  });

  it('lets a container carrying no protocol label fall through to the other evidence', async () => {
    const w = await world();
    const unlabelled = routerDoc(w.layout.routerDynamicDir);
    const rest = { ...labels('router') };
    delete rest[`${NS}.router-protocol`];
    unlabelled.Config = { ...unlabelled.Config, Labels: rest };
    w.fake.docs.set('router-1', unlabelled);
    w.fake.onRecreate = w.healthy;

    // Not ours, or started under another namespace: no answer, not an invalid one.
    await expect(w.router.ensure()).resolves.toBeUndefined();
  });

  it('records nothing when the first ever start fails, because there was no history to record', async () => {
    const w = await world();
    await taken(w, undefined);
    w.fake.upExitCode = 1;
    w.fake.upStderr = 'bind: address already in use';

    await expect(w.router.ensure()).rejects.toThrow('The shared router failed to start');
    expect(await pathExists(w.layout.routerProtocolFile)).toBe(false);
  });

  it("refuses a router name that could rewrite Traefik's discovery predicate", () => {
    const base = { controlHome: '/tmp/unused', port: 80, runner: new DockerFake(), logger: new RecordingLogger() };
    expect(() => new RouterService({ ...base, projectName: 'berth-router`,`x' })).toThrow('Invalid router project name');
    expect(() => new RouterService({ ...base, network: 'n`,`x' })).toThrow('Invalid router network name');
    expect(() => new RouterService({ ...base, projectName: 'berth-router-2.test_a' })).not.toThrow();
  });

  it('refuses a router whose protocol label is not a protocol', async () => {
    const w = await world();
    const odd = routerDoc(w.layout.routerDynamicDir);
    odd.Config = { ...odd.Config, Labels: { ...labels('router'), [`${NS}.router-protocol`]: '1e3' } };
    w.fake.docs.set('router-1', odd);

    await expect(w.router.ensure()).rejects.toThrow(
      "The shared router carries an invalid protocol label: 1e3. Remove the router with 'duke router down' and run this again."
    );
  });

  it('remembers what it last brought up, so an upgrade that failed to start is still announced next time', async () => {
    const w = await world();
    await taken(w, ROUTED_BY_SINCE_PROTOCOL - 1);
    w.fake.upExitCode = 1;
    w.fake.upStderr = 'bind: address already in use';

    await expect(w.router.ensure()).rejects.toThrow('The shared router failed to start');
    // `write()` already stamped the new protocol into the stack file; the record of what ran must not have moved.
    expect(w.logger.lines).not.toContain(UNROUTED);

    const second = await world(80, w.controlHome);
    await taken(second, undefined);   // same control home, still nothing running

    await second.router.ensure();

    expect(second.logger.lines).toContain(`info: Upgrading the shared router from protocol ${ROUTED_BY_SINCE_PROTOCOL - 1} to ${ROUTER_PROTOCOL}.`);
    expect(second.logger.lines).toContain(UNROUTED);
  });

  it('trusts what it last brought up over a container a failed start left behind', async () => {
    const w = await world();
    await taken(w, ROUTED_BY_SINCE_PROTOCOL - 1);
    // A start that failed after creating the containers leaves them already carrying
    // the new protocol label, though no router of that protocol has ever served anything.
    w.fake.ids = { router: 'router-1', 'docker-api': 'api-1', index: 'index-1' };
    w.healthy();
    const leftover = routerDoc(w.layout.routerDynamicDir);
    leftover.State = { Status: 'created' };   // created by the failed `up`, never started
    w.fake.docs.set('router-1', leftover);

    await w.router.ensure();

    expect(w.logger.lines).toContain(`info: Upgrading the shared router from protocol ${ROUTED_BY_SINCE_PROTOCOL - 1} to ${ROUTER_PROTOCOL}.`);
    expect(w.logger.lines).toContain(UNROUTED);
  });

  it('says nothing about upgrades when the router it brought up is the one this version speaks', async () => {
    const w = await world();
    await w.router.ensure();
    // The record is written by `ensure` itself, and is the only source left once the stack file is gone.
    expect(await readFile(w.layout.routerProtocolFile, 'utf8')).toBe(`${ROUTER_PROTOCOL}\n`);
    await rm(w.layout.routerComposeFile);
    const again = await world(80, w.controlHome);
    await taken(again, undefined);   // taken down, then brought back on the same version

    await again.router.ensure();

    expect(again.logger.lines.some((line) => line.includes('Upgrading'))).toBe(false);
    expect(again.logger.lines).not.toContain(UNROUTED);
  });

  it('refuses to take over from a newer version this control home last ran, and hands over once that record is gone', async () => {
    const w = await world();
    await taken(w, ROUTER_PROTOCOL + 1);
    // The newer version wrote its stack file before it ever started; that alone must not keep refusing.
    await writeFile(w.layout.routerComposeFile, JSON.stringify({
      services: { router: { labels: { [`${NS}.router-protocol`]: String(ROUTER_PROTOCOL + 1) } } }
    }));

    await expect(w.router.ensure()).rejects.toThrow(
      `This control home last recorded the shared router at protocol ${ROUTER_PROTOCOL + 1}; this version speaks ${ROUTER_PROTOCOL}. Run that version again, or remove the record with \`rm `
    );

    // The remedy the message names has to work, or it is not a remedy.
    await rm(w.layout.routerProtocolFile);
    await expect(w.router.ensure()).resolves.toBeUndefined();
  });

  it('records what it found on a control home that has none yet, so a failed start cannot erase it', async () => {
    const w = await world();
    await taken(w, undefined);   // no record, as on every control home from before this version
    await writeFile(w.layout.routerComposeFile, JSON.stringify({
      services: { router: { labels: { [`${NS}.router-protocol`]: String(ROUTED_BY_SINCE_PROTOCOL - 1) } } }
    }));
    w.fake.upExitCode = 1;
    w.fake.upStderr = 'bind: address already in use';

    await expect(w.router.ensure()).rejects.toThrow('The shared router failed to start');
    expect(await readFile(w.layout.routerProtocolFile, 'utf8')).toBe(`${ROUTED_BY_SINCE_PROTOCOL - 1}\n`);

    const second = await world(80, w.controlHome);
    await taken(second, undefined);
    await second.router.ensure();

    expect(second.logger.lines).toContain(`info: Upgrading the shared router from protocol ${ROUTED_BY_SINCE_PROTOCOL - 1} to ${ROUTER_PROTOCOL}.`);
    expect(second.logger.lines).toContain(UNROUTED);
  });

  it('refuses to downgrade a newer router that is running, whatever its own record says', async () => {
    const w = await world();
    // The router is machine-wide; another control home may have upgraded it.
    await secureControlDirectories(w.layout);
    await writeFile(w.layout.routerProtocolFile, `${ROUTER_PROTOCOL}\n`);
    const newer = routerDoc(w.layout.routerDynamicDir);
    newer.Config = { ...newer.Config, Labels: { ...labels('router'), [`${NS}.router-protocol`]: String(ROUTER_PROTOCOL + 1) } };
    w.fake.docs.set('router-1', newer);

    await expect(w.router.ensure()).rejects.toThrow(
      `The shared router is at protocol ${ROUTER_PROTOCOL + 1}; this version speaks ${ROUTER_PROTOCOL}.`
    );
    // The record is at this version's protocol, so it is not in the way and must not be named.
    await expect(w.router.ensure()).rejects.toThrow(/remove it with 'duke router down'\.$/);
    expect(w.ups()).toEqual([]);   // the refusal precedes every mutation
    expect(await readFile(w.layout.routerProtocolFile, 'utf8')).toBe(`${ROUTER_PROTOCOL}\n`);   // record left alone
  });

  it('does not announce unrouted worktrees when the upgrade itself fails', async () => {
    const w = await world();
    const older = routerDoc(w.layout.routerDynamicDir);
    older.Config = { ...older.Config, Labels: { ...labels('router'), [`${NS}.router-protocol`]: String(ROUTED_BY_SINCE_PROTOCOL - 1) } };
    w.fake.docs.set('router-1', older);
    w.fake.upExitCode = 1;
    w.fake.upStderr = 'bind: address already in use';
    await expect(w.router.ensure()).rejects.toThrow('The shared router failed to start');
    expect(w.logger.lines).not.toContain(UNROUTED);
  });

  it('tells Traefik to discover only containers labelled for this router, and labels the index that way', () => {
    // Pinned as literal text: the verifier compares against the same constants, so it alone cannot catch a typo here.
    expect(routerCommand({ network: 'n' })).toContain('--providers.docker.constraints=Label(`dev.berth.router`,`n`)');
    expect(indexRouteLabels('n')).toMatchObject({ 'dev.berth.router': 'n', 'traefik.enable': 'true' });
    // Every fixture derives from this, so nothing else would notice it moving.
    expect(ROUTED_BY_SINCE_PROTOCOL).toBe(3);
  });

  it('refuses a router it cannot inspect rather than recreating it blind', async () => {
    const w = await world();
    w.fake.docs.clear();   // the containers are there; `docker inspect` says nothing about them

    await expect(w.router.ensure()).rejects.toThrow('The shared router exists but could not be inspected.');
    expect(w.ups()).toEqual([]);   // a router this version may not downgrade is never recreated blind
  });

  // Root bypasses the mode this stages, so there would be nothing to warn about.
  it.skipIf(process.getuid?.() === 0)('warns instead of failing a verified up when the protocol record cannot be written', async () => {
    const w = await world();
    await taken(w, ROUTED_BY_SINCE_PROTOCOL - 1);
    const bring = w.fake.onUp;
    w.fake.onUp = () => {
      bring?.();
      chmodSync(w.layout.routerDir, 0o500);   // read-only by the time the record is promoted
    };

    try {
      await expect(w.router.ensure()).resolves.toBeUndefined();   // the router is up; only the record failed
      expect(w.logger.lines.some((line) =>
        line.startsWith(`warn: Could not record the router protocol in ${w.layout.routerProtocolFile}: `))).toBe(true);
    } finally {
      chmodSync(w.layout.routerDir, 0o700);   // or the cleanup cannot unlink
    }
  });

  it('explains a failed start in terms of the port', async () => {
    const w = await world(8080);
    w.fake.upExitCode = 1;
    w.fake.upStderr = 'Error response from daemon: Bind for 127.0.0.1:8080 failed: port is already allocated';
    await expect(w.router.ensure()).rejects.toThrow(/127\.0\.0\.1:8080 is free, or choose another port with BERTH_ROUTER_PORT[\s\S]*port is already allocated/);
  });

  it('refuses a control home the Go build marked, before writing the stack or asking Docker to start it', async () => {
    const w = await world();
    await writeFile(path.join(w.controlHome, CONTROL_HOME_MARKER), 'format 2\n');

    await expect(w.router.ensure()).rejects.toThrow(
      'is managed by the Go build of berth; this TypeScript build does not change it.'
    );

    expect(w.fake.calls).toEqual([]);
    expect(await pathExists(w.layout.routerComposeFile)).toBe(false);
  });

  it('refuses when the marker turns up while the previous protocol was being read under the lock, without writing the stack', async () => {
    const w = await world();
    const marker = path.join(w.controlHome, CONTROL_HOME_MARKER);
    const originalRun = w.fake.run.bind(w.fake);
    // `previousProtocol()` is the first thing `ensure()` does once it holds
    // the router lock; writing the marker from inside its `compose ps` call
    // stands in for the Go build claiming the control home while another
    // `router up` held that lock, well before the stack is written.
    w.fake.run = async (command, args = [], options) => {
      if (args[0] === 'compose' && args.includes('ps')) await writeFile(marker, 'format 2\n');
      return originalRun(command, args, options);
    };

    await expect(w.router.ensure()).rejects.toThrow(
      'is managed by the Go build of berth; this TypeScript build does not change it.'
    );

    expect(w.fake.calls.some((c) => c.args.includes('up'))).toBe(false);
    expect(await pathExists(w.layout.routerComposeFile)).toBe(false);
  });
});

describe('RouterService.verify', () => {
  const tampering: [string, (router: ContainerInspect, api: ContainerInspect, index: ContainerInspect) => void, string][] = [
    ['the index serving another directory', (_r, _a, i) => { i.Mounts = [{ Type: 'bind', Source: '/etc', Destination: INDEX_MOUNT, RW: false }]; }, 'index: mounts are not exactly the read-only index directory'],
    ['the index publishing a port', (_r, _a, i) => { i.NetworkSettings = { ...i.NetworkSettings, Ports: { '8080/tcp': [{ HostIp: '0.0.0.0', HostPort: '8080' }] } }; }, 'index: publishes a port'],
    ['the index routed elsewhere', (_r, _a, i) => { i.Config = { ...i.Config, Labels: { ...i.Config?.Labels, 'traefik.http.routers.berth-index.rule': 'Host(`evil.localhost`)' } }; }, 'index: routing labels differ'],
    ['a router still carrying an older protocol label', (r) => { r.Config = { ...r.Config, Labels: { ...labels('router'), [`${NS}.router-protocol`]: String(ROUTER_PROTOCOL - 1) } }; }, 'router: protocol label'],
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
  it('reports a stack it cannot inspect as unverifiable rather than clean', async () => {
    const w = await world();
    w.fake.docs.delete('api-1');   // the containers are listed; one of them says nothing

    await expect(w.router.verify()).resolves.toEqual(['the router could not be inspected']);
  });

  it('reports a router that is not running', async () => {
    const w = await world();
    w.fake.ids = {};
    await expect(w.router.status()).resolves.toEqual({ reachable: true, running: false, problems: ['the router is not running'], port: 80, network: NETWORK });
  });

  // A stack with a hole in it is still bound to its port and still routing
  // whatever Traefik already discovered. Calling that "not running" would send
  // an operator to `router up` when what they need to see is which container
  // went away — and `routerStatusReport` maps `running: false` to exit 0.
  it('names the containers a half-present stack is missing rather than calling it stopped', async () => {
    const w = await world();
    w.fake.ids = { router: 'router-1' };

    await expect(w.router.status()).resolves.toEqual({
      reachable: true,
      running: true,
      problems: ['docker-api: container is missing', 'index: container is missing'],
      port: 80,
      network: NETWORK
    });
  });

  it('says nothing is missing when every container is there', async () => {
    const w = await world();
    const problems = await w.router.verify();
    expect(problems.filter((problem) => problem.includes('container is missing'))).toEqual([]);
  });

  it('reports Docker as unreachable when `compose ps` fails for a reason it does not recognize', async () => {
    const w = await world();
    w.fake.psExitCode = 125;
    w.fake.psStderr = 'context "desktop-linux" not found\n';

    await expect(w.router.status()).resolves.toMatchObject({
      reachable: false,
      running: false,
      problems: ['Docker is not available: context "desktop-linux" not found']
    });
  });

  it('reports Docker as unreachable, not the router as stopped, when Compose cannot talk to the daemon', async () => {
    const w = await world();
    w.fake.psExitCode = 1;
    w.fake.psStderr = 'failed to connect to the docker API at unix:///var/run/docker.sock; check if the path is correct and if the daemon is running: dial unix /var/run/docker.sock: connect: no such file or directory\n';

    await expect(w.router.status()).resolves.toEqual({
      reachable: false,
      running: false,
      problems: ['Docker is not available: failed to connect to the docker API at unix:///var/run/docker.sock; check if the path is correct and if the daemon is running: dial unix /var/run/docker.sock: connect: no such file or directory'],
      port: 80,
      network: NETWORK
    });
  });

  it('asks Docker about the router by project name alone, so a missing stack file is never mistaken for a missing daemon', async () => {
    const w = await world();
    await w.router.status();
    const ps = w.fake.calls.find((c) => c.args.includes('ps'))?.args ?? [];
    expect(ps).toEqual(['compose', '--project-name', PROJECT, 'ps', '--all', '--quiet', 'router']);
  });

  it('reports the line Docker meant, not the warning it printed first', async () => {
    const w = await world();
    w.fake.psExitCode = 1;
    w.fake.psStderr = 'WARNING: Error parsing config file (/home/me/.docker/config.json): invalid character\n' +
      'Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n';

    await expect(w.router.status()).resolves.toMatchObject({
      reachable: false,
      problems: ['Docker is not available: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?']
    });
  });

  it('reports the reason, not the help hint Docker prints after it', async () => {
    const w = await world();
    w.fake.psExitCode = 1;
    w.fake.psStderr = 'docker: unknown command: docker compose\n\nRun \'docker --help\' for more information\n';

    await expect(w.router.status()).resolves.toMatchObject({
      reachable: false,
      problems: ['Docker is not available: docker: unknown command: docker compose']
    });
  });

  it('does not blame the port when the socket is not ours', async () => {
    const w = await world();
    w.fake.ids = {};
    w.fake.upExitCode = 1;
    w.fake.upStderr = 'permission denied while trying to connect to the docker API at unix:///var/run/docker.sock\n';

    await expect(w.router.ensure()).rejects.toThrow('Docker is not available: permission denied while trying to connect to the docker API at unix:///var/run/docker.sock');
  });

  it('says that docker is not installed instead of blaming the port when it cannot be started', async () => {
    const w = await world();
    w.fake.dockerMissing = true;

    await expect(w.router.ensure()).rejects.toThrow('Docker is not available: the `docker` command is not on PATH (ENOENT); is Docker installed?');
    await expect(w.router.status()).resolves.toMatchObject({ reachable: false, running: false });
  });

  it('takes the router down by project name alone, so a control home with no stack file can still free it', async () => {
    const w = await world();

    await w.router.down();

    expect(w.fake.calls.find((c) => c.args.includes('down'))?.args)
      .toEqual(['compose', '--project-name', PROJECT, 'down', '--remove-orphans']);
    expect(await pathExists(w.layout.routerComposeFile)).toBe(false);   // nothing was written first
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

  it('refuses to take down a control home the Go build marked, without asking Docker', async () => {
    const w = await world();
    await writeFile(path.join(w.controlHome, CONTROL_HOME_MARKER), 'format 2\n');

    await expect(w.router.down()).rejects.toThrow(
      'is managed by the Go build of berth; this TypeScript build does not change it.'
    );

    expect(w.fake.calls).toEqual([]);
  });

  it('still reports status on a control home the Go build marked', async () => {
    const w = await world();
    await writeFile(path.join(w.controlHome, CONTROL_HOME_MARKER), 'format 2\n');

    await expect(w.router.status()).resolves.toEqual({ reachable: true, running: true, problems: [], port: 80, network: NETWORK });
  });
});

describe('what `router status` says, and where', () => {
  const base = { port: 80, network: 'berth-router' };

  it('says nothing on stdout when Docker could not be asked, and fails', () => {
    expect(routerStatusReport({
      ...base,
      reachable: false,
      running: false,
      problems: ['Docker is not available: the `docker` command is not on PATH (ENOENT).']
    })).toEqual({
      output: [],
      errors: ['Router state unknown: Docker is not available: the `docker` command is not on PATH (ENOENT)'],
      failed: true
    });
  });

  it('will not call a drifted stack running on the stream a script reads', () => {
    // `running` only means a container exists; these two say it is not serving.
    expect(routerStatusReport({
      ...base,
      reachable: true,
      running: true,
      problems: ['router: not running', 'router: not healthy']
    })).toEqual({
      output: ['degraded on 127.0.0.1:80, network berth-router (2 problem(s) reported on stderr)'],
      errors: ['Router problem: router: not running', 'Router problem: router: not healthy'],
      failed: true
    });
  });

  it('reports a clean router, and a stopped one, without failing', () => {
    expect(routerStatusReport({ ...base, reachable: true, running: true, problems: [] })).toEqual({
      output: ['running on 127.0.0.1:80, network berth-router'],
      errors: [],
      failed: false
    });
    expect(routerStatusReport({ ...base, reachable: true, running: false, problems: ['the router is not running'] })).toEqual({
      output: ['not running (would use 127.0.0.1:80, network berth-router)'],
      errors: [],
      failed: false
    });
  });
});
