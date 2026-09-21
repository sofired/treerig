import { describe, expect, it } from 'vitest';
import { ExecaRunner } from '../../src/process-runner.ts';

// Spawns a real child: the guarantee under test is what the child sees, which
// a mocked execa cannot answer.
describe('ExecaRunner environment', () => {
  it('passes a supplied environment through verbatim instead of merging this process in', async () => {
    process.env.BERTH_TEST_LEAK = 'from-the-parent';
    const script = 'process.stdout.write(String("BERTH_TEST_LEAK" in process.env))';
    try {
      const verbatim = await new ExecaRunner().run(process.execPath, ['-e', script], { env: { PATH: process.env.PATH } });
      expect(verbatim.stdout).toBe('false');
      const inherited = await new ExecaRunner().run(process.execPath, ['-e', script]);
      expect(inherited.stdout).toBe('true');
    } finally {
      delete process.env.BERTH_TEST_LEAK;
    }
  });
});
