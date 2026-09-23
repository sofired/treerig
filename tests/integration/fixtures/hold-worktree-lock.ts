import { spawn } from 'node:child_process';
import { writeFile } from 'node:fs/promises';
import { silentLogger as logger } from '../../../src/logger.ts';
import { ScopedLock, worktreeLockTarget } from '../../../src/operation-lock.ts';

// Holds a worktree's operation lock the way `up` does, then waits to be
// killed or stopped — or, in `exit` mode, exits while still holding it. In
// `grandchild` mode it first leaves behind what a hook can: a process in its
// own session with the worktree's marker in its environment.

const [controlHome, repositoryId, worktreeId, readyFile, mode = 'hold'] = process.argv.slice(2);
if (!controlHome || !repositoryId || !worktreeId || !readyFile) {
  throw new Error('Expected a control home, repository and worktree UUIDs, and a ready-file path.');
}

const lock = new ScopedLock(worktreeLockTarget(controlHome, repositoryId, worktreeId), 'worktree main', logger, { holder: 'berth up' });
await lock.acquire();

let grandchild: number | undefined;
if (mode === 'grandchild') {
  const child = spawn(process.execPath, ['-e', 'setInterval(() => {}, 1_000)'], {
    detached: true,
    stdio: 'ignore',
    env: { ...process.env, BERTH_WORKTREE_ID: worktreeId }
  });
  child.unref();
  grandchild = child.pid;
}

await writeFile(readyFile, JSON.stringify({ pid: process.pid, grandchild }), { mode: 0o600 });
if (mode === 'exit') process.exit(0);
setInterval(() => undefined, 1_000);
