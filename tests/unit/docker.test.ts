import { describe, expect, it } from 'vitest';
import { DockerUnavailableError, dockerComplaint, dockerSaid, runDocker } from '../../src/docker.ts';
import { CommandStartError, type CommandRunner, type RunResult } from '../../src/process-runner.ts';

const answering = (result: Partial<RunResult>): CommandRunner => ({
  run: () => Promise.resolve({ stdout: '', stderr: '', exitCode: 0, ...result }),
  which: () => Promise.resolve(true)
});

describe('dockerSaid', () => {
  it('skips the warnings Docker prints before the reason', () => {
    const stderr = 'WARNING: Error parsing config file (/home/me/.docker/config.json): invalid character\n' +
      'time="2026-09-21T10:00:00Z" level=warning msg="something"\n' +
      'Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n';
    expect(dockerSaid(stderr, 'fallback')).toBe('Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?');
  });

  it('skips the help hint Docker prints after the reason', () => {
    expect(dockerSaid("docker: 'compose' is not a docker command.\nSee 'docker --help'\n", 'fallback')).toBe("docker: 'compose' is not a docker command.");
    expect(dockerSaid("docker: unknown command: docker compose\n\nRun 'docker --help' for more information\n", 'fallback')).toBe('docker: unknown command: docker compose');
  });

  it('settles for the last warning when that is all there is, and the fallback when there is nothing', () => {
    expect(dockerSaid('WARN[0000] first\nWARN[0001] second\n', 'fallback')).toBe('WARN[0001] second');
    expect(dockerSaid('  \n', 'fallback')).toBe('fallback');
  });
});

