import { defineConfig } from 'vitest/config';

// Integration tests need a real Docker daemon, real Git worktrees or real
// cross-process locks. They are kept out of the default `pnpm test` run, which
// holds the hermetic unit suite and the CLI stream tests.
export default defineConfig({
  test: {
    include: ['tests/integration/**/*.integration.test.ts'],
    testTimeout: 30_000,
    // Docker suites stop and remove real containers in their hooks. A hook that
    // times out is not cancelled: it keeps running into the next test.
    hookTimeout: 60_000,
    // One `docker info` for the whole run; each Docker suite reads the answer with `inject('docker')`.
    globalSetup: ['tests/helpers/docker-setup.ts'],
    // The Docker suites share one host-local daemon. This serializes files
    // within the process; global setup locks out same-user peer processes.
    fileParallelism: false
  }
});
