import { defineConfig } from 'vitest/config';

// `tests/cli` spawns the CLI as a process but needs nothing else — no Docker,
// no network — so it runs on every `pnpm check` beside the unit suite. What it
// proves, that a pipe gets the data and nothing else, cannot be proved without
// a real process.
export default defineConfig({
  test: {
    include: ['tests/unit/**/*.test.ts', 'tests/cli/**/*.test.ts'],
    testTimeout: 30_000
  }
});
