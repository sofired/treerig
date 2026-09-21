import { writeFile } from 'node:fs/promises';
import { silentLogger as logger } from '../../../src/logger.ts';
import { ScopedLock } from '../../../src/operation-lock.ts';

const [target, readyFile] = process.argv.slice(2);
if (!target || !readyFile) {
  throw new Error('Expected a lock target and ready-file path.');
}

const lock = new ScopedLock(target, 'integration development session', logger);
let stop: (() => void) | undefined;
const stopped = new Promise<void>((resolve) => {
  stop = resolve;
});
const keepAlive = setInterval(() => undefined, 1_000);

process.once('SIGINT', () => stop?.());
process.once('SIGTERM', () => stop?.());

await lock.acquire();
await writeFile(readyFile, `${process.pid}\n`, { mode: 0o600 });
await stopped;
clearInterval(keepAlive);
await lock.release();
