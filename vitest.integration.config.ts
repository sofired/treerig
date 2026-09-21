import { defineConfig } from 'vitest/config';

// Integration tests spawn real subprocesses and hold real locks. They are kept
// out of the default `pnpm test` run so the unit suite stays fast and hermetic.
export default defineConfig({
  test: {
    include: ['tests/integration/**/*.integration.test.ts'],
    testTimeout: 30_000
  }
});
