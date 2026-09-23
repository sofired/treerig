import { execFile } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';
import { describe, expect, it } from 'vitest';

// Where a child's output lands is a property of file descriptors, which only
// a real process can show: a parent that runs a child through the runner, and
// whose own two streams are then read apart.

const run = promisify(execFile);
const RUNNER = fileURLToPath(new URL('../../src/process-runner.ts', import.meta.url));

async function parentRunning(printOutput: 'true' | "'stderr'"): Promise<{ stdout: string; stderr: string }> {
  const script = `
    import { ExecaRunner } from ${JSON.stringify(RUNNER)};
    const result = await new ExecaRunner().run(process.execPath, ['-e', 'console.log("child-out"); console.error("child-err")'], { printOutput: ${printOutput}, reject: false });
    if (result.stdout !== 'child-out' || result.stderr !== 'child-err') throw new Error('not captured: ' + JSON.stringify(result));
  `;
  return run(process.execPath, ['--input-type=module', '-e', script]);
}

describe('ExecaRunner printOutput', () => {
  it("tees both of a child's streams to the parent's stderr when asked to, and still captures them", async () => {
    const { stdout, stderr } = await parentRunning("'stderr'");
    expect(stdout).toBe('');
    expect(stderr).toContain('child-out');
    expect(stderr).toContain('child-err');
  });

  it("tees each stream to its own by default", async () => {
    const { stdout, stderr } = await parentRunning('true');
    expect(stdout).toBe('child-out\n');
    expect(stderr).toBe('child-err\n');
  });
});
