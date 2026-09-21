import { access, mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { execa } from 'execa';
import { afterEach, describe, expect, it } from 'vitest';
import { silentLogger as logger } from '../../src/logger.ts';
import { ScopedLock } from '../../src/operation-lock.ts';

const temporaryDirectories: string[] = [];

async function temporaryDirectory(): Promise<string> {
  const directory = await mkdtemp(path.join(tmpdir(), 'berth-lock-integration-'));
  temporaryDirectories.push(directory);
  return directory;
}

async function waitForFile(file: string): Promise<void> {
  for (let attempt = 0; attempt < 100; attempt += 1) {
    try {
      await access(file);
      return;
    } catch {
      await new Promise((resolve) => setTimeout(resolve, 50));
    }
  }
  throw new Error(`Timed out waiting for ${file}.`);
}

afterEach(async () => {
  await Promise.all(
    temporaryDirectories.splice(0).map((directory) =>
      rm(directory, { recursive: true, force: true })
    )
  );
});

describe('development locking across processes', () => {
  it('rejects a contender without terminating the owner and allows a later session', async () => {
    const directory = await temporaryDirectory();
    const target = path.join(directory, 'development');
    const readyFile = path.join(directory, 'owner.ready');
    const fixture = new URL('./fixtures/hold-development-lock.ts', import.meta.url);
    const owner = execa(process.execPath, [fixture.pathname, target, readyFile], {
      cleanup: true,
      forceKillAfterDelay: 5_000,
      reject: false
    });

    try {
      await waitForFile(readyFile);
      expect(owner.pid).toBeTypeOf('number');
      expect(() => process.kill(owner.pid!, 0)).not.toThrow();
      const contender = new ScopedLock(target, 'integration contender', logger);

      await expect(contender.acquire()).rejects.toThrow(
        'Another process holds the integration contender lock.'
      );
      expect(() => process.kill(owner.pid!, 0)).not.toThrow();
    } finally {
      owner.kill('SIGTERM');
      await owner;
    }

    const successor = new ScopedLock(target, 'integration successor', logger);
    await expect(successor.run(() => Promise.resolve('acquired'))).resolves.toBe('acquired');
  });
});