describe('runDocker', () => {
  it('reports a daemon that cannot be reached as Docker being unavailable, whatever the caller asked for', async () => {
    const runner = answering({ exitCode: 1, stderr: 'failed to connect to the docker API at unix:///var/run/docker.sock; check if the path is correct and if the daemon is running\n' });
    const attempt = runDocker(runner, ['ps'], { reject: false });
    await expect(attempt).rejects.toBeInstanceOf(DockerUnavailableError);
    await expect(attempt).rejects.toThrow('Docker is not available: failed to connect to the docker API at unix:///var/run/docker.sock; check if the path is correct and if the daemon is running');
  });

  it('reports a missing Compose plugin the same way', async () => {
    const runner = answering({ exitCode: 1, stderr: "docker: 'compose' is not a docker command.\nSee 'docker --help'\n" });
    await expect(runDocker(runner, ['compose', 'ps'], { reject: false })).rejects.toThrow("Docker is not available: docker: 'compose' is not a docker command.");
  });

  it('reports a docker that is not installed the same way, with the obvious question', async () => {
    const runner: CommandRunner = {
      run: (command) => Promise.reject(new CommandStartError(command, 'ENOENT', `spawn ${command} ENOENT`)),
      which: () => Promise.resolve(false)
    };
    await expect(runDocker(runner, ['ps'])).rejects.toThrow('Docker is not available: the `docker` command is not on PATH (ENOENT); is Docker installed?');
  });

  it('reports the complaint line itself, not the progress Compose printed before the daemon went away', async () => {
    const runner = answering({ exitCode: 1, stderr: ' Network acme_default  Creating\n Container acme-db  Created\nerror during connect: Get "http://%2Fvar%2Frun%2Fdocker.sock/v1.47/containers/json": EOF\n' });
    await expect(runDocker(runner, ['compose', 'up'], { reject: false })).rejects.toThrow('Docker is not available: error during connect: Get "http://%2Fvar%2Frun%2Fdocker.sock/v1.47/containers/json": EOF');
  });

  it('does not mistake a build step that talks about Docker for Docker itself', async () => {
    const stderr = '#5 [2/3] RUN docker version\n#5 0.412 Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n#5 ERROR: process "/bin/sh -c docker version" did not complete successfully: exit code: 1\n';
    expect(dockerComplaint(stderr)).toBeUndefined();
    await expect(runDocker(answering({ exitCode: 17, stderr }), ['compose', 'up', '--build'], { reject: false })).resolves.toMatchObject({ exitCode: 17 });
    await expect(runDocker(answering({ exitCode: 17, stderr }), ['compose', 'up', '--build'])).rejects.toThrow('Command failed (docker compose up --build): #5 [2/3] RUN docker version');
  });

  it('recognises a socket that is not ours, in the wording of Docker before and after 28', async () => {
    const before = answering({ exitCode: 1, stderr: 'permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock: Get "http://%2Fvar%2Frun%2Fdocker.sock/v1.24/containers/json": dial unix /var/run/docker.sock: connect: permission denied\n' });
    await expect(runDocker(before, ['ps'], { reject: false })).rejects.toBeInstanceOf(DockerUnavailableError);
    const after = answering({ exitCode: 1, stderr: 'permission denied while trying to connect to the docker API at unix:///var/run/docker.sock\n' });
    await expect(runDocker(after, ['compose', 'up'], { reject: false })).rejects.toThrow('Docker is not available: permission denied while trying to connect to the docker API at unix:///var/run/docker.sock');
    const engine20 = answering({ exitCode: 1, stderr: 'Got permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock: connect: permission denied\n' });
    await expect(runDocker(engine20, ['ps'], { reject: false })).rejects.toBeInstanceOf(DockerUnavailableError);
  });

  it('keeps the words for a docker that exists but cannot run', async () => {
    const runner: CommandRunner = {
      run: (command) => Promise.reject(new CommandStartError(command, 'EACCES', `spawn ${command} EACCES`)),
      which: () => Promise.resolve(false)
    };
    await expect(runDocker(runner, ['ps'])).rejects.toThrow('Docker is not available: the `docker` command is not executable (EACCES).');
  });

  it('recognises Docker Desktop while it is still starting, in each of its wordings', async () => {
    for (const line of [
      'Error response from daemon: dial unix docker.raw.sock: connect: connection refused',
      'Error response from daemon: Bad response from Docker engine',
      'request returned Internal Server Error for API route and version http://%2Fvar%2Frun%2Fdocker.sock/v1.47/containers/json, check if the server supports the requested API version'
    ]) {
      await expect(runDocker(answering({ exitCode: 1, stderr: `${line}\n` }), ['ps'], { reject: false })).rejects.toThrow(`Docker is not available: ${line}`);
    }
  });

  it('honours a timeout the way the runner does', async () => {
    const runner = answering({ exitCode: 0, timedOut: true });
    await expect(runDocker(runner, ['ps'], { timeoutMs: 5 })).rejects.toMatchObject({ exitCode: 124 });
    await expect(runDocker(runner, ['ps'], { timeoutMs: 5, reject: false })).resolves.toMatchObject({ timedOut: true });
  });

  it("leaves an ordinary failed request to the caller — that is the request's verdict, not Docker's", async () => {
    const runner = answering({ exitCode: 1, stderr: 'Error response from daemon: No such container: abc\n' });
    await expect(runDocker(runner, ['inspect', 'abc'], { reject: false })).resolves.toMatchObject({ exitCode: 1 });
    await expect(runDocker(runner, ['inspect', 'abc'])).rejects.toThrow('Command failed (docker inspect abc): Error response from daemon: No such container: abc');
  });

  it('reads Docker before honouring a caller that wanted failures thrown, passing everything else through', async () => {
    const calls: unknown[] = [];
    const runner: CommandRunner = {
      run: (_command, _args, options) => {
        calls.push(options);
        return Promise.resolve({ stdout: '', stderr: 'Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n', exitCode: 1 });
      },
      which: () => Promise.resolve(true)
    };
    const controller = new AbortController();
    await expect(runDocker(runner, ['compose', 'down'], { env: { A: '1' }, cancelSignal: controller.signal, printOutput: true, timeoutMs: 5 }))
      .rejects.toThrow('Docker is not available: Cannot connect to the Docker daemon');
    expect(calls).toEqual([{ env: { A: '1' }, cancelSignal: controller.signal, printOutput: true, timeoutMs: 5, reject: false }]);
  });
});
