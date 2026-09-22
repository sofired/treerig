import { defineConfig } from 'vitest/config';

// Integration tests need a real Docker daemon, real Git worktrees or real
// cross-process locks. They are kept out of the default `pnpm test` run, which
// holds the hermetic unit suite and the CLI stream tests.
export default defineConfig({
  test: {
    include: ['tests/integration/**/*.integration.test.ts'],
    testTimeout: 30_000,
    // One `docker info` for the whole run; each Docker suite reads the answer with `inject('docker')`.
    globalSetup: ['tests/helpers/docker-setup.ts'],
    // The Docker suites share one daemon; one file's cleanup must never run
    // while another file's containers are still being asserted on.
    fileParallelism: false
  }
});
