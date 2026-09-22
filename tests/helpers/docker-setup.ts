import { execa } from 'execa';
import type { TestProject } from 'vitest/node';
import { dockerSaid } from '../../src/docker.ts';

/**
 * Global setup for the integration run: asks `docker info` once, in the main
 * process (where a warning actually reaches the terminal), and tells every
 * file whether a daemon answered. Suites that need one skip themselves
 * otherwise — except when `BERTH_TEST_REQUIRE_DOCKER` is `1` (CI's Linux
 * leg): then a missing daemon fails the run rather than quietly passing it.
 */
const REQUIRE = ['1', 'true', 'yes', 'on'];
const IGNORE = ['0', 'false', 'no', 'off'];

export default async function setup(project: TestProject): Promise<void> {
  // Read before anything else, so a typo is reported on the machine where it was made.
  const knob = (process.env.BERTH_TEST_REQUIRE_DOCKER ?? '').trim();
  if (knob !== '' && ![...REQUIRE, ...IGNORE].includes(knob.toLowerCase())) {
    throw new Error(
      `Unrecognized BERTH_TEST_REQUIRE_DOCKER=${knob}. Use ${REQUIRE.join('/')} to require Docker, ` +
      `${IGNORE.join('/')} for the default, or leave it unset.`
    );
  }
  const result = await execa('docker', ['info'], { reject: false, timeout: 30_000 });
  const available = result.exitCode === 0;
  if (!available) {
    const said = result.timedOut
      ? '`docker info` did not answer within 30 s'
      : dockerSaid(result.stderr, result.shortMessage?.split('\n')[0] ?? `\`docker info\` exited ${result.exitCode}`);
    // A missing daemon must not quietly become a skip where the suites are required.
    if (REQUIRE.includes(knob.toLowerCase())) {
      throw new Error(`Docker is required here (BERTH_TEST_REQUIRE_DOCKER=${knob}) but is not available: ${said}`);
    }
    console.warn(`No Docker daemon answered \`docker info\` (${said}); the suites that need one will skip.`);
  }
  project.provide('docker', available);
}

declare module 'vitest' {
  export interface ProvidedContext {
    docker: boolean;
  }
}
