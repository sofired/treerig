import { CliError } from './errors.ts';
import { CommandStartError, commandFailure, type CommandRunner, type RunOptions, type RunResult } from './process-runner.ts';

/**
 * Docker could not do what was asked of it — not installed, daemon down, the
 * socket not ours, the Compose plugin missing — so nothing about containers
 * can be known. Every verb reports this the same way, whichever call hit it.
 */
export class DockerUnavailableError extends CliError {
  constructor(detail: string) {
    super(`Docker is not available: ${detail}`);
    this.name = 'DockerUnavailableError';
  }
}

/**
 * A line Docker prints when the problem is Docker itself rather than the
 * request. Anchored to the start of a line so that a build step's or a
 * container's own output, which may embed the very same phrases, never
 * matches: those lines carry a `#N t.ttt` or progress prefix.
 */
const DOCKER_ITSELF = new RegExp(
  '^(?:docker: |ERROR: |Error response from daemon: )?(?:' + [
    'failed to connect to the docker API',
    'Cannot connect to the Docker daemon',
    'error during connect',
    // Docker 28 reworded this from "the Docker daemon socket" to "the docker API"; Engine 20 said "Got".
    '(?:Got )?permission denied while trying to connect to the docker (?:API|daemon socket)',
    // Docker Desktop while its engine is still starting.
    'Bad response from Docker engine',
    'request returned (?:500 )?Internal Server Error for API route',
    String.raw`dial unix \S*docker\S*\.sock: connect: connection refused`,
    String.raw`'[^']+' is not a docker command`,
    String.raw`unknown command: docker\b`
  ].join('|') + ')',
  'i'
);

const trimmedLines = (text: string): string[] => text.split('\n').map((line) => line.trim()).filter(Boolean);

/** The line, if any, on which Docker says it is the problem — the same line that gets reported. */
export function dockerComplaint(stderr: string): string | undefined {
  return trimmedLines(stderr).find((line) => DOCKER_ITSELF.test(line));
}

/**
 * Runs `docker`. A `docker` that cannot start, or one that answers that it
 * cannot reach its daemon or lacks a plugin, is Docker being unavailable —
 * reported as such whatever the verb and whatever `reject` the caller chose,
 * since no exit code of the request describes it. Only then is a rejecting
 * caller's failure thrown, exactly as the runner itself would.
 */
export async function runDocker(
  runner: CommandRunner,
  args: readonly string[],
  options?: Omit<RunOptions, 'cwd'>
): Promise<RunResult> {
  let result: RunResult;
  try {
    // Never let the runner reject first: Docker's own complaint must be read before the request's verdict.
    result = await runner.run('docker', args, { ...options, reject: false });
  } catch (error) {
    if (error instanceof CommandStartError) {
      // 127 is the runner's verdict for "not found"; every other start failure keeps its own words.
      throw new DockerUnavailableError(error.exitCode === 127
        ? 'the `docker` command is not on PATH (ENOENT); is Docker installed?'
        : `the \`docker\` command ${error.detail}`);
    }
    throw error;
  }
  const complaint = result.exitCode === 0 ? undefined : dockerComplaint(result.stderr);
  if (complaint !== undefined) throw new DockerUnavailableError(complaint);
  if ((result.exitCode !== 0 || result.timedOut || result.outputOverflow) && options?.reject !== false) {
    throw commandFailure('docker', args, result, options);
  }
  return result;
}

/** Lines Docker prints around the reason, never as it: warnings before, help hints after. */
const NOISE = /^(WARN(ING)?\b|time=.*\blevel=(warn|warning|info|debug)\b|(see|run) 'docker\b[^']*--help')/i;

/**
 * The line of Docker's stderr that says what went wrong, for callers that
 * report a failed request themselves: the first one that is neither a
 * warning nor a hint.
 */
export function dockerSaid(stderr: string, fallback: string): string {
  const lines = trimmedLines(stderr);
  return lines.find((line) => !NOISE.test(line)) ?? lines.at(-1) ?? fallback;
}
